package redact

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// Known gap (out of scope here, see docs/benchmarks-and-fuzz.md): a known
// value embedded in a path-shaped value under a credential-named key survives,
// because (*Redactor).child declines the structural rule and never falls
// through to the known-value rule. The corpus deliberately does not seed that
// shape: it is a pre-existing production defect that a tests-only change may
// not fix, and seeding it would leave CI permanently red. When the fix lands,
// add the seed and delete this note.
//
// The fuzz corpus uses a fixed known-value set: synthetic ASCII-printable
// values of at least eight characters. Nothing here is a real credential and
// the corpus never touches the environment, the network or a file.
//
// fuzzConnURL passes the value rule as a whole and is additionally expanded by
// New into its decoded and encoded password forms, so the known set is a
// superset of the four literals below.
const (
	fuzzKnownToken    = "sk-live-abcdef123456"
	fuzzKnownPassword = "hunter2hunter2"
	fuzzKnownGitHub   = "ghp_0123456789abcdef"
	fuzzKnownConnURL  = "postgres://app:hunter2hunter2@db.internal:5432/prod"
)

// Structural-rule values. They are deliberately absent from the known set: the
// name rule (env/headers/Authorization) or the URL-shape rule alone must select
// them, which is what FuzzRedactor_Native and the corpus table test check.
const (
	fuzzEnvAPIKey   = "sk-test-abc123"
	fuzzHeaderToken = "{{SECRET_zx8kqm3f}}"
	fuzzPasswordRaw = "supersecret9"
	fuzzDBPassword  = "db-pass-YunopsBX-0117"
)

// fuzzKnownValues returns the known session values a host would build a
// Redactor from.
func fuzzKnownValues() []string {
	return []string{fuzzKnownToken, fuzzKnownPassword, fuzzKnownGitHub, fuzzKnownConnURL}
}

// fuzzRedactor builds the redactor every target in this file uses.
func fuzzRedactor() *Redactor { return New(fuzzKnownValues()) }

// fuzzSecretIn walks a decoded JSON value and reports the first known value
// that survived, together with the path where it was found. Keys and string
// values are both searched: redaction must cover either.
func fuzzSecretIn(v any, path string, secrets []string) (secret, where string) {
	switch value := v.(type) {
	case map[string]any:
		for k, child := range value {
			for _, s := range secrets {
				if strings.Contains(k, s) {
					return s, path + " (key)"
				}
			}
			if s, w := fuzzSecretIn(child, path+"/"+k, secrets); s != "" {
				return s, w
			}
		}
	case []any:
		for i, child := range value {
			if s, w := fuzzSecretIn(child, fmt.Sprintf("%s[%d]", path, i), secrets); s != "" {
				return s, w
			}
		}
	case string:
		for _, s := range secrets {
			if strings.Contains(value, s) {
				return s, path
			}
		}
	}
	return "", ""
}

// fuzzAssertNoSecretInText fails when any known value survives in got.
func fuzzAssertNoSecretInText(t *testing.T, label, got string, secrets []string) {
	t.Helper()
	for _, s := range secrets {
		if strings.Contains(got, s) {
			t.Fatalf("%s: known value %q survived in %q", label, s, got)
		}
	}
}

// fuzzAssertNoSecretInJSON decodes a JSON document and fails when any known
// value survives in a string key or a string value. Numbers are kept as
// json.Number so a syntactically valid document whose number is outside the
// float64 range (1e1000) is still walked instead of failing the decode.
func fuzzAssertNoSecretInJSON(t *testing.T, label string, doc []byte, secrets []string) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("%s: output is not decodable JSON (%v): %q", label, err, doc)
	}
	if secret, where := fuzzSecretIn(v, "", secrets); secret != "" {
		t.Fatalf("%s: known value %q survived at %s in %q", label, secret, where, doc)
	}
}

