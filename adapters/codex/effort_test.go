package codex

// Issue #54 §E / §G-W-D: effort on a resumed thread.
//
// codex persists a thread's reasoning effort, so a resume cannot carry the
// `config.model_reasoning_effort` override every turn of a new thread rides on
// (any override on a resume rebuilds the thread's settings from config.toml).
// The adapter therefore sets the level with thread/settings/update, waits for
// codex's own thread/settings/updated, and refuses the open unless that report
// names exactly the requested level. Everything below is asserted from the
// wire the fake app-server saw and from the recorded stream — never from
// Session internals.

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/observe"
)

// §G-W-D 1: a resume with Effort sets it with thread/settings/update
// {threadId, effort}, codex's confirmation is a recorded
// thread/settings/updated frame carrying the effort event, the open succeeds
// and the readback answers the requested level.
func TestResumeEffortIsSetAndConfirmed(t *testing.T) {
	const level = "high"
	// resume-effort-kept: the thread is saved with effort "medium", so the open
	// can only succeed by really setting — and confirming — the new level
	// instead of trusting the thread's saved settings.
	e := newFake(t, "resume-effort-kept")
	s := e.mustOpen(oarlock.Options{Resume: rootThread, Effort: level}, Config{})
	if s.ThreadID() != rootThread {
		t.Fatalf("thread = %q, want %q", s.ThreadID(), rootThread)
	}

	// The exact request: the root thread, the requested level, nothing else.
	want := map[string]any{"threadId": rootThread, "effort": level}
	if got := e.sent("thread/settings/update", 0); !reflect.DeepEqual(got, want) {
		t.Fatalf("thread/settings/update params = %#v, want %#v", got, want)
	}
	// The whole wire: exactly one settings update, after the resume, and no
	// other RPC on the way to a usable session.
	wantWire := []string{"PROBE", "START", "initialize", "initialized", "thread/resume", "thread/settings/update"}
	if got := e.methods(); !reflect.DeepEqual(got, wantWire) {
		t.Fatalf("wire = %v, want %v", got, wantWire)
	}
	resume := e.sent("thread/resume", 0)
	if resume["threadId"] != rootThread {
		t.Errorf("thread/resume params = %v", resume)
	}
	if cfg, ok := resume["config"]; ok {
		t.Errorf("thread/resume carried the config override %v; a resume must set effort with thread/settings/update", cfg)
	}

	// codex's own confirmation: a recorded frame of the root thread, and the
	// effort event the readback folds (the pump may land it just after open).
	waitFor(t, "the thread/settings/updated frame", func() bool {
		return len(settingsFrames(s.Records(oarlock.Cursor{}))) == 1
	})
	recs := s.Records(oarlock.Cursor{})
	frame := settingsFrames(recs)[0]
	if frame.Kind != oarlock.KindFrame || frame.SessionID != rootThread {
		t.Fatalf("confirmation frame = kind %v session %q; stream = %q", frame.Kind, frame.SessionID, skeleton(recs))
	}
	var confirmed []oarlock.Event
	for _, ev := range frame.Events {
		if ev.Kind == oarlock.EventEffort {
			confirmed = append(confirmed, ev)
		}
	}
	if len(confirmed) != 1 || confirmed[0].Effort != level {
		t.Fatalf("confirmation frame effort events = %+v, want one %q (stream = %q)", confirmed, level, skeleton(recs))
	}

	// The readback answers the confirmed level, from that very record.
	if got := observe.EffortOf(recs); got.Value != level || got.Seq != frame.Seq {
		t.Errorf("observe.EffortOf = {%q @%d}, want {%q @%d} (the thread/settings/updated record)",
			got.Value, got.Seq, level, frame.Seq)
	}
	if got := s.Effort().Value; got != level {
		t.Errorf("Session.Effort() = %q, want %q", got, level)
	}
}

