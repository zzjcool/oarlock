package fold

import "github.com/zzjcool/oarlock"

// foldContext keeps the latest context fullness a root record reported. A
// report without a context number says nothing about fullness and is skipped;
// a child agent's or child session's report never answers for the root.
func (f *Fold) foldContext(r oarlock.Record, root bool) {
	if !root {
		return
	}
	for _, ev := range r.Events {
		if ev.Kind != oarlock.EventUsage || ev.Usage == nil || ev.Usage.Context == nil {
			continue
		}
		f.context = &ContextReport{Usage: cloneContextUsage(*ev.Usage.Context), Seq: r.Seq}
	}
}

// ContextUsage answers the latest reported context fullness for the root
// agent, nil before any report. The answer's own Seq is the reporting record's;
// the QueryResult's Seq is the last record consumed, which is what aligns the
// answer with the stream.
func (f *Fold) ContextUsage() QueryResult[*ContextReport] {
	var report *ContextReport
	if f.context != nil {
		copied := *f.context
		copied.Usage = cloneContextUsage(f.context.Usage)
		report = &copied
	}
	return QueryResult[*ContextReport]{Value: report, Seq: f.seq}
}
