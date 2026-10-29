package oarlock

import (
	"errors"
	"testing"
)

func TestRegistry_RegisterAndRequire(t *testing.T) {
	const id = "test-registry-runtime"
	Register(Runtime{ID: id})
	rt, err := Require(id)
	if err != nil {
		t.Fatalf("Require: %v", err)
	}
	if rt.ID != id {
		t.Fatalf("Runtime.ID = %q, want %q", rt.ID, id)
	}
}

func TestRegistry_UnknownRuntimeIsTyped(t *testing.T) {
	_, err := Require("test-registry-absent")
	if !errors.Is(err, ErrRuntimeNotFound) {
		t.Fatalf("err = %v, want ErrRuntimeNotFound", err)
	}
}

func TestRegistry_RegisteredIsSorted(t *testing.T) {
	Register(Runtime{ID: "test-registry-z"})
	Register(Runtime{ID: "test-registry-a"})
	ids := Registered()
	for i := 1; i < len(ids); i++ {
		if ids[i-1] > ids[i] {
			t.Fatalf("Registered() is not sorted: %v", ids)
		}
	}
}
