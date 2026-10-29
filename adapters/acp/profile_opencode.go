package acp

import "github.com/zzjcool/oarlock"

// OpenCode, two release lines behind one runtime id:
//
//   - v1: `opencode acp` forwards only its own sessions' parts. A `task`
//     subagent runs in a child session whose frames never reach this
//     transport, so only the parent's tool call shows: opaque. A plain prompt
//     RPC mid-turn joins the running loop at its next step, so v1 steers with
//     an empty `_meta`; both prompts are answered at idle, and only the
//     original prompt's answer ends the turn.
//   - v2: forwarded child updates carry `_meta["opencode/child-session"]`
//     {id, parentID}. That is the vendor naming the child, so the record goes
//     under the child's own session id and a session_linked edge is stated:
//     nested. Concurrent native prompts are refused in v2, so no steer.
//
// The line is chosen from Installation.Version; an unknown version is v1, the
// conservative (opaque) one.
//
// Evidence: v2 child frames are a recording (testdata/opencode-acp-v2-child.json,
// opencode 2.0.26, 2026-10-09); v1 opacity is [src] opencode 1.18.34 acp/event.ts.
// OpenCode selects its release line from the installation (see above).
var OpenCode = Profile{
	ID:          "opencode",
	Attribution: OpenCodeV1.Attribution,
	Evidence:    "v1 opaque: source-derived, unverified-live; v2 nested: recorded fixture opencode 2.0.26",
	Args:        OpenCodeV1.Args,
	Refused:     OpenCodeV1.Refused,
	Select: func(inst oarlock.Installation) Profile {
		if len(inst.Version) > 0 && inst.Version[0] == '2' {
			return OpenCodeV2
		}
		return OpenCodeV1
	},
}

// OpenCodeV1 is the v1 line, selectable directly.
var OpenCodeV1 = Profile{
	ID:          "opencode",
	Args:        func(oarlock.Options) []string { return []string{"acp"} },
	Attribution: oarlock.AttributionOpaque,
	Evidence:    "opaque: source-derived (opencode 1.18.34 acp/event.ts), unverified-live",
	SteerMeta:   func() map[string]any { return map[string]any{} },
	Refused:     opencodeRefused(true),
}

// OpenCodeV2 is the v2 line, selectable directly.
var OpenCodeV2 = Profile{
	ID:          "opencode",
	Args:        func(oarlock.Options) []string { return []string{"acp"} },
	Attribution: oarlock.AttributionNested,
	Evidence:    "nested: recorded fixture opencode 2.0.26 (_meta opencode/child-session)",
	Refused:     opencodeRefused(false),
	Annotate: func(f *FrameInfo) {
		if f.Update == nil {
			return
		}
		child := asMap(asMap(f.Update["_meta"])["opencode/child-session"])
		if child == nil {
			child = asMap(asMap(f.Params["_meta"])["opencode/child-session"])
		}
		id, parent := asString(child["id"]), asString(child["parentID"])
		if id == "" || parent == "" || id == parent {
			return // a malformed marker is not lineage: stay on the envelope's session
		}
		f.SessionID = id
		f.Link(parent, id, "opencode/child-session")
	},
}

func opencodeRefused(v1 bool) oarlock.RefusedOptions {
	r := oarlock.RefusedOptions{
		oarlock.OptionUserConfiguration: "opencode has no verified separation of user configuration from its native login",
		oarlock.OptionOutputSchema:      "OpenCode ACP exposes no final-output schema channel",
		oarlock.OptionName:              "opencode ACP exposes no verified session name setting",
		oarlock.OptionServiceTier:       "opencode exposes no verified per-session service-tier setting",
		oarlock.OptionDisallowedTools:   "opencode ACP has no session tool denylist",
	}
	if v1 {
		r[oarlock.OptionAdditionalDirectories] = "opencode v1 ACP exposes no additional workspace directory setting"
	} else {
		r[oarlock.OptionSystemPrompt] = "opencode v2 ACP exposes no per-session system prompt replacement"
		r[oarlock.OptionAppendSystemPrompt] = "opencode v2 ACP exposes no transient instruction channel"
	}
	return r
}
