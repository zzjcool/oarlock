package codex

// Tests for the model listing (models.go, issue #98).
//
// The fake is the one ops_test.go and accountusage_test.go already use: a
// `#!/bin/sh` script on a narrowed PATH that logs every app-server request line
// it is handed and answers by matching that line. So a test proves what was
// asked (initialize -> initialized -> model/list, and nothing else), what was
// mapped and what was refused without a codex binary, a login or a network.
// Windows skips the script tests and only compile-checks.
//
// LIVE STATUS: UNVERIFIED. No real app-server is involved below; a host with a
// signed-in account still has to prove that `model/list` answers this shape.

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
)

// modelsReply is one answer of the fake app-server: the substring that selects
// a request line, and the JSONL the fake prints for it.
type modelsReply struct{ match, jsonl string }

// modelsFakeServer puts a fake `codex` on PATH whose app-server answers each
// request line with the FIRST modelsReply whose match is a substring of that
// line, so a test can answer a cursor before the method it belongs to. Every
// line is appended to <script>.requests; an unmatched line (an `initialized`
// notification, say) is logged and not answered.
func modelsFakeServer(t *testing.T, replies ...modelsReply) (log, requests string) {
	t.Helper()
	body := []string{
		`while IFS= read -r line; do`,
		`  printf '%s\n' "$line" >> "$0.requests"`,
		`  case $line in`,
	}
	for _, reply := range replies {
		body = append(body, `    *`+shQuote(reply.match)+`*) printf '%s\n' `+shQuote(reply.jsonl)+` ;;`)
	}
	body = append(body, `  esac`, `done`)
	dir, log := fakeCodexScript(t, strings.Join(body, "\n"))
	return log, filepath.Join(dir, executableName+".requests")
}

// modelsInitReply is the initialize answer. The ids below are the ones the
// shared client assigns — initialize is 1, then one model/list per page — so an
// extra request before model/list shifts them and fails the test loudly, which
// is the point: the listing asks for a catalogue and for nothing else.
const modelsInitReply = `{"id": 1, "result": {"userAgent": "codex-cli 0.165.0"}}`

// modelListReply is one model/list result. next is "" for the last page (the
// wire spells that null) and the cursor of the next page otherwise. data is the
// raw JSON of the data array, on one line because the dialect is JSONL.
func modelListReply(id int, data, next string) string {
	cursor := "null"
	if next != "" {
		cursor = strconv.Quote(next)
	}
	if data == "" {
		data = "[]"
	}
	return fmt.Sprintf(`{"id": %d, "result": {"data": [%s], "nextCursor": %s}}`, id, data, cursor)
}

// modelListRefusal is a JSON-RPC error answer to model/list.
func modelListRefusal(code int, message string) string {
	return fmt.Sprintf(`{"id": 2, "error": {"code": %d, "message": %s}}`, code, strconv.Quote(message))
}

