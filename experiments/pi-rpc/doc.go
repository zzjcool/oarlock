// Package pirpc is the issue #5 Pi RPC spike.
//
// The live client and probe use POSIX process-group calls, so on Windows this
// package is an empty shell; the production transport (#9) is portable and
// lives in adapters/pi.
package pirpc
