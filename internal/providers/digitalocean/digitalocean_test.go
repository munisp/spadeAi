package digitalocean

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/munisp/spadeai/internal/providers/cloud/cloudtest"
)

// fakeAPI implements the slice of the DigitalOcean API the driver uses, with one droplet per page to
// exercise pagination.
func fakeAPI(t *testing.T) *httptest.Server {
	var mu sync.Mutex
	droplets := map[int64]map[string]any{}
	next := int64(100)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"id":"unauthorized","message":"Unable to authenticate you"}`))
			return
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/account":
			_, _ = w.Write([]byte(`{"account":{"status":"active"}}`))
		case r.Method == "GET" && r.URL.Path == "/droplets":
			var match []map[string]any
			for id := int64(0); id <= next; id++ {
				if d, ok := droplets[id]; ok && slices.Contains(d["tags"].([]any), any(r.URL.Query().Get("tag_name"))) {
					match = append(match, d)
				}
			}
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			out := map[string]any{"droplets": []any{}, "links": map[string]any{}}
			if page >= 1 && page <= len(match) {
				out["droplets"] = []any{match[page-1]}
				if page < len(match) {
					out["links"] = map[string]any{"pages": map[string]any{"next": "more"}}
				}
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == "POST" && r.URL.Path == "/droplets":
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			if !strings.HasPrefix(in["user_data"].(string), "#cloud-config") || in["region"] != "lon1" || in["image"] != "ubuntu-24-04-x64" {
				t.Errorf("bad create body: %v", in)
			}
			next++
			d := map[string]any{"id": next, "name": in["name"], "status": "new", "tags": in["tags"],
				"networks": map[string]any{"v4": []any{map[string]any{"ip_address": "10.0.0.2", "type": "private"}, map[string]any{"ip_address": "203.0.113.8", "type": "public"}}}}
			droplets[next] = d
			w.WriteHeader(202)
			_ = json.NewEncoder(w).Encode(map[string]any{"droplet": d})
		case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/droplets/"):
			id, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/droplets/"), 10, 64)
			if _, ok := droplets[id]; !ok {
				w.WriteHeader(404)
				return
			}
			delete(droplets, id)
			w.WriteHeader(204)
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

func TestPublicIPChosen(t *testing.T) {
	var d droplet
	_ = json.Unmarshal([]byte(`{"id":1,"networks":{"v4":[{"ip_address":"10.0.0.2","type":"private"},{"ip_address":"203.0.113.8","type":"public"}]}}`), &d)
	if s := d.server("a"); s.IPv4 != "203.0.113.8" {
		t.Fatalf("got %q", s.IPv4)
	}
}