// TestListModelsMapsModelsEffortsAndServiceTiers is the success row: two models
// with the provider, id and name the app-server reported, one carrying effort
// levels and service tiers (deduped, wire order kept).
func TestListModelsMapsModelsEffortsAndServiceTiers(t *testing.T) {
	data := `{"model": "gpt-5.5", "displayName": "GPT 5.5",` +
		` "supportedReasoningEfforts": [{"reasoningEffort": "low"}, {"reasoningEffort": "high"}, {"reasoningEffort": "low"}],` +
		` "serviceTiers": [{"id": "priority", "name": "Fast"}, {"id": "flex"}, {"id": "priority"}]},` +
		`{"model": "gpt-5.5-mini", "serviceTiers": []}`
	log, requests := modelsFakeServer(t,
		modelsReply{`"method":"initialize"`, modelsInitReply},
		modelsReply{`"method":"model/list"`, modelListReply(2, data, "")},
	)

	models, err := codexModels{}.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	want := []oarlock.Model{
		{
			Provider:     "codex",
			ID:           "gpt-5.5",
			Name:         "GPT 5.5",
			EffortLevels: []string{"low", "high"},
			ServiceTiers: []string{"priority", "flex"},
		},
		{
			// No displayName: the id is the name. An empty serviceTiers array
			// codex sent stays an empty slice, and an absent effort array stays
			// nil — the presence difference is codex's.
			Provider:     "codex",
			ID:           "gpt-5.5-mini",
			Name:         "gpt-5.5-mini",
			ServiceTiers: []string{},
		},
	}
	if !reflect.DeepEqual(models, want) {
		t.Fatalf("models = %#v, want %#v", models, want)
	}

	// One throwaway app-server, invoked the way accountusage.go invokes it.
	if lines := logLines(t, log); len(lines) != 1 || lines[0] != "app-server --listen stdio://" {
		t.Fatalf("invocations = %q, want exactly `app-server --listen stdio://`", lines)
	}
	sent := readRequests(t, requests)
	for _, wanted := range []string{
		`"method":"initialize"`,
		`"clientInfo":{"name":"oarlock","version":"0.0.0"}`,
		`"capabilities":{"experimentalApi":true}`,
		`"method":"initialized"`,
		`"method":"model/list"`,
	} {
		if !strings.Contains(sent, wanted) {
			t.Fatalf("requests do not carry %s:\n%s", wanted, sent)
		}
	}
	// The default-model marker is the only thing config/read would add and
	// oarlock.Model has no field for it, so a listing must never ask.
	for _, forbidden := range []string{`"method":"config/read"`, `"method":"account/read"`} {
		if strings.Contains(sent, forbidden) {
			t.Fatalf("a listing asked for %s:\n%s", forbidden, sent)
		}
	}
}

// TestListModelsSkipsHiddenAndIdlessEntries pins the per-entry rules: a hidden
// model, a blank id, an absent id and an entry that is not an object are
// skipped — never emitted as "" — while the readable entries still come back.
func TestListModelsSkipsHiddenAndIdlessEntries(t *testing.T) {
	data := `{"model": "secret", "hidden": true},` +
		`{"model": "   ", "displayName": "blank id"},` +
		`{"displayName": "no id"},` +
		`42,` +
		`{"model": "visible"}`
	modelsFakeServer(t,
		modelsReply{`"method":"initialize"`, modelsInitReply},
		modelsReply{`"method":"model/list"`, modelListReply(2, data, "")},
	)

	models, err := codexModels{}.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	want := []oarlock.Model{{Provider: "codex", ID: "visible", Name: "visible"}}
	if !reflect.DeepEqual(models, want) {
		t.Fatalf("models = %#v, want %#v", models, want)
	}
}

// TestListModelsReturnsAnHonestEmptyList is the empty row: `data: []` is codex
// answering "none", so it is an empty list and no error — and a slice, because
// nil is this method's failure value.
func TestListModelsReturnsAnHonestEmptyList(t *testing.T) {
	modelsFakeServer(t,
		modelsReply{`"method":"initialize"`, modelsInitReply},
		modelsReply{`"method":"model/list"`, modelListReply(2, "", "")},
	)

	models, err := codexModels{}.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 0 {
		t.Fatalf("models = %+v, want none", models)
	}
	if models == nil {
		t.Fatal("an answered empty catalogue must be an empty list: nil is the failure value")
	}
}

// TestListModelsMapsAReauthRefusalToErrNotAuthenticated is the signed-out row:
// the app-server's own "authentication required" is a login problem, it is typed
// and it never reaches the caller as vendor text or as an empty list.
func TestListModelsMapsAReauthRefusalToErrNotAuthenticated(t *testing.T) {
	modelsFakeServer(t,
		modelsReply{`"method":"initialize"`, modelsInitReply},
		modelsReply{`"method":"model/list"`, modelListRefusal(-32000, "ChatGPT authentication required")},
	)

	models, err := codexModels{}.ListModels(context.Background())
	if !errors.Is(err, ErrNotAuthenticated) {
		t.Fatalf("err = %v (%T), want ErrNotAuthenticated", err, err)
	}
	if models != nil {
		t.Fatalf("models = %+v next to an error, want nil", models)
	}
	if got := err.Error(); strings.Contains(got, "ChatGPT") || strings.Contains(got, "authentication") {
		t.Fatalf("the error quotes the vendor: %v", err)
	}
}

