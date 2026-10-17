package hetzner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeAPI implements the slice of the Hetzner Cloud API the driver uses.
func fakeAPI(t *testing.T) (*httptest.Server, *int) {
	var mu sync.Mutex
	servers := map[int64]map[string]any{}
	creates := 0
	next := int64(100)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"error":{"code":"unauthorized","message":"bad token"}}`))
			return
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/servers":
			var out []any
			for _, s := range servers {
				match := true
				for _, kv := range strings.Split(r.URL.Query().Get("label_selector"), ",") {
					k, v, _ := strings.Cut(kv, "=")
					match = match && s["labels"].(map[string]any)[k] == v
				}
				if match {
					out = append(out, s)
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"servers": out})
		case r.Method == "POST" && r.URL.Path == "/servers":
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			if !strings.HasPrefix(in["user_data"].(string), "#cloud-config") {
				t.Errorf("user_data is not cloud-init: %v", in["user_data"])
			}
			creates++
			next++
			s := map[string]any{"id": next, "name": in["name"], "status": "initializing", "labels": in["labels"],
				"public_net": map[string]any{"ipv4": map[string]any{"ip": "203.0.113.7"}}}
			servers[next] = s
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(map[string]any{"server": s})
		case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/servers/"):
			for id, s := range servers {
				if "/servers/"+jsonNum(id) == r.URL.Path {
					delete(servers, id)
					_ = s
					_, _ = w.Write([]byte(`{"action":{"id":1}}`))
					return
				}
			}
			w.WriteHeader(404)
		default:
			w.WriteHeader(404)
		}
	}))
	return srv, &creates
}

func jsonNum(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestEnsureIsIdempotentAndDeleteRemoves(t *testing.T) {
	srv, creates := fakeAPI(t)
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Token: "tok"}
	ctx := context.Background()
	o := CreateOpts{ServiceID: "0123456789abcdef", Attempt: "a1", ServerType: "cx22", Location: "fsn1", Image: "ubuntu-24.04", UserData: CloudInit("echo hi")}
	s1, created, err := c.Ensure(ctx, o)
	if err != nil || !created || s1.IPv4 != "203.0.113.7" {
		t.Fatalf("first ensure: %+v %v %v", s1, created, err)
	}
	s2, created, err := c.Ensure(ctx, o)
	if err != nil || created || s2.ID != s1.ID || *creates != 1 {
		t.Fatalf("retry must reuse the server: %+v %v %v creates=%d", s2, created, err, *creates)
	}
	o2 := o
	o2.Attempt = "a2"
	if s3, created, err := c.Ensure(ctx, o2); err != nil || !created || s3.ID == s1.ID {
		t.Fatalf("new attempt must get a new server: %+v %v %v", s3, created, err)
	}
	if ok, err := c.Delete(ctx, o.ServiceID); err != nil || !ok {
		t.Fatalf("delete: %v %v", ok, err)
	}
	if ok, err := c.Delete(ctx, o.ServiceID); err != nil || ok {
		t.Fatalf("second delete must be a no-op: %v %v", ok, err)
	}
	if left, _ := c.list(ctx, Label(o.ServiceID)); len(left) != 0 {
		t.Fatalf("delete must remove servers of every attempt, %d left", len(left))
	}
}

func TestFailsClosedWithoutOrWithBadToken(t *testing.T) {
	srv, _ := fakeAPI(t)
	defer srv.Close()
	if err := (&Client{BaseURL: srv.URL}).Ping(context.Background()); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("missing token must fail: %v", err)
	}
	if _, _, err := (&Client{BaseURL: srv.URL, Token: "wrong"}).Ensure(context.Background(), CreateOpts{ServiceID: "0123456789abcdef", Attempt: "a1"}); err == nil || !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("bad token must fail: %v", err)
	}
}

func TestCloudInitQuotesBootstrap(t *testing.T) {
	ci := CloudInit(`curl -fsSL "https://gw" | sudo SPADE_X=1 sh`)
	if !strings.Contains(ci, `"curl -fsSL \"https://gw\" | sudo SPADE_X=1 sh"`) {
		t.Fatalf("bootstrap not JSON-quoted: %s", ci)
	}
}
