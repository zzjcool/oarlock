package pi

import "github.com/zzjcool/oarlock/observe"

// Compile-time proof that a *Session carries observe.Readback's whole surface,
// not just one reader: it lives in its own file so a concurrent edit to
// readback.go cannot conflict with this assertion.
var _ observe.Readback = (*Session)(nil)
