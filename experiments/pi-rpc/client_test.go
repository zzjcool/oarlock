package pirpc

import (
	"bufio"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// TestReadFramesIsLFOOnly pins the dialect: LF is the only separator, U+2028 and
// U+2029 may live inside a JSON string, and a trailing LF yields no empty frame.
func TestReadFramesIsLFOOnly(t *testing.T) {
	one := `{"type":"message_update","text":"a` + "\u2028" + `b"}` + "\n"
	var got []Frame
	err := ReadFrames(strings.NewReader(one+`{"type":"turn_end"}`+"\n"), MaxLineBytes, func(f Frame) error {
		got = append(got, f)
		return nil
	})
	if err != nil || len(got) != 2 || got[0].Type != "message_update" || string(got[0].Raw) != strings.TrimSpace(one) || got[1].Type != "turn_end" {
		t.Fatalf("frames=%d err=%v got=%+v", len(got), err, got)
	}
}

// TestReadFramesReadsFiveMegabyteLine: the 64 KiB start buffer must grow, not
// truncate, and the 5 MB field must decode intact.
func TestReadFramesReadsFiveMegabyteLine(t *testing.T) {
	text := strings.Repeat("a", 5<<20)
	line := `{"type":"message_update","text":"` + text + `"}`
	var got []Frame
	if err := ReadFrames(strings.NewReader(line+"\n"), MaxLineBytes, func(f Frame) error {
		got = append(got, f)
		return nil
	}); err != nil || len(got) != 1 || len(got[0].Raw) != len(line) {
		t.Fatalf("frames=%d err=%v", len(got), err)
	}
	var m struct{ Text string }
	if err := json.Unmarshal(got[0].Raw, &m); err != nil || len(m.Text) != len(text) {
		t.Fatalf("text=%d err=%v", len(m.Text), err)
	}
}

// TestReadFramesRejectsOverCapLine: above the cap the line errors, never truncates.
func TestReadFramesRejectsOverCapLine(t *testing.T) {
	var got []Frame
	err := ReadFrames(strings.NewReader(`{"t":"`+strings.Repeat("x", MaxLineBytes)+`"}`+"\n"), MaxLineBytes, func(f Frame) error {
		got = append(got, f)
		return nil
	})
	if !errors.Is(err, bufio.ErrTooLong) || len(got) != 0 {
		t.Fatalf("err=%v frames=%d", err, len(got))
	}
}

// TestDispatchResolvesResponseByIDAndForwardsEveryFrame proves the two deliveries
// are independent: the waiter resolves and every frame, response included, still
// reaches Frames() in order with unknown types byte-for-byte.
func TestDispatchResolvesResponseByIDAndForwardsEveryFrame(t *testing.T) {
	c := &Client{frames: make(chan Frame, 8), pending: map[string]chan Frame{}, exited: make(chan struct{})}
	wait := make(chan Frame, 1)
	c.pending["probe-1"] = wait
	in := []Frame{
		{Type: "unknown_type", Raw: json.RawMessage(`{"type":"unknown_type","x":1}`)},
		{Type: "response", ID: "probe-1", Raw: json.RawMessage(`{"type":"response","id":"probe-1","command":"get_state","success":true}`)},
	}
	for _, f := range in {
		if err := c.dispatch(f); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case f := <-wait:
		if f.ID != "probe-1" || !strings.Contains(string(f.Raw), `"success":true`) {
			t.Fatalf("waiter got %+v", f)
		}
	default:
		t.Fatal("waiter did not resolve")
	}
	for i, f := range in {
		got := <-c.frames
		if got.Type != f.Type || string(got.Raw) != string(f.Raw) {
			t.Fatalf("frame %d = %+v, want %+v", i, got, f)
		}
	}
	if _, still := c.pending["probe-1"]; still {
		t.Fatal("pending entry survived its response")
	}
}
