package acp

// Install plans for the ACP family (issue #69, docs/ops.md §3). Each vendor
// installs itself through one shell line — the four vendor scripts and
// opencode's npm command — so InstallPlan only names the argv, and Install —
// reached only on an explicit call — runs it once and lets the probe, never the
// pipeline's exit code, decide whether it worked. `curl | sh` exits 0 after a
// failed download, so the re-probe is the only evidence.
//
// What is absent is as deliberate as what is present: antigravity has no
// command-line installer (OAR records `requires_gui`: Google installs the agent
// from an editor's agent registry), so its Runtime.Install stays nil instead of
// a refusal-shaped fake installer.

import (
	"context"
	"fmt"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/ops"
)

// installLines are each vendor's documented installer, verbatim (OAR
// docs/spec/install.md §Per runtime and runtimes/*/install.ts). A profile
// without an entry has no installer at all.
var installLines = map[string]string{
	"grok":  `curl -fsSL https://x.ai/cli/install.sh | bash`,
	"kimi":  `curl -fsSL https://code.kimi.com/kimi-code/install.sh | bash`,
	"qwen":  `curl -fsSL https://qwen-code-assets.oss-cn-hangzhou.aliyuncs.com/installation/install-qwen-standalone.sh | bash -s -- --method standalone --no-modify-path`,
	"qoder": `curl -fsSL https://qoder.com/install | bash -s -- --skip-path`,
}

// opencode's two npm lines. Its v1 and v2 releases do not install side by side
// (both write ~/.opencode/bin/opencode, each replacing the other), so oarlock
// never chooses: InstallRequest.Line names the line and the package follows
// from it.
const (
	// opencodeV1Package is the v1 (1.x) npm package.
	opencodeV1Package = "opencode-ai"
	// opencodeV2Package is the v2 (2.x) npm package.
	opencodeV2Package = "@opencode/cli"

	opencodeV1Line = "npm install -g " + opencodeV1Package
	opencodeV2Line = "npm install -g " + opencodeV2Package
)

// installFor is the Installer a profile exposes, nil where the family has none.
// A single-line profile is ops.Installer itself: it already probes first, runs
// the line once, re-probes and refuses a caller-supplied line, so there is
// nothing to copy here. Every installer re-probes through probe(id), the same
// probe Runtime.Probe answers with.
func installFor(id string) oarlock.Installer {
	if id == "opencode" {
		return opencodeInstaller{probe: probe(id)}
	}
	if line, ok := installLines[id]; ok {
		return ops.Installer{Line: line, Probe: probe(id)}
	}
	return nil
}

// opencodeInstaller is opencode's installer: one command, two parallel release
// lines selected by the request. The line is validated first — a typed refusal,
// never a default — and the rest is delegated to ops.Installer, so the
// already-installed short-circuit and the re-probe are the shared behaviour
// rather than a copy of it.
type opencodeInstaller struct{ probe pathProbe }

var _ oarlock.Installer = opencodeInstaller{}

// opencodeInstallLine maps one request line to its npm command. The two
// refusals are the ones docs/ops.md §5 documents: no line named, and a line the
// runtime does not ship.
func opencodeInstallLine(name string) (string, error) {
	switch name {
	case "v1":
		return opencodeV1Line, nil
	case "v2":
		return opencodeV2Line, nil
	case "":
		return "", &oarlock.UnsupportedOperation{
			Operation: "install",
			Reason:    "this runtime ships two install lines (v1: npm " + opencodeV1Package + ", v2: npm " + opencodeV2Package + "); InstallRequest.Line must name one",
		}
	default:
		return "", &oarlock.UnsupportedOperation{
			Operation: "install",
			Reason:    fmt.Sprintf("unknown install line %q: opencode installs v1 or v2", name),
		}
	}
}

// InstallPlan resolves the line to argv and runs nothing: a plan is read-only
// for every profile, opencode included.
func (i opencodeInstaller) InstallPlan(ctx context.Context, req oarlock.InstallRequest) (oarlock.InstallPlan, error) {
	line, err := opencodeInstallLine(req.Line)
	if err != nil {
		return oarlock.InstallPlan{}, err
	}
	return i.singleLine(line).InstallPlan(ctx, oarlock.InstallRequest{})
}

// Install validates the line before anything runs (an unnamed line must not
// pick one), then delegates: probe, run once, re-probe, and a copy that is
// already installed runs nothing.
func (i opencodeInstaller) Install(ctx context.Context, req oarlock.InstallRequest) (oarlock.InstallResult, error) {
	line, err := opencodeInstallLine(req.Line)
	if err != nil {
		return oarlock.InstallResult{}, err
	}
	return i.singleLine(line).Install(ctx, oarlock.InstallRequest{})
}

// singleLine is the resolved one-line installer: the line is already chosen, so
// the delegate's empty-request plan is the whole plan.
func (i opencodeInstaller) singleLine(line string) ops.Installer {
	return ops.Installer{Line: line, Probe: i.probe}
}
