package acp

import "github.com/zzjcool/oarlock"

// Kimi: `kimi acp` binds the native session's main agent only, so children
// exist natively but never reach this transport: opaque, honestly root-only.
// The ACP method set has no steer, so there is no SteerMeta and the session
// does not satisfy oarlock.Steerer. Nothing is fabricated from display text.
//
// Evidence: [src] kimi-code f9ca33376 acp-server session.ts (OAR); no kimi
// binary here, so the tier is declared from source and replay-tested only.
var Kimi = Profile{
	ID:          "kimi",
	Args:        func(oarlock.Options) []string { return []string{"acp"} },
	Attribution: oarlock.AttributionOpaque,
	Evidence:    "unverified-live: opaque per kimi-code source (main agent only); no binary here",
	YoloMode:    true,
	Refused: oarlock.RefusedOptions{
		oarlock.OptionUserConfiguration:  "kimi isolated homes cannot safely share native history",
		oarlock.OptionOutputSchema:       "Kimi ACP exposes no final-output schema channel",
		oarlock.OptionName:               "kimi exposes no verified session name setting",
		oarlock.OptionServiceTier:        "kimi exposes no verified per-session service-tier setting",
		oarlock.OptionDisallowedTools:    "kimi ACP exposes no session tool denylist",
		oarlock.OptionSystemPrompt:       "kimi acp has no per-session system prompt input",
		oarlock.OptionAppendSystemPrompt: "kimi acp has no per-session system prompt append input",
	},
	Validate: func(opt oarlock.Options) error {
		if opt.Resume != "" && len(opt.AdditionalDirectories) > 0 {
			return &oarlock.UnsupportedOption{Option: oarlock.OptionAdditionalDirectories, Reason: "kimi applies additionalDirectories only when creating a session"}
		}
		return nil
	},
	AuthMethod: func(init map[string]any) string {
		for _, m := range asSlice(init["authMethods"]) {
			if asString(asMap(m)["id"]) == "login" {
				return "login"
			}
		}
		return ""
	},
}
