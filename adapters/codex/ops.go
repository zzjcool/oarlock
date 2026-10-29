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
//   - CheckUpdate is a *oarlock.UnsupportedOperation: `codex doctor --json`
//     carries the update verdict and that row is not wired yet. Upgrade runs
//     `codex update` and re-reads `--version`, because the updater can report
//     success after a failed download.
//   - All four inventories are unsupported sections: models, skills and MCP
//     tools are app-server requests (model/list, skills/list,
//     mcpServerStatus/list) and codex has no command catalog, so no CLI
//     command can answer them.
//
// Everything that does run goes through internal/ops: no stdin, a bounded
// time, and the vendor's exit code is data rather than proof of success.

import (
	"context"
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

// upgrader is codex's self-update. The check is refused and the upgrade is
// judged by the version read-back, never by `codex update`'s exit code.
type upgrader struct{}

var _ oarlock.Upgrader = upgrader{}

// CheckUpdate is a typed refusal: the verdict is the `updates.status` row of
// `codex doctor --json`, which this adapter does not parse yet.
func (upgrader) CheckUpdate(context.Context) (oarlock.UpdateInfo, error) {
	return oarlock.UpdateInfo{}, &oarlock.UnsupportedOperation{
		Operation: "check-update",
		Reason:    "codex doctor --json is not wired yet; Upgrade re-reads --version",
	}
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
