// Command voyage-gen rebuilds the committed Pi voyage fixtures
// (testdata/voyage/pi-*.jsonl) from the real Pi wire frames in testdata/pi.
//
// It is a deterministic generator: running it twice yields byte-identical
// files. It uses no shell and no child process.
//
//	go run ./internal/voyage/cmd/voyage-gen -pi testdata/pi -out testdata/voyage
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/zzjcool/oarlock/internal/redact"
	"github.com/zzjcool/oarlock/internal/voyage/pifixture"
)

func main() {
	pi := flag.String("pi", filepath.Join("testdata", "pi"), "directory with <scenario>.frames.jsonl / .requests.jsonl")
	out := flag.String("out", filepath.Join("testdata", "voyage"), "directory the pi-*.jsonl fixtures are written to")
	flag.Parse()
	if err := run(*pi, *out); err != nil {
		fmt.Fprintln(os.Stderr, "voyage-gen:", err)
		os.Exit(1)
	}
}

func run(pi, out string) error {
	red := redact.New(nil)
	for _, scenario := range pifixture.Scenarios() {
		if err := pifixture.Write(pi, out, scenario, red); err != nil {
			return fmt.Errorf("%s: %w", scenario, err)
		}
		fmt.Println("wrote", filepath.ToSlash(filepath.Join(out, pifixture.Name(scenario)+".jsonl")))
	}
	return nil
}