// TestListModelsReportsAnUnknownMethodAsUnsupported is the no-models-surface
// row: a build without model/list is an UnsupportedOperation, not an empty list.
func TestListModelsReportsAnUnknownMethodAsUnsupported(t *testing.T) {
	modelsFakeServer(t,
		modelsReply{`"method":"initialize"`, modelsInitReply},
		modelsReply{`"method":"model/list"`, modelListRefusal(-32601, "method not found")},
	)

	models, err := codexModels{}.ListModels(context.Background())
	var unsupported *oarlock.UnsupportedOperation
	if !errors.As(err, &unsupported) {
		t.Fatalf("err = %v (%T), want *oarlock.UnsupportedOperation", err, err)
	}
	if unsupported.Operation != "list-models" {
		t.Fatalf("operation = %q, want list-models", unsupported.Operation)
	}
	if want := "this codex build has no app-server model/list method"; unsupported.Reason != want {
		t.Fatalf("reason = %q, want %q", unsupported.Reason, want)
	}
	if models != nil {
		t.Fatalf("models = %+v next to an error, want nil", models)
	}
}

// TestListModelsRejectsAReplyWithoutADataArray: an absent or non-array data is a
// reply this reader cannot trust, never "zero models".
func TestListModelsRejectsAReplyWithoutADataArray(t *testing.T) {
	for name, reply := range map[string]string{
		"absent":    `{"id": 2, "result": {"nextCursor": null}}`,
		"not array": `{"id": 2, "result": {"data": "gpt-5.5"}}`,
		"null":      `{"id": 2, "result": null}`,
	} {
		t.Run(name, func(t *testing.T) {
			modelsFakeServer(t,
				modelsReply{`"method":"initialize"`, modelsInitReply},
				modelsReply{`"method":"model/list"`, reply},
			)
			models, err := codexModels{}.ListModels(context.Background())
			if err == nil || err.Error() != "codex model/list returned no data array" {
				t.Fatalf("err = %v, want the no-data-array error", err)
			}
			if models != nil {
				t.Fatalf("models = %+v next to an error, want nil", models)
			}
		})
	}
}

// TestListModelsFollowsPaginationForTwoPages: the second page is asked for with
// the first page's cursor, and both pages' models are returned in wire order.
func TestListModelsFollowsPaginationForTwoPages(t *testing.T) {
	_, requests := modelsFakeServer(t,
		modelsReply{`"method":"initialize"`, modelsInitReply},
		// Before the method match: page 2's line carries both.
		modelsReply{`"cursor":"next"`, modelListReply(3, `{"model": "page-2"}`, "")},
		modelsReply{`"method":"model/list"`, modelListReply(2, `{"model": "page-1"}`, "next")},
	)

	models, err := codexModels{}.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	want := []oarlock.Model{
		{Provider: "codex", ID: "page-1", Name: "page-1"},
		{Provider: "codex", ID: "page-2", Name: "page-2"},
	}
	if !reflect.DeepEqual(models, want) {
		t.Fatalf("models = %#v, want %#v", models, want)
	}
	if sent := readRequests(t, requests); !strings.Contains(sent, `"params":{"cursor":"next"}`) {
		t.Fatalf("the second page was not asked for with its cursor:\n%s", sent)
	}
}

// TestListModelsFailsOnARepeatedCursor: a server that hands out the same cursor
// twice is a loop, not more models.
func TestListModelsFailsOnARepeatedCursor(t *testing.T) {
	modelsFakeServer(t,
		modelsReply{`"method":"initialize"`, modelsInitReply},
		modelsReply{`"cursor":"again"`, modelListReply(3, `{"model": "loop"}`, "again")},
		modelsReply{`"method":"model/list"`, modelListReply(2, `{"model": "loop"}`, "again")},
	)

	models, err := codexModels{}.ListModels(context.Background())
	if err == nil || err.Error() != "codex model/list repeated a pagination cursor" {
		t.Fatalf("err = %v, want the repeated-cursor error", err)
	}
	if models != nil {
		t.Fatalf("models = %+v next to an error, want nil", models)
	}
}

