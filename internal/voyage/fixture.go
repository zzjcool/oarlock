package voyage

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/zzjcool/oarlock"
)

const (
	// MaxFixtureBytes caps one committed voyage fixture.
	MaxFixtureBytes = 256 << 10
	// MaxTotalBytes caps every fixture in one directory together.
	MaxTotalBytes = 2 << 20
	// IndexName is the committed index file next to the fixtures.
	IndexName = "index.json"
	// IndexFormat identifies the index schema.
	IndexFormat = "oar-voyage-index/1"
)

// regenerateHint is appended to every index failure a human can fix by
// regenerating the index.
const regenerateHint = "run `go generate ./internal/voyage`"

// Finding is one credential-shaped match in a fixture. Excerpt is clipped and
// defanged so that reporting a finding never reproduces a full credential.
type Finding struct {
	Pattern string
	Line    int
	Excerpt string
}

// credentialPattern is one named scan rule.
type credentialPattern struct {
	name string
	re   *regexp.Regexp
	// plain marks a rule whose match carries no secret material (a path, a
	// PEM header, the redaction placeholder): its excerpt is shown verbatim,
	// clipped. Every other rule's excerpt has its middle replaced by "...".
	plain bool
	// group is the submatch that holds the match proper; 0 is the whole match.
	// Prefix-style rules consume a leading boundary and report group 1.
	group int
	// keep, when set, may reject a match after the fact (RE2 has no
	// look-around). loc is the submatch index slice for line.
	keep func(line string, loc []int) bool
}

// secretLead is the boundary a prefix-style token must follow: the start of the
// line, a character that cannot continue an identifier, or a JSON line-break
// escape (in raw bytes "\nsk-..." would otherwise hide behind the letter n).
// RE2 has no look-behind, so the boundary is consumed and the token is group 1.
const secretLead = `(?:^|[^A-Za-z0-9_]|\\[nrt])`

// credentialPatterns are compiled once. A value is selected by its shape,
// never by a bare word such as "password" or "apiKey": a model transcript may
// legitimately say those.
var credentialPatterns = []credentialPattern{
	{name: "openai-anthropic-key", re: regexp.MustCompile(secretLead + `(sk-[A-Za-z0-9_-]{16,})`), group: 1},
	{name: "github-token", re: regexp.MustCompile(secretLead + `(gh[pousr]_[A-Za-z0-9]{20,})`), group: 1},
	{name: "github-fine-grained-token", re: regexp.MustCompile(secretLead + `(github_pat_[A-Za-z0-9_]{20,})`), group: 1},
	{name: "aws-access-key-id", re: regexp.MustCompile(secretLead + `(AKIA[0-9A-Z]{16})`), group: 1},
	{name: "google-api-key", re: regexp.MustCompile(secretLead + `(AIza[0-9A-Za-z_-]{30,})`), group: 1},
	{name: "slack-token", re: regexp.MustCompile(secretLead + `(xox[baprs]-[A-Za-z0-9-]{10,})`), group: 1},
	{name: "pem-private-key", re: regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`), plain: true},
	// "Bearer [redacted]" does not match: '[' is outside the token class.
	{name: "bearer-token", re: regexp.MustCompile(`Bearer\s+[A-Za-z0-9._~+/=-]{16,}`)},
	{name: "jwt", re: regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]*`)},
	{
		name: "url-password",
		re:   regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s/:@"'\\]+:([^\s/@"'\\]+)@`),
		keep: func(line string, loc []int) bool {
			return line[loc[2]:loc[3]] != "[redacted]"
		},
	},
	{
		// "[redacted]" alone is the placeholder; "[redacted]" glued to
		// credential characters means a partial redaction left part of a
		// credential behind.
		name:  "redacted-adjacent",
		re:    regexp.MustCompile(`[A-Za-z0-9_+.-]\[redacted\]|\[redacted\][A-Za-z0-9_+.-]`),
		plain: true,
		keep: func(line string, loc []int) bool {
			start := loc[0]
			// In raw JSON "\n[redacted]" is a line break before the
			// placeholder, not a letter touching it.
			if line[start] != '[' && start > 0 && line[start-1] == '\\' && strings.IndexByte("nrtbf", line[start]) >= 0 {
				return false
			}
			return true
		},
	},
	{
		// The boundary also accepts a JSON line-break escape, so "\n/home/x"
		// in raw bytes is not hidden by the letter n.
		name:  "home-path",
		re:    regexp.MustCompile(`(?:^|[^A-Za-z0-9._-]|\\[nrt])/(?:root|home/[A-Za-z0-9._-]+|Users/[A-Za-z0-9._-]+)`),
		plain: true,
	},
}

