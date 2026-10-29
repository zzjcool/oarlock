package claude

// Operating capabilities for issue #23: the installation probe, Claude's own
// installer, native login/logout/status, self-update and the four inventory
// sections. Every command here is Claude's own (`claude --version`, the
// official install line, `claude auth *`, `claude update`) and runs through
// internal/ops, which always closes stdin: a `claude auth login` that would
// open a browser and wait for a prompt can only ever hand back a challenge
// line. Nothing here writes configuration; an installer or an updater is the
// vendor's own program and oarlock judges it by re-reading the
// installation/version afterwards.
//
// What is absent is as deliberate as what is present: no Models (this adapter
// reads no native model list) and no account usage (Claude reports it only over
// the control channel, a documented gap). The inventory is four explicit
// `unsupported` sections, never an empty `ok` list.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/ops"
)

const (
	// installLine is Claude's official installer, run by the shell. Claude
	// ships one release line, so there is nothing to select: ops.Installer
	// refuses a non-empty InstallRequest.Line instead of running a caller's
	// line.
	installLine = `curl -fsSL https://claude.ai/install.sh | bash`

	// authStatusTimeout bounds the read-only status read (OAR: 20 s).
	authStatusTimeout = 20 * time.Second
	// logoutTimeout bounds `claude auth logout` (OAR: 60 s).
	logoutTimeout = 60 * time.Second

	// outputTailLimit bounds how much vendor output an error carries.
	outputTailLimit = 400
)

// resolveBinary finds the claude executable the ops commands run: PATH first,
// then the official installer's ~/.local/bin. It is the same lookup the
// installation probe does, so Login/Logout/AuthStatus/Upgrade run the binary
// the probe would have reported instead of a second guess.
func resolveBinary() (string, error) {
	fallback := ops.FallbackPath(".local/bin/claude")
	path, _, ok := ops.Find("claude", fallback)
	if !ok {
		if fallback == "" {
			return "", errors.New("claude: no claude executable on PATH")
		}
		return "", fmt.Errorf("claude: no claude executable on PATH or at %s", fallback)
	}
	return path, nil
}

// outputTail is the bounded, last-resort fragment of vendor output an error
// carries. Internal/ops never forwards output to logs; an error message is the
// only place a caller can see why the vendor refused.
func outputTail(res ops.Result) string {
	out := strings.TrimSpace(res.Stderr)
	if out == "" {
		out = strings.TrimSpace(res.Stdout)
	}
	if out == "" {
		return "(no output)"
	}
	if len(out) > outputTailLimit {
		out = "..." + out[len(out)-outputTailLimit:]
	}
	return out
}

// installer runs Claude's official install script. The plan is pure and the
// install is judged by the probe, never by the pipeline's exit code: a
// `curl | bash` can exit 0 without installing anything.
type installer struct{}

var _ oarlock.Installer = installer{}

// plan is the one configuration every method shares: one release line, the
// installation probe as the judge.
func (installer) plan() ops.Installer {
	return ops.Installer{Line: installLine, Probe: installationProbe()}
}

// InstallPlan returns the command that would run, without running it. A
// non-empty InstallRequest.Line is refused (typed UnsupportedOperation): there
// is no per-line selection for Claude.
func (i installer) InstallPlan(ctx context.Context, req oarlock.InstallRequest) (oarlock.InstallPlan, error) {
	return i.plan().InstallPlan(ctx, req)
}

// Install runs the official line once, then re-probes. An already available
// installation returns without executing anything, so a retry after a failure
// is safe and nothing is recorded locally between attempts.
func (i installer) Install(ctx context.Context, req oarlock.InstallRequest) (oarlock.InstallResult, error) {
	return i.plan().Install(ctx, req)
}

// loginProvider is Claude's own auth surface. Login relays the challenge URL
// and stops the helper; it never writes stdin and never claims the sign-in
// completed — AuthStatus is the only completion source.
type loginProvider struct{}

var _ oarlock.LoginProvider = loginProvider{}

// loginURLPattern is what counts as a challenge line in `claude auth login`
// output: the OAuth URL, up to the first space.
var loginURLPattern = regexp.MustCompile(`https?://\S+`)

