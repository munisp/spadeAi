package linode

import (
	"encoding/base64"
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

// fakeAPI implements the slice of the Linode v4 API the driver uses, one instance per page.
func fakeAPI(t *testing.T) *httptest.Server {
	var mu sync.Mutex
	instances := map[int64]map[string]any{}
	next := int64(100)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"errors":[{"reason":"Invalid Token"}]}`))
			return
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/profile":
			_, _ = w.Write([]byte(`{"username":"x"}`))
		case r.Method == "GET" && r.URL.Path == "/linode/instances":
			var f map[string]string
			if err := json.Unmarshal([]byte(r.Header.Get("X-Filter")), &f); err != nil {
				t.Errorf("X-Filter: %v", err)
			}
			var match []map[string]any
			for id := int64(0); id <= next; id++ {
				if i, ok := instances[id]; ok && slices.Contains(i["tags"].([]any), any(f["tags"])) {
					match = append(match, i)
				}
			}
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			out := map[string]any{"data": []any{}, "page": page, "pages": max(1, len(match))}
			if page >= 1 && page <= len(match) {
				out["data"] = []any{match[page-1]}
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == "POST" && r.URL.Path == "/linode/instances":
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			ud, err := base64.StdEncoding.DecodeString(in["metadata"].(map[string]any)["user_data"].(string))
			if err != nil || !strings.HasPrefix(string(ud), "#cloud-config") || in["region"] != "gb-lon" || len(in["root_pass"].(string)) < 32 {
				t.Errorf("bad create body: %v", in)
			}
			next++
			i := map[string]any{"id": next, "label": in["label"], "status": "provisioning", "ipv4": []string{"203.0.113.9"}, "tags": in["tags"]}
			instances[next] = i
			_ = json.NewEncoder(w).Encode(i)
		case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/linode/instances/"):
			id, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/linode/instances/"), 10, 64)
			if _, ok := instances[id]; !ok {
				w.WriteHeader(404)
				return
			}
			delete(instances, id)
			_, _ = w.Write([]byte(`{}`))
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
