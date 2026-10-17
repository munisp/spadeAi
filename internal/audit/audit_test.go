package audit

import (
	"testing"
	"time"
)

func TestHashIsDeterministicAndChained(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e := Event{Actor: "u", Verb: "org.create", TargetKind: "organization", TargetID: "o1", Detail: map[string]any{"b": 1, "a": 2}}
	h1, err := Hash("0", at, e)
	if err != nil {
		t.Fatal(err)
	}
	h2, _ := Hash("0", at, e)
	if h1 != h2 {
		t.Fatal("hash not deterministic")
	}
	if h3, _ := Hash(h1, at, e); h3 == h1 {
		t.Fatal("predecessor not part of hash")
	}
	e.Detail["a"] = 3
	if h4, _ := Hash("0", at, e); h4 == h1 {
		t.Fatal("tampered detail produced same hash")
	}
}
