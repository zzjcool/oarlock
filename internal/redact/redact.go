// Package redact implements the credential rules of docs/spec/redaction.md.
//
// Two families of rule live here:
//
//   - structural rules, which select a value by the name that holds it
//     (env / headers / Authorization) or by its shape (a URL with a password);
//   - known-value rules, which replace values the session already knows are
//     credentials, longest first.
//
// A value is never selected by a vendor name or by a substring of a word: the
// name rules are exact, so SSH_AUTH_SOCK, GIT_AUTHOR_EMAIL, HOME and PATH stay
// readable.
package redact

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/zzjcool/oarlock"
)

// RedactedPlaceholder replaces every selected credential value.
const RedactedPlaceholder = "[redacted]"

// credentialWords are the final underscore- or hyphen-separated segments that
// select a value. Comparison is case-insensitive.
var credentialWords = map[string]bool{
	"KEY":         true,
	"APIKEY":      true,
	"TOKEN":       true,
	"SECRET":      true,
	"PASSWORD":    true,
	"PASSWD":      true,
	"PASSPHRASE":  true,
	"CREDENTIAL":  true,
	"CREDENTIALS": true,
	"COOKIE":      true,
	"PWD":         true,
}

// Redactor applies the rules. A nil *Redactor is valid and applies the
// structural rules with no known values.
type Redactor struct {
	// secrets are known credential values, sorted longest first so a short
	// prefix cannot hide a longer value.
	secrets []string
}

// New builds a redactor for the known session values, such as env values and
// MCP credentials. Values that fail the value rule are ignored.
func New(known []string) *Redactor {
	return (&Redactor{}).with(known)
}

func (r *Redactor) with(known []string) *Redactor {
	seen := make(map[string]bool)
	out := &Redactor{}
	if r != nil {
		for _, s := range r.secrets {
			if !seen[s] {
				seen[s] = true
				out.secrets = append(out.secrets, s)
			}
		}
	}
	for _, raw := range known {
		for _, s := range expandSecrets(raw) {
			if !seen[s] {
				seen[s] = true
				out.secrets = append(out.secrets, s)
			}
		}
	}
	sort.Slice(out.secrets, func(i, j int) bool {
		if len(out.secrets[i]) != len(out.secrets[j]) {
			return len(out.secrets[i]) > len(out.secrets[j])
		}
		return out.secrets[i] < out.secrets[j]
	})
	return out
}

// expandSecrets returns the raw value when it passes the value rule, plus the
// whole URL and its decoded/encoded passwords when it is a password-bearing
// connection URL.
func expandSecrets(raw string) []string {
	var out []string
	if SelectValue(raw) {
		out = append(out, raw)
	}
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return out
	}
	password, ok := u.User.Password()
	if !ok || password == "" {
		return out
	}
	out = append(out, raw) // the whole connection string is protected
	if SelectValue(password) {
		out = append(out, password)
	}
	// The encoded form as it appears in the URL.
	if encoded := u.User.String(); encoded != "" {
		if i := strings.IndexByte(encoded, ':'); i >= 0 {
			if enc := encoded[i+1:]; SelectValue(enc) && enc != password {
				out = append(out, enc)
			}
		}
	}
	return out
}

// SelectValue applies the value rule: at least eight characters, not empty,
// not a bare slash or home path, not a Windows drive path.
func SelectValue(s string) bool {
	if len(s) < 8 {
		return false
	}
	if strings.HasPrefix(s, "/") || strings.HasPrefix(s, "~/") {
		return false
	}
	if len(s) >= 3 && isASCIILetter(s[0]) && s[1] == ':' && (s[2] == '\\' || s[2] == '/') {
		return false
	}
	return true
}

func isASCIILetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// NameIsCredential reports whether a key's last underscore-separated segment
// is a credential word, or the key is exactly PGPASSWORD.
func NameIsCredential(key string) bool {
	if strings.EqualFold(key, "PGPASSWORD") {
		return true
	}
	return credentialWords[strings.ToUpper(lastSegment(key, '_'))]
}

// HeaderIsCredential reports whether a header name selects a credential:
// Authorization, Proxy-Authorization, or a final hyphen-separated segment in
// the credential word list.
func HeaderIsCredential(name string) bool {
	if strings.EqualFold(name, "Authorization") || strings.EqualFold(name, "Proxy-Authorization") {
		return true
	}
	return credentialWords[strings.ToUpper(lastSegment(name, '-'))]
}

func lastSegment(s string, sep byte) string {
	if i := strings.LastIndexByte(s, sep); i >= 0 {
		return s[i+1:]
	}
	return s
}

// Text replaces every known credential value, longest first. It returns the
// original string and false when nothing matched (no copy).
func (r *Redactor) Text(s string) (string, bool) {
	if r == nil || len(r.secrets) == 0 || s == "" {
		return s, false
	}
	out := s
	for _, secret := range r.secrets {
		if strings.Contains(out, secret) {
			out = strings.ReplaceAll(out, secret, RedactedPlaceholder)
		}
	}
	return out, out != s
}

