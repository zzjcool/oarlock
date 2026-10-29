package cursor

// ops.go — the operating capabilities of the Cursor CLI (issue #23).
//
// cursor-agent ships its own auth, its own updater and a model list, so the
// Runtime declares exactly those and nothing else. Every command goes through
// internal/ops, which is the only place in oarlock that starts a vendor CLI for
// an operation: stdin is closed before a prompt can block, the whole process
// tree is owned and killed on timeout, and no output is forwarded anywhere
// except into a bounded error excerpt.
//
// The matrix this file implements (docs/ops.md):
//
//   - Models: `cursor-agent models` is a read-only list, `ID - Name` per line.
//     An unparsable line or an empty list is an error, never silently accepted.
//   - Login: `cursor-agent login` with NO_OPEN_BROWSER=1; the URL is relayed and
//     the helper is stopped. No stdin is ever written, and the challenge is not
//     a claim that the sign-in happened — AuthStatus is the only completion
//     source. Logout is `cursor-agent logout`, read back through AuthStatus so a
//     CLI that exits 0 without ending the session is an error.
//   - Upgrade: `cursor-agent update` has no check-only sibling, so CheckUpdate
//     is a typed *oarlock.UnsupportedOperation; Upgrade is judged only by a
//     `--version` read-back, and an unchanged version is ErrVersionUnchanged.
//   - Inventory: models come from the CLI; skills/commands/tools have no CLI
//     surface at all and are explicit `unsupported` sections, never empty lists.
//   - Install: absent (the host installs the CLI; oarlock has no installer line
//     for it). Usage: absent (the CLI exposes no account quota).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/ops"
)

const (
	// execName is the vendor CLI this adapter drives; it is resolved on PATH.
	execName = "cursor-agent"

	// modelsTimeout bounds `cursor-agent models`.
	modelsTimeout = 15 * time.Second
	// statusTimeout bounds `cursor-agent status --format json`.
	statusTimeout = 20 * time.Second
	// logoutTimeout bounds `cursor-agent logout`.
	logoutTimeout = 60 * time.Second

	excerptLimit = 400
)

// loginURL is the challenge `cursor-agent login` prints before it would open a
// browser. With NO_OPEN_BROWSER=1 the URL is all that has to be relayed.
var loginURL = regexp.MustCompile(`https?://\S+`)

// The operating capabilities this package implements. Installer and
// AccountUsageReader are deliberately absent: nil is the capability.
var (
	_ oarlock.ModelLister     = cursorModels{}
	_ oarlock.LoginProvider   = cursorLogin{}
	_ oarlock.Upgrader        = cursorUpgrade{}
	_ oarlock.InventoryReader = cursorInventory{}
)

// findBinary resolves cursor-agent on PATH. There is no known fallback
// location: the host installs the CLI wherever it likes.
func findBinary() (string, error) {
	path, _, ok := ops.Find(execName)
	if !ok {
		return "", fmt.Errorf("%s: not found on PATH (install the Cursor CLI first)", execName)
	}
	return path, nil
}

// ---- models ------------------------------------------------------------------

// cursorModels lists what `cursor-agent models` reports.
type cursorModels struct{}

func (cursorModels) ListModels(ctx context.Context) ([]oarlock.Model, error) {
	path, err := findBinary()
	if err != nil {
		return nil, err
	}
	res, err := ops.Run(ctx, ops.Command{Path: path, Args: []string{"models"}}, modelsTimeout)
	if err != nil {
		return nil, fmt.Errorf("%s models: %w", execName, err)
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("%s models: exit %d: %s", execName, res.ExitCode, excerpt(res.Stdout+"\n"+res.Stderr))
	}
	return parseModels(res.Stdout)
}