// §G-W-D 2: every way codex can fail to confirm the level refuses the open:
// a silent app-server, a report naming another level, and codex's own RPC
// error. Each refusal names what went wrong and returns no Session.
func TestResumeEffortRefusals(t *testing.T) {
	const level = "high"
	for _, tc := range []struct {
		name     string
		scenario string
		want     string // the refusal text the contract fixes
		names    bool   // the error must also say which level it could not set
		why      string
	}{
		{"silent", "settings-silent", "cannot be confirmed", true,
			"no thread/settings/updated within SettingsReportTimeout"},
		{"mismatch", "settings-mismatch", "minimal", false,
			"codex reports the level it applied, not the requested one"},
		{"rpc-error", "settings-error", "thread not found", false,
			"codex's own message reaches the caller"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newFake(t, tc.scenario)
			s, err := e.open(oarlock.Options{Resume: rootThread, Effort: level}, Config{})
			if s != nil {
				id := s.ThreadID()
				_ = s.Dispose(bg)
				t.Fatalf("open returned a session for thread %q although the effort was not confirmed", id)
			}
			if err == nil {
				t.Fatal("open succeeded although the effort was not confirmed")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v; want it to contain %q (%s)", err, tc.want, tc.why)
			}
			if tc.names && !strings.Contains(err.Error(), level) {
				t.Errorf("err = %v; a refused %q must name the level it could not confirm", err, level)
			}
			// The refusal happens *at* the settings call: it went out, and the
			// open sent nothing else after it (a refused open leaves no agent
			// running on an unconfirmed level).
			methods := e.methods()
			if !slices.Contains(methods, "thread/settings/update") {
				t.Fatalf("wire = %v; the effort was never sent to codex", methods)
			}
			if last := methods[len(methods)-1]; last != "thread/settings/update" {
				t.Errorf("wire = %v; nothing may run after a refused thread/settings/update", methods)
			}
		})
	}
}

// §G-W-D 3: a new thread still sets effort with the thread/start
// config.model_reasoning_effort override (codex persists it with the thread and
// applies it to every turn, queued turns included) and never with
// thread/settings/update.
func TestNewThreadEffortIsAConfigOverride(t *testing.T) {
	const level = "high"
	e := newFake(t, "")
	s := e.mustOpen(oarlock.Options{Effort: level}, Config{})

	want := map[string]any{"model_reasoning_effort": level}
	start := e.sent("thread/start", 0)
	if got, _ := start["config"].(map[string]any); !reflect.DeepEqual(got, want) {
		t.Fatalf("thread/start config = %#v, want %#v", start["config"], want)
	}
	wantWire := []string{"PROBE", "START", "initialize", "initialized", "thread/start"}
	if got := e.methods(); !reflect.DeepEqual(got, wantWire) {
		t.Fatalf("wire = %v, want %v (a new thread needs no thread/settings/update)", got, wantWire)
	}
	// The fake echoes the override as the thread's reasoningEffort, so the
	// ordinary open readback answers — no settings round trip involved.
	waitFor(t, "the effort readback of the new thread", func() bool { return s.Effort().Value == level })
	if got := observe.EffortOf(s.Records(oarlock.Cursor{})); got.Value != level {
		t.Errorf("observe.EffortOf = %q, want %q (stream = %q)", got.Value, level, skeleton(s.Records(oarlock.Cursor{})))
	}
}

// settingsFrames are the recorded thread/settings/updated notifications: the
// evidence §E waits on, kept as a frame of the stream like every other
// notification.
func settingsFrames(recs []oarlock.Record) []oarlock.Record {
	var out []oarlock.Record
	for _, r := range recs {
		if r.Kind == oarlock.KindFrame && frameMethod(r.Native) == "thread/settings/updated" {
			out = append(out, r)
		}
	}
	return out
}

// frameMethod is the notification/request method a retained frame carries
// ("" for anything without one, e.g. a reply).
func frameMethod(raw []byte) string {
	var m struct {
		Method string `json:"method"`
	}
	_ = json.Unmarshal(raw, &m)
	return m.Method
}
