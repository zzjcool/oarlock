package voyage_test

// The issue #17 live acceptance: record a real Pi session with the voyage
// writer, replay it with no process or network, and prove the folded
// Status/Usage/Context match what the live session reported.
//
// It needs the real pi 1.4.0, its login and its session directory, so it is
// opt-in. The recorded log stays in a temp dir; the session file Pi writes is
// removed with its per-cwd session directory.

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/adapters/pi"
	"github.com/zzjcool/oarlock/internal/fold"
	"github.com/zzjcool/oarlock/internal/voyage"
)

func TestLiveRecordReplayMatchesTheSession(t *testing.T) {
	if os.Getenv("OAR_PI_LIVE") != "1" {
		t.Skip("set OAR_PI_LIVE=1 to record a real Pi session")
	}
	if _, err := exec.LookPath("pi"); err != nil {
		t.Skipf("pi is not on PATH: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cwd := t.TempDir()
	agent, err := pi.AgentDir(os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(pi.SessionDir(cwd, agent)) })

	sess, err := pi.Open(ctx, oarlock.Installation{}, oarlock.Options{Cwd: cwd})
	if err != nil {
		t.Skipf("pi could not be opened here (login or model missing?): %v", err)
	}
	if out, err := sess.Prompt(ctx, "Reply with the single word: ok", oarlock.PromptOptions{}); err != nil || out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt = %+v, %v", out, err)
	}
	deadline := time.Now().Add(150 * time.Second)
	for fold.StatusOf(sess.Records(oarlock.Cursor{})).Value.Kind != fold.StatusIdle {
		if time.Now().After(deadline) {
			t.Fatalf("no idle after the turn; %d records", len(sess.Records(oarlock.Cursor{})))
		}
		time.Sleep(50 * time.Millisecond)
	}
	// The post-settlement get_session_stats frame is what makes context part of
	// the live answer; wait for it rather than comparing a half-recorded stream.
	for fold.ContextUsageOf(sess.Records(oarlock.Cursor{})).Value == nil {
		if time.Now().After(deadline) {
			break // an honest "Pi did not report context" is still comparable
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := sess.Dispose(ctx); err != nil {
		t.Fatal(err)
	}
	records := sess.Records(oarlock.Cursor{})
	live := struct {
		Status  fold.QueryResult[fold.Status]
		Usage   fold.QueryResult[fold.Usage]
		Context fold.QueryResult[*fold.ContextReport]
	}{
		Status:  fold.StatusOf(records),
		Usage:   fold.UsageOf(records),
		Context: fold.ContextUsageOf(records),
	}

	path := filepath.Join(t.TempDir(), "live.jsonl")
	w, err := voyage.Create(path, voyage.Header{
		Runtime:   "pi",
		Cwd:       cwd,
		SessionID: records[0].SessionID,
		StartedAt: records[0].ReceivedAt.UnixMilli(),
		Recorder:  "oarlock-live-test",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range records {
		if err := w.Append(r); err != nil {
			t.Fatalf("append seq %d: %v", r.Seq, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	log, err := voyage.Read(path)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !log.Complete() {
		t.Fatal("the recorded log has no end line")
	}
	replayed := struct {
		Status  fold.QueryResult[fold.Status]
		Usage   fold.QueryResult[fold.Usage]
		Context fold.QueryResult[*fold.ContextReport]
	}{
		Status:  fold.StatusOf(log.Records),
		Usage:   fold.UsageOf(log.Records),
		Context: fold.ContextUsageOf(log.Records),
	}
	a, _ := json.Marshal(live)
	b, _ := json.Marshal(replayed)
	if string(a) != string(b) {
		t.Fatalf("replay differs from live:\n live=%s\n replay=%s", a, b)
	}
	if live.Status.Value.Kind != fold.StatusExited {
		t.Fatalf("live status = %s, want exited after dispose", live.Status.Value.Kind)
	}
}