// parseModels reads the `models` listing: a blank line, the `Available models`
// header, then one `ID - Name` model per line. A line that is neither blank
// nor a model is an error (never skipped): silently dropping a line would turn
// a changed CLI output into a shorter, plausible-looking list.
func parseModels(out string) ([]oarlock.Model, error) {
	var models []oarlock.Model
	for i, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.EqualFold(line, "Available models") {
			continue
		}
		id, name, ok := strings.Cut(line, " - ")
		id, name = strings.TrimSpace(id), strings.TrimSpace(name)
		if !ok || id == "" || name == "" {
			return nil, fmt.Errorf("%s models: line %d is not \"ID - Name\": %q", execName, i+1, line)
		}
		models = append(models, oarlock.Model{ID: id, Name: name})
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("%s models listed no models", execName)
	}
	return models, nil
}

// ---- login -------------------------------------------------------------------

// cursorLogin is the CLI's own sign-in: URL relay, logout, and the status read
// that alone decides whether a machine is signed in.
type cursorLogin struct{}

func (cursorLogin) Login(ctx context.Context) (oarlock.LoginChallenge, error) {
	path, err := findBinary()
	if err != nil {
		return oarlock.LoginChallenge{}, err
	}
	// NO_OPEN_BROWSER=1 keeps the CLI from trying to open a browser on the
	// host; the URL it would have opened is the challenge. CaptureLogin closes
	// stdin, so a CLI that waits for a prompt gets EOF, not a hung host.
	env := append(append([]string(nil), os.Environ()...), "NO_OPEN_BROWSER=1")
	challenge, _, err := ops.CaptureLogin(ctx,
		ops.Command{Path: path, Args: []string{"login"}, Env: env},
		loginURL, ops.LoginTimeout)
	if err != nil {
		return oarlock.LoginChallenge{}, fmt.Errorf("%s login: %w", execName, err)
	}
	return challenge, nil
}

