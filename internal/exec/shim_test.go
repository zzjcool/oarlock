package exec

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeFileInfo is the minimum os.FileInfo needed by resolveShimTarget: it only
// ever asks whether the candidate is a regular file.
type fakeFileInfo struct {
	name string
	mode fs.FileMode
}

func (fi fakeFileInfo) Name() string       { return fi.name }
func (fi fakeFileInfo) Size() int64        { return 0 }
func (fi fakeFileInfo) Mode() fs.FileMode  { return fi.mode }
func (fi fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (fi fakeFileInfo) IsDir() bool        { return fi.mode.IsDir() }
func (fi fakeFileInfo) Sys() any           { return nil }

// fakeFS builds a stat func over a fixed set of paths and records every probe,
// so a test can prove resolveShimTarget looked at the .ps1 candidate and
// nothing else.
type fakeFS struct {
	files  map[string]fs.FileMode
	probed []string
}

func (f *fakeFS) stat(path string) (os.FileInfo, error) {
	f.probed = append(f.probed, path)
	mode, ok := f.files[path]
	if !ok {
		return nil, &fs.PathError{Op: "stat", Path: path, Err: fs.ErrNotExist}
	}
	return fakeFileInfo{name: filepath.Base(path), mode: mode}, nil
}

func regularPowerShell(p string) func() (string, bool) {
	return func() (string, bool) { return p, true }
}

func noPowerShell() (string, bool) { return "", false }

func TestShimResolvesPS1SiblingForCmdLauncher(t *testing.T) {
	dir := filepath.Join("home", "user", "AppData", "Roaming", "npm")
	cmd := filepath.Join(dir, "cursor-agent.cmd")
	ps1 := filepath.Join(dir, "cursor-agent.ps1")
	const psExe = `C:\Program Files\PowerShell\7\pwsh.exe`

	f := &fakeFS{files: map[string]fs.FileMode{ps1: 0}}
	args := []string{"--version"}
	exe, argv, ok := resolveShimTarget(cmd, args, f.stat, regularPowerShell(psExe))
	if !ok {
		t.Fatalf("resolveShimTarget(%q) = not ok, want ok", cmd)
	}
	if exe != psExe {
		t.Fatalf("exe = %q, want %q", exe, psExe)
	}
	want := []string{psExe, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", ps1, "--version"}
	if !reflect.DeepEqual(argv, want) {
		t.Fatalf("argv = %#v, want %#v", argv, want)
	}
	// argv[0] must be the executable, as Start relies on osexec.Command(exe, argv...).
	if argv[0] != exe {
		t.Fatalf("argv[0] = %q, want the executable %q", argv[0], exe)
	}
	// The exact tail the contract pins, asserted independently of argv[0].
	wantTail := []string{"-NoProfile", "-ExecutionPolicy", "Bypass", "-File", ps1, "--version"}
	if !reflect.DeepEqual(argv[1:], wantTail) {
		t.Fatalf("argv[1:] = %#v, want %#v", argv[1:], wantTail)
	}
	// The launcher body is never inspected: the only path probed is the .ps1 sibling.
	if len(f.probed) != 1 || f.probed[0] != ps1 {
		t.Fatalf("probed %#v, want exactly [%q] (the .cmd body must never be opened)", f.probed, ps1)
	}
}

func TestShimKeepsMultilinePromptInOneArgvElement(t *testing.T) {
	dir := filepath.Join("tmp", "npm")
	cmd := filepath.Join(dir, "cursor-agent.cmd")
	ps1 := filepath.Join(dir, "cursor-agent.ps1")
	const psExe = `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`

	// #16 acceptance: a prompt that cmd.exe %* expansion would shred into
	// several tokens (spaces, quotes, newlines, an explicit %*) must reach the
	// child as exactly one argv element, byte for byte.
	prompt := "line one\nline two\n\n  line three \"quoted\" %* & | > <\nline six  "
	args := []string{"-p", prompt, "--output-format", "stream-json"}

	f := &fakeFS{files: map[string]fs.FileMode{ps1: 0}}
	exe, argv, ok := resolveShimTarget(cmd, args, f.stat, regularPowerShell(psExe))
	if !ok {
		t.Fatalf("resolveShimTarget(%q) = not ok, want ok", cmd)
	}
	if exe != psExe {
		t.Fatalf("exe = %q, want %q", exe, psExe)
	}
	// 6 prefix elements (exe + 5 flags) then the caller's args verbatim.
	if len(argv) != 6+len(args) {
		t.Fatalf("len(argv) = %d, want %d: %#v", len(argv), 6+len(args), argv)
	}
	if argv[6] != "-p" {
		t.Fatalf("argv[6] = %q, want %q", argv[6], "-p")
	}
	if argv[7] != prompt {
		t.Fatalf("prompt was re-tokenised or altered:\n got %q\nwant %q", argv[7], prompt)
	}
	if !strings.Contains(argv[7], "\n") || strings.Count(argv[7], "\n") != strings.Count(prompt, "\n") {
		t.Fatalf("argv[7] lost newlines: %q", argv[7])
	}
	if got, want := argv[8:], []string{"--output-format", "stream-json"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("trailing args = %#v, want %#v", got, want)
	}
}

func TestShimRequiresPS1Sibling(t *testing.T) {
	dir := filepath.Join("tmp", "npm")
	for _, name := range []string{"cursor-agent.cmd", "cursor-agent.bat"} {
		path := filepath.Join(dir, name)
		f := &fakeFS{files: map[string]fs.FileMode{}} // no .ps1 on disk
		exe, argv, ok := resolveShimTarget(path, []string{"-p", "hi"}, f.stat, regularPowerShell("pwsh.exe"))
		if ok {
			t.Fatalf("%s without a .ps1 sibling: ok = true (exe=%q argv=%#v), want false", name, exe, argv)
		}
		if exe != "" || argv != nil {
			t.Fatalf("%s without a .ps1 sibling: exe=%q argv=%#v, want empty values", name, exe, argv)
		}
	}
}

func TestShimIgnoresNonShimExtension(t *testing.T) {
	dir := filepath.Join("tmp", "npm")
	for _, name := range []string{"cursor-agent.exe", "cursor-agent.ps1", "cursor-agent", "cursor-agent.cmdx"} {
		path := filepath.Join(dir, name)
		// Even with a matching sibling script on disk a non-.cmd/.bat path is left alone.
		sibling := filepath.Join(dir, strings.TrimSuffix(name, filepath.Ext(name))+".ps1")
		f := &fakeFS{files: map[string]fs.FileMode{sibling: 0}}
		exe, argv, ok := resolveShimTarget(path, []string{"-p", "hi"}, f.stat, regularPowerShell("pwsh.exe"))
		if ok {
			t.Fatalf("%q: ok = true (exe=%q argv=%#v), want false", path, exe, argv)
		}
		if len(f.probed) != 0 {
			t.Fatalf("%q: probed %#v, want no filesystem access at all", path, f.probed)
		}
	}
}

func TestShimRejectsDirectoryPS1(t *testing.T) {
	dir := filepath.Join("tmp", "npm")
	cmd := filepath.Join(dir, "cursor-agent.cmd")
	ps1 := filepath.Join(dir, "cursor-agent.ps1")

	f := &fakeFS{files: map[string]fs.FileMode{ps1: fs.ModeDir | 0o755}}
	exe, argv, ok := resolveShimTarget(cmd, []string{"-p", "hi"}, f.stat, regularPowerShell("pwsh.exe"))
	if ok {
		t.Fatalf("a .ps1 directory: ok = true (exe=%q argv=%#v), want false", exe, argv)
	}
	if exe != "" || argv != nil {
		t.Fatalf("a .ps1 directory: exe=%q argv=%#v, want empty values", exe, argv)
	}
}

func TestShimRequiresPowerShell(t *testing.T) {
	dir := filepath.Join("tmp", "npm")
	cmd := filepath.Join(dir, "cursor-agent.cmd")
	ps1 := filepath.Join(dir, "cursor-agent.ps1")

	f := &fakeFS{files: map[string]fs.FileMode{ps1: 0}}
	exe, argv, ok := resolveShimTarget(cmd, []string{"-p", "hi"}, f.stat, noPowerShell)
	if ok {
		t.Fatalf("no PowerShell host: ok = true (exe=%q argv=%#v), want false", exe, argv)
	}
	if exe != "" || argv != nil {
		t.Fatalf("no PowerShell host: exe=%q argv=%#v, want empty values", exe, argv)
	}

	// A host that reports success but an empty path is not usable either.
	exe, argv, ok = resolveShimTarget(cmd, []string{"-p", "hi"}, f.stat, func() (string, bool) { return "", true })
	if ok {
		t.Fatalf("empty PowerShell path: ok = true (exe=%q argv=%#v), want false", exe, argv)
	}
}

func TestShimHandlesUppercaseExtension(t *testing.T) {
	dir := filepath.Join("tmp", "npm")
	const psExe = `C:\Program Files\PowerShell\7\pwsh.exe`

	for _, name := range []string{"cursor-agent.CMD", "Cursor-Agent.Bat", "cursor-agent.Cmd"} {
		path := filepath.Join(dir, name)
		// The .ps1 sibling keeps the launcher's canonical lowercase-ish base name.
		base := strings.TrimSuffix(name, filepath.Ext(name))
		ps1 := filepath.Join(dir, base+".ps1")
		f := &fakeFS{files: map[string]fs.FileMode{ps1: 0}}
		exe, argv, ok := resolveShimTarget(path, []string{"-p", "hi"}, f.stat, regularPowerShell(psExe))
		if !ok {
			t.Fatalf("%q: ok = false, want true (candidate %q)", name, ps1)
		}
		if exe != psExe || len(argv) < 6 || argv[4] != "-File" || argv[5] != ps1 {
			t.Fatalf("%q: exe=%q argv=%#v, want -File %q", name, exe, argv, ps1)
		}
		if got := argv[len(argv)-1]; got != "hi" {
			t.Fatalf("%q: last argv element = %q, want %q", name, got, "hi")
		}
		if len(f.probed) != 1 || f.probed[0] != ps1 {
			t.Fatalf("%q: probed %#v, want exactly [%q]", name, f.probed, ps1)
		}
	}
}

func TestShimExtMatchesCmdAndBat(t *testing.T) {
	cases := map[string]bool{
		"cursor-agent.cmd":              true,
		"cursor-agent.CMD":              true,
		"cursor-agent.Cmd":              true,
		"cursor-agent.bat":              true,
		"cursor-agent.BAT":              true,
		"cursor-agent.exe":              false,
		"cursor-agent.ps1":              false,
		"cursor-agent.cm_":              false,
		"cursor-agent.cmdx":             false,
		"cursor-agent":                  false,
		"":                              false,
		filepath.Join("a", "b.cmd.txt"): false,
	}
	for path, want := range cases {
		if got := isShimExt(path); got != want {
			t.Errorf("isShimExt(%q) = %v, want %v", path, got, want)
		}
	}
}

// TestShimResolveInvocationIsIdentityOffWindows pins the non-Windows half of
// the seam: path and args pass through untouched, with argv[0] = base name.
func TestShimResolveInvocationIsIdentityOffWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("non-Windows identity; the Windows branch is covered by shim_windows.go")
	}
	for _, path := range []string{
		filepath.Join("usr", "bin", "cursor-agent"),
		filepath.Join("tmp", "npm", "cursor-agent.cmd"), // a .cmd path is still started as given off Windows
	} {
		args := []string{"-p", "multi\nline prompt", "--json"}
		exe, argv, err := resolveInvocation(path, args)
		if err != nil {
			t.Fatalf("resolveInvocation(%q) error = %v, want nil (identity off Windows)", path, err)
		}
		if exe != path {
			t.Fatalf("exe = %q, want %q (identity)", exe, path)
		}
		want := append([]string{filepath.Base(path)}, args...)
		if !reflect.DeepEqual(argv, want) {
			t.Fatalf("argv = %#v, want %#v", argv, want)
		}
		// argv[0] is the base name, so the caller's args start at index 1.
		if !reflect.DeepEqual(argv[1:], args) {
			t.Fatalf("argv[1:] = %#v, want the caller args untouched %#v", argv[1:], args)
		}
		if argv[2] != args[1] {
			t.Fatalf("argv[2] = %q, want the prompt untouched %q", argv[2], args[1])
		}
	}
}
