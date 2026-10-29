package acp

import (
	"github.com/zzjcool/oarlock"
)

// Grok: nested sessions on one connection (a child has its own ACP sessionId),
// steer through `_meta.sendNow`, and a usage view that must not be added up.
//
// Usage: grok reports several overlapping ledgers. The authoritative view is
// the root's own `session/prompt` answer (`_meta.usage`), which already sums
// a foreground child's model calls (live 1.0.25: four response_completed
// frames 48+78+119+155 = the parent's 300, two of them the child's). Child
// sessions' `response_completed` / `turn_completed` ledgers are NOT added, or
// the child would be billed twice. The one independent root ledger is a
// `turn_completed` whose prompt_id starts "subagent-completed-": grok injects
// that prompt outside session/prompt, so it is added once per native eventId.
//
// Evidence: nested + `_x.ai/session_notification` lifecycle are [src]/[env]
// from the OAR recordings of grok 1.0.25; no grok binary was available here,
// so only the fake-agent replay of those shapes ran (tests: grok_test.go).
var Grok = Profile{
	ID:                   "grok",
	Args:                 func(oarlock.Options) []string { return []string{"agent", "--always-approve", "--no-leader", "stdio"} },
	LaunchArgsBeforeLast: true, // `grok agent [OPTIONS] stdio`: agent options go before `stdio`
	Attribution:          oarlock.AttributionNested,
	Evidence:             "unverified-live: nested child sessions per OAR grok 1.0.25 recordings; replayed by a fake agent only",
	Images:               ptr(true), // initialize says image:false, but grok 1.0.44 hands the block to the model
	TerminalShellCommand: true,
	Refused: oarlock.RefusedOptions{
		oarlock.OptionUserConfiguration:     "grok has no verified separation of user configuration from its native login",
		oarlock.OptionAdditionalDirectories: "grok ACP exposes no effective additional workspace directory setting",
		oarlock.OptionServiceTier:           "grok exposes no verified per-session service-tier setting and readback",
		oarlock.OptionName:                  "grok exposes no verified session name setting",
		oarlock.OptionDisallowedTools:       "grok agent stdio does not forward the CLI tool denylist",
	},
	Validate: func(opt oarlock.Options) error {
		if opt.SystemPrompt != "" && len(trimSpace(opt.SystemPrompt)) == 0 {
			return &oarlock.UnsupportedOption{Option: oarlock.OptionSystemPrompt, Reason: "grok treats a blank systemPrompt as omitted"}
		}
		if opt.Resume != "" && opt.AppendSystemPrompt != "" && opt.SystemPrompt == "" {
			return &oarlock.UnsupportedOption{Option: oarlock.OptionAppendSystemPrompt, Reason: "grok does not reapply rules when resuming a session"}
		}
		return nil
	},
	AuthMethod: func(init map[string]any) string {
		ids := map[string]bool{}
		for _, m := range asSlice(init["authMethods"]) {
			ids[asString(asMap(m)["id"])] = true
		}
		if d := asString(asMap(init["_meta"])["defaultAuthMethodId"]); ids[d] {
			return d
		}
		if ids["cached_token"] {
			return "cached_token"
		}
		return ""
	},
	InitializeMeta: func(opt oarlock.Options) map[string]any {
		m := map[string]any{"clientIdentifier": "oarlock", "clientType": "generic",
			"startupHints": map[string]any{"nonInteractive": true, "skipGitStatus": true, "skipProjectLayout": true}}
		if opt.SystemPrompt != "" {
			m["systemPromptOverride"] = opt.SystemPrompt
		}
		if opt.AppendSystemPrompt != "" {
			m["rules"] = opt.AppendSystemPrompt
		}
		return m
	},
	SessionMeta: func(oarlock.Options) map[string]any { return map[string]any{"yoloMode": true} },
	PromptMeta: func(opt oarlock.Options) map[string]any {
		if len(opt.OutputSchema) == 0 {
			return nil
		}
		return map[string]any{"outputSchema": opt.OutputSchema}
	},
	IsResumeNotFound: func(_ int, data any) bool { return asString(asMap(data)["code"]) == "FS_NOT_FOUND" },
	// A send-now steer cancels the running prompt (cancelTrigger send_now) and
	// starts a fresh turn; both answers belong to one oarlock turn.
	SteerMeta:       func() map[string]any { return map[string]any{"sendNow": true} },
	SteerSupersedes: true,
	PromptContext: func(r map[string]any) *oarlock.ContextUsage {
		meta := asMap(r["_meta"])
		tokens, ok1 := asInt64(meta["totalTokens"]) // the running context count, not billing
		window, ok2 := asInt64(meta["contextWindow"])
		if !ok1 && !ok2 {
			return nil
		}
		c := &oarlock.ContextUsage{}
		if ok1 {
			c.Tokens = &tokens
		}
		if ok2 {
			c.ContextWindow = &window
		}
		return c
	},
	PromptTokens: func(r map[string]any) *oarlock.TokenTotals {
		return tokenParts(asMap(asMap(r["_meta"])["usage"]), "inputTokens", "outputTokens")
	},
	Annotate: grokAnnotate,
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t' || s[0] == '\n' || s[0] == '\r') {
		s = s[1:]
	}
	return s
}

// grokAnnotate links children from the vendor lifecycle and bills the one
// independent root ledger. The child's own session/update frames need no help:
// they already carry the child's sessionId.
func grokAnnotate(f *FrameInfo) {
	if f.Method != "_x.ai/session_notification" || f.Update == nil {
		return
	}
	switch asString(f.Update["sessionUpdate"]) {
	case "subagent_spawned":
		f.Link(asString(f.Update["parent_session_id"]), asString(f.Update["child_session_id"]), "grok subagent_spawned")
	case "turn_completed":
		prompt := asString(f.Update["prompt_id"])
		if f.SessionID != f.Root || len(prompt) <= len("subagent-completed-") || prompt[:len("subagent-completed-")] != "subagent-completed-" {
			return
		}
		meta := asMap(f.Params["_meta"])
		if meta["isReplay"] == true {
			return
		}
		if t := tokenParts(asMap(f.Update["usage"]), "inputTokens", "outputTokens"); t != nil {
			f.Bill(asString(meta["eventId"]), *t)
		}
	}
}
