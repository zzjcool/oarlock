package streamjson

import (
	"testing"

	"github.com/zzjcool/oarlock"
)

// nullProfile is the smallest vendor: proves the fold needs nothing of Claude.
type nullProfile struct{ compacted, other int }

func (p *nullProfile) Assistant(Rec, bool) {}
func (p *nullProfile) Result(m Rec, path []string, aborted bool) (oarlock.TurnOutcome, []oarlock.Event, []oarlock.Event) {
	if aborted {
		return oarlock.TurnOutcome{Kind: oarlock.OutcomeAborted}, nil, nil
	}
	return oarlock.TurnOutcome{Kind: oarlock.OutcomeCompleted}, nil, nil
}
func (p *nullProfile) Init(Rec, bool) []oarlock.Event       { return nil }
func (p *nullProfile) Compacted(bool)                       { p.compacted++ }
func (p *nullProfile) System(Rec, []string) []oarlock.Event { return nil }
func (p *nullProfile) Other(Rec)                            { p.other++ }

func TestFolderIsVendorNeutral(t *testing.T) {
	p := &nullProfile{}
	f := NewFolder(p)
	fr := f.Fold([]byte(`{"type":"system","subtype":"init","model":"m"}`))
	if len(fr.Events) != 2 || fr.Events[0].Kind != oarlock.EventTurnActive || fr.Events[1].Model != "m" {
		t.Fatalf("init = %+v", fr.Events)
	}
	f.Prompted()
	if fr := f.Fold([]byte(`{"type":"system","subtype":"init"}`)); len(fr.Events) != 0 {
		t.Fatalf("a prompted turn's init must not read as spontaneous: %+v", fr.Events)
	}
	f.Fold([]byte(`{"type":"rate_limit_event"}`))
	f.Fold([]byte(`{"type":"system","subtype":"compact_boundary"}`))
	if p.other != 1 || p.compacted != 1 {
		t.Fatalf("hooks: other=%d compacted=%d", p.other, p.compacted)
	}
	f.AbortRequested()
	if fr := f.Fold([]byte(`{"type":"result","subtype":"success"}`)); fr.Events[0].Outcome.Kind != oarlock.OutcomeAborted || !fr.TurnEnded {
		t.Fatalf("result = %+v", fr)
	}
	// The abort intent is spent: the next result is a normal one.
	if fr := f.Fold([]byte(`{"type":"result","subtype":"success"}`)); fr.Events[0].Outcome.Kind != oarlock.OutcomeCompleted {
		t.Fatalf("result = %+v", fr)
	}
}

func TestSubAgentResultDoesNotEndTheRootTurn(t *testing.T) {
	f := NewFolder(&nullProfile{})
	f.Fold([]byte(`{"type":"assistant","message":{"id":"m1","content":[{"type":"tool_use","id":"call1","name":"Task","input":{}}]}}`))
	child := f.Fold([]byte(`{"type":"result","parent_tool_use_id":"call1"}`))
	if child.TurnEnded || len(child.AgentPath) != 1 || child.AgentPath[0] != "call1" {
		t.Fatalf("child result = %+v", child)
	}
}

func TestControlFrames(t *testing.T) {
	f := NewFolder(&nullProfile{})
	if r := f.Fold([]byte(`{"type":"control_response","response":{"subtype":"error","request_id":"r1","error":"nope"}}`)).Response; r == nil || r.OK || r.Error != "nope" || r.RequestID != "r1" {
		t.Fatalf("response = %+v", r)
	}
	if r := f.Fold([]byte(`{"type":"control_response","response":{"subtype":"success","request_id":"r2"}}`)).Response; r == nil || !r.OK {
		t.Fatalf("response = %+v", r)
	}
	if fr := f.Fold([]byte(`{"type":"control_cancel_request","request_id":"p1"}`)); fr.Cancelled != "p1" || fr.Events[0].Kind != oarlock.EventAppRequestCancelled {
		t.Fatalf("cancel = %+v", fr)
	}
	if fr := f.Fold([]byte(`not json`)); fr.Type != "unknown" || f.Unknown()[""] != 1 {
		t.Fatalf("garbage = %+v / %v", fr, f.Unknown())
	}
}

func TestToolResultContentShapes(t *testing.T) {
	cases := map[string]struct {
		content string
		parts   int
		first   string
	}{
		"string": {`"hi"`, 1, "text"},
		"list":   {`[{"type":"text","text":"a"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA=="}}]`, 2, "text"},
		"empty":  {`[]`, 0, ""},
		"object": {`{"k":1}`, 1, "other"},
	}
	for name, c := range cases {
		ev := ToolResults([]byte(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t","content":` + c.content + `}]}}`))
		if len(ev) != 1 || ev[0].Result != oarlock.ToolOK || len(ev[0].Content) != c.parts || (c.parts > 0 && ev[0].Content[0].Type != c.first) {
			t.Errorf("%s: %+v", name, ev)
		}
	}
	ev := ToolResults([]byte(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t","content":"x","is_error":true}]}}`))
	if ev[0].Result != oarlock.ToolFailed {
		t.Fatalf("is_error:true = %+v", ev[0])
	}
	if ev := ToolResults([]byte(`{"type":"user","message":{"content":"plain string"}}`)); len(ev) != 0 {
		t.Fatalf("string content = %+v", ev)
	}
}

func TestRedactedThinkingAndEmptyThinking(t *testing.T) {
	f := NewFolder(&nullProfile{})
	ev := f.Fold([]byte(`{"type":"assistant","message":{"id":"m","content":[{"type":"redacted_thinking"},{"type":"thinking","thinking":""}]}}`)).Events
	if len(ev) != 2 || !ev[0].Redacted || ev[1].Kind != oarlock.EventReasoning || ev[1].Text != "" {
		t.Fatalf("events = %+v", ev)
	}
}
