package acp

import "github.com/zzjcool/oarlock"

// Qwen and Qoder attribute a forwarded child frame by the vendor's
// `parent_tool_use_id` (the tool call that spawned the child), which becomes
// the record's AgentPath. Identity of a tool call stays (AgentPath, id).
// Nothing is inferred: a frame without the field is the root's; no child
// session edge is stated because neither vendor states one.
//
// Evidence: OAR documents both as `attributed` through the shared stream-json
// fold, with "live nested subagent interleavings remain unverified". This
// package speaks ACP (`--acp`), a transport OAR does not use for them, so the
// tier is declared from that mapping and the ACP argument/field placement is
// unverified live: no qwen/qoder binary was available.
//
// Qoder additionally accepts `priority: next` on a user message, which joins
// at a tool boundary: that is its steer. Qwen's busy writes queue natively, so
// qwen has no steer.

func parentToolAnnotate(f *FrameInfo) {
	if f.Update == nil {
		return
	}
	id := asString(f.Update["parent_tool_use_id"])
	if id == "" {
		id = asString(asMap(f.Update["_meta"])["parent_tool_use_id"])
	}
	if id != "" {
		f.AgentPath = []string{id}
	}
}

var Qwen = Profile{
	ID:          "qwen",
	Args:        func(oarlock.Options) []string { return []string{"--acp"} },
	Attribution: oarlock.AttributionAttributed,
	Evidence:    "unverified-live: parent_tool_use_id mapping per OAR stream-json fold; ACP placement untested against a binary",
	Annotate:    parentToolAnnotate,
	Refused: oarlock.RefusedOptions{
		oarlock.OptionUserConfiguration: "qwen has no verified separation of user configuration from its native login",
		oarlock.OptionServiceTier:       "qwen does not expose a service-tier setting",
		oarlock.OptionOutputSchema:      "qwen exposes no schema channel on a long-lived session",
		oarlock.OptionName:              "qwen does not expose a session-name readback",
	},
}

var Qoder = Profile{
	ID:          "qoder",
	Args:        func(oarlock.Options) []string { return []string{"--acp"} },
	Attribution: oarlock.AttributionAttributed,
	Evidence:    "unverified-live: parent_tool_use_id mapping per OAR stream-json fold; ACP placement untested against a binary",
	Annotate:    parentToolAnnotate,
	SteerMeta:   func() map[string]any { return map[string]any{"priority": "next"} },
	Refused: oarlock.RefusedOptions{
		oarlock.OptionServiceTier:       "qoder has no verified service-tier option",
		oarlock.OptionOutputSchema:      "qoder has no verified structured-output option for long-lived sessions",
		oarlock.OptionUserConfiguration: "qoder has no verified way to exclude user configuration while keeping credentials",
		oarlock.OptionName:              "qoder's session-name persistence is unverified",
	},
}
