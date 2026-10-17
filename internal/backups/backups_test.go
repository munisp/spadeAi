package backups

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const svc = "0f0e0d0c-0b0a-4900-8800-112233445566"

func TestUser(t *testing.T) {
	if u, err := User(strings.ToUpper(svc)); err != nil || u != "s-"+svc {
		t.Fatalf("got %q %v", u, err)
	}
	for _, bad := range []string{"", "x", "../" + svc, svc + ":x", "0f0e0d0c-0b0a-4900-8800-11223344556\n"} {
		if _, err := User(bad); err == nil {
			t.Fatalf("%q must be rejected", bad)
		}
	}
}

func TestRepo(t *testing.T) {
	u, _ := User(svc)
	got, err := Repo("rest:https://backups.example:8000/base/", u, "p@ss/w:d", false)
	if err != nil {
		t.Fatal(err)
	}
	want := "rest:https://" + u + ":p%40ss%2Fw%3Ad@backups.example:8000/base/" + u
	if got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
	cases := map[string]string{
		"rest:http://10.0.0.1:8000":       "plain HTTP",
		"rest:https://spade:pw@host:8000": "must not carry credentials",
		"s3:https://host/bucket":          "rest: URL",
		"rest:ftp://host":                 "rest:https://",
		"rest:https://":                   "rest:https://",
	}
	for base, msg := range cases {
		if _, err := Repo(base, u, "pw", false); err == nil || !strings.Contains(err.Error(), msg) {
			t.Fatalf("%s: want %q, got %v", base, msg, err)
		}
	}
	if _, err := Repo("rest:http://10.0.0.1:8000", u, "pw", true); err != nil {
		t.Fatalf("dev plaintext opt-in: %v", err)
	}
}

func TestHtpasswd(t *testing.T) {
	dir := t.TempDir()
	h := &Htpasswd{Path: filepath.Join(dir, "htpasswd")}
	os.WriteFile(h.Path, []byte("admin:$2y$05$keepme\n"), 0o644)
	u1, _ := User(svc)
	u2, _ := User("aaaaaaaa-0b0a-4900-8800-112233445566")
	if ch, err := h.Set(u1, "one"); err != nil || !ch {
		t.Fatalf("first set: %v %v", ch, err)
	}
	if ch, err := h.Set(u1, "one"); err != nil || ch {
		t.Fatalf("same password must not rewrite: %v %v", ch, err)
	}
	if ch, _ := h.Set(u2, "two"); !ch {
		t.Fatal("second user")
	}
	if ch, _ := h.Set(u1, "rotated"); !ch {
		t.Fatal("rotation must rewrite")
	}
	data, _ := os.ReadFile(h.Path)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 || lines[0] != "admin:$2y$05$keepme" {
		t.Fatalf("other entries must be preserved once each: %q", lines)
	}
	hashes := map[string]string{}
	for _, l := range lines {
		n, hsh, _ := strings.Cut(l, ":")
		hashes[n] = hsh
	}
	if bcrypt.CompareHashAndPassword([]byte(hashes[u1]), []byte("rotated")) != nil || bcrypt.CompareHashAndPassword([]byte(hashes[u2]), []byte("two")) != nil {
		t.Fatal("hashes must verify")
	}
	if err := h.Remove(u1); err != nil {
		t.Fatal(err)
	}
	if err := h.Remove(u1); err != nil {
		t.Fatal("removing again is a no-op")
	}
	data, _ = os.ReadFile(h.Path)
	if strings.Contains(string(data), u1) || !strings.Contains(string(data), u2) {
		t.Fatalf("remove: %s", data)
	}
	if _, err := h.Set("admin", "x"); err == nil {
		t.Fatal("only service logins may be managed")
	}
	if _, err := (&Htpasswd{}).Set(u1, "x"); err == nil {
		t.Fatal("unconfigured must fail closed")
	}
}

func TestWaitLogin(t *testing.T) {
	u, _ := User(svc)
	accept := time.Now().Add(1500 * time.Millisecond)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pw, ok := r.BasicAuth()
		if !ok || user != u || pw != "pw" || time.Now().Before(accept) || r.URL.Path != "/"+u+"/config" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	if err := WaitLogin(context.Background(), srv.Client(), "rest:"+srv.URL, u, "pw", 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := WaitLogin(context.Background(), srv.Client(), "rest:"+srv.URL, u, "wrong", 1500*time.Millisecond); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("rejected login must fail, got %v", err)
	}
}

func TestPrunerFailsClosed(t *testing.T) {
	u, _ := User(svc)
	if _, err := (&Pruner{}).Prune(context.Background(), u, "pw", "spade-x", 7); err == nil {
		t.Fatal("unconfigured")
	}
	for _, admin := range []string{"rest:http://admin:pw@10.0.0.5:8001", "rest:https://host:8001"} {
		p := &Pruner{AdminRepo: admin, Restic: "/bin/false"}
		if _, err := p.Prune(context.Background(), u, "pw", "spade-x", 7); err == nil || strings.Contains(err.Error(), "restic forget") {
			t.Fatalf("%s must be refused before running restic, got %v", admin, err)
		}
	}
	p := &Pruner{AdminRepo: "rest:http://admin:pw@127.0.0.1:1", Restic: "/bin/false"}
	if _, err := p.Prune(context.Background(), "admin", "pw", "spade-x", 7); err == nil {
		t.Fatal("non-service user")
	}
	if _, err := p.Prune(context.Background(), u, "pw", "spade-x", 7); err == nil || !strings.Contains(err.Error(), "restic forget") {
		t.Fatalf("restic failure must surface, got %v", err)
	}
}