// Logout ends the CLI's session and then re-reads AuthStatus: an exit 0 that
// leaves the machine signed in is a failure of the logout, not a success.
func (cursorLogin) Logout(ctx context.Context) error {
	path, err := findBinary()
	if err != nil {
		return err
	}
	res, err := ops.Run(ctx, ops.Command{Path: path, Args: []string{"logout"}}, logoutTimeout)
	if err != nil {
		return fmt.Errorf("%s logout: %w", execName, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("%s logout: exit %d: %s", execName, res.ExitCode, excerpt(res.Stdout+"\n"+res.Stderr))
	}
	status, err := (cursorLogin{}).AuthStatus(ctx)
	if err != nil {
		return fmt.Errorf("%s logout: verify: %w", execName, err)
	}
	if status.Authenticated {
		return fmt.Errorf("%s logout exited 0 but the account is %w (account %q)", execName, errStillLoggedIn, status.Account)
	}
	return nil
}

// AuthStatus reads `cursor-agent status --format json`. The JSON's
// isAuthenticated is required: a status the adapter cannot parse must never be
// reported as "signed out", so a missing key, a non-JSON body or a failed
// command is an error.
func (cursorLogin) AuthStatus(ctx context.Context) (oarlock.AuthStatus, error) {
	path, err := findBinary()
	if err != nil {
		return oarlock.AuthStatus{}, err
	}
	res, err := ops.Run(ctx, ops.Command{Path: path, Args: []string{"status", "--format", "json"}}, statusTimeout)
	if err != nil {
		return oarlock.AuthStatus{}, fmt.Errorf("%s status: %w", execName, err)
	}
	body := jsonObject(res.Stdout)
	if body == "" {
		return oarlock.AuthStatus{}, fmt.Errorf("%s status --format json: exit %d, no JSON in output: %s", execName, res.ExitCode, excerpt(res.Stdout+"\n"+res.Stderr))
	}
	var status struct {
		IsAuthenticated *bool  `json:"isAuthenticated"`
		Status          string `json:"status"`
		UserInfo        *struct {
			Email string `json:"email"`
		} `json:"userInfo"`
	}
	if err := json.Unmarshal([]byte(body), &status); err != nil {
		return oarlock.AuthStatus{}, fmt.Errorf("%s status --format json: %w (output: %s)", execName, err, excerpt(res.Stdout))
	}
	if status.IsAuthenticated == nil {
		return oarlock.AuthStatus{}, fmt.Errorf("%s status --format json: JSON has no isAuthenticated key: %s", execName, excerpt(res.Stdout))
	}
	out := oarlock.AuthStatus{Authenticated: *status.IsAuthenticated, Detail: status.Status}
	if status.UserInfo != nil {
		out.Account = status.UserInfo.Email
	}
	return out, nil
}

// jsonObject returns the first JSON object in s, so a CLI that prints a
// warning line before the payload still parses. "" means s holds no object.
func jsonObject(s string) string {
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start < 0 || end < start {
		return ""
	}
	return s[start : end+1]
}

// ---- upgrade -----------------------------------------------------------------

// cursorUpgrade is `cursor-agent update` judged by a version read-back.
type cursorUpgrade struct{}

// CheckUpdate is unsupported, not absent: cursor-agent has no command that only
// asks whether an update exists, and guessing from the current version would be
// a made-up answer.
func (cursorUpgrade) CheckUpdate(context.Context) (oarlock.UpdateInfo, error) {
	return oarlock.UpdateInfo{}, &oarlock.UnsupportedOperation{
		Operation: "check-update",
		Reason:    "cursor-agent has no check-only update command; Upgrade re-reads --version",
	}
}

// Upgrade runs the vendor's own updater. The version read-back decides: the
// same version is ops.ErrVersionUnchanged even when `update` exited 0.
func (cursorUpgrade) Upgrade(ctx context.Context) error {
	path, err := findBinary()
	if err != nil {
		return err
	}
	if _, err := ops.Upgrade(ctx, path, []string{"update"}, ops.UpgradeTimeout); err != nil {
		return fmt.Errorf("%s update: %w", execName, err)
	}
	return nil
}

// ---- inventory ---------------------------------------------------------------

// cursorInventory answers "what can this runtime run now?". Models come from
// the CLI; the other three sections are explicit `unsupported`, because
// cursor-agent exposes no such inventory and an empty list would read as "none".
type cursorInventory struct{}

func (cursorInventory) Inventory(ctx context.Context) (oarlock.Inventory, error) {
	return oarlock.Inventory{Sections: []oarlock.InventorySection{
		modelsSection(ctx),
		{
			Kind:   oarlock.InventorySkills,
			Status: oarlock.InventoryUnsupported,
			Reason: "cursor-agent exposes no skill inventory: its rules live in the user's Cursor configuration and have no CLI listing",
		},
		{
			Kind:   oarlock.InventoryCommands,
			Status: oarlock.InventoryUnsupported,
			Reason: "cursor-agent exposes no command inventory: it is a prompt CLI, not a slash-command host",
		},
		{
			Kind:   oarlock.InventoryTools,
			Status: oarlock.InventoryUnsupported,
			Reason: "cursor-agent exposes no tool/MCP inventory: its tools are chosen per run and never listed by the CLI",
		},
	}}, nil
}

// modelsSection is the one readable inventory. A failed read is an `error`
// section with the reason, so the other three sections still answer.
func modelsSection(ctx context.Context) oarlock.InventorySection {
	section := oarlock.InventorySection{Kind: oarlock.InventoryModels, Status: oarlock.InventoryOK}
	models, err := (cursorModels{}).ListModels(ctx)
	if err != nil {
		section.Status = oarlock.InventoryError
		section.Reason = err.Error()
		return section
	}
	for _, model := range models {
		section.Items = append(section.Items, oarlock.InventoryItem{
			Name:        model.ID,
			Source:      execName + " models",
			Description: model.Name,
		})
	}
	return section
}

// ---- errors ------------------------------------------------------------------

// excerpt bounds a CLI output for an error message: enough to recognise the
// failure, never a transcript.
func excerpt(s string) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= excerptLimit {
		return s
	}
	return string([]rune(s)[:excerptLimit]) + "…"
}

// errStillLoggedIn is Logout's own failure: the CLI said it signed out and the
// status read says otherwise. It is never reported as a successful logout.
var errStillLoggedIn = errors.New("still logged in")
