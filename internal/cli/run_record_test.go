package cli

// Integration tests of the crash-safe `--record` (issue #99): the voyage log is
// written while the run proceeds, so the durable prefix survives a killed
// process, and voyage.Create's O_EXCL is the authoritative no-overwrite guard
// even when the file appears between prepare and the first append.
//
// Everything here reuses the it* seam builders from run_integration_test.go. New
// helpers are prefixed "itRec" so they cannot collide with the other test files
// in this package, and no test calls t.Parallel: each signal test owns the App's
// injected interrupt channel while the run is live.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/testing/scripted"
	"github.com/zzjcool/oarlock/internal/voyage"
)

// itRecPollPrefix waits, bounded by itDeadline, until path is a readable voyage
// log that is still being written: the header names the session, at least
// minRecords records are durable, and the read reports ErrTruncated (no end
// line yet). It returns the last read, so the caller can compare the final log
// against the prefix it actually saw.
func itRecPollPrefix(t *testing.T, path string, minRecords int) *voyage.Log {
	t.Helper()
	var prefix *voyage.Log
	itPoll(t, fmt.Sprintf("an incremental record prefix at %s", path), func() bool {
		log, err := voyage.Read(path)
		if log == nil || !errors.Is(err, voyage.ErrTruncated) {
			return false
		}
		if log.Header.SessionID == "" || len(log.Records) < minRecords {
			return false
		}
		prefix = log
		return true
	})
	return prefix
}

// itRecAssertDense checks the record-stream invariant every voyage log promises
// (docs/spec/record-stream.md): Seq runs 1, 2, 3, ... with no gap, no repeat and
// no restart.
func itRecAssertDense(t *testing.T, records []oarlock.Record) {
	t.Helper()
	for i, rec := range records {
		if want := uint64(i + 1); rec.Seq != want {
			t.Fatalf("record %d has seq %d, want the dense value %d", i, rec.Seq, want)
		}
	}
}

