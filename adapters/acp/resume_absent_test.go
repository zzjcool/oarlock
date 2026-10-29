package acp

// resume_absent_test.go — the evidence rule behind Profile.ResumeAbsence:
// absence may be claimed only from a session/list walk that completed and
// lacked the id. Every weaker outcome (a list error, a repeated cursor, an
// endless cursor, the page bound) is "not shown" and leaves the agent's own
// refusal untouched.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/zzjcool/oarlock"
)

// scriptPage is one page a scriptedLister serves.
type scriptPage struct {
	ids  []string
	next *string
	err  error
}

// scriptedLister serves pages in order and records the cursors it was asked
// for, so a test can pin both what the walk saw and how far it went.
type scriptedLister struct {
	t     *testing.T
	pages []scriptPage
	asked []*string
}

func (s *scriptedLister) list(_ context.Context, cursor *string) ([]string, *string, error) {
	s.asked = append(s.asked, cursor)
	if n := len(s.asked); n > len(s.pages) {
		s.t.Fatalf("page %d requested: the walk read past its last page", n)
	}
	p := s.pages[len(s.asked)-1]
	return p.ids, p.next, p.err
}

func TestResumeAbsentAfterACompleteTwoPageWalk(t *testing.T) {
	second := "page-2"
	s := &scriptedLister{t: t, pages: []scriptPage{
		{ids: []string{"other-1", "other-2"}, next: &second},
		{ids: []string{"other-3"}}, // next nil: the walk completed here
	}}
	absent, err := resumeAbsent(context.Background(), s.list, "gone")
	if err != nil || !absent {
		t.Fatalf("resumeAbsent = %v, %v; want true, nil (the walk completed and lacks the id)", absent, err)
	}
	if len(s.asked) != 2 {
		t.Fatalf("pages read = %d, want 2", len(s.asked))
	}
	if s.asked[0] != nil {
		t.Fatalf("the first page carried a cursor %q; the walk must start without one", *s.asked[0])
	}
	if got := s.asked[1]; got == nil || *got != second {
		t.Fatalf("the second page cursor = %v, want %q", got, second)
	}
}

func TestResumeAbsentIsFalseWhenTheIDIsOnTheLastPage(t *testing.T) {
	second := "page-2"
	s := &scriptedLister{t: t, pages: []scriptPage{
		{ids: []string{"other-1"}, next: &second},
		{ids: []string{"wanted", "other-2"}},
	}}
	absent, err := resumeAbsent(context.Background(), s.list, "wanted")
	if err != nil || absent {
		t.Fatalf("resumeAbsent = %v, %v; want false, nil (the id is listed)", absent, err)
	}
	if len(s.asked) != 2 {
		t.Fatalf("pages read = %d, want 2", len(s.asked))
	}
}

func TestResumeAbsentIsFalseWhenTheIDIsOnTheFirstPage(t *testing.T) {
	second := "page-2"
	s := &scriptedLister{t: t, pages: []scriptPage{
		{ids: []string{"other-1", "wanted"}, next: &second},
	}}
	absent, err := resumeAbsent(context.Background(), s.list, "wanted")
	if err != nil || absent {
		t.Fatalf("resumeAbsent = %v, %v; want false, nil (the id is listed)", absent, err)
	}
	if len(s.asked) != 1 {
		t.Fatalf("pages read = %d, want 1: a listed id ends the walk", len(s.asked))
	}
}

func TestResumeAbsentIsNotShownByAListError(t *testing.T) {
	boom := errors.New("session/list: Internal error")
	s := &scriptedLister{t: t, pages: []scriptPage{{err: boom}}}
	absent, err := resumeAbsent(context.Background(), s.list, "gone")
	if absent {
		t.Fatal("a failed session/list page claimed absence")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("walk error = %v, want the list error unchanged", err)
	}
}

func TestResumeAbsentRejectsAnEndlessCursor(t *testing.T) {
	// Every page hands out a fresh cursor, so the walk never terminates. The
	// page bound must turn that into an error, never into absence.
	pages := make([]scriptPage, resumeListPages+1)
	for i := range pages {
		next := fmt.Sprintf("page-%d", i+1)
		pages[i] = scriptPage{ids: []string{fmt.Sprintf("other-%d", i)}, next: &next}
	}
	s := &scriptedLister{t: t, pages: pages}
	absent, err := resumeAbsent(context.Background(), s.list, "gone")
	if absent {
		t.Fatal("an endless session/list walk claimed absence")
	}
	if err == nil {
		t.Fatal("an endless session/list walk returned no error")
	}
	if len(s.asked) != resumeListPages {
		t.Fatalf("pages read = %d, want the bound %d", len(s.asked), resumeListPages)
	}
}

func TestResumeAbsentRejectsARepeatingCursor(t *testing.T) {
	loop := "page-2"
	s := &scriptedLister{t: t, pages: []scriptPage{
		{ids: []string{"other-1"}, next: &loop},
		{ids: []string{"other-2"}, next: &loop}, // same cursor: the walk cannot progress
	}}
	absent, err := resumeAbsent(context.Background(), s.list, "gone")
	if absent {
		t.Fatal("a repeating session/list cursor claimed absence")
	}
	if err == nil || !strings.Contains(err.Error(), "repeats") {
		t.Fatalf("walk error = %v, want a repeating-cursor error", err)
	}
	if len(s.asked) != 2 {
		t.Fatalf("pages read = %d, want 2", len(s.asked))
	}
}

// TestResumeAbsenceNeedsACompleteWalkToClaimAbsence is the caller's half: the
// fake agent advertises sessionCapabilities.list but has no session/list
// method, so the walk fails and the native refusal must stay a plain error —
// never *oarlock.SessionNotFound.
func TestResumeAbsenceNeedsACompleteWalkToClaimAbsence(t *testing.T) {
	prof := OpenCodeV1
	prof.ResumeAbsence = true
	_, err := tryOpen(prof, "opencode", oarlock.Options{Resume: "missing"})
	if err == nil {
		t.Fatal("resume of a missing id returned no error")
	}
	var nf *oarlock.SessionNotFound
	if errors.As(err, &nf) {
		t.Fatalf("an incomplete session/list walk claimed absence: %v", err)
	}
	if !strings.Contains(err.Error(), "resume missing") {
		t.Fatalf("the native refusal was replaced: %v", err)
	}
}
