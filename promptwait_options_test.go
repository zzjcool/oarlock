package oarlock_test

import (
	"context"
	"reflect"
	"slices"
	"sync"
	"testing"

	"github.com/zzjcool/oarlock"
)

// The ids of the one fixture below. They are constants only so the request,
// the frames and the cursor agree without a helper.
const (
	promptOptsSessionID = "po-sess"
	promptOptsRequestID = "po-r1"
)

// promptOptsStream is the minimal record stream of the prompt-options tests:
// the retained records and the live observers under ONE mutex, the callback
// invoked under it, exactly like the kernel's ObserveFrom dispatch on the
// appending goroutine. The fixture appends everything inside Prompt, so no
// subscription is ever live here; the stream still implements the seam honestly.
type promptOptsStream struct {
	mu      sync.Mutex
	records []oarlock.Record
	subs    []func(oarlock.Record)
}

// emit stamps the next Seq, retains rec and hands it to every live observer,
// then returns the stamped record: the caller's request carries its position.
func (f *promptOptsStream) emit(rec oarlock.Record) oarlock.Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec.Seq = uint64(len(f.records) + 1)
	f.records = append(f.records, rec)
	for _, fn := range f.subs {
		if fn != nil {
			fn(rec)
		}
	}
	return rec
}

// events replays and registers in one critical section, like ObserveFrom.
func (f *promptOptsStream) events(cursor *oarlock.Cursor, fn func(oarlock.Record)) func() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if cursor != nil {
		for _, rec := range f.records {
			if rec.Seq > cursor.AfterSeq && (cursor.SessionID == "" || cursor.SessionID == rec.SessionID) {
				fn(rec)
			}
		}
	}
	f.subs = append(f.subs, fn)
	id := len(f.subs) - 1
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.subs[id] = nil
	}
}

func (f *promptOptsStream) read(cursor oarlock.Cursor) []oarlock.Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []oarlock.Record
	for _, rec := range f.records {
		if rec.Seq > cursor.AfterSeq && (cursor.SessionID == "" || cursor.SessionID == rec.SessionID) {
			out = append(out, rec)
		}
	}
	return out
}

// promptOptsSession is a session whose only job is to record what Prompt was
// handed and then answer with one completed turn, so PromptAndWait returns
// promptly and the test can look at the options alone.
type promptOptsSession struct {
	st promptOptsStream

	// got and input are written inside Prompt and read after PromptAndWait
	// returned: both run on the test's own goroutine, so no lock is needed.
	got   oarlock.PromptOptions
	input string
}

func (s *promptOptsSession) Prompt(_ context.Context, input string, opt oarlock.PromptOptions) (oarlock.ControlOutcome, error) {
	s.got = opt
	s.input = input
	req := s.st.emit(oarlock.Record{
		Kind:      oarlock.KindRequest,
		SessionID: promptOptsSessionID,
		ID:        promptOptsRequestID,
		Direction: oarlock.ToRuntime,
		Request:   &oarlock.RequestBody{Action: oarlock.ActionPrompt, Input: input},
	})
	s.st.emit(oarlock.Record{
		Kind:      oarlock.KindFrame,
		SessionID: promptOptsSessionID,
		Events:    []oarlock.Event{{Kind: oarlock.EventTextDelta, Text: "ok", MessageID: "m1"}},
	})
	s.st.emit(oarlock.Record{
		Kind:      oarlock.KindFrame,
		SessionID: promptOptsSessionID,
		Events: []oarlock.Event{{
			Kind:    oarlock.EventTurnEnded,
			Outcome: &oarlock.TurnOutcome{Kind: oarlock.OutcomeCompleted},
		}},
	})
	return oarlock.ControlOutcome{
		Kind:      oarlock.ControlAccepted,
		Seq:       req.Seq,
		RequestID: promptOptsRequestID,
		Request:   req,
	}, nil
}

