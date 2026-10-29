// Package cursor is the oarlock adapter for Cursor's agent CLI
// (`cursor-agent -p --output-format stream-json ...`), issue #22 and
// docs/adr/0004-cursor-via-cli.md. The parser, framing and transport are
// internal/streamjson — the stream-json family Cursor shares with Claude Code,
// reused rather than re-parsed. This package owns what is Cursor's alone:
// launch flags, the refused-option table, the chat id from `system/init`, one
// process per turn, and the projection (project.go).
//
// The CLI is one-shot: it reads the prompt from stdin to EOF, emits
// `system/init` … `result`, and exits. A Session therefore starts one process
// per prompt and continues the chat with `--resume <chat id>`.
//
// Layout: args.go (argv, environment + refused options), open.go (StartSession,
// Runtime), session.go (oarlock.Session over the shared transport), project.go
// (the Profile: frames, usage, failure, unknown subtypes).
package cursor

import (
	"strings"

	"github.com/zzjcool/oarlock"
)

// OptionEffort names Options.Effort for a refusal. The frozen option set
// (oarlock.AllRefusableOptions) has no effort entry, but a caller sending
// effort to every runtime must still get a typed refusal from cursor rather
// than have it silently dropped, so cursor declares the name locally. It is
// deliberately not added to the declared table: a table is a declaration a
// host can read before opening, and this refusal exists to name the value the
// CLI cannot take.
const OptionEffort oarlock.Option = "effort"

// BaseArgs are the arguments every cursor-agent run starts with, in order.
//
//	-p                        print mode: read the prompt from stdin, EOF starts
//	                          the run, and the process exits after `result`
//	--output-format stream-json
//	                          the shared stream-json dialect
//	--stream-partial-output   text arrives in pieces, then once cumulatively
//	--force --trust           headless: a permission prompt is a hang, not
//	                          safety
//	--sandbox disabled        the workspace is the sandbox oarlock was given
func BaseArgs() []string {
	return []string{
		"-p",
		"--output-format", "stream-json",
		"--stream-partial-output",
		"--force",
		"--trust",
		"--sandbox", "disabled",
	}
}

// LaunchArgs is the argv of one turn: BaseArgs, then the model, then the chat
// to continue. chatID "" is a fresh chat (the CLI assigns the id and reports it
// in `system/init`).
func LaunchArgs(opt oarlock.Options, chatID string) []string {
	args := BaseArgs()
	if opt.Model != "" {
		args = append(args, "--model", opt.Model)
	}
	if chatID != "" {
		args = append(args, "--resume", chatID)
	}
	return args
}

// refusedTable is the ONE table: Refused() hands a copy to a host and
// CheckOptions checks this very table, so the declaration and the behaviour
// cannot drift. An option added to oarlock.AllRefusableOptions later is refused
// until it is decided (the undecided reason below), never silently dropped.
var refusedTable = buildRefused()

func buildRefused() oarlock.RefusedOptions {
	t := make(oarlock.RefusedOptions, len(oarlock.AllRefusableOptions))
	for _, o := range oarlock.AllRefusableOptions {
		if reason, ok := refusedReasons[o]; ok {
			t[o] = reason
		} else if !honoured[o] {
			t[o] = "the Cursor adapter has not decided this option yet; it is refused rather than dropped"
		}
	}
	return t
}

// honoured are the options this adapter really applies. Only one so far:
// Options.Env is the process environment (sessionEnv), everything else has no
// channel on the CLI.
var honoured = map[oarlock.Option]bool{
	oarlock.OptionEnv: true,
}

