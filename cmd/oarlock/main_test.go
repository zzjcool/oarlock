package main

// Process-level tests of the real `oarlock` command (issue #99). Everything the
// in-process cli tests stub out — the adapter registry, stdout/stderr, the
// binary's own exit codes — is real here: the test builds the command with
// `go build` and runs it as a child process.
//
// Every spawn is bounded by binaryBound, so a regression that hangs fails the
// test instead of blocking CI.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/oarlock/internal/cli"
)

// binaryBound bounds the build and every run of the built command.
const binaryBound = 30 * time.Second

// binaryName is the built command's file name on this platform.
func binaryName() string {
	if runtime.GOOS == "windows" {
		return "oarlock.exe"
	}
	return "oarlock"
}

// buildOarlock builds the command into the test's temp dir and returns its path.
// The working directory is the package directory (go test sets it), so "." is
// exactly the command a release builds.
func buildOarlock(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), binaryName())

	ctx, cancel := context.WithTimeout(context.Background(), binaryBound)
	defer cancel()

	cmd := exec.CommandContext(ctx, "go", "build", "-o", bin, ".")
	cmd.Dir = "."
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			t.Fatalf("go build -o %s . did not finish within %v", bin, binaryBound)
		}
		t.Fatalf("go build -o %s . failed: %v\n%s", bin, err, stderr.String())
	}
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("go build reported success but %s is not there: %v", bin, err)
	}
	return bin
}

// oarlockRun is one finished child process: both streams and the exit code.
type oarlockRun struct {
	stdout string
	stderr string
	exit   int
}

// runOarlock runs the built command in dir, bounded by binaryBound.
func runOarlock(t *testing.T, bin, dir string, args ...string) oarlockRun {
	t.Helper()
	what := binaryName() + " " + strings.Join(args, " ")

	ctx, cancel := context.WithTimeout(context.Background(), binaryBound)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("%s did not finish within %v\nstdout: %s\nstderr: %s", what, binaryBound, stdout.String(), stderr.String())
	}

	exit := 0
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("running %s: %v (stderr: %s)", what, err, stderr.String())
		}
		exit = exitErr.ExitCode()
	}
	return oarlockRun{stdout: stdout.String(), stderr: stderr.String(), exit: exit}
}

// decodeOneJSONLine decodes out as exactly one JSON document, the CLI's --json
// contract: one line, nothing else on stdout.
func decodeOneJSONLine(t *testing.T, what, out string, v any) {
	t.Helper()
	trimmed := strings.TrimSpace(out)
	if trimmed == "" {
		t.Fatalf("%s wrote no JSON document (stdout is empty)", what)
	}
	if lines := strings.Count(trimmed, "\n"); lines != 0 {
		t.Fatalf("%s wrote %d lines, want one JSON document:\n%s", what, lines+1, out)
	}
	dec := json.NewDecoder(strings.NewReader(trimmed))
	if err := dec.Decode(v); err != nil {
		t.Fatalf("%s did not write valid JSON: %v\n%s", what, err, out)
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		t.Fatalf("%s wrote more than one JSON value (second decode: %v)\n%s", what, err, out)
	}
}

// TestBinaryListJSON pins the built binary's `list --json` document: real
// adapters registered by main, one line, and every runtime able to open a
// session.
func TestBinaryListJSON(t *testing.T) {
	bin := buildOarlock(t)
	res := runOarlock(t, bin, t.TempDir(), "list", "--json")

	if res.exit != 0 {
		t.Fatalf("exit = %d, want 0\nstdout: %s\nstderr: %s", res.exit, res.stdout, res.stderr)
	}
	var dto cli.ListDTO
	decodeOneJSONLine(t, "list --json", res.stdout, &dto)

	if dto.Schema != cli.Schema {
		t.Errorf("schema = %q, want %q", dto.Schema, cli.Schema)
	}
	if dto.Command != "list" {
		t.Errorf("command = %q, want %q", dto.Command, "list")
	}
	if len(dto.Runtimes) == 0 {
		t.Fatalf("runtimes is empty: the binary registered no adapter")
	}
	for _, rt := range dto.Runtimes {
		if rt.ID == "" {
			t.Errorf("a runtime has no id: %+v", rt)
		}
		if !rt.Capabilities.Session {
			t.Errorf("runtime %q declares capabilities.session = false, want true for every registered runtime", rt.ID)
		}
	}
}

