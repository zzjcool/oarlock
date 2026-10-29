package oarlock

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

// ErrRuntimeNotFound is returned by Require when no imported adapter
// registered the id. Importing an adapter package registers its Runtime.
var ErrRuntimeNotFound = errors.New("oarlock: runtime not registered")

var registry = struct {
	sync.RWMutex
	m map[string]Runtime
}{m: map[string]Runtime{}}

// Register adds one runtime to the process registry. It is called from an
// adapter package's init, so a host enables a runtime by importing it:
//
//	import _ "github.com/zzjcool/oarlock/adapters/pi"
//
// Registering the same id twice replaces the earlier value, which keeps tests
// and hosts that import overlapping adapter sets deterministic.
func Register(rt Runtime) {
	if rt.ID == "" {
		panic("oarlock: Register(Runtime) with an empty ID")
	}
	registry.Lock()
	registry.m[rt.ID] = rt
	registry.Unlock()
}

// Require returns a registered runtime by id. The id is only known when the
// adapter package was imported (directly or for its side effect).
func Require(id string) (Runtime, error) {
	registry.RLock()
	rt, ok := registry.m[id]
	registry.RUnlock()
	if !ok {
		return Runtime{}, fmt.Errorf("%w: %q (import its adapter package)", ErrRuntimeNotFound, id)
	}
	return rt, nil
}

// Registered lists the runtime ids this process has registered, sorted.
func Registered() []string {
	registry.RLock()
	ids := make([]string, 0, len(registry.m))
	for id := range registry.m {
		ids = append(ids, id)
	}
	registry.RUnlock()
	sort.Strings(ids)
	return ids
}
