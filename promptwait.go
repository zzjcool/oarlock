package oarlock

import (
	"context"
	"errors"
	"strings"
	"time"
)

// RunOutcome is how PromptAndWait's run ended. It is the small, host-facing
// summary of the turn; the record stream remains the source of truth.
type RunOutcome string

const (
	// RunCompleted: the runtime reported a completed turn.
	RunCompleted RunOutcome = "completed"
	// RunFailed: the runtime reported a failed turn, or the process exited.
	RunFailed RunOutcome = "failed"
	// RunAborted: the runtime reported an aborted turn.
	RunAborted RunOutcome = "aborted"
	// RunInterrupted: PromptAndWait's own deadline or the caller's context
	// caused an abort. It is not a vendor outcome.
	RunInterrupted RunOutcome = "interrupted"
	// RunRejected: the prompt itself was refused and never started a turn.
	RunRejected RunOutcome = "rejected"
)

// RunResult is what PromptAndWait saw. Text is the root agent's streamed text
// (child agents are not concatenated). Code/Reason are set when the prompt was
// rejected. Turn is the runtime's own completion when it reported one.
type RunResult struct {
	Outcome   RunOutcome
	Text      string
	Turn      *TurnOutcome
	Seq       uint64
	RequestID string
	Code      Code
	Reason    string
}

// WaitOption configures PromptAndWait.
type WaitOption func(*waitConfig)

type waitConfig struct {
	timeout time.Duration
}

// WithTimeout bounds the wait for the runtime's own turn end. On expiry
// PromptAndWait aborts the turn and reports RunInterrupted; it never invents a
// turn end. A non-positive duration means no extra deadline.
func WithTimeout(d time.Duration) WaitOption {
	return func(c *waitConfig) { c.timeout = d }
}

const (
	promptWaitPoll = 20 * time.Millisecond
	abortWaitBound = 10 * time.Second
)

// PromptAndWait sends one prompt and waits for the runtime's own completion.
// It is a convenience over Prompt + Records; it never synthesizes a turn end.
//
// A rejected prompt returns RunRejected with Code/Reason and no error. A
// deadline or a cancelled caller context aborts the turn, waits for the
// runtime's own end within a bounded grace, and reports RunInterrupted (with
// the caller's context error when the caller cancelled). If the runtime
// reports a completion after the abort raced it, that real outcome wins.
func PromptAndWait(ctx context.Context, sess Session, input string, opts ...WaitOption) (RunResult, error) {
	if ctx == nil {
		return RunResult{}, errors.New("oarlock: PromptAndWait(nil context)")
	}
	if sess == nil {
		return RunResult{}, errors.New("oarlock: PromptAndWait(nil session)")
	}
	var cfg waitConfig
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	waitCtx := ctx
	if cfg.timeout > 0 {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, cfg.timeout)
		defer cancel()
	}

	out, err := sess.Prompt(ctx, input, PromptOptions{})
	if err != nil {
		return RunResult{RequestID: out.RequestID, Seq: out.Seq}, err
	}
	res := RunResult{RequestID: out.RequestID, Seq: out.Seq}
	if out.Kind == ControlRejected {
		res.Outcome = RunRejected
		res.Code = out.Code
		res.Reason = out.Reason
		return res, nil
	}
	root := out.Request.SessionID
	after := out.Seq
	var text strings.Builder
	for {
		handled, exited := consumeRun(sess, root, &after, out.RequestID, &text, &res)
		if handled {
			res.Outcome = RunCompleted
			res.Text = text.String()
			return res, nil
		}
		if res.Turn != nil {
			res.Outcome = outcomeOfRun(res.Turn)
			res.Text = text.String()
			return res, nil
		}
		if exited {
			res.Outcome = RunFailed
			if res.Turn == nil {
				res.Turn = &TurnOutcome{Kind: OutcomeFailed, Failure: FailureRuntimeExited, Reason: "runtime exited"}
			}
			res.Text = text.String()
			return res, nil
		}
		select {
		case <-waitCtx.Done():
			return abortAndWait(ctx, waitCtx, sess, root, &after, out.RequestID, &text, res)
		case <-time.After(promptWaitPoll):
		}
	}
}

// consumeRun reads the records after after, updates the text and result, and
// reports whether the prompt was handled without a turn or the process exited.
func consumeRun(sess Session, root string, after *uint64, requestID string, text *strings.Builder, res *RunResult) (handled, exited bool) {
	for _, rec := range sess.Records(Cursor{SessionID: root, AfterSeq: *after}) {
		if rec.Seq > *after {
			*after = rec.Seq
			res.Seq = rec.Seq
		}
		if rec.SessionID != root || len(rec.AgentPath) != 0 {
			continue
		}
		for _, ev := range rec.Events {
			switch ev.Kind {
			case EventTextDelta:
				text.WriteString(ev.Text)
			case EventTurnEnded:
				res.Turn = ev.Outcome
			case EventInputHandled:
				if ev.RequestID == requestID {
					handled = true
				}
			}
		}
		if rec.Kind == KindResponse && rec.Response != nil && rec.Response.Kind == ResponseExited {
			exited = true
		}
	}
	return handled, exited
}

func abortAndWait(ctx, waitCtx context.Context, sess Session, root string, after *uint64, requestID string, text *strings.Builder, res RunResult) (RunResult, error) {
	// Ask the runtime to stop. A rejection is a normal race; the record stream
	// decides what actually happened.
	_, _ = sess.Abort(context.WithoutCancel(ctx))
	deadline := time.Now().Add(abortWaitBound)
	for {
		handled, exited := consumeRun(sess, root, after, requestID, text, &res)
		if handled {
			res.Outcome = RunCompleted
			res.Text = text.String()
			return res, callerErr(ctx, waitCtx)
		}
		if res.Turn != nil {
			// A real completion after the abort raced it wins; otherwise the
			// accepted abort is what ended the turn.
			if res.Turn.Kind == OutcomeCompleted {
				res.Outcome = RunCompleted
			} else {
				res.Outcome = RunInterrupted
			}
			res.Text = text.String()
			return res, callerErr(ctx, waitCtx)
		}
		if exited || time.Now().After(deadline) {
			res.Outcome = RunInterrupted
			res.Text = text.String()
			if res.Turn == nil {
				res.Turn = &TurnOutcome{Kind: OutcomeAborted}
			}
			return res, callerErr(ctx, waitCtx)
		}
		time.Sleep(promptWaitPoll)
	}
}

func callerErr(ctx, waitCtx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// A WaitOption deadline is PromptAndWait's own interruption, not a caller
	// error; the result says RunInterrupted.
	_ = waitCtx
	return nil
}

func outcomeOfRun(t *TurnOutcome) RunOutcome {
	switch t.Kind {
	case OutcomeCompleted:
		return RunCompleted
	case OutcomeAborted:
		return RunAborted
	default:
		return RunFailed
	}
}
