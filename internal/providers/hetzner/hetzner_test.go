package hetzner

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/munisp/spadeai/internal/providers/cloud/cloudtest"
)

// fakeAPI implements the slice of the Hetzner Cloud API the driver uses.
func fakeAPI(t *testing.T) *httptest.Server {
	var mu sync.Mutex
	servers := map[int64]map[string]any{}
	next := int64(100)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"error":{"code":"unauthorized","message":"bad token"}}`))
			return
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/locations":
			_, _ = w.Write([]byte(`{"locations":[]}`))
		case r.Method == "GET" && r.URL.Path == "/servers":
			out := []any{}
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
			if !strings.HasPrefix(in["user_data"].(string), "#cloud-config") || in["server_type"] != "hetzner-size" || in["location"] != "fsn1" {
				t.Errorf("bad create body: %v", in)
			}
			next++
			s := map[string]any{"id": next, "name": in["name"], "status": "initializing", "labels": in["labels"],
				"public_net": map[string]any{"ipv4": map[string]any{"ip": "203.0.113.7"}}}
			servers[next] = s
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(map[string]any{"server": s})
		case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/servers/"):
			id, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/servers/"), 10, 64)
			if _, ok := servers[id]; !ok {
				w.WriteHeader(404)
				return
			}
			delete(servers, id)
			_, _ = w.Write([]byte(`{"action":{"id":1}}`))
		default:
			w.WriteHeader(404)
		}
	}))
}

func TestContract(t *testing.T) {
	srv := fakeAPI(t)
	defer srv.Close()
	cloudtest.Contract(t, &Client{BaseURL: srv.URL, Token: "tok"}, &Client{BaseURL: srv.URL, Token: "wrong"}, &Client{BaseURL: srv.URL})
}