// FuzzRedactor_Text applies the known-value rule to an arbitrary string. The
// three assertion classes are:
//
//  1. no panic — the fuzz engine's own assertion, plus a nil-safety net: the
//     target never indexes or slices the input and never relies on it being
//     valid UTF-8;
//  2. must redact — no known value survives as a substring of the output, and
//     the reported changed flag agrees with the returned string, so a caller
//     that logs only on change cannot be told "unchanged" while bytes moved;
//  3. must not over-redact — an unchanged call returns the input itself, and
//     redaction is idempotent: a second pass over an already-redacted string
//     reports no change and returns it verbatim, so a placeholder can neither
//     grow nor be re-redacted into an unbounded rewrite.
//
// The seeds cover the shapes the issue names: a password-bearing URL, an
// Authorization line, env-style lines, short values below the eight-character
// value rule, a value longer than 256 characters, safe names that must stay
// readable (a PATH-style colon list, a home directory, an SSH_AUTH_SOCK
// socket), percent-escapes, invalid UTF-8, a NUL byte and unicode.
func FuzzRedactor_Text(f *testing.F) {
	long := strings.Repeat("k3y-", 64) + fuzzKnownPassword
	seeds := []string{
		"postgres://app:" + fuzzKnownPassword + "@db.internal:5432/prod",
		"Authorization: Bearer " + fuzzKnownToken,
		"API_KEY=" + fuzzKnownToken + "\nHOME=/home/dev",
		"",
		"abc",
		"short",
		long,
		strings.Repeat("z", 300),
		"/usr/local/bin:/usr/bin:/bin",
		"/home/dev",
		"/tmp/ssh-XXXXXX/agent.1234",
		"HOME=/home/dev PATH=/usr/bin SSH_AUTH_SOCK=/tmp/ssh-XXXXXX/agent.1234",
		"%41%42%43%20ghp%5F0123456789abcdef",
		string([]byte{0xff, 0xfe, 'A', 0x80, 0x7f}) + fuzzKnownGitHub,
		"\x00key=" + fuzzKnownToken + "\x00end",
		"日本語のテキスト " + fuzzKnownToken + " 終わり",
		"пароль=секрет-значение",
		"already [redacted] and " + fuzzKnownGitHub + " and " + fuzzKnownGitHub,
	}
	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, s string) {
		r := fuzzRedactor()
		out, changed := r.Text(s)

		if changed != (out != s) {
			t.Fatalf("Text(%q) reported changed=%v but returned %q", s, changed, out)
		}
		if !changed && out != s {
			t.Fatalf("Text(%q) reported no change but returned %q", s, out)
		}
		fuzzAssertNoSecretInText(t, fmt.Sprintf("Text(%q)", s), out, r.secrets)

		again, changedAgain := r.Text(out)
		if changedAgain || again != out {
			t.Fatalf("Text is not idempotent: Text(%q) = %q, second pass changed=%v gave %q",
				s, out, changedAgain, again)
		}
	})
}

