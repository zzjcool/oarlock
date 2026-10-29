package acp

import (
	"runtime"

	"github.com/zzjcool/oarlock"
)

// Antigravity (agy_acp_server): subagents run inside the harness and are
// flattened onto the PARENT session: the child's tool calls and text arrive
// under the parent's sessionId (the child id survives only as a toolCallId
// prefix) and no usage is reported for any session. So everything lands on the
// root and nothing is fabricated: opaque. No steer in the method set.
//
// Evidence: [env] agy_acp_server 1.2.1 (OAR recordings). No binary here.
var Antigravity = Profile{
	ID: "antigravity",
	Args: func(oarlock.Options) []string {
		if runtime.GOOS == "linux" {
			return []string{"--uid="} // else it drops to group `nobody` and may abort before ACP
		}
		return nil
	},
	Attribution: oarlock.AttributionOpaque,
	Evidence:    "unverified-live: flattened subagent per OAR env recording of agy_acp_server 1.2.1",
	YoloMode:    true,
	Refused: oarlock.RefusedOptions{
		oarlock.OptionUserConfiguration:     "antigravity has no verified separation of user configuration from its native login",
		oarlock.OptionOutputSchema:          "antigravity ACP exposes no final-output schema channel",
		oarlock.OptionName:                  "antigravity exposes no verified session name setting",
		oarlock.OptionAdditionalDirectories: "antigravity accepts but ignores ACP additionalDirectories",
		oarlock.OptionServiceTier:           "antigravity exposes no verified per-session service-tier setting",
		oarlock.OptionSystemPrompt:          "the antigravity ACP server has no system prompt override",
		oarlock.OptionMCPServerCredentials:  "antigravity stores MCP env and header values in plain text in its conversation database; it attaches no entry with credentials",
	},
	Validate: func(opt oarlock.Options) error {
		if opt.AppendSystemPrompt != "" || len(opt.DisallowedTools) > 0 {
			// Both need the vendor's `_meta.antigravity` capability, read from
			// initialize; that handshake-time check is a documented gap.
			return &oarlock.UnsupportedOption{Option: oarlock.OptionAppendSystemPrompt, Reason: "antigravity prompt/tool extensions need its _meta capability, which this adapter does not negotiate yet"}
		}
		return nil
	},
	IsResumeNotFound: func(code int, _ any) bool { return code == -32002 },
}
