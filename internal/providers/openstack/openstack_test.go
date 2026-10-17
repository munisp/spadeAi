package openstack

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/munisp/spadeai/internal/providers/cloud/cloudtest"
)

// fakeCloud is Keystone v3 (application credentials) plus the Nova calls the driver uses, one server per page.
func fakeCloud(t *testing.T) *httptest.Server {
	var mu sync.Mutex
	type srv struct {
		id   string
		tags []string
	}
	var all []*srv
	var base string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "POST" && r.URL.Path == "/v3/auth/tokens" {
			var in struct {
				Auth struct {
					Identity struct {
						Methods []string                    `json:"methods"`
						AppCred struct{ ID, Secret string } `json:"application_credential"`
					} `json:"identity"`
				} `json:"auth"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			if in.Auth.Identity.AppCred.ID != "cred" || in.Auth.Identity.AppCred.Secret != "good" {
				w.WriteHeader(401)
				_, _ = w.Write([]byte(`{"error":{"code":401,"message":"bad credential"}}`))
				return
			}
			w.Header().Set("X-Subject-Token", "tok")
			w.WriteHeader(201)
			fmt.Fprintf(w, `{"token":{"expires_at":"2099-01-01T00:00:00.000000Z","catalog":[{"type":"compute","name":"nova","endpoints":[{"id":"1","interface":"public","region":"RegionOne","region_id":"RegionOne","url":"%s/compute/v2.1"}]}]}}`, base)
			return
		}
		if r.Header.Get("X-Auth-Token") != "tok" {
			w.WriteHeader(401)
			return
		}
		if mv := r.Header.Get("X-OpenStack-Nova-API-Version"); mv != "2.52" {
			t.Errorf("microversion %q", mv)
		}
		p := strings.TrimPrefix(r.URL.Path, "/compute/v2.1")
		switch {
		case r.Method == "GET" && p == "/flavors/detail":
			_, _ = w.Write([]byte(`{"flavors":[{"id":"f-other","name":"m1.tiny"},{"id":"f-1","name":"openstack-size"}]}`))
		case r.Method == "GET" && p == "/servers/detail":
			tag := r.URL.Query().Get("tags")
			var match []*srv
			for _, s := range all {
				if slices.Contains(s.tags, tag) {
					match = append(match, s)
				}
			}
			n, _ := strconv.Atoi(r.URL.Query().Get("marker"))
			out := map[string]any{"servers": []any{}}
			if n < len(match) {
				s := match[n]
				out["servers"] = []any{map[string]any{"id": s.id, "name": "x", "status": "ACTIVE", "tags": s.tags,
					"addresses": map[string]any{"net": []any{map[string]any{"addr": "10.0.0.5", "version": 4}, map[string]any{"addr": "198.51.100.4", "version": 4, "OS-EXT-IPS:type": "floating"}}}}}
				if n+1 < len(match) {
					out["servers_links"] = []any{map[string]any{"rel": "next", "href": fmt.Sprintf("%s/compute/v2.1/servers/detail?tags=%s&marker=%d", base, tag, n+1)}}
				}
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == "POST" && p == "/servers":
			var in struct {
				Server struct {
					Flavor   string   `json:"flavorRef"`
					Image    string   `json:"imageRef"`
					UserData string   `json:"user_data"`
					Networks any      `json:"networks"`
					Tags     []string `json:"tags"`
				} `json:"server"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			ud, _ := base64.StdEncoding.DecodeString(in.Server.UserData)
			if in.Server.Flavor != "f-1" || in.Server.Image != "img" || in.Server.Networks != "auto" || !strings.HasPrefix(string(ud), "#cloud-config") {
				t.Errorf("bad create: %+v", in.Server)
			}
			s := &srv{id: fmt.Sprintf("s-%d", len(all)+1), tags: in.Server.Tags}
			all = append(all, s)
			w.WriteHeader(202)
			fmt.Fprintf(w, `{"server":{"id":%q}}`, s.id)
		case r.Method == "DELETE" && strings.HasPrefix(p, "/servers/"):
			id := strings.TrimPrefix(p, "/servers/")
			for i, s := range all {
				if s.id == id {
					all = slices.Delete(all, i, i+1)
					w.WriteHeader(204)
					return
				}
			}
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"itemNotFound":{"code":404}}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			w.WriteHeader(404)
		}
	})
	s := httptest.NewServer(h)
	base = s.URL
	return s
}

func TestContract(t *testing.T) {
	srv := fakeCloud(t)
	defer srv.Close()
	mk := func(secret string) *Client {
		return &Client{AuthURL: srv.URL + "/v3/", AppCredID: "cred", Secret: secret, Region: "RegionOne", Image: "img"}
	}
	cloudtest.Contract(t, mk("good"), mk("bad"), &Client{AuthURL: srv.URL + "/v3/", Image: "img"})
}

func TestUnknownFlavorFailsClosed(t *testing.T) {
	srv := fakeCloud(t)
	defer srv.Close()
	c := &Client{AuthURL: srv.URL + "/v3/", AppCredID: "cred", Secret: "good", Region: "RegionOne", Image: "img"}
	sc, err := c.compute(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := flavor(t.Context(), sc, "m9.huge"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unknown flavor must fail: %v", err)
	}
}
