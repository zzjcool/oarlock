package voyage

// Fixture regeneration. The committed files under testdata/voyage are
// generated, never hand-edited: voyage-gen rebuilds the pi-*.jsonl fixtures
// from the raw Pi wire frames in testdata/pi, and voyage-index rewrites
// testdata/voyage/index.json after running the size cap and credential scan on
// every fixture. Run `go generate ./internal/voyage` and commit the result;
// CI runs it again and fails on any diff.

//go:generate go run ./cmd/voyage-gen -pi ../../testdata/pi -out ../../testdata/voyage
//go:generate go run ./cmd/voyage-index -dir ../../testdata/voyage