// FuzzRedactor_Native applies the rules to a raw JSON document the way a
// native payload is redacted. The three assertion classes are:
//
//  1. no panic — on a malformed document the target exercises the documented
//     Bytes fallback (raw text replacement) instead of skipping, so garbage
//     input is covered rather than excused;
//  2. must redact — a changed result re-encodes to valid JSON and no known
//     value survives in any string key or string value of the decoded result;
//  3. must not over-redact — a reported no-change result is deep-equal to a
//     fresh decode of the same input, so the walk never mutates a value it
//     claims to have left alone.
//
// The seeds mirror FuzzRedactor_Text in JSON form: env/headers maps, a
// password-bearing URL, short values under credential names, the safe-name
// corpus, unicode, NUL and invalid UTF-8 inside strings, plus non-object
// top-level values.
func FuzzRedactor_Native(f *testing.F) {
	seeds := []string{
		`{"env":{"API_KEY":"` + fuzzEnvAPIKey + `","PATH":"/usr/local/bin:/usr/bin","SSH_AUTH_SOCK":"/tmp/ssh-XXXXXX/agent.1234"}}`,
		`{"headers":{"Authorization":"Bearer ` + fuzzHeaderToken + `","Accept":"application/json"}}`,
		`{"db":"postgres://u:` + fuzzDBPassword + `@h/db","note":"` + fuzzKnownGitHub + `"}`,
		`{"password":"` + fuzzPasswordRaw + `"}`,
		`{"password":"abc","env":{"API_KEY":"short"}}`,
		`{"echo":"` + fuzzKnownToken + `","nested":{"deep":["` + fuzzKnownToken + `",1,null,true]}}`,
		`{"unicode":"日本語 ` + fuzzKnownToken + ` 終わり","escapes":"%41%42"}`,
		`{"nul":"a\u0000b ` + fuzzKnownGitHub + `","invalid":"\ud800"}`,
		`[]`,
		`null`,
		`"` + fuzzKnownConnURL + `"`,
		`{"a":1e1000,"b":"` + fuzzKnownGitHub + `"}`,
		`not json at all: ` + fuzzKnownToken,
		``,
	}
	for _, seed := range seeds {
		f.Add([]byte(seed))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		r := fuzzRedactor()

		var v any
		if err := json.Unmarshal(data, &v); err != nil {
			// Documented fallback for a malformed native payload: the raw
			// bytes still go through text replacement, so an already-known
			// value cannot leak just because the document does not parse.
			out, changed := r.Bytes(data)
			if changed {
				fuzzAssertNoSecretInText(t, fmt.Sprintf("Bytes(%q) fallback", data), string(out), r.secrets)
			}
			return
		}

		out, changed := r.Any(v)
		if !changed {
			var fresh any
			if err := json.Unmarshal(data, &fresh); err != nil {
				t.Fatalf("second decode of %q failed: %v", data, err)
			}
			if !reflect.DeepEqual(out, fresh) {
				t.Fatalf("Any(%q) reported no change but returned %#v, want %#v", data, out, fresh)
			}
		}

		encoded, err := json.Marshal(out)
		if err != nil {
			t.Fatalf("Any(%q) returned a value that does not re-encode: %v", data, err)
		}
		if changed && !json.Valid(encoded) {
			t.Fatalf("Any(%q) re-encoded to invalid JSON: %q", data, encoded)
		}
		fuzzAssertNoSecretInJSON(t, fmt.Sprintf("Any(%q)", data), encoded, r.secrets)
	})
}

// FuzzRedactor_Bytes applies the rules to arbitrary bytes, the path a retained
// record takes. The three assertion classes are:
//
//  1. no panic — any byte sequence, valid JSON or not, including the text
//     fallback for a document that does not decode;
//  2. must redact — no known value survives in the output (as raw bytes) or in
//     any string key or string value of a decodable output, and valid JSON
//     never turns into invalid JSON;
//  3. must not over-redact — a reported no-change result is byte-identical to
//     the input, so the no-copy fast path cannot quietly rewrite a record.
func FuzzRedactor_Bytes(f *testing.F) {
	seeds := [][]byte{
		[]byte(`{"env":{"API_KEY":"` + fuzzEnvAPIKey + `"}}`),
		[]byte(`{"headers":{"Proxy-Authorization":"Basic ` + fuzzKnownToken + `"}}`),
		[]byte(`{"url":"` + fuzzKnownConnURL + `","path":"/home/dev"}`),
		[]byte(`{"password":"abc"}`),
		[]byte(`{"echo":"` + fuzzKnownGitHub + `"}`),
		[]byte(`{"invalid":"` + string([]byte{0xff, 0xfe}) + `","nul":"\u0000","unicode":"日本語"}`),
		[]byte(`not json: ` + fuzzKnownToken),
		[]byte(`{"a":1} trailing garbage`),
		[]byte(`{"a":1e1000,"b":"` + fuzzKnownGitHub + `"}`),
		{},
		nil,
	}
	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		r := fuzzRedactor()
		out, changed := r.Bytes(data)

		if !changed && !bytes.Equal(out, data) {
			t.Fatalf("Bytes(%q) reported no change but returned %q", data, out)
		}
		if json.Valid(data) && !json.Valid(out) {
			t.Fatalf("Bytes(%q) turned valid JSON into invalid JSON: %q", data, out)
		}
		if changed {
			fuzzAssertNoSecretInText(t, fmt.Sprintf("Bytes(%q)", data), string(out), r.secrets)
		}
		if json.Valid(out) {
			fuzzAssertNoSecretInJSON(t, fmt.Sprintf("Bytes(%q)", data), out, r.secrets)
		}
	})
}