func (s *promptOptsSession) Queue(context.Context, string, oarlock.InputOptions) (oarlock.ControlOutcome, error) {
	return oarlock.ControlOutcome{}, nil
}

func (s *promptOptsSession) Abort(context.Context) (oarlock.ControlOutcome, error) {
	return oarlock.ControlOutcome{}, nil
}

func (s *promptOptsSession) Dispose(context.Context) error { return nil }

func (s *promptOptsSession) Records(c oarlock.Cursor) []oarlock.Record { return s.st.read(c) }

func (s *promptOptsSession) Events(c *oarlock.Cursor, fn func(oarlock.Record)) func() {
	return s.st.events(c, fn)
}

func (s *promptOptsSession) Capabilities() oarlock.Capabilities { return oarlock.Capabilities{} }

// TestPromptAndWait_WithPromptOptionsReachesTheSession pins that the options
// WithPromptOptions hands over survive the call unchanged: origin, inputID,
// every image's path and media type, and the carried input ids.
func TestPromptAndWait_WithPromptOptionsReachesTheSession(t *testing.T) {
	s := &promptOptsSession{}
	want := oarlock.PromptOptions{
		InputOptions: oarlock.InputOptions{
			InputID: "in-42",
			Origin:  "oarlock-cli",
			Images: []oarlock.InputImage{
				{Path: "/tmp/one.png", MediaType: "image/png"},
				{Path: "/tmp/two.jpg", MediaType: "image/jpeg"},
			},
		},
		Carries: []string{"in-1", "in-2"},
	}

	run, err := oarlock.PromptAndWait(context.Background(), s, "look", oarlock.WithPromptOptions(want))
	if err != nil {
		t.Fatalf("PromptAndWait: %v", err)
	}
	if run.Outcome != oarlock.RunCompleted {
		t.Fatalf("Outcome = %q, want %q", run.Outcome, oarlock.RunCompleted)
	}
	if run.Text != "ok" {
		t.Fatalf("Text = %q, want %q", run.Text, "ok")
	}

	if s.input != "look" {
		t.Errorf("Prompt input = %q, want %q", s.input, "look")
	}
	got := s.got
	if got.Origin != want.Origin {
		t.Errorf("Origin = %q, want %q", got.Origin, want.Origin)
	}
	if got.InputID != want.InputID {
		t.Errorf("InputID = %q, want %q", got.InputID, want.InputID)
	}
	if len(got.Images) != len(want.Images) {
		t.Fatalf("len(Images) = %d, want %d", len(got.Images), len(want.Images))
	}
	for i, img := range got.Images {
		if img.Path != want.Images[i].Path || img.MediaType != want.Images[i].MediaType {
			t.Errorf("Images[%d] = %+v, want %+v", i, img, want.Images[i])
		}
	}
	if !slices.Equal(got.Carries, want.Carries) {
		t.Errorf("Carries = %q, want %q", got.Carries, want.Carries)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("PromptOptions = %+v, want %+v", got, want)
	}
}

// TestPromptAndWait_WithoutPromptOptionsSendsTheZeroValue pins the other half:
// a caller that omits the option (or passes a nil one) still sends the zero
// PromptOptions, exactly as before the option existed.
func TestPromptAndWait_WithoutPromptOptionsSendsTheZeroValue(t *testing.T) {
	s := &promptOptsSession{}

	run, err := oarlock.PromptAndWait(context.Background(), s, "plain", nil, oarlock.WithTimeout(0))
	if err != nil {
		t.Fatalf("PromptAndWait: %v", err)
	}
	if run.Outcome != oarlock.RunCompleted {
		t.Fatalf("Outcome = %q, want %q", run.Outcome, oarlock.RunCompleted)
	}
	if !reflect.DeepEqual(s.got, oarlock.PromptOptions{}) {
		t.Errorf("PromptOptions = %+v, want the zero value", s.got)
	}
}
