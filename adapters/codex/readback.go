package codex

import (
	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/observe"
)

func (s *Session) Status() observe.QueryResult[observe.Status] {
	return observe.StatusOf(s.Records(oarlock.Cursor{}))
}

func (s *Session) Usage() observe.QueryResult[observe.Usage] {
	return observe.UsageOf(s.Records(oarlock.Cursor{}))
}

func (s *Session) ContextUsage() observe.QueryResult[*observe.ContextReport] {
	return observe.ContextUsageOf(s.Records(oarlock.Cursor{}))
}

func (s *Session) Model() observe.QueryResult[string] {
	return observe.ModelOf(s.Records(oarlock.Cursor{}))
}

func (s *Session) Effort() observe.QueryResult[string] {
	return observe.EffortOf(s.Records(oarlock.Cursor{}))
}

var (
	_ observe.StatusReader       = (*Session)(nil)
	_ observe.UsageReader        = (*Session)(nil)
	_ observe.ContextUsageReader = (*Session)(nil)
	_ observe.ModelReader        = (*Session)(nil)
	_ observe.EffortReader       = (*Session)(nil)
)
