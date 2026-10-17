package vultr

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/munisp/spadeai/internal/providers/cloud/cloudtest"
)

// fakeAPI implements the slice of the Vultr v2 API the driver uses, one instance per cursor page.
func fakeAPI(t *testing.T) *httptest.Server {
	var mu sync.Mutex
	var order []string
	instances := map[string]map[string]any{}
	n := 0
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"error":"Invalid API token.","status":401}`))
			return
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/account":
			_, _ = w.Write([]byte(`{"account":{}}`))
		case r.Method == "GET" && r.URL.Path == "/instances":
			var match []map[string]any
			for _, id := range order {
				if i, ok := instances[id]; ok && slices.Contains(i["tags"].([]any), any(r.URL.Query().Get("tag"))) {
					match = append(match, i)
				}
			}
			idx := 0
			fmt.Sscan(r.URL.Query().Get("cursor"), &idx)
			out := map[string]any{"instances": []any{}, "meta": map[string]any{"links": map[string]any{"next": ""}}}
			if idx < len(match) {
				out["instances"] = []any{match[idx]}
				if idx+1 < len(match) {
					out["meta"] = map[string]any{"links": map[string]any{"next": fmt.Sprint(idx + 1)}}
				}
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == "POST" && r.URL.Path == "/instances":
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			ud, err := base64.StdEncoding.DecodeString(in["user_data"].(string))
			if err != nil || !strings.HasPrefix(string(ud), "#cloud-config") || in["region"] != "jnb" || in["os_id"] != float64(2284) {
				t.Errorf("bad create body: %v", in)
			}
			n++
			id := fmt.Sprintf("inst-%d", n)
			i := map[string]any{"id": id, "label": in["label"], "status": "pending", "main_ip": "0.0.0.0", "tags": in["tags"]}
			instances[id] = i
			order = append(order, id)
			w.WriteHeader(202)
			_ = json.NewEncoder(w).Encode(map[string]any{"instance": i})
		case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/instances/"):
			id := strings.TrimPrefix(r.URL.Path, "/instances/")
			if _, ok := instances[id]; !ok {
				w.WriteHeader(404)
				return
			}
			delete(instances, id)
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