// refusedReasons is why each refused option is refused. Two kinds of reason
// appear, and they are not interchangeable:
//
//   - the CLI has no flag for it. cursor-agent's surface is print mode, model,
//     chat id and the permission/sandbox switches; there is no per-session MCP
//     list, no tool deny list, no system prompt, no extra directories, no
//     output schema, no name, no service tier and no way to skip the user's own
//     Cursor configuration;
//   - oarlock owns the argv. `--resume`, `--output-format`, `--force` and the
//     rest are protocol, not host policy: a host flag that contradicted them
//     would leave a process whose frames the session cannot read, or a chat the
//     session cannot name.
//
// In no case is a refused option spliced into the prompt. The prompt is the
// user's text on stdin; putting a system prompt, a schema or an MCP server
// list there would make policy text indistinguishable from the user's own
// words, and no host could tell what the model was actually told.
var refusedReasons = map[oarlock.Option]string{
	oarlock.OptionMCPServers:            "cursor-agent has no per-session MCP server flag: its servers come from the user's Cursor configuration, and writing a server into the prompt would put policy text where the user's message belongs",
	oarlock.OptionMCPServerCredentials:  "cursor-agent has no per-session MCP server flag, so there is no channel for a server's environment or headers; passing them in argv would expose credentials to every process on the machine",
	oarlock.OptionLaunchArgs:            "oarlock owns cursor-agent's argv (BaseArgs + --model + --resume): a host flag could contradict --output-format stream-json and the session would have no frames to read",
	oarlock.OptionSystemPrompt:          "cursor-agent has no system-prompt flag: its instructions come from the user's Cursor rules, and splicing text into the prompt would be indistinguishable from the user's own words",
	oarlock.OptionAppendSystemPrompt:    "cursor-agent has no system-prompt flag: its instructions come from the user's Cursor rules, and splicing text into the prompt would be indistinguishable from the user's own words",
	oarlock.OptionDisallowedTools:       "cursor-agent has no tool deny list: --force is what oarlock runs, and the CLI exposes no per-tool switch",
	oarlock.OptionAdditionalDirectories: "cursor-agent works in one directory (Options.Cwd): the CLI has no --add-dir flag, and a second root cannot be expressed",
	oarlock.OptionOutputSchema:          "cursor-agent has no structured-output flag: a schema in the prompt would be advice, not a contract, and oarlock never fakes one",
	oarlock.OptionName:                  "cursor-agent names a chat itself (its UI owns the name) and the CLI has no --name flag; the chat id is reported in system/init instead",
	oarlock.OptionServiceTier:           "cursor-agent takes --model only: routing lives in the model name (for example \"auto\"), and the CLI has no service-tier flag",
	oarlock.OptionUserConfiguration:     "cursor-agent always reads the user's Cursor configuration and has no flag to exclude it, so oarlock cannot promise a run without it",
}

// Refused is the declaration for oarlock.Runtime.Refused: a copy of the very
// table Open checks, so a host reading the table and a session opening with the
// same options cannot disagree.
func Refused() oarlock.RefusedOptions {
	out := make(oarlock.RefusedOptions, len(refusedTable))
	for k, v := range refusedTable {
		out[k] = v
	}
	return out
}

// CheckOptions refuses, before any process starts, every option this adapter
// cannot honour. It is Refused().Check plus the one option outside the frozen
// set: cursor-agent has no effort flag, so a non-empty Options.Effort is named
// rather than dropped.
func CheckOptions(opt oarlock.Options) error {
	if err := refusedTable.Check(opt); err != nil {
		return err
	}
	if opt.Effort != "" {
		return &oarlock.UnsupportedOption{
			Option: OptionEffort,
			Reason: "cursor-agent has no effort flag",
		}
	}
	return nil
}

// sessionEnv is base with the host's Options.Env applied on top: a nil value
// removes the variable, a value replaces it. The host environment itself is
// never changed. cursor-agent's own variables are left alone (the CLI has no
// documented parent-session marker to strip, and guessing at one is worse than
// inheriting it).
func sessionEnv(base []string, set map[string]*string) []string {
	if len(set) == 0 {
		return append([]string(nil), base...)
	}
	out := make([]string, 0, len(base)+len(set))
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if _, ok := set[k]; ok {
			continue
		}
		out = append(out, kv)
	}
	for k, v := range set {
		if v != nil {
			out = append(out, k+"="+*v)
		}
	}
	return out
}
