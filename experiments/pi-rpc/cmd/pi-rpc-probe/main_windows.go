//go:build windows

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "the issue #5 Pi RPC probe is POSIX-only; use adapters/pi on Windows")
	os.Exit(2)
}
