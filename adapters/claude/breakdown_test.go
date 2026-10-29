package claude

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
	sj "github.com/zzjcool/oarlock/internal/streamjson"
)

func answer(t *testing.T) sj.Rec {
	t.Helper()
	return sj.ParseRec([]byte(helperContextAnswer))
}

func TestParseContextBreakdownKeepsClaudesOrderWordsAndKinds(t *testing.T) {
	b, err := ParseContextBreakdown(answer(t))
	if err != nil {
		t.Fatal(err)
	}
	if b.Tokens != 3162 || b.ContextWindow != 1_000_000 {
		t.Fatalf("totals = %d / %d", b.Tokens, b.ContextWindow)
	}
	type nk struct {
		n string
		k BreakdownKind
	}
	var got []nk
	for _, c := range b.Categories {
		got = append(got, nk{c.Name, c.Kind})
	}
	want := []nk{{"System prompt", KindUsed}, {"MCP tools", KindUsed}, {"MCP tools (deferred)", KindDeferred},
		{"Memory files", KindUsed}, {"Skills", KindUsed}, {"Autocompact buffer", KindReserved},
		{"Free space", KindFree}, {"Something new", KindUnknown}}
	if len(got) != len(want) {
		t.Fatalf("categories = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("category %d = %v, want %v", i, got[i], want[i])
		}
	}
	// The sums hold as documented: used = tokens; all but deferred (and unknown) = the window.
	var used, window int64
	for _, c := range b.Categories {
		if c.Kind == KindUsed {
			used += c.Tokens
		}
		if c.Kind == KindUsed || c.Kind == KindReserved || c.Kind == KindFree {
			window += c.Tokens
		}
	}
	if used != b.Tokens || window != b.ContextWindow {
		t.Fatalf("used %d (want %d), window sum %d (want %d)", used, b.Tokens, window, b.ContextWindow)
	}
}

func TestParseContextBreakdownItemizesEachCategory(t *testing.T) {
	b, _ := ParseContextBreakdown(answer(t))
	items := map[string][]BreakdownItem{}
	for _, c := range b.Categories {
		items[c.Name] = c.Items
	}
	eq := func(name string, want ...BreakdownItem) {
		t.Helper()
		got := items[name]
		if len(got) != len(want) {
			t.Fatalf("%s items = %v, want %v", name, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s items = %v, want %v", name, got, want)
			}
		}
	}
	eq("Memory files", BreakdownItem{"/home/user/.claude/CLAUDE.md", 111})
	eq("MCP tools", BreakdownItem{"mcp__echo__echo", 120})
	eq("MCP tools (deferred)", BreakdownItem{"mcp__docs__search", 165})
	eq("Skills", BreakdownItem{"dataviz", 482}, BreakdownItem{"frontend-design", 74})
	if len(items["System prompt"]) != 0 {
		t.Fatalf("a category without a list has no items: %v", items["System prompt"])
	}
}

func TestParseContextBreakdownWithOneMCPCategoryTakesEveryTool(t *testing.T) {
	// claude 2.1.292 with tool search off: only "MCP tools", every tool isLoaded:false.
	var payload map[string]any
	_ = json.Unmarshal([]byte(helperContextAnswer), &payload)
	var cats []any
	for _, c := range payload["categories"].([]any) {
		if c.(map[string]any)["name"] != "MCP tools (deferred)" {
			cats = append(cats, c)
		}
	}
	payload["categories"] = cats
	b, _ := ParseContextBreakdown(payload)
	for _, c := range b.Categories {
		if c.Name == "MCP tools" && len(c.Items) != 2 {
			t.Fatalf("MCP tools items = %v, want both tools", c.Items)
		}
	}
}

func TestParseContextBreakdownWithoutTotalsIsAnError(t *testing.T) {
	if _, err := ParseContextBreakdown(sj.Rec{"categories": []any{}}); err == nil || !strings.Contains(err.Error(), "totalTokens or maxTokens") {
		t.Fatalf("err = %v", err)
	}
}

func TestContextBreakdownReaderAsksSharesAndRecordsNothing(t *testing.T) {
	s, _ := helperSession(t, "chat", SessionOptions{SessionID: "sess-bd"})
	var r oarlock.ContextBreakdownReader = s // Claude-only interface; Pi has no such method
	before := len(s.Records(oarlock.Cursor{}))
	ctx := ctxT(t)
	res := make(chan *oarlock.ContextBreakdown, 2)
	for i := 0; i < 2; i++ {
		go func() {
			b, err := r.ContextBreakdown(ctx)
			if err != nil {
				t.Error(err)
			}
			res <- b
		}()
	}
	a, b := <-res, <-res
	if a == nil || b == nil || len(a.Categories) != 8 || a.Categories[0].Name != "System prompt" || a.Categories[0].Tokens != 2375 {
		t.Fatalf("breakdown = %+v / %+v", a, b)
	}
	if n := len(s.Records(oarlock.Cursor{})); n != before {
		t.Fatalf("the read recorded %d records: the answer carries local paths and must never be recorded", n-before)
	}
	for _, rec := range s.Records(oarlock.Cursor{}) {
		if strings.Contains(string(rec.Native), "CLAUDE.md") {
			t.Fatal("a memory-file path reached a record")
		}
	}
}

func TestContextBreakdownRefusalCarriesClaudesWords(t *testing.T) {
	s, _ := helperSession(t, "chat", SessionOptions{SessionID: "sess-bd2"}, "nobreakdown")
	_, err := s.ContextBreakdown(ctxT(t))
	if err == nil || !strings.Contains(err.Error(), "not supported in this context") {
		t.Fatalf("err = %v", err)
	}
}

func TestContextBreakdownAfterDisposeIsNilNil(t *testing.T) {
	s, _ := helperSession(t, "chat", SessionOptions{SessionID: "sess-bd3"})
	if err := s.Dispose(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	b, err := s.ContextBreakdown(ctxT(t))
	if b != nil || err != nil {
		t.Fatalf("after dispose = %+v, %v; want nil, nil (nobody to ask)", b, err)
	}
}

func TestContextBreakdownHonoursCtx(t *testing.T) {
	s, _ := helperSession(t, "chat", SessionOptions{SessionID: "sess-bd4"}, "nobreakdown")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.ContextBreakdown(ctx); err == nil {
		t.Fatal("a cancelled ctx must not return a breakdown")
	}
	_ = time.Second
}
