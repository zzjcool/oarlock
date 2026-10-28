package scripted_test

// Runnable examples of the scripted runtime (issue #18, acceptance 4). Each one
// is a complete host test in miniature: script a runtime, drive it through the
// oarlock.Session interface, and read the answer off the record stream the way
// a host does (internal/fold). The README in this directory explains the same
// pieces in prose.
//
// Every helper below carries the prefix ex so it cannot collide with the
// helpers of the suite, fault and leak tests.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/fold"
	"github.com/zzjcool/oarlock/internal/testing/scripted"
)

// exWaitStatus polls the stream until the status folded from Records is want.
// Status is read the way a host reads it, as a fold over the retained records;
// the session keeps no flag for it. It polls every 2ms and gives up after 5s
// with a panic, so a hung example fails loudly instead of stalling go test.
func exWaitStatus(sess oarlock.Session, want fold.StatusKind) fold.Status {
	deadline := time.Now().Add(5 * time.Second)
	for {
		st := fold.StatusOf(sess.Records(oarlock.Cursor{})).Value
		if st.Kind == want {
			return st
		}
		if time.Now().After(deadline) {
			panic(fmt.Sprintf("example: status is %s after 5s, want %s", st.Kind, want))
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// exWaitIdle is the waitIdle of the README: block until no turn is running.
func exWaitIdle(sess oarlock.Session) fold.Status {
	return exWaitStatus(sess, fold.StatusIdle)
}

// exShow renders a control outcome as a host would branch on it: the answer and,
// for a rejection, its Code (never its prose).
func exShow(out oarlock.ControlOutcome) string {
	if out.Kind == oarlock.ControlRejected {
		return "rejected " + string(out.Code)
	}
	return string(out.Kind)
}

// exFrameType reads the "type" discriminator out of a frame's native bytes.
func exFrameType(r oarlock.Record) string {
	var h struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(r.Native, &h)
	return h.Type
}

// exCountEvents counts the events of one kind in the whole stream.
func exCountEvents(records []oarlock.Record, kind oarlock.EventKind) int {
	n := 0
	for _, r := range records {
		for _, ev := range r.Events {
			if ev.Kind == kind {
				n++
			}
		}
	}
	return n
}

// exExit returns the exited response of the stream, if there is one.
func exExit(records []oarlock.Record) (oarlock.Record, bool) {
	for _, r := range records {
		if r.Kind == oarlock.KindResponse && r.Response.Kind == oarlock.ResponseExited {
			return r, true
		}
	}
	return oarlock.Record{}, false
}

// A prompt, a wait for idle, and the answer read off the stream: the whole
// loop of a host test. The script says what the fake runtime does in turn 0.
func ExampleNew() {
	sess := scripted.New(scripted.Turn{
		scripted.Think("a greeting is wanted"),
		scripted.Say("Hello,"),
		scripted.Echo(), // says the input that began this turn
		scripted.Tool("read_file", `{"path":"go.mod"}`, "module example"),
	})
	ctx := context.Background()
	defer sess.Dispose(ctx)

	out, _ := sess.Prompt(ctx, "world", oarlock.PromptOptions{})
	fmt.Println("prompt:", exShow(out))

	// The turn runs on a goroutine of its own, so wait on the stream: exWaitIdle
	// polls fold.StatusOf(sess.Records(...)) every 2ms (the README shows it).
	st := exWaitIdle(sess)

	for _, m := range fold.ConversationOf(sess.Records(oarlock.Cursor{})).Value {
		if m.Role == fold.RoleTool {
			fmt.Printf("tool: %s(%s) %s: %s\n", m.Tool, m.Input, m.Result, m.Output)
			continue
		}
		fmt.Printf("%s: %s\n", m.Role, m.Text)
	}
	fmt.Println("status:", st.Kind, st.LastTurnOutcome.Kind)
	// Output:
	// prompt: accepted
	// reasoning: a greeting is wanted
	// assistant: Hello,
	// assistant: world
	// tool: read_file({"path":"go.mod"}) ok: module example
	// status: idle completed
}

// One control plane, at most one active turn: a prompt while a turn runs is
// rejected busy, never queued implicitly. Hang keeps the first turn running
// until the abort ends it.
func ExampleNew_busy() {
	sess := scripted.New(scripted.Turn{scripted.Say("working"), scripted.Hang()})
	ctx := context.Background()
	defer sess.Dispose(ctx)

	first, _ := sess.Prompt(ctx, "start a long job", oarlock.PromptOptions{})
	fmt.Println("first prompt:", exShow(first))

	// A rejection is an outcome, not an error: branch on Code, never on prose.
	second, _ := sess.Prompt(ctx, "and another", oarlock.PromptOptions{})
	fmt.Println("second prompt:", exShow(second))

	// Abort returns once the runtime ended the turn with its own turn_ended.
	abort, _ := sess.Abort(ctx)
	fmt.Println("abort:", exShow(abort))

	st := exWaitIdle(sess)
	fmt.Println("status:", st.Kind, st.LastTurnOutcome.Kind)

	// Nothing is running any more: a late abort is the normal race.
	late, _ := sess.Abort(ctx)
	fmt.Println("late abort:", exShow(late))
	// Output:
	// first prompt: accepted
	// second prompt: rejected busy
	// abort: accepted
	// status: idle aborted
	// late abort: rejected no_active_turn
}

// Die kills the fake process mid-turn. The stream gets one standalone exited
// response and no turn_ended, and from then on the kernel refuses every
// control without consulting the session.
func ExampleNew_die() {
	sess := scripted.New(scripted.Turn{scripted.Say("about to crash"), scripted.Die(3)})
	ctx := context.Background()
	defer sess.Dispose(ctx)

	out, _ := sess.Prompt(ctx, "go", oarlock.PromptOptions{})
	fmt.Println("prompt:", exShow(out))

	exWaitStatus(sess, fold.StatusExited)

	again, _ := sess.Prompt(ctx, "again", oarlock.PromptOptions{})
	queued, _ := sess.Queue(ctx, "later", oarlock.InputOptions{})
	abort, _ := sess.Abort(ctx)
	fmt.Println("prompt after die:", exShow(again))
	fmt.Println("queue after die:", exShow(queued))
	fmt.Println("abort after die:", exShow(abort))

	records := sess.Records(oarlock.Cursor{})
	if exit, ok := exExit(records); ok {
		fmt.Printf("exited: code %d, reported %t, answers a request %t\n",
			*exit.Response.ExitCode, exit.Response.ExitCodeReported, exit.RequestID != "")
	}
	fmt.Println("turn_ended events:", exCountEvents(records, oarlock.EventTurnEnded))
	// Output:
	// prompt: accepted
	// prompt after die: rejected runtime_exited
	// queue after die: rejected runtime_exited
	// abort after die: rejected runtime_exited
	// exited: code 3, reported true, answers a request false
	// turn_ended events: 0
}

// A native frame the session does not understand is kept verbatim, carries no
// events and is counted: counted, never coerced. A non-zero tally is the test
// signal that a runtime said something the adapter does not know yet.
func ExampleNew_unknownEvent() {
	sess := scripted.New(scripted.Turn{
		scripted.Say("before"),
		scripted.UnknownEvent("vendor/new_thing"),
		scripted.Say("after"),
	})
	ctx := context.Background()
	defer sess.Dispose(ctx)

	_, _ = sess.Prompt(ctx, "go", oarlock.PromptOptions{})
	st := exWaitIdle(sess)

	fmt.Println("unknown:", sess.Unknown())
	for _, r := range sess.Records(oarlock.Cursor{}) {
		if r.Kind == oarlock.KindFrame && exFrameType(r) == "vendor/new_thing" {
			fmt.Printf("kept verbatim: %s with %d events\n", r.Native, len(r.Events))
		}
	}
	fmt.Println("status:", st.Kind, st.LastTurnOutcome.Kind)
	// Output:
	// unknown: map[vendor/new_thing:1]
	// kept verbatim: {"type":"vendor/new_thing"} with 0 events
	// status: idle completed
}

// A runtime that does not answer an abort is taken over once AbortGrace is up,
// as adapters/pi does by killing the process group: the abort is still
// accepted, the stream gets one standalone exited response (no exit number) and
// no turn_ended is invented for it.
func ExampleNewWithOptions_takeOver() {
	sess, err := scripted.NewWithOptions(
		scripted.Options{AbortGrace: 20 * time.Millisecond},
		scripted.Turn{scripted.IgnoreAbort(), scripted.Hang()},
	)
	if err != nil {
		fmt.Println(err)
		return
	}
	ctx := context.Background()
	defer sess.Dispose(ctx)

	_, _ = sess.Prompt(ctx, "go", oarlock.PromptOptions{})

	// Blocks for the grace period, then answers accepted: the stop is ours.
	abort, _ := sess.Abort(ctx)
	fmt.Println("abort:", exShow(abort))

	exWaitStatus(sess, fold.StatusExited)
	records := sess.Records(oarlock.Cursor{})
	if exit, ok := exExit(records); ok {
		fmt.Printf("exited: answers a request %t, exit number known %t, reported %t\n",
			exit.RequestID != "", exit.Response.ExitCode != nil, exit.Response.ExitCodeReported)
	}
	fmt.Println("turn_ended events:", exCountEvents(records, oarlock.EventTurnEnded))

	next, _ := sess.Prompt(ctx, "again", oarlock.PromptOptions{})
	fmt.Println("prompt after take-over:", exShow(next))
	// Output:
	// abort: accepted
	// exited: answers a request false, exit number known false, reported true
	// turn_ended events: 0
	// prompt after take-over: rejected runtime_exited
}
