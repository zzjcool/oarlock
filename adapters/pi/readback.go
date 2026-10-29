package pi

// Readback: the values Pi reports about its own session, folded from the
// retained records (issue #13). Nothing here is cached in the adapter and
// nothing echoes a request: model and effort come from Pi's get_state or a
// later native report, context comes from get_session_stats, and status/usage
// are the shared folds over the record stream.
//
// The public result types live in oarlock/observe, so a host can name them
// without importing an internal package.

import (
	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/observe"
)

func (s *Session) Status() observe.QueryResult[observe.Status] {
	return observe.StatusOf(s.records())
}

func (s *Session) Usage() observe.QueryResult[observe.Usage] {
	return observe.UsageOf(s.records())
}

func (s *Session) ContextUsage() observe.QueryResult[*observe.ContextReport] {
	return observe.ContextUsageOf(s.records())
}

func (s *Session) Model() observe.QueryResult[string] { return observe.ModelOf(s.records()) }

func (s *Session) Effort() observe.QueryResult[string] { return observe.EffortOf(s.records()) }

func (s *Session) records() []oarlock.Record {
	if s == nil || s.k == nil {
		return nil
	}
	return s.k.Records(oarlock.Cursor{})
}

var (
	_ observe.StatusReader       = (*Session)(nil)
	_ observe.UsageReader        = (*Session)(nil)
	_ observe.ContextUsageReader = (*Session)(nil)
	_ observe.ModelReader        = (*Session)(nil)
	_ observe.EffortReader       = (*Session)(nil)
)