// TestListModelsCapsThePaginationWalk: an app-server that hands out a fresh
// cursor forever is bounded instead of followed. Every answer is distinct (the
// k-th model/list is answered with id k+1, initialize being 1), so the walk also
// keeps correlating ids across pages.
func TestListModelsCapsThePaginationWalk(t *testing.T) {
	body := strings.Join([]string{
		`n=0`,
		`while IFS= read -r line; do`,
		`  case $line in`,
		`    *` + shQuote(`"method":"initialize"`) + `*) printf '%s\n' ` + shQuote(modelsInitReply) + ` ;;`,
		`    *` + shQuote(`"method":"model/list"`) + `*)`,
		`      n=$((n+1))`,
		`      printf '%s\n' "{\"id\": $((n+1)), \"result\": {\"data\": [], \"nextCursor\": \"page-$n\"}}"`,
		`      ;;`,
		`  esac`,
		`done`,
	}, "\n")
	fakeCodexScript(t, body)

	models, err := codexModels{}.ListModels(context.Background())
	want := fmt.Sprintf("codex model/list did not finish within %d pages", modelsMaxPages)
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
	if models != nil {
		t.Fatalf("models = %+v next to an error, want nil", models)
	}
}

// TestListModelsFailsWhenTheStreamEndsEarly: a codex that starts and dies
// without answering is an error, never an empty catalogue.
func TestListModelsFailsWhenTheStreamEndsEarly(t *testing.T) {
	fakeCodexScript(t, "exit 1")

	models, err := codexModels{}.ListModels(context.Background())
	if err == nil {
		t.Fatal("a codex that ended before answering must be an error")
	}
	if models != nil {
		t.Fatalf("models = %+v next to an error, want nil", models)
	}
	if !strings.Contains(err.Error(), "codex app-server model/list") {
		t.Fatalf("err = %v, want it to name the model/list read", err)
	}

	// The same for a codex that cannot be started at all.
	missing := codexModels{Command: filepath.Join(t.TempDir(), "no-such-codex")}
	if models, err := missing.ListModels(context.Background()); err == nil || models != nil {
		t.Fatalf("models = %+v, err = %v, want a failed listing", models, err)
	}
}

// TestListModelsTimesOut: a listing that outlives its bound is a timeout named
// as such, and it returns promptly (the elapsed check is a bound, not proof that
// the child was reaped — the fake cannot observe its own death).
func TestListModelsTimesOut(t *testing.T) {
	fakeCodexScript(t, "sleep 30")

	start := time.Now()
	models, err := codexModels{Timeout: 100 * time.Millisecond}.ListModels(context.Background())
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want a timeout", err)
	}
	if models != nil {
		t.Fatalf("models = %+v next to an error, want nil", models)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("the listing took %v, far past its 100ms bound", elapsed)
	}
}

// TestListModelsSkipsNotificationFrames is the branch a real app-server
// exercises constantly: notifications, server requests and non-JSON lines arrive
// while a reply is outstanding and must be skipped, not read as an answer.
func TestListModelsSkipsNotificationFrames(t *testing.T) {
	noisy := `{"method": "model/list/started", "params": {}}` + "\n" +
		`{"id": 99, "method": "config/updated", "params": {}}` + "\n" +
		`not json at all` + "\n" +
		modelsInitReply
	modelsFakeServer(t,
		modelsReply{`"method":"initialize"`, noisy},
		modelsReply{`"method":"model/list"`, modelListReply(2, `{"model": "gpt-5.5"}`, "")},
	)

	models, err := codexModels{}.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	want := []oarlock.Model{{Provider: "codex", ID: "gpt-5.5", Name: "gpt-5.5"}}
	if !reflect.DeepEqual(models, want) {
		t.Fatalf("models = %#v, want %#v", models, want)
	}
}

// TestListModelsRefusesANilContext pins the guard every other entry point in
// oarlock has: a nil context is an error, not a panic inside the transport.
func TestListModelsRefusesANilContext(t *testing.T) {
	models, err := codexModels{}.ListModels(nil)
	if err == nil || models != nil {
		t.Fatalf("models = %+v, err = %v, want a refusal", models, err)
	}
}
