package capability

import "testing"

func TestUnknownCapabilityIsUnavailable(t *testing.T) {
	r := New(nil)
	if got := r.Status("provider.unknown"); got != "unavailable" {
		t.Fatalf("got %q, want unavailable", got)
	}
}