// itRecAbandonedPath is a record path in a directory the test framework is not
// asked to delete. The forced-exit path abandons the writer without closing it
// (the contract leaves that to the process exit, which a test process does not
// do), and Windows refuses to remove a file whose handle is still open. A
// best-effort cleanup keeps the temp directory tidy where the platform allows
// it.
func itRecAbandonedPath(t *testing.T, name string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "oarlock-it-record-")
	if err != nil {
		t.Fatalf("creating a temp dir for the record: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, name)
}

// itRecAssertLive fails when a started run has already returned. It is how a
// test proves the record reached the disk before the command finished rather
// than in one piece at the end.
func itRecAssertLive(t *testing.T, done <-chan int, what string) {
	t.Helper()
	select {
	case code := <-done:
		t.Fatalf("%s returned %d while the turn was still live: the record was written only at the finish", what, code)
	default:
	}
}

// TestITRunRecordIncrementalBeforeFinish pins the crash-safe write: while the
// turn is still live the file already carries the header and a durable prefix,
// and the first SIGINT then ends the run as interrupted with a complete log.
func TestITRunRecordIncrementalBeforeFinish(t *testing.T) {
	cwd := t.TempDir()
	path := filepath.Join(t.TempDir(), "it-incremental.oar")
	signals := make(chan os.Signal, 2)
	rt := itNewRuntime(itRuntimeID, scripted.Turn{scripted.Say("partial"), scripted.Hang()})
	app, out, _ := itNewApp(t, rt, signals)

	done := itRunAsync(t, app,
		"run", itRuntimeID, "it hello",
		"--json",
		"--cwd", cwd,
		"--record", path,
	)

	// The prefix is on disk while the turn is live: header + the session's own
	// first records, and no end line yet.
	prefix := itRecPollPrefix(t, path, 2)
	itRecAssertLive(t, done, "run --record before the interrupt")
	if prefix.Header.Runtime != itRuntimeID {
		t.Errorf("prefix header runtime = %q, want %q", prefix.Header.Runtime, itRuntimeID)
	}

	signals <- os.Interrupt
	code := itAwaitExit(t, done, "first SIGINT with --record")
	if code != ExitInterrupt {
		t.Fatalf("exit = %d, want %d (stdout: %s)", code, ExitInterrupt, out.String())
	}

	final, err := voyage.Read(path)
	if err != nil {
		t.Fatalf("voyage.Read(%s) after the interrupt: %v", path, err)
	}
	if final.End == nil {
		t.Fatalf("the record has no end line after the interrupted run: %d records, header %+v", len(final.Records), final.Header)
	}
	if want := string(oarlock.RunInterrupted); final.End.Reason != want {
		t.Errorf("end reason = %q, want %q", final.End.Reason, want)
	}
	if final.Header.SessionID == "" {
		t.Errorf("header sessionId is empty, want the session's id")
	}
	if final.Header.SessionID != prefix.Header.SessionID {
		t.Errorf("header sessionId = %q, want the prefix's %q", final.Header.SessionID, prefix.Header.SessionID)
	}
	if len(final.Records) < len(prefix.Records) {
		t.Fatalf("the final record has %d records, fewer than the %d already durable before the interrupt", len(final.Records), len(prefix.Records))
	}
	itRecAssertDense(t, final.Records)
	// The incremental writer plus the finish-time tail must be the session's
	// stream exactly: no lost record, no duplicate.
	if err := itCompareRecords(final.Records, rt.itOpenedSession(t).Records(oarlock.Cursor{})); err != nil {
		t.Fatalf("the incremental record is not the session's stream: %v", err)
	}
}

// TestITRunRecordSecondSIGINTLeavesTruncatedPrefix pins the killed-process case:
// the second SIGINT disposes and leaves at once, so what is on disk is the
// durable prefix with no end line. Read salvages it, Replay refuses it.
func TestITRunRecordSecondSIGINTLeavesTruncatedPrefix(t *testing.T) {
	cwd := t.TempDir()
	path := itRecAbandonedPath(t, "it-stubborn.oar")
	signals := make(chan os.Signal, 2)
	rt := itNewStubbornRuntime(itRuntimeID, scripted.Turn{scripted.IgnoreAbort(), scripted.Hang()})
	app, out, _ := itNewApp(t, rt, signals)

	done := itRunAsync(t, app,
		"run", itRuntimeID, "it hello",
		"--json",
		"--cwd", cwd,
		"--record", path,
	)

	prefix := itRecPollPrefix(t, path, 2)
	itRecAssertLive(t, done, "run --record before the forced interrupt")

	start := time.Now()
	signals <- os.Interrupt
	signals <- os.Interrupt
	code := itAwaitExit(t, done, "second SIGINT with --record")
	elapsed := time.Since(start)

	if code != ExitInterrupt {
		t.Fatalf("exit = %d, want %d (stdout: %s)", code, ExitInterrupt, out.String())
	}
	if elapsed >= itForcedBound {
		t.Fatalf("the forced exit took %v (bound %v): the record writer waited for the turn instead of abandoning the prefix", elapsed, itForcedBound)
	}

	log, err := voyage.Read(path)
	if log == nil {
		t.Fatalf("voyage.Read(%s) = nil log, want the salvaged prefix (err: %v)", path, err)
	}
	if !errors.Is(err, voyage.ErrTruncated) {
		t.Fatalf("voyage.Read(%s) error = %v, want %v", path, err, voyage.ErrTruncated)
	}
	if log.End != nil {
		t.Fatalf("the record has an end line (%+v), want the end-less prefix of a killed process", *log.End)
	}
	if log.Header.SessionID == "" {
		t.Fatalf("the salvaged prefix has no header session id: %+v", log.Header)
	}
	if log.Header.SessionID != prefix.Header.SessionID {
		t.Errorf("header sessionId = %q, want the prefix's %q", log.Header.SessionID, prefix.Header.SessionID)
	}
	if len(log.Records) < len(prefix.Records) {
		t.Fatalf("the salvaged prefix has %d records, fewer than the %d durable before the forced exit", len(log.Records), len(prefix.Records))
	}
	itRecAssertDense(t, log.Records)
	if records, err := voyage.Replay(path); err == nil {
		t.Fatalf("voyage.Replay accepted the truncated log (%d records); a replay must refuse it", len(records))
	}
}

// TestITRunRecordCreateRaceFailsCommand pins O_EXCL as the authoritative guard:
// a file that appears after prepare's check but before the writer opens is a
// command failure, and the file that won the race keeps its bytes.
func TestITRunRecordCreateRaceFailsCommand(t *testing.T) {
	cwd := t.TempDir()
	path := filepath.Join(t.TempDir(), "it-raced.oar")
	const raced = "raced by the runtime\n"

	rt := itNewRuntime(itRuntimeID, scripted.Turn{scripted.Say("ok")})
	value := rt.itRuntimeValue()
	inner := value.Session
	var raceErr error
	value.Session = func(ctx context.Context, inst oarlock.Installation, opt oarlock.Options) (oarlock.Session, error) {
		// prepare's checkRecordPath already passed; the file appears here, so
		// only voyage.Create's O_EXCL can refuse the record now.
		if raceErr = os.WriteFile(path, []byte(raced), 0o644); raceErr != nil {
			return nil, raceErr
		}
		return inner(ctx, inst, opt)
	}

	out, errOut := &itBuffer{}, &itBuffer{}
	app := &App{
		Stdout: out,
		Stderr: errOut,
		Lookup: func(id string) (oarlock.Runtime, error) {
			if id != itRuntimeID {
				return oarlock.Runtime{}, fmt.Errorf("runtime %q is not registered", id)
			}
			return value, nil
		},
		Registered: func() []string { return []string{itRuntimeID} },
	}

	code := itRunBounded(t, app,
		"run", itRuntimeID, "it hello",
		"--json",
		"--cwd", cwd,
		"--record", path,
	)
	if code != ExitError {
		t.Fatalf("exit = %d, want %d (stdout: %s, stderr: %s)", code, ExitError, out.String(), errOut.String())
	}
	if raceErr != nil {
		t.Fatalf("pre-creating %s from the session closure: %v", path, raceErr)
	}
	if opens := rt.itOpenCount(); opens != 1 {
		t.Fatalf("the runtime's Session was constructed %d times, want 1 (prepare passed; the race is at create time)", opens)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the raced record file: %v", err)
	}
	if string(got) != raced {
		t.Fatalf("the file that won the race was changed:\n got %q\nwant %q", got, raced)
	}

	dto := itParseRunDTO(t, out)
	if !strings.Contains(dto.Error, "writing record") {
		t.Fatalf("error = %q, want it to name the record write", dto.Error)
	}
	if want := string(oarlock.RunCompleted); dto.Outcome != want {
		t.Errorf("outcome = %q, want %q: the turn itself completed, only the record failed", dto.Outcome, want)
	}
}