// ScanCredentials reports every credential-shaped value in data: known vendor
// token shapes, JWTs, password-bearing URLs, PEM private keys, a "[redacted]"
// glued to credential characters, and absolute home-directory paths. The
// placeholder "[redacted]" on its own is fine. Lines are 1-based; data is
// scanned as raw bytes, so JSON escapes are seen as written. Findings come out
// in line order, then rule order.
func ScanCredentials(data []byte) []Finding {
	var out []Finding
	lineNo := 0
	for rest := data; len(rest) > 0; {
		lineNo++
		var raw []byte
		if i := bytes.IndexByte(rest, '\n'); i >= 0 {
			raw, rest = rest[:i], rest[i+1:]
		} else {
			raw, rest = rest, nil
		}
		line := string(raw)
		for _, p := range credentialPatterns {
			for _, loc := range p.re.FindAllStringSubmatchIndex(line, -1) {
				if p.keep != nil && !p.keep(line, loc) {
					continue
				}
				out = append(out, Finding{
					Pattern: p.name,
					Line:    lineNo,
					Excerpt: excerpt(line[loc[2*p.group]:loc[2*p.group+1]], p.plain),
				})
			}
		}
	}
	return out
}

// excerpt makes a match safe to print: control characters are replaced and a
// secret-bearing match keeps only its first few and last two characters.
func excerpt(match string, plain bool) string {
	match = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, strings.ToValidUTF8(match, "?"))
	r := []rune(match)
	if plain {
		if len(r) > 40 {
			return string(r[:37]) + "..."
		}
		return match
	}
	head := len(r) / 3
	if head > 6 {
		head = 6
	}
	if len(r) <= 12 {
		return string(r[:head]) + "..."
	}
	return string(r[:head]) + "..." + string(r[len(r)-2:])
}

