package codex

// Codex's operating capabilities (issue #23, docs/ops.md): the installation
// probe, OpenAI's official standalone installer, sign-out and auth status,
// self-update, and the four inventories.
//
// What codex cannot do through this adapter is declared, never faked:
//
//   - Login is a *oarlock.UnsupportedOperation. Codex's sign-in is the
//     app-server's account/login/start flow (chatgptDeviceCode), which this
//     adapter does not drive, and `codex login` clears the stored credentials
//     before the new sign-in completes, so it is not run here at all.
//   - All four inventories are unsupported sections: models, skills and MCP
//     tools are app-server requests (model/list, skills/list,
//     mcpServerStatus/list) and codex has no command catalog, so no CLI
//     command can answer them.
//
// The two update surfaces are codex's own: CheckUpdate reads the `updates.status`
// row of `codex doctor --json` (the JSON decides, never doctor's exit code,
// which is 1 whenever any other row fails), and Upgrade runs `codex update` and
// re-reads `--version`, because the updater can report success after a failed
// download.
//
// Everything that does run goes through internal/ops: no stdin, a bounded
// time, and the vendor's exit code is data rather than proof of success.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/ops"
)

const (
	// executableName is the CLI the probe and every operating command run.
	executableName = "codex"
	// installFallback is where the standalone installer links the binary (its
	// default $CODEX_INSTALL_DIR). The installer adds that directory to the
	// shell profile, which a running host does not read, so the probe looks
	// there too.
	installFallback = ".local/bin/codex"
	// installLine is OpenAI's documented standalone installer. install.sh's own
	// usage text documents CODEX_NON_INTERACTIVE=1 as the switch that skips its
	// prompts (start codex now, remove an npm or Homebrew copy).
	installLine = `curl -fsSL https://chatgpt.com/codex/install.sh | CODEX_NON_INTERACTIVE=1 sh`

	// loginStatusTimeout bounds `codex login status` (OAR uses 20 s).
	loginStatusTimeout = 20 * time.Second
	// logoutTimeout bounds `codex logout`: its refresh-token revoke has 10 s.
	logoutTimeout = 60 * time.Second

	// The status lines `codex login status` prints on stderr. Exit 0 with a
	// line oarlock does not know is an error, never "signed out".
	chatgptStatus     = "Logged in using ChatGPT"
	apiKeyStatus      = "Logged in using an API key"
	apiKeyConfigured  = "API key configured"
	notLoggedInStatus = "Not logged in"

	// loginReason is the typed refusal Login returns.
	loginReason = "codex device-code login needs the app-server account/login/start flow, which this adapter does not drive; run `codex login` in a terminal"

	// checkTimeout bounds `codex doctor --json`. The verdict row probes the
	// release channel, so the bound is generous; a host whose network blocks
	// that probe still answers quickly (codex reports the HTTP failure itself).
	checkTimeout = 60 * time.Second

	// The `codex doctor --json` row and detail keys this adapter reads. The
	// report mixes strings, arrays and booleans inside one `details` map, so
	// every value is type-asserted instead of decoded into a fixed struct.
	updateStatusRow     = "updates.status"
	latestVersionKey    = "latest version"
	latestVersionProbe  = "latest version probe"
	latestVersionStatus = "latest version status"
	updateActionKey     = "update action"

	// manualUpdateAction is doctor's answer for a copy `codex update` cannot
	// update (a copied binary, an app bundle).
	manualUpdateAction = "manual or unknown"

	// The two verdicts doctor puts in `latest version status`.
	newerVersionAvailable = "newer version is available"
	notOlderVerdict       = "not older"

	// outputTail bounds the vendor output kept in an error.
	outputTail = 300
)

// codexProbe is the installation probe (`codex --version`, then the installer's
// known location). It is built per call so the home directory is read when the
// host asks, not when the package loads.
func codexProbe() ops.Probe {
	return ops.Probe{Name: executableName, Fallbacks: []string{ops.FallbackPath(installFallback)}}
}

// installer is codex's own standalone installer. The plan is pure and Install
// re-probes, so `curl | sh` exiting 0 is not success on its own.
var installer = ops.Installer{
	Line:  installLine,
	Probe: codexProbe(),
}

// findCodex locates the executable the operating commands run: the same lookup
// the probe does, so a host that probed and a host that runs agree on the path.
func findCodex() (string, error) {
	fallback := ops.FallbackPath(installFallback)
	path, _, ok := ops.Find(executableName, fallback)
	if ok {
		return path, nil
	}
	if fallback == "" {
		return "", fmt.Errorf("codex: `%s` is not on PATH", executableName)
	}
	return "", fmt.Errorf("codex: `%s` is not on PATH and not at %s", executableName, fallback)
}

// loginProvider is codex's sign-out and auth status. Login itself is refused:
// see loginReason.
type loginProvider struct{}

var _ oarlock.LoginProvider = loginProvider{}

