package acp

import (
	"context"

	sdk "github.com/coder/acp-go-sdk"
)

// choosePermission is the default answer: allow_always, else allow_once, else
// cancelled. A reject option is never picked on the host's behalf, and a
// request with nothing to allow is cancelled rather than guessed at.
func choosePermission(options []sdk.PermissionOption) sdk.RequestPermissionResponse {
	for _, want := range []sdk.PermissionOptionKind{sdk.PermissionOptionKindAllowAlways, sdk.PermissionOptionKindAllowOnce} {
		for _, o := range options {
			if o.Kind == want {
				return sdk.RequestPermissionResponse{Outcome: sdk.RequestPermissionOutcome{
					Selected: &sdk.RequestPermissionOutcomeSelected{Outcome: "selected", OptionId: o.OptionId},
				}}
			}
		}
	}
	return cancelledPermission()
}

func cancelledPermission() sdk.RequestPermissionResponse {
	return sdk.RequestPermissionResponse{Outcome: sdk.RequestPermissionOutcome{
		Cancelled: &sdk.RequestPermissionOutcomeCancelled{Outcome: "cancelled"},
	}}
}

// client is the sdk.Client the connection routes agent requests to. It holds no
// records: the wire tap already recorded every request as a toApp request and
// will record the reply as the `answered` response when it is written.
type client struct {
	s *Session
	t *terminals
}

var _ sdk.Client = (*client)(nil)

// RequestPermission answers the agent. While an abort is in flight every
// pending request is answered cancelled, as the protocol requires.
func (c *client) RequestPermission(_ context.Context, p sdk.RequestPermissionRequest) (sdk.RequestPermissionResponse, error) {
	if c.s.cancelling.Load() {
		return cancelledPermission(), nil
	}
	return choosePermission(p.Options), nil
}

// SessionUpdate is a no-op: the tap recorded the notification in wire order
// before the SDK decoded it.
func (c *client) SessionUpdate(context.Context, sdk.SessionNotification) error { return nil }

func (c *client) ReadTextFile(context.Context, sdk.ReadTextFileRequest) (sdk.ReadTextFileResponse, error) {
	return sdk.ReadTextFileResponse{}, sdk.NewMethodNotFound(sdk.ClientMethodFsReadTextFile)
}

func (c *client) WriteTextFile(context.Context, sdk.WriteTextFileRequest) (sdk.WriteTextFileResponse, error) {
	return sdk.WriteTextFileResponse{}, sdk.NewMethodNotFound(sdk.ClientMethodFsWriteTextFile)
}

func (c *client) CreateTerminal(ctx context.Context, p sdk.CreateTerminalRequest) (sdk.CreateTerminalResponse, error) {
	return c.t.create(ctx, p, c.s.prof.TerminalShellCommand, c.s.cwd)
}

func (c *client) TerminalOutput(_ context.Context, p sdk.TerminalOutputRequest) (sdk.TerminalOutputResponse, error) {
	return c.t.output(p)
}

func (c *client) WaitForTerminalExit(ctx context.Context, p sdk.WaitForTerminalExitRequest) (sdk.WaitForTerminalExitResponse, error) {
	return c.t.wait(ctx, p)
}

func (c *client) KillTerminal(_ context.Context, p sdk.KillTerminalRequest) (sdk.KillTerminalResponse, error) {
	return c.t.kill(p)
}

func (c *client) ReleaseTerminal(_ context.Context, p sdk.ReleaseTerminalRequest) (sdk.ReleaseTerminalResponse, error) {
	return c.t.release(p)
}
