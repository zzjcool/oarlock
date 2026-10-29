package pi

// Withdraw (issue #52): taking back an input the adapter still holds.
//
// The queue is the adapter's own (Capabilities.Queue.Durable is false), so a
// withdraw is a record plus one edit of that list: Pi has no withdraw command
// and nothing goes on the wire. The drain takes an entry off the same list,
// under the same mutex, in one step before it sends it, so a withdraw either
// removes the entry before the drain reaches it or finds it gone.
//
// The queue's own request and its response are never rewritten: the withdraw
// adds one request/response pair of its own and leaves the stream additive, as
// the spec requires.
//
// Nothing is retried and nothing is held on a rejection; the input's
// disposition is the caller's again.

import (
	"context"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/kernel"
)

var _ oarlock.Withdrawer = (*Session)(nil)

// Withdraw takes back an input the adapter is still holding. It removes every
// held entry whose inputID matches and answers accepted; with no match it
// answers rejected{not_queued, "no held input with this inputId is waiting"}.
// Already sent, never queued in this session and already withdrawn are all
// not_queued, never accepted: an input that may already have gone is never
// reported as taken back.
//
// Reachability, dispose and exit gates come from the kernel before the decision
// runs (errors.go, "Process exit"), so a dead runtime answers
// rejected{runtime_exited} with the held input still unexplained by the stream.
// ctx is checked first, like every other control here: a cancelled context is a
// call that never happened, not a refusal.
func (s *Session) Withdraw(ctx context.Context, inputID string) (oarlock.ControlOutcome, error) {
	if err := ctx.Err(); err != nil {
		return oarlock.ControlOutcome{}, err
	}
	return s.k.Control(
		oarlock.RequestBody{Action: oarlock.ActionWithdraw, InputID: inputID},
		func(oarlock.Record) oarlock.ResponseBody { return s.withdrawHeld(inputID) },
		kernel.RecordAt{},
	), nil
}

// withdrawHeld is the withdraw's decision: remove every held entry for inputID
// and answer accepted, or answer not_queued when none is waiting (OAR's
// withdrawHeld). It runs synchronously from the control's decision, under s.mu,
// the mutex the drain pops the head of the list under — that is what makes a
// withdraw either win before the send or find the entry gone.
func (s *Session) withdrawHeld(inputID string) oarlock.ResponseBody {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := make([]heldInput, 0, len(s.held))
	for _, h := range s.held {
		if h.inputID != inputID {
			kept = append(kept, h)
		}
	}
	if len(kept) == len(s.held) {
		return sessRejected(oarlock.NotQueued, "no held input with this inputId is waiting")
	}
	s.held = kept
	return oarlock.ResponseBody{Kind: oarlock.ResponseAccepted}
}
