// Package cli implements oarlock's Phase 1 command line: list, probe, models
// and run. It is deliberately a thin host over the public oarlock API: the
// record stream stays the source of truth, and --json is a stable DTO rather
// than a marshalled internal struct.
//
// The command is testable from the outside: App carries the registry, clock-free
// seams and the interrupt channel, and Run never calls os.Exit.
package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"sort"
	"strings"

	"github.com/zzjcool/oarlock"
)

// Schema is the version tag of every --json document. It is part of the CLI's
// contract: adding a field is compatible, renaming one is not.
const Schema = "oarlock.cli/v1"

// Exit codes. They match the issue's contract and shell conventions: 0 success,
// 1 runtime/record error, 2 usage error, 130 interrupted by the first SIGINT.
const (
	ExitOK        = 0
	ExitError     = 1
	ExitUsage     = 2
	ExitInterrupt = 130
)

const usageText = `oarlock — one command line over registered coding-agent runtimes

usage:
  oarlock list [--json]
  oarlock probe [<runtime>|all] [--json]
  oarlock models [<runtime>|all] [--json]
  oarlock run <runtime> <prompt> [flags]

run flags:
  --cwd DIR            directory the runtime operates in (default: current directory)
  --model NAME         runtime-native model id
  --effort LEVEL       runtime-native reasoning level
  --service-tier TIER  runtime-native service tier
  --resume ID          reopen a native conversation
  --image FILE         attach an image (repeatable)
  --timeout DURATION   turn deadline, 0 disables it (default 10m)
  --json               emit one stable JSON DTO instead of progress and tables
  --record FILE        write a voyage log after the run (never overwrites)

exit codes:
  0  success
  1  runtime, turn, or record failure
  2  usage error
  130 interrupted by SIGINT (the first cancels; the second disposes immediately)
`

// App is the command implementation with its process seams exposed. The zero
// value is usable: nil seams fall back to the process registry, the process
// standard streams and the real SIGINT.
type App struct {
	Stdout io.Writer
	Stderr io.Writer
	// Lookup resolves a registered runtime. nil means oarlock.Require.
	Lookup func(string) (oarlock.Runtime, error)
	// Registered lists the runtime ids. nil means oarlock.Registered.
	Registered func() []string
	// Signals is the interrupt channel. nil means signal.Notify on os.Interrupt.
	Signals <-chan os.Signal
}

// Run is the plain entry point. It builds an App over the given streams.
func Run(args []string, stdout, stderr io.Writer) int {
	return (&App{Stdout: stdout, Stderr: stderr}).Run(args)
}

// Run dispatches one command line. It never calls os.Exit.
func (a *App) Run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(a.stderr(), usageText)
		return ExitUsage
	}
	switch args[0] {
	case "list":
		return a.runList(args[1:])
	case "probe":
		return a.runProbe(args[1:])
	case "models":
		return a.runModels(args[1:])
	case "run":
		return a.runRun(args[1:])
	case "help", "-h", "--help":
		fmt.Fprint(a.stdout(), usageText)
		return ExitOK
	default:
		fmt.Fprintf(a.stderr(), "oarlock: unknown command %q\n\n%s", args[0], usageText)
		return ExitUsage
	}
}

func (a *App) stdout() io.Writer {
	if a.Stdout != nil {
		return a.Stdout
	}
	return os.Stdout
}

func (a *App) stderr() io.Writer {
	if a.Stderr != nil {
		return a.Stderr
	}
	return os.Stderr
}

func (a *App) lookup(id string) (oarlock.Runtime, error) {
	if a.Lookup != nil {
		return a.Lookup(id)
	}
	return oarlock.Require(id)
}

func (a *App) registered() []string {
	if a.Registered != nil {
		return a.Registered()
	}
	return oarlock.Registered()
}

func (a *App) newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet("oarlock "+name, flag.ContinueOnError)
	fs.SetOutput(a.stderr())
	return fs
}

// parseInterspersed parses flags that appear before, between or after the
// positional arguments. The standard flag package stops at the first non-flag;
// a CLI whose documented shape is `run <runtime> <prompt> [flags]` cannot
// accept that, so each positional run is peeled off and parsing continues.
// Everything after a lone "--" is positional.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var tail []string
	for i, arg := range args {
		if arg == "--" {
			tail = args[i+1:]
			args = args[:i]
			break
		}
	}
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			break
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
	return append(positional, tail...), nil
}

// parseCommand parses one command's flags. The bool result is false when the
// caller must return the accompanying exit code immediately (help or a parse
// error).
func (a *App) parseCommand(fs *flag.FlagSet, args []string) ([]string, int, bool) {
	positional, err := parseInterspersed(fs, args)
	if err == nil {
		return positional, 0, true
	}
	if errors.Is(err, flag.ErrHelp) {
		return nil, ExitOK, false
	}
	return nil, ExitUsage, false
}

// writeJSON emits exactly one DTO line. HTML escaping is off so a prompt or a
// path containing <, > or & stays readable.
func (a *App) writeJSON(v any) int {
	enc := json.NewEncoder(a.stdout())
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		fmt.Fprintf(a.stderr(), "oarlock: encoding JSON: %v\n", err)
		return ExitError
	}
	return ExitOK
}

// installationDTO snapshots a probe result. nil stays nil so a missing
// installation is distinguishable from an unavailable one.
func installationDTO(inst oarlock.Installation) *InstallationDTO {
	return &InstallationDTO{
		Available: inst.Available,
		Version:   inst.Version,
		Path:      inst.Path,
		Source:    inst.Source,
	}
}

// selectIDs resolves the <runtime|all> argument for probe and models. An empty
// argument means all. The ids are always sorted, so concurrent results can be
// reordered deterministically.
func (a *App) selectIDs(arg string) []string {
	if arg != "" && arg != "all" {
		return []string{arg}
	}
	ids := append([]string(nil), a.registered()...)
	sort.Strings(ids)
	return ids
}

// parallelMap runs fn for every id concurrently and returns the results in the
// input order. One runtime's failure cannot hide another's result.
func parallelMap[T any](ids []string, fn func(string) T) []T {
	out := make([]T, len(ids))
	done := make(chan struct{}, len(ids))
	for i, id := range ids {
		go func(i int, id string) {
			out[i] = fn(id)
			done <- struct{}{}
		}(i, id)
	}
	for range ids {
		<-done
	}
	return out
}

// capabilitiesDTO is the list view: one bool per optional operation. Session is
// required, so it is always true for a registered runtime.
func capabilitiesDTO(rt oarlock.Runtime) CapabilitiesDTO {
	return CapabilitiesDTO{
		Session:   rt.Session != nil,
		Probe:     rt.Probe != nil,
		Models:    rt.Models != nil,
		Install:   rt.Install != nil,
		Login:     rt.Login != nil,
		Usage:     rt.Usage != nil,
		Upgrade:   rt.Upgrade != nil,
		Inventory: rt.Inventory != nil,
	}
}

// version is the module version recorded in a voyage header. A local build has
// no module version, so it honestly says "dev".
func version() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		if v := info.Main.Version; v != "" && v != "(devel)" {
			return strings.TrimPrefix(v, "v")
		}
	}
	return "dev"
}