// Login is a typed refusal, and it starts nothing. Codex's sign-in is the
// app-server's account/login/start flow; `codex login` is not an alternative,
// because it clears the stored credentials before the new sign-in completes.
func (loginProvider) Login(context.Context) (oarlock.LoginChallenge, error) {
	return oarlock.LoginChallenge{}, &oarlock.UnsupportedOperation{Operation: "login", Reason: loginReason}
}

// Logout runs `codex logout` and then reads the status back: an exit 0 that
// leaves the machine signed in is a failure, not a success.
func (loginProvider) Logout(ctx context.Context) error {
	path, err := findCodex()
	if err != nil {
		return err
	}
	res, err := ops.Run(ctx, ops.Command{Path: path, Args: []string{"logout"}}, logoutTimeout)
	if err != nil {
		return fmt.Errorf("codex: `codex logout` did not finish: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("codex: `codex logout` exited %d: %s", res.ExitCode, clipOutput(res.Stderr+"\n"+res.Stdout))
	}
	status, err := loginProvider{}.AuthStatus(ctx)
	if err != nil {
		return fmt.Errorf("codex: `codex logout` exited 0, but `codex login status` cannot confirm it: %w", err)
	}
	if status.Authenticated {
		return fmt.Errorf("codex: `codex logout` exited 0, yet `codex login status` still reads signed in (%s); the credential store was not cleared", status.Account)
	}
	return nil
}

// AuthStatus reads `codex login status`, which prints on stderr: exit 0 with
// "Logged in using ChatGPT" or an API-key line is signed in, exit 1 with "Not
// logged in" is signed out. Any other answer (a broken configuration, a crash,
// a timeout) is an error: a runtime that cannot tell must never be reported as
// signed out.
func (loginProvider) AuthStatus(ctx context.Context) (oarlock.AuthStatus, error) {
	path, err := findCodex()
	if err != nil {
		return oarlock.AuthStatus{}, err
	}
	res, err := ops.Run(ctx, ops.Command{Path: path, Args: []string{"login", "status"}}, loginStatusTimeout)
	if err != nil {
		return oarlock.AuthStatus{}, fmt.Errorf("codex: `codex login status` did not finish: %w", err)
	}
	// stdout is read too, so a wrapper that relays the status cannot turn an
	// answer into "no answer".
	text := res.Stderr + "\n" + res.Stdout
	switch {
	case res.ExitCode == 0 && strings.Contains(text, chatgptStatus):
		return oarlock.AuthStatus{
			Authenticated: true,
			Account:       "chatgpt",
			Detail:        "Logged in using ChatGPT: the credential is the signed-in account and metering follows its plan",
		}, nil
	case res.ExitCode == 0 && (strings.Contains(text, apiKeyStatus) || strings.Contains(text, apiKeyConfigured)):
		return oarlock.AuthStatus{
			Authenticated: true,
			Account:       "api-key",
			Detail:        "Logged in with an API key: the credential is the key, and `codex login status` does not say which provider bills it",
		}, nil
	case res.ExitCode == 1 && strings.Contains(text, notLoggedInStatus):
		return oarlock.AuthStatus{}, nil
	default:
		return oarlock.AuthStatus{}, fmt.Errorf("codex: `codex login status` exited %d and oarlock reads no status in it: %s", res.ExitCode, clipOutput(text))
	}
}

// upgrader is codex's self-update: the check reads the CLI's own doctor report,
// and the upgrade is judged by the version read-back, never by `codex update`'s
// exit code.
type upgrader struct{}

var _ oarlock.Upgrader = upgrader{}

// doctorReport is the part of `codex doctor --json` this adapter reads: the
// rows keyed by check name. Everything else in the report is ignored, because
// one row carries the update verdict.
type doctorReport struct {
	Checks map[string]struct {
		Details map[string]any `json:"details"`
	} `json:"checks"`
}

// detailString returns one `details` value as a trimmed string, "" when the key
// is absent or holds something else: doctor reports strings, arrays and
// booleans in the same map.
func detailString(details map[string]any, key string) string {
	value, _ := details[key].(string)
	return strings.TrimSpace(value)
}

// versionToken reduces a `--version` line to the release it names: the last
// word without a leading `v` ("codex-cli 0.165.0" -> "0.165.0"). Codex prints a
// product prefix, and doctor compares bare versions.
func versionToken(line string) string {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return ""
	}
	return strings.TrimPrefix(fields[len(fields)-1], "v")
}

// jsonObject returns the outermost JSON object in s, so a warning line printed
// before the report does not turn an answer into no answer. "" means s holds
// no object.
func jsonObject(s string) string {
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start < 0 || end < start {
		return ""
	}
	return s[start : end+1]
}