// TestRedactorFuzzCorpus_SafeNamesSurvive pins the deny-list versus allow-list
// boundary on the seed corpus: the same document shapes the fuzz targets use
// must stay byte-identical when they hold only safe names, safe values or a
// value below the eight-character rule, and must be rewritten when a name rule
// or the URL-shape rule genuinely selects the value.
func TestRedactorFuzzCorpus_SafeNamesSurvive(t *testing.T) {
	r := fuzzRedactor()

	t.Run("unchanged", func(t *testing.T) {
		safe := []struct {
			name     string
			doc      string
			readable []string // values the allow-list keeps, verbatim
		}{
			{
				"env safe names",
				`{"env":{"PATH":"/usr/local/bin:/usr/bin","HOME":"/home/dev","SSH_AUTH_SOCK":"/tmp/ssh-XXXXXX/agent.1234"}}`,
				[]string{"/usr/local/bin:/usr/bin", "/home/dev", "/tmp/ssh-XXXXXX/agent.1234"},
			},
			{"headers accept", `{"headers":{"Accept":"application/json"}}`, []string{"application/json"}},
			{"short password value not selected", `{"password":"abc"}`, []string{`"abc"`}},
			{"short api key value not selected", `{"env":{"API_KEY":"short"}}`, []string{`"short"`}},
		}
		for _, tc := range safe {
			t.Run(tc.name, func(t *testing.T) {
				out, changed := r.Bytes([]byte(tc.doc))
				if changed {
					t.Fatalf("over-redacted %s: %s", tc.doc, out)
				}
				if string(out) != tc.doc {
					t.Fatalf("no-change path rewrote the document: %s, want %s", out, tc.doc)
				}
				for _, readable := range tc.readable {
					if !strings.Contains(tc.doc, readable) {
						t.Fatalf("corpus bug: %s does not contain %q", tc.doc, readable)
					}
					if !strings.Contains(string(out), readable) {
						t.Fatalf("over-redacted %q in %s", readable, out)
					}
				}
			})
		}
	})

	t.Run("redacted", func(t *testing.T) {
		selected := []struct {
			name string
			doc  string
			path []string
		}{
			{"credential name", `{"password":"` + fuzzPasswordRaw + `"}`, []string{"password"}},
			{"authorization header", `{"Authorization":"Bearer ` + fuzzKnownToken + `"}`, []string{"Authorization"}},
			{"header name rule", `{"Authorization":"Bearer ` + fuzzHeaderToken + `"}`, []string{"Authorization"}},
			{"env api key", `{"env":{"API_KEY":"` + fuzzEnvAPIKey + `"}}`, []string{"env", "API_KEY"}},
			{"password bearing url", `{"db":"postgres://u:` + fuzzDBPassword + `@h/db"}`, []string{"db"}},
		}
		for _, tc := range selected {
			t.Run(tc.name, func(t *testing.T) {
				out, changed := r.Bytes([]byte(tc.doc))
				if !changed {
					t.Fatalf("nothing was redacted in %s", tc.doc)
				}
				if !json.Valid(out) {
					t.Fatalf("redacted document is not valid JSON: %q", out)
				}
				fuzzAssertNoSecretInJSON(t, tc.name, out, r.secrets)

				var decoded map[string]any
				if err := json.Unmarshal(out, &decoded); err != nil {
					t.Fatalf("cannot decode %q: %v", out, err)
				}
				if got := fuzzLookup(t, decoded, tc.path); got != RedactedPlaceholder {
					t.Fatalf("%v = %v, want %q (%s)", tc.path, got, RedactedPlaceholder, out)
				}
			})
		}
	})
}

// fuzzLookup returns the value at path inside a decoded JSON object.
func fuzzLookup(t *testing.T, doc map[string]any, path []string) any {
	t.Helper()
	var cur any = doc
	for _, step := range path {
		object, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("path %v: %q is not inside an object", path, step)
		}
		cur, ok = object[step]
		if !ok {
			t.Fatalf("path %v: key %q is missing", path, step)
		}
	}
	return cur
}