// Any redacts one decoded JSON value: names select env/header credentials,
// strings select password-bearing URLs and known values. The input is not
// mutated; a changed copy is returned.
func (r *Redactor) Any(v any) (any, bool) {
	extra := r.collect(v)
	rr := r.with(extra)
	return rr.any("", v)
}

func (r *Redactor) any(key string, v any) (any, bool) {
	switch value := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(value))
		changed := false
		for k, child := range value {
			newKey, keyChanged := r.Text(k)
			if keyChanged {
				changed = true
			}
			newChild, childChanged := r.child(k, child)
			if childChanged {
				changed = true
			}
			out[newKey] = newChild
		}
		if !changed {
			return v, false
		}
		return out, true
	case []any:
		out := make([]any, len(value))
		changed := false
		for i, child := range value {
			newChild, childChanged := r.any(key, child)
			if childChanged {
				changed = true
			}
			out[i] = newChild
		}
		if !changed {
			return v, false
		}
		return out, true
	case string:
		return r.string(value)
	default:
		return v, false
	}
}

func (r *Redactor) child(key string, v any) (any, bool) {
	if s, ok := v.(string); ok {
		if NameIsCredential(key) || HeaderIsCredential(key) {
			if SelectValue(s) {
				return RedactedPlaceholder, true
			}
			// The structural value rule declines a path-shaped value, but a
			// known session credential embedded in it is still a credential.
			// Fall through to the known-value rule instead of returning the
			// whole value untouched.
			return r.string(s)
		}
		return r.string(s)
	}
	return r.any(key, v)
}

func (r *Redactor) string(s string) (any, bool) {
	if u, err := url.Parse(s); err == nil && u.User != nil {
		if password, ok := u.User.Password(); ok && password != "" {
			return RedactedPlaceholder, true
		}
	}
	if out, changed := r.Text(s); changed {
		return out, true
	}
	return s, false
}

// collect gathers credential values that structural rules select, so a token
// first seen in an Authorization header is also replaced when it appears
// elsewhere in the same value tree. The part after an authorization scheme is
// collected too.
func (r *Redactor) collect(v any) []string {
	var out []string
	var walk func(key string, v any)
	walk = func(key string, v any) {
		switch value := v.(type) {
		case map[string]any:
			for k, child := range value {
				if s, ok := child.(string); ok && (NameIsCredential(k) || HeaderIsCredential(k)) {
					if SelectValue(s) {
						out = append(out, s)
						if suffix := authorizationSuffix(s); suffix != "" && SelectValue(suffix) {
							out = append(out, suffix)
						}
					}
					continue
				}
				walk(k, child)
			}
		case []any:
			for _, child := range value {
				walk(key, child)
			}
		case string:
			if u, err := url.Parse(value); err == nil && u.User != nil {
				if password, ok := u.User.Password(); ok && password != "" && SelectValue(password) {
					out = append(out, password)
				}
			}
		}
	}
	walk("", v)
	return out
}

// authorizationSuffix returns the text after the first space, e.g. the token
// in "Bearer sk-...".
func authorizationSuffix(value string) string {
	if i := strings.IndexByte(value, ' '); i >= 0 {
		return strings.TrimSpace(value[i+1:])
	}
	return ""
}

// Bytes applies the rules to a raw JSON document. When nothing changes the
// original slice is returned (no copy); otherwise a re-encoded document is.
func (r *Redactor) Bytes(raw []byte) ([]byte, bool) {
	if len(raw) == 0 {
		return raw, false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		// Not JSON: fall back to text replacement so a malformed native
		// payload cannot leak a known value.
		out, changed := r.Text(string(raw))
		if !changed {
			return raw, false
		}
		return []byte(out), true
	}
	out, changed := r.Any(v)
	if !changed {
		return raw, false
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return raw, false
	}
	return encoded, true
}

// Record applies the rules to every byte a record retains: native payloads,
// request inputs, responses and projected event text. The runtime's own
// configuration is not touched; only the retained copy changes.
func (r *Redactor) Record(rec oarlock.Record) (oarlock.Record, bool) {
	raw, err := json.Marshal(rec)
	if err != nil {
		return rec, false
	}
	redacted, changed := r.Bytes(raw)
	if !changed {
		return rec, false
	}
	var out oarlock.Record
	if err := json.Unmarshal(redacted, &out); err != nil {
		return rec, false
	}
	return out, true
}

// Error returns an error whose printed text is redacted. The returned value
// unwraps to the original error, so errors.Is/As still reach the native cause;
// the original object is not mutated and a caller that prints it directly is
// outside this guarantee.
func (r *Redactor) Error(err error) error {
	if err == nil {
		return nil
	}
	msg, changed := r.Text(err.Error())
	if !changed {
		return err
	}
	return &redactedError{msg: msg, cause: err}
}

type redactedError struct {
	msg   string
	cause error
}

func (e *redactedError) Error() string { return e.msg }

func (e *redactedError) Unwrap() error { return e.cause }

func (e *redactedError) Format(s fmt.State, verb rune) {
	switch verb {
	case 'v', 's':
		_, _ = fmt.Fprint(s, e.msg)
	default:
		_, _ = fmt.Fprintf(s, "%%!%c(*redact.redactedError)", verb)
	}
}