// CheckUpdate reads the update verdict the CLI prints itself: the
// `updates.status` row of `codex doctor --json` (codex 0.165.0). doctor exits 1
// whenever any other row fails (no login, a blocked network probe), so the JSON
// decides and the exit code is only reported inside an error. A report with no
// verdict is an error, never a fabricated "up to date".
func (upgrader) CheckUpdate(ctx context.Context) (oarlock.UpdateInfo, error) {
	path, err := findCodex()
	if err != nil {
		return oarlock.UpdateInfo{}, err
	}
	read, err := ops.Version(ctx, path)
	if err != nil {
		return oarlock.UpdateInfo{}, fmt.Errorf("codex: read the installed version: %w", err)
	}
	current := versionToken(read)
	if current == "" {
		return oarlock.UpdateInfo{}, fmt.Errorf("codex: `codex --version` gave no version token: %q", clipOutput(read))
	}
	res, err := ops.Run(ctx, ops.Command{Path: path, Args: []string{"doctor", "--json"}}, checkTimeout)
	if err != nil {
		return oarlock.UpdateInfo{}, fmt.Errorf("codex: `codex doctor --json` did not finish: %w", err)
	}
	body := jsonObject(res.Stdout)
	if body == "" {
		return oarlock.UpdateInfo{}, fmt.Errorf("codex: `codex doctor --json` printed no JSON (exit %d): %s", res.ExitCode, clipOutput(res.Stdout+"\n"+res.Stderr))
	}
	var report doctorReport
	if err := json.Unmarshal([]byte(body), &report); err != nil {
		return oarlock.UpdateInfo{}, fmt.Errorf("codex: `codex doctor --json` printed unreadable JSON (%v): %s", err, clipOutput(body))
	}
	details := report.Checks[updateStatusRow].Details
	if details == nil {
		return oarlock.UpdateInfo{}, fmt.Errorf("codex: `codex doctor --json` has no %q row: the update verdict is unknown", updateStatusRow)
	}
	latest := detailString(details, latestVersionKey)
	if latest == "" {
		// The detail is the whole explanation: a blocked probe reports
		// "latest version probe": "HTTP 403 Forbidden" and no version at all.
		probe := detailString(details, latestVersionProbe)
		if probe == "" {
			probe = "no probe detail"
		}
		return oarlock.UpdateInfo{}, fmt.Errorf("codex: `codex doctor --json` reports no %q (%s: %s)", latestVersionKey, latestVersionProbe, clipOutput(probe))
	}
	// An unmanaged copy (a copied binary, an app bundle) is the one answer that
	// is a refusal rather than a comparison: `codex update` cannot move it, so
	// a check claiming "up to date" would be a lie.
	if action := detailString(details, updateActionKey); action == manualUpdateAction {
		return oarlock.UpdateInfo{}, fmt.Errorf("codex: `codex doctor --json` reports update action %q: `codex update` cannot update this copy", action)
	}
	// The vendor's own verdict wins; the version comparison is the fallback for
	// a report that carries a latest version but no verdict.
	available := latest != current
	status := detailString(details, latestVersionStatus)
	if strings.Contains(status, newerVersionAvailable) {
		available = true
	} else if strings.Contains(status, notOlderVerdict) {
		available = false
	}
	return oarlock.UpdateInfo{Available: available, Current: current, Latest: latest}, nil
}

// Upgrade runs `codex update` and lets internal/ops compare `--version` before
// and after: the same version is ops.ErrVersionUnchanged even on exit 0.
func (upgrader) Upgrade(ctx context.Context) error {
	path, err := findCodex()
	if err != nil {
		return err
	}
	if _, err := ops.Upgrade(ctx, path, []string{"update"}, ops.UpgradeTimeout); err != nil {
		return fmt.Errorf("codex: `codex update`: %w", err)
	}
	return nil
}

// inventories is the four native inventories. None of them is readable through
// the CLI this adapter drives, and an unsupported section is declared per kind:
// an empty ok list would claim codex has nothing.
type inventories struct{}

var _ oarlock.InventoryReader = inventories{}

// Inventory returns four unsupported sections with the gap named in each.
func (inventories) Inventory(context.Context) (oarlock.Inventory, error) {
	return oarlock.Inventory{Sections: []oarlock.InventorySection{
		{
			Kind:   oarlock.InventoryModels,
			Status: oarlock.InventoryUnsupported,
			Reason: "codex lists models only over the app-server (model/list); no CLI subcommand prints them",
		},
		{
			Kind:   oarlock.InventorySkills,
			Status: oarlock.InventoryUnsupported,
			Reason: "skills come from the app-server's skills/list request; no CLI subcommand prints them",
		},
		{
			Kind:   oarlock.InventoryCommands,
			Status: oarlock.InventoryUnsupported,
			Reason: "codex has no command catalog to read: no CLI subcommand or app-server request enumerates its commands",
		},
		{
			Kind:   oarlock.InventoryTools,
			Status: oarlock.InventoryUnsupported,
			Reason: "MCP tools come from the app-server's mcpServerStatus/list request; no CLI subcommand prints them",
		},
	}}, nil
}

// clipOutput bounds vendor output kept in an error without splitting a rune.
func clipOutput(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= outputTail {
		return s
	}
	start := len(s) - outputTail
	for start < len(s) && s[start]&0xC0 == 0x80 {
		start++
	}
	return "...[truncated]..." + s[start:]
}
