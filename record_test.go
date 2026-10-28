package oarlock

import (
	"encoding/json"
	"testing"
	"time"
)

func TestRecord_JSONRoundTripsAFrameWithEvents(t *testing.T) {
	exit := 143
	in := Record{
		Seq:        7,
		ReceivedAt: time.UnixMilli(1_700_000_000_000).UTC(),
		Kind:       KindFrame,
		SessionID:  "sess-1",
		AgentPath:  []string{"root"},
		SpanID:     "turn-9",
		Native:     json.RawMessage(`{"type":"tool_end","exit_code":143}`),
		Events: []Event{{
			Kind:             EventToolCallEnded,
			CallID:           "call-3",
			Tool:             "bash",
			Result:           ToolFailed,
			ExitCode:         &exit,
			ExitCodeReported: true,
			Content:          []ToolOutputPart{{Type: "text", Text: "boom"}},
		}},
	}

	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out Record
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if out.Seq != in.Seq || out.Kind != KindFrame || out.SessionID != "sess-1" {
		t.Fatalf("envelope changed: %+v", out)
	}
	if string(out.Native) != string(in.Native) {
		t.Fatalf("native changed: %s", out.Native)
	}
	if len(out.Events) != 1 || out.Events[0].Kind != EventToolCallEnded {
		t.Fatalf("events changed: %+v", out.Events)
	}
	if !out.Events[0].ExitCodeReported || out.Events[0].ExitCode == nil || *out.Events[0].ExitCode != 143 {
		t.Fatalf("exit code changed: %+v", out.Events[0])
	}
}

func TestEvent_UnreportedExitCodeStaysDistinctFromReportedNull(t *testing.T) {
	unreported := Event{Kind: EventToolCallEnded, CallID: "c1"}
	raw, err := json.Marshal(unreported)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Event
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.ExitCodeReported {
		t.Fatalf("unreported exit became reported: %s", raw)
	}

	reportedNull := Event{Kind: EventToolCallEnded, CallID: "c2", ExitCodeReported: true}
	raw, err = json.Marshal(reportedNull)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !back.ExitCodeReported || back.ExitCode != nil {
		t.Fatalf("reported null was lost: %s", raw)
	}
}

func TestCursor_ZeroValueIsTheStartOfTheStream(t *testing.T) {
	var c Cursor
	if c.AfterSeq != 0 {
		t.Fatalf("zero cursor must mean after-seq 0 (the first record is 1), got %d", c.AfterSeq)
	}
	raw, err := json.Marshal(Cursor{SessionID: "s", AfterSeq: 41})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Cursor
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.SessionID != "s" || back.AfterSeq != 41 {
		t.Fatalf("cursor changed: %+v", back)
	}
}
