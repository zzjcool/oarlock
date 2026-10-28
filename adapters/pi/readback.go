package pi

// Readback: the values Pi reports about its own session, folded from the
// retained records (issue #13). Nothing here is cached in the adapter and
// nothing echoes a request: model and effort come from Pi's get_state or a
// later native report, context comes from get_session_stats, and status/usage
// are the shared folds over the record stream.
//
// The returned types are internal/fold's: a host that imports this adapter can
// call the methods and read the values, but cannot name the type in its own
// declaration. That limitation is deliberate for now: the public fold types
// would otherwise have to move out of internal/, and #14's inventory work is a
// better place to decide their final public shape.

import (
	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/fold"
)

// Status folds the root agent's state over the retained records.
func (s *Session) Status() fold.QueryResult[fold.Status] {
	return fold.StatusOf(s.records())
}

// Usage folds the session's token accounting over the retained records.
func (s *Session) Usage() fold.QueryResult[fold.Usage] {
	return fold.UsageOf(s.records())
}

// ContextUsage is the latest context fullness Pi reported, with the Seq of the
// record that reported it. A nil value means Pi has not reported one yet.
func (s *Session) ContextUsage() fold.QueryResult[*fold.ContextReport] {
	return fold.ContextUsageOf(s.records())
}

// Model is the model Pi reports as in effect, as "provider/id". The value is
// empty and Seq is 0 until Pi reports one. It is never the requested model
// echoed back.
func (s *Session) Model() fold.QueryResult[string] {
	return s.latestEvent(oarlock.EventModel)
}

// Effort is the reasoning level Pi reports as in effect, in Pi's own spelling.
// The value is empty and Seq is 0 until Pi reports one.
func (s *Session) Effort() fold.QueryResult[string] {
	return s.latestEvent(oarlock.EventEffort)
}

func (s *Session) records() []oarlock.Record {
	if s == nil || s.k == nil {
		return nil
	}
	return s.k.Records(oarlock.Cursor{})
}

// latestEvent reads the latest model/effort event out of the stream. Seq is the
// Seq of the record that carried it, so a host can align the value with the
// stream; a later record that says nothing about the value does not move it.
func (s *Session) latestEvent(kind oarlock.EventKind) fold.QueryResult[string] {
	var out fold.QueryResult[string]
	for _, rec := range s.records() {
		for _, ev := range rec.Events {
			if ev.Kind != kind {
				continue
			}
			switch kind {
			case oarlock.EventModel:
				out.Value = ev.Model
			case oarlock.EventEffort:
				out.Value = ev.Effort
			}
			out.Seq = rec.Seq
		}
	}
	return out
}