// CheckFixture enforces the committed-fixture rules on one file: it is no
// larger than MaxFixtureBytes, it decodes as a complete voyage log (an
// ErrTruncated log is a failure), and ScanCredentials finds nothing in its raw
// bytes. Errors name the file and the finding but never a full secret.
func CheckFixture(path string) error {
	name := filepath.Base(path)
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("voyage fixture %s: %w", name, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("voyage fixture %s: not a regular file", name)
	}
	if info.Size() > MaxFixtureBytes {
		return fmt.Errorf("voyage fixture %s: %d bytes exceeds the %d byte cap (MaxFixtureBytes)", name, info.Size(), MaxFixtureBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("voyage fixture %s: %w", name, err)
	}
	log, err := Decode(bytes.NewReader(data))
	if err != nil {
		if errors.Is(err, ErrTruncated) {
			return fmt.Errorf("voyage fixture %s is truncated (no end line): %w", name, err)
		}
		return fmt.Errorf("voyage fixture %s: %w", name, err)
	}
	if !log.Complete() {
		return fmt.Errorf("voyage fixture %s is incomplete (no end line): %w", name, ErrTruncated)
	}
	if found := ScanCredentials(data); len(found) > 0 {
		f := found[0]
		return fmt.Errorf("voyage fixture %s: line %d matches credential pattern %q (%s); %d finding(s) in total",
			name, f.Line, f.Pattern, f.Excerpt, len(found))
	}
	return nil
}

// IndexEntry describes one committed fixture. SHA256 covers the raw file
// bytes, so any edit shows up in the index diff.
type IndexEntry struct {
	Name      string `json:"name"`
	File      string `json:"file"`
	Runtime   string `json:"runtime"`
	SessionID string `json:"sessionId"`
	EndReason string `json:"endReason"`
	Records   int    `json:"records"`
	Frames    int    `json:"frames"`
	Requests  int    `json:"requests"`
	Responses int    `json:"responses"`
	Bytes     int64  `json:"bytes"`
	SHA256    string `json:"sha256"`
	Complete  bool   `json:"complete"`
}

// Index is the committed testdata/voyage/index.json: one entry per fixture,
// sorted by Name.
type Index struct {
	Format   string       `json:"format"`
	Fixtures []IndexEntry `json:"fixtures"`
}

// BuildIndex describes every *.jsonl file in dir (index.json and other files
// are ignored). The result is deterministic: sorted by Name, no timestamps, no
// dependence on directory order. It fails when the fixtures together exceed
// MaxTotalBytes or when one is not a decodable voyage log; a truncated log is
// listed with Complete false.
func BuildIndex(dir string) (Index, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return Index{}, fmt.Errorf("voyage index: %w", err)
	}
	var names []string
	var total int64
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || n == IndexName || !strings.HasSuffix(n, ".jsonl") {
			continue
		}
		info, err := os.Stat(filepath.Join(dir, n))
		if err != nil {
			return Index{}, fmt.Errorf("voyage index: %w", err)
		}
		if !info.Mode().IsRegular() {
			return Index{}, fmt.Errorf("voyage index: %s is not a regular file", n)
		}
		total += info.Size()
		names = append(names, n)
	}
	if total > MaxTotalBytes {
		return Index{}, fmt.Errorf("voyage index: fixtures in %s total %d bytes, over the %d byte cap (MaxTotalBytes)", dir, total, MaxTotalBytes)
	}
	idx := Index{Format: IndexFormat, Fixtures: []IndexEntry{}}
	for _, n := range names {
		data, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			return Index{}, fmt.Errorf("voyage index: %w", err)
		}
		log, err := Decode(bytes.NewReader(data))
		if log == nil {
			return Index{}, fmt.Errorf("voyage index: %s: %w", n, err)
		}
		sum := sha256.Sum256(data)
		entry := IndexEntry{
			Name:      strings.TrimSuffix(n, ".jsonl"),
			File:      filepath.ToSlash(n),
			Runtime:   log.Header.Runtime,
			SessionID: log.Header.SessionID,
			Records:   len(log.Records),
			Bytes:     int64(len(data)),
			SHA256:    hex.EncodeToString(sum[:]),
			Complete:  log.Complete(),
		}
		if log.End != nil {
			entry.EndReason = log.End.Reason
		}
		for _, rec := range log.Records {
			switch rec.Kind {
			case oarlock.KindFrame:
				entry.Frames++
			case oarlock.KindRequest:
				entry.Requests++
			case oarlock.KindResponse:
				entry.Responses++
			}
		}
		idx.Fixtures = append(idx.Fixtures, entry)
	}
	sort.Slice(idx.Fixtures, func(i, j int) bool { return idx.Fixtures[i].Name < idx.Fixtures[j].Name })
	return idx, nil
}

// marshalIndex is the one serialisation of an Index: 2-space indent and a
// trailing LF.
func marshalIndex(idx Index) ([]byte, error) {
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// WriteIndex checks every fixture in dir with CheckFixture, then writes
// dir/index.json atomically (a temporary file in dir, then a rename).
func WriteIndex(dir string) error {
	idx, err := BuildIndex(dir)
	if err != nil {
		return err
	}
	for _, e := range idx.Fixtures {
		if err := CheckFixture(filepath.Join(dir, e.File)); err != nil {
			return err
		}
	}
	data, err := marshalIndex(idx)
	if err != nil {
		return fmt.Errorf("voyage index: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".index-*.tmp")
	if err != nil {
		return fmt.Errorf("voyage index: %w", err)
	}
	tmpName := tmp.Name()
	fail := func(err error) error {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("voyage index: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("voyage index: %w", err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("voyage index: %w", err)
	}
	if err := os.Rename(tmpName, filepath.Join(dir, IndexName)); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("voyage index: %w", err)
	}
	return nil
}

// VerifyIndex fails unless dir/index.json equals, byte for byte, the index
// WriteIndex would write for the fixtures now in dir. The error says how to
// repair it.
func VerifyIndex(dir string) error {
	idx, err := BuildIndex(dir)
	if err != nil {
		return fmt.Errorf("%w (fix the fixture, then %s)", err, regenerateHint)
	}
	want, err := marshalIndex(idx)
	if err != nil {
		return fmt.Errorf("voyage index: %w", err)
	}
	path := filepath.Join(dir, IndexName)
	got, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("voyage index: cannot read %s: %w; %s", path, err, regenerateHint)
	}
	if !bytes.Equal(got, want) {
		return fmt.Errorf("voyage index: %s is stale: it does not match the fixtures in %s; %s and commit the result", path, dir, regenerateHint)
	}
	return nil
}
