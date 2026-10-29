// Package claude is the oarlock adapter for Claude Code's stream-json mode
// (`claude -p --input-format stream-json --output-format stream-json ...`),
// issue #19. The parser, framing and transport are internal/streamjson (the
// family claude and cursor share); this package owns what is Claude's alone:
// launch flags, usage and context readings, failure classification, tasks,
// the steer echo protocol, resume and the context breakdown.
//
// Layout: args.go (argv + refused options), project.go (the Profile: result,
// usage, context, tasks), failure.go, usage.go, session.go (controls),
// open.go (StartSession, resume), breakdown.go (ContextBreakdownReader).
package claude

import (
	"fmt"
	"strings"

	"github.com/zzjcool/oarlock"
)

// BaseArgs are the arguments every session starts with, in order. Permission
// prompts in headless mode are a hang, not safety, so permissions are skipped.
func BaseArgs() []string {
	return []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose", "--replay-user-messages", "--include-partial-messages",
		"--dangerously-skip-permissions",
	}
}

// managedFlags are flags oarlock sets itself, which a host's LaunchArgs would
// contradict (OAR launch-args.ts). Each maps to the reason the open is refused.
var managedFlags = map[string]string{
	"--continue":                     "oarlock assigns the session id; use Options.Resume with the id to continue",
	"-c":                             "oarlock assigns the session id; use Options.Resume with the id to continue",
	"--resume":                       "use Options.Resume",
	"-r":                             "use Options.Resume",
	"--session-id":                   "oarlock assigns the session id",
	"--fork-session":                 "a fork runs under a new native id that the session would not report",
	"--permission-mode":              "oarlock runs claude with --dangerously-skip-permissions, which overrides it",
	"--dangerously-skip-permissions": "oarlock always sets it (headless: a permission gate is a hang)",
	"--print":                        "oarlock drives claude in print mode over stream-json already",
	"-p":                             "oarlock drives claude in print mode over stream-json already",
	"--input-format":                 "oarlock drives claude over stream-json",
	"--output-format":                "oarlock drives claude over stream-json",
	"--json-schema":                  "use Options.OutputSchema",
	"--verbose":                      "oarlock already sets it",
	"--replay-user-messages":         "oarlock already sets it (steer needs the echo)",
	"--include-partial-messages":     "oarlock already sets it",
}

// checkLaunchArgs refuses, before anything starts, a LaunchArgs entry that
// collides with a managed flag. A short alias is checked whole, a long flag
// also in its --flag=value form.
func checkLaunchArgs(args []string) error {
	for _, a := range args {
		flag := a
		if strings.HasPrefix(a, "--") {
			flag, _, _ = strings.Cut(a, "=")
		}
		if reason, bad := managedFlags[flag]; bad {
			return &oarlock.UnsupportedOption{
				Option: oarlock.OptionLaunchArgs,
				Reason: fmt.Sprintf("claude launchArgs cannot include %s: %s", flag, reason),
			}
		}
	}
	return nil
}

// refusedReasons are the options this adapter does not honour, each with why.
// Everything else in oarlock.AllRefusableOptions is honoured (see launchArgs).
var refusedReasons = map[oarlock.Option]string{
	oarlock.OptionMCPServers:           "the Claude adapter does not hand --mcp-config over yet (issue #19 gap)",
	oarlock.OptionMCPServerCredentials: "the Claude adapter does not hand --mcp-config over yet (issue #19 gap)",
	oarlock.OptionServiceTier:          "the Claude adapter does not apply --settings fastMode yet (issue #19 gap)",
	oarlock.OptionUserConfiguration:    "the Claude adapter does not apply userConfiguration: exclude yet (issue #19 gap)",
}

// Refused is the declaration for oarlock.Runtime.Refused: a copy of the very
// table Open checks.
func Refused() oarlock.RefusedOptions {
	out := make(oarlock.RefusedOptions, len(refusedTable))
	for k, v := range refusedTable {
		out[k] = v
	}
	return out
}

// launchArgs is the argv of a session for opt, after the options were checked.
// sessionID is the id claude runs under: a fresh one, or opt.Resume.
func launchArgs(opt oarlock.Options, sessionID string) []string {
	args := BaseArgs()
	if opt.Resume != "" {
		args = append(args, "--resume", sessionID)
	} else {
		args = append(args, "--session-id", sessionID)
	}
	// --name on a resume would replace the name the session has, which may be
	// the user's.
	if opt.Resume == "" && opt.Name != "" {
		args = append(args, "--name", opt.Name)
	}
	if opt.Model != "" {
		args = append(args, "--model", opt.Model)
	}
	if opt.Effort != "" {
		args = append(args, "--effort", opt.Effort)
	}
	if opt.SystemPrompt != "" {
		args = append(args, "--system-prompt", opt.SystemPrompt)
	}
	if opt.AppendSystemPrompt != "" {
		args = append(args, "--append-system-prompt", opt.AppendSystemPrompt)
	}
	if len(opt.OutputSchema) > 0 {
		args = append(args, "--json-schema", string(opt.OutputSchema))
	}
	// The host's own flags ahead of the variadic ones below, so none swallows
	// them as values.
	args = append(args, opt.LaunchArgs...)
	if len(opt.AdditionalDirectories) > 0 {
		args = append(args, "--add-dir")
		args = append(args, opt.AdditionalDirectories...)
	}
	if len(opt.DisallowedTools) > 0 {
		args = append(args, "--disallowed-tools")
		args = append(args, opt.DisallowedTools...)
	}
	return args
}

// parentSessionMarkers are variables a running Claude Code sets for its own
// children: markers of that parent session, not configuration. A claude started
// from inside another claude would believe it is part of that session.
var parentSessionMarkers = []string{
	"CLAUDECODE", "CLAUDE_CODE_SESSION_ID", "CLAUDE_CODE_CHILD_SESSION",
	"CLAUDE_CODE_SESSION_ATTENDED", "CLAUDE_CODE_CHROME_MCP_ORG_DENIED",
	"CLAUDE_CODE_EVAL_INTERVIEW_SESSION", "CLAUDE_CODE_BRIDGE_SESSION_ID",
	"CLAUDE_CODE_HOST_WORKTREE", "CLAUDE_CODE_HOST_WORKTREE_FENCE",
	"CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_MESSAGING_SOCKET", "CLAUDE_CODE_MESSAGING_TOKEN",
}

// sessionEnv is base with every parent-session marker removed, then the host's
// Options.Env on top (a nil value removes). CLAUDECODE is never settable:
// claude refuses to start nested under it.
func sessionEnv(base []string, set map[string]*string) []string {
	drop := map[string]bool{"CLAUDECODE": true}
	for _, m := range parentSessionMarkers {
		drop[m] = true
	}
	for k := range set {
		if k != "CLAUDECODE" {
			delete(drop, k)
		}
	}
	for k := range set {
		drop[k] = true
	}
	out := make([]string, 0, len(base)+len(set))
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if drop[k] {
			continue
		}
		out = append(out, kv)
	}
	for k, v := range set {
		if v != nil && k != "CLAUDECODE" {
			out = append(out, k+"="+*v)
		}
	}
	return out
}
