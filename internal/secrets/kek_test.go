package secrets

import (
	"path/filepath"
	"testing"
)

func TestSealOpenBindsAAD(t *testing.T) {
	k, err := LoadOrCreate(filepath.Join(t.TempDir(), "kek"), true)
	if err != nil {
		t.Fatal(err)
	}
	s, err := k.Seal([]byte("secret"), "a")
	if err != nil {
		t.Fatal(err)
	}
	if p, err := k.Open(s, "a"); err != nil || string(p) != "secret" {
		t.Fatalf("open: %q %v", p, err)
	}
	if _, err := k.Open(s, "b"); err == nil {
		t.Fatal("opened with wrong AAD")
	}
}

func TestMissingKEKFailsClosed(t *testing.T) {
	if _, err := LoadOrCreate(filepath.Join(t.TempDir(), "absent"), false); err == nil {
		t.Fatal("expected error for missing KEK")
	}
}