// loginEnv is base with BROWSER=true, so the vendor CLI does not try to launch
// a browser on the host that asked for a URL. An inherited BROWSER is replaced,
// not appended to (the last value would win otherwise).
func loginEnv(base []string) []string {
	out := make([]string, 0, len(base)+1)
	for _, kv := range base {
		if k, _, _ := strings.Cut(kv, "="); k == "BROWSER" {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "BROWSER=true")
}

// Login starts `claude auth login`, returns the first URL as the challenge and
// stops the helper. Claude's own output is never logged or returned: the
// challenge is the whole answer.
func (p loginProvider) Login(ctx context.Context) (oarlock.LoginChallenge, error) {
	path, err := resolveBinary()
	if err != nil {
		return oarlock.LoginChallenge{}, err
	}
	challenge, _, err := ops.CaptureLogin(ctx, ops.Command{
		Path: path,
		Args: []string{"auth", "login"},
		Env:  loginEnv(os.Environ()),
	}, loginURLPattern, 0)
	if err != nil {
		return oarlock.LoginChallenge{}, fmt.Errorf("claude: login: %w", err)
	}
	return challenge, nil
}

// Logout runs `claude auth logout` and then re-reads the status: a zero exit is
// not evidence that the credential is gone, so a still-authenticated answer is
// an error rather than a claim of having signed out.
func (p loginProvider) Logout(ctx context.Context) error {
	path, err := resolveBinary()
	if err != nil {
		return err
	}
	res, err := ops.Run(ctx, ops.Command{Path: path, Args: []string{"auth", "logout"}}, logoutTimeout)
	if err != nil {
		return fmt.Errorf("claude: logout: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("claude: logout: exit %d: %s", res.ExitCode, outputTail(res))
	}
	status, err := p.AuthStatus(ctx)
	if err != nil {
		// Unknown is not success: the sign-out is unconfirmed.
		return fmt.Errorf("claude: logout: could not confirm the sign-out: %w", err)
	}
	if status.Authenticated {
		return errors.New("claude: logout exited 0 but claude is still logged in")
	}
	return nil
}

// authStatusJSON is `claude auth status --json`. LoggedIn is a pointer so a
// missing field is an error instead of a silent "logged out".
type authStatusJSON struct {
	LoggedIn         *bool  `json:"loggedIn"`
	Email            string `json:"email"`
	SubscriptionType string `json:"subscriptionType"`
	AuthMethod       string `json:"authMethod"`
}

// AuthStatus asks Claude whether this machine is signed in. Exit 1 is Claude's
// documented "not signed in"; anything else non-zero, an unreadable body or a
// missing loggedIn field is an error, because a timeout or a crash says nothing
// about the credential and must never be reported as logged out.
func (p loginProvider) AuthStatus(ctx context.Context) (oarlock.AuthStatus, error) {
	path, err := resolveBinary()
	if err != nil {
		return oarlock.AuthStatus{}, err
	}
	res, err := ops.Run(ctx, ops.Command{Path: path, Args: []string{"auth", "status", "--json"}}, authStatusTimeout)
	if err != nil {
		return oarlock.AuthStatus{}, fmt.Errorf("claude: auth status: %w", err)
	}
	switch res.ExitCode {
	case 0:
	case 1:
		return oarlock.AuthStatus{}, nil
	default:
		return oarlock.AuthStatus{}, fmt.Errorf("claude: auth status: exit %d: %s", res.ExitCode, outputTail(res))
	}
	var body authStatusJSON
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &body); err != nil {
		return oarlock.AuthStatus{}, fmt.Errorf("claude: auth status: unreadable JSON (%v): %s", err, outputTail(res))
	}
	if body.LoggedIn == nil {
		return oarlock.AuthStatus{}, fmt.Errorf("claude: auth status JSON has no loggedIn field: %s", outputTail(res))
	}
	status := oarlock.AuthStatus{Authenticated: *body.LoggedIn, Account: body.Email, Detail: body.SubscriptionType}
	if status.Detail == "" {
		status.Detail = body.AuthMethod
	}
	return status, nil
}

// upgrader is Claude's own self-update. There is no check-only command: the
// update check is an HTTP release lookup, so CheckUpdate is a typed
// UnsupportedOperation and Upgrade is the only path that reports a version.
type upgrader struct{}

var _ oarlock.Upgrader = upgrader{}

// checkUpdateReason is the gap, in one line, for the host's log.
const checkUpdateReason = "claude's update check is an HTTP release lookup, not a CLI command; Upgrade re-reads --version"

// CheckUpdate is unsupported, typed, so a host branches with errors.As instead
// of parsing prose. It is never a fake "up to date".
func (upgrader) CheckUpdate(context.Context) (oarlock.UpdateInfo, error) {
	return oarlock.UpdateInfo{}, &oarlock.UnsupportedOperation{Operation: "check-update", Reason: checkUpdateReason}
}

// Upgrade runs `claude update` and lets internal/ops judge it by reading
// `--version` before and after: the same version is ops.ErrVersionUnchanged
// even when the updater exits 0.
func (upgrader) Upgrade(ctx context.Context) error {
	path, err := resolveBinary()
	if err != nil {
		return err
	}
	if _, err := ops.Upgrade(ctx, path, []string{"update"}, ops.UpgradeTimeout); err != nil {
		return fmt.Errorf("claude: upgrade: %w", err)
	}
	return nil
}

// inventories answers the four-section inventory question for Claude. This
// adapter does not read Claude's native skills/commands/tools inventories (and
// its model list is not a local read), so every section is explicitly
// `unsupported` with the reason naming the gap. It is never an empty `ok` list
// (which would claim "nothing installed") and never an error (which would claim
// the question was asked and failed).
type inventories struct{}

var _ oarlock.InventoryReader = inventories{}

// Inventory returns the four sections in the fixed order models, skills,
// commands, tools.
func (inventories) Inventory(context.Context) (oarlock.Inventory, error) {
	return oarlock.Inventory{Sections: []oarlock.InventorySection{
		{
			Kind:   oarlock.InventoryModels,
			Status: oarlock.InventoryUnsupported,
			Reason: "this adapter does not read Claude's native model inventory yet",
		},
		{
			Kind:   oarlock.InventorySkills,
			Status: oarlock.InventoryUnsupported,
			Reason: "this adapter does not read Claude's native skills inventory yet",
		},
		{
			Kind:   oarlock.InventoryCommands,
			Status: oarlock.InventoryUnsupported,
			Reason: "this adapter does not read Claude's native commands inventory yet",
		},
		{
			Kind:   oarlock.InventoryTools,
			Status: oarlock.InventoryUnsupported,
			Reason: "this adapter does not read Claude's native context/MCP inventory yet (tool names are only visible inside a live session)",
		},
	}}, nil
}