// TestBinaryProbePiJSON pins `probe pi --json`: the one result names pi, the
// installation answer is present (available or not), and the probe did not
// fail.
func TestBinaryProbePiJSON(t *testing.T) {
	bin := buildOarlock(t)
	res := runOarlock(t, bin, t.TempDir(), "probe", "pi", "--json")

	if res.exit != 0 {
		t.Fatalf("exit = %d, want 0\nstdout: %s\nstderr: %s", res.exit, res.stdout, res.stderr)
	}
	var dto cli.ProbeDTO
	decodeOneJSONLine(t, "probe pi --json", res.stdout, &dto)

	if dto.Schema != cli.Schema {
		t.Errorf("schema = %q, want %q", dto.Schema, cli.Schema)
	}
	if dto.Command != "probe" {
		t.Errorf("command = %q, want %q", dto.Command, "probe")
	}
	if len(dto.Results) != 1 {
		t.Fatalf("results has %d entries, want the one pi result: %+v", len(dto.Results), dto.Results)
	}
	result := dto.Results[0]
	if result.Runtime != "pi" {
		t.Errorf("runtime = %q, want %q", result.Runtime, "pi")
	}
	if result.Installation == nil {
		t.Fatalf("installation is absent, want the probe's answer (available is the data, not an error)")
	}
	if result.Error != "" {
		t.Errorf("error = %q, want none", result.Error)
	}
}

// TestBinaryRecordRefusesExistingPath pins the no-overwrite rule through the
// real binary: an existing record file fails before a session opens, keeps its
// bytes, and reports the same failure as a --json DTO.
func TestBinaryRecordRefusesExistingPath(t *testing.T) {
	bin := buildOarlock(t)
	const sentinel = "sentinel: do not overwrite me\n"
	path := filepath.Join(t.TempDir(), "existing.oar")
	if err := os.WriteFile(path, []byte(sentinel), 0o644); err != nil {
		t.Fatalf("pre-creating %s: %v", path, err)
	}

	human := runOarlock(t, bin, t.TempDir(),
		"run", "pi", "hello",
		"--record", path,
		"--cwd", t.TempDir(),
	)
	if human.exit != cli.ExitError {
		t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", human.exit, cli.ExitError, human.stdout, human.stderr)
	}
	if !strings.Contains(human.stderr, "record file already exists") {
		t.Errorf("stderr does not name the refusal, want %q:\n%s", "record file already exists", human.stderr)
	}
	if strings.Contains(human.stdout, "[runtime") {
		t.Errorf("stdout has a progress header, want none: the refusal happens before a session opens:\n%s", human.stdout)
	}

	jsonRes := runOarlock(t, bin, t.TempDir(),
		"run", "pi", "hello",
		"--record", path,
		"--cwd", t.TempDir(),
		"--json",
	)
	if jsonRes.exit != cli.ExitError {
		t.Fatalf("--json exit = %d, want %d\nstdout: %s\nstderr: %s", jsonRes.exit, cli.ExitError, jsonRes.stdout, jsonRes.stderr)
	}
	var dto cli.RunDTO
	decodeOneJSONLine(t, "run --record --json", jsonRes.stdout, &dto)
	if dto.Schema != cli.Schema {
		t.Errorf("schema = %q, want %q", dto.Schema, cli.Schema)
	}
	if dto.Command != "run" {
		t.Errorf("command = %q, want %q", dto.Command, "run")
	}
	if dto.Error == "" {
		t.Errorf("error is empty, want the record refusal: %+v", dto)
	}
	if dto.Outcome != "" {
		t.Errorf("outcome = %q, want it absent: no turn ran", dto.Outcome)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s after the refused runs: %v", path, err)
	}
	if string(got) != sentinel {
		t.Fatalf("the existing record file changed:\n got %q\nwant %q", got, sentinel)
	}
}
