package acp

import sdk "github.com/coder/acp-go-sdk"

type sdkOption struct{ id, kind string }

// choose runs the default permission rule over plain options.
func choose(in []sdkOption) string {
	var opts []sdk.PermissionOption
	for _, o := range in {
		opts = append(opts, sdk.PermissionOption{OptionId: sdk.PermissionOptionId(o.id), Kind: sdk.PermissionOptionKind(o.kind), Name: o.id})
	}
	r := choosePermission(opts)
	if r.Outcome.Selected != nil {
		return string(r.Outcome.Selected.OptionId)
	}
	return ""
}

func sdkTerminalOutput(id string) sdk.TerminalOutputRequest {
	return sdk.TerminalOutputRequest{TerminalId: id}
}
