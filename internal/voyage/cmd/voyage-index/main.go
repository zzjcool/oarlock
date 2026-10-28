// Command voyage-index rewrites the index.json of a voyage fixture directory.
//
// It runs the size cap and the credential scan on every *.jsonl fixture and
// refuses to write the index when one fails. It is invoked by
// `go generate ./internal/voyage`; it starts no process and uses no shell.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/zzjcool/oarlock/internal/voyage"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

func run(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("voyage-index", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", "", "directory holding the *.jsonl voyage fixtures (required)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *dir == "" || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "voyage-index: usage: voyage-index -dir <fixture directory>")
		return 2
	}
	if err := voyage.WriteIndex(*dir); err != nil {
		fmt.Fprintf(stderr, "voyage-index: %v\n", err)
		return 1
	}
	return 0
}
