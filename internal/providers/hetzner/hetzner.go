// Package hetzner is the Hetzner Cloud provider driver. Servers are found by the label
// spade-service=<service id>, so create and delete are idempotent across workflow retries.
package hetzner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	BaseURL string // default https://api.hetzner.cloud/v1
	Token   string
	HTTP    *http.Client
}

type Server struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	IPv4   string `json:"ipv4"`
}

type apiServer struct {
	ID        int64             `json:"id"`
	Name      string            `json:"name"`
	Status    string            `json:"status"`
	Labels    map[string]string `json:"labels"`
	PublicNet struct {
		IPv4 struct {
			IP string `json:"ip"`
		} `json:"ipv4"`
	} `json:"public_net"`
}

func (a apiServer) server() *Server {
	return &Server{ID: a.ID, Name: a.Name, Status: a.Status, IPv4: a.PublicNet.IPv4.IP}
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) (int, error) {
	if c.Token == "" {
		return 0, fmt.Errorf("hetzner: SPADE_HETZNER_TOKEN not configured")
	}
	base := c.BaseURL
	if base == "" {
		base = "https://api.hetzner.cloud/v1"
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, rd)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		var e struct {
			Error struct{ Code, Message string } `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		return resp.StatusCode, fmt.Errorf("hetzner %s %s: %d %s %s", method, path, resp.StatusCode, e.Error.Code, e.Error.Message)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}

func Label(serviceID string) string { return "spade-service=" + serviceID }

func (c *Client) list(ctx context.Context, selector string) ([]apiServer, error) {
	var out struct {
		Servers []apiServer `json:"servers"`
	}
	_, err := c.do(ctx, http.MethodGet, "/servers?label_selector="+url.QueryEscape(selector), nil, &out)
	return out.Servers, err
}

// Find returns the server for this service and provisioning attempt, or nil.
func (c *Client) Find(ctx context.Context, serviceID, attempt string) (*Server, error) {
	servers, err := c.list(ctx, Label(serviceID)+",spade-attempt="+attempt)
	if err != nil {
		return nil, err
	}
	out := struct{ Servers []apiServer }{servers}
	switch len(out.Servers) {
	case 0:
		return nil, nil
	case 1:
		return out.Servers[0].server(), nil
	}
	return nil, fmt.Errorf("hetzner: %d servers carry label %s; refusing to guess", len(out.Servers), Label(serviceID))
}

type CreateOpts struct {
	// Attempt ties the server to the enrolment token baked into its cloud-init, so a retry with a
	// fresh token never reuses a server that would enrol with a stale one.
	ServiceID, Attempt, ServerType, Location, Image, UserData string
}

// Ensure creates the service's server unless one already exists (retries never create a second).
func (c *Client) Ensure(ctx context.Context, o CreateOpts) (*Server, bool, error) {
	if s, err := c.Find(ctx, o.ServiceID, o.Attempt); err != nil || s != nil {
		return s, false, err
	}
	body := map[string]any{
		"name": "spade-" + o.ServiceID[:8] + "-" + o.Attempt[:min(6, len(o.Attempt))], "server_type": o.ServerType, "image": o.Image, "location": o.Location,
		"labels":    map[string]string{"spade-service": o.ServiceID, "spade-attempt": o.Attempt, "spade-managed": "true"},
		"user_data": o.UserData, "start_after_create": true,
	}
	var out struct {
		Server apiServer `json:"server"`
	}
	if _, err := c.do(ctx, http.MethodPost, "/servers", body, &out); err != nil {
		return nil, false, err
	}
	return out.Server.server(), true, nil
}

// Delete removes every server for the service (all attempts); none left is success.
func (c *Client) Delete(ctx context.Context, serviceID string) (bool, error) {
	servers, err := c.list(ctx, Label(serviceID))
	if err != nil {
		return false, err
	}
	deleted := false
	for _, s := range servers {
		code, err := c.do(ctx, http.MethodDelete, "/servers/"+strconv.FormatInt(s.ID, 10), nil, nil)
		if err != nil && code != http.StatusNotFound {
			return deleted, err
		}
		deleted = deleted || err == nil
	}
	return deleted, nil
}

// DeleteOthers removes the service's servers from other provisioning attempts: they carry a stale
// enrolment token, or were created by a POST whose response was lost.
func (c *Client) DeleteOthers(ctx context.Context, serviceID, keepAttempt string) error {
	servers, err := c.list(ctx, Label(serviceID))
	if err != nil {
		return err
	}
	for _, s := range servers {
		if s.Labels["spade-attempt"] == keepAttempt {
			continue
		}
		if code, err := c.do(ctx, http.MethodDelete, "/servers/"+strconv.FormatInt(s.ID, 10), nil, nil); err != nil && code != http.StatusNotFound {
			return err
		}
	}
	return nil
}

// Ping verifies the token against a cheap read-only endpoint.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.do(ctx, http.MethodGet, "/locations", nil, nil)
	return err
}

// Boot is a first-boot recipe: install packages, write files, run Pre steps, then Run.
type Boot struct {
	Packages []string
	Files    map[string]string // path -> content (mode 0700)
	Pre      []string
	Run      string
}

// CloudInit renders Boot as cloud-config. Every value is JSON-quoted, which is valid YAML.
func CloudInit(b Boot) string {
	q := func(v string) string { j, _ := json.Marshal(v); return string(j) }
	var sb strings.Builder
	sb.WriteString("#cloud-config\npackage_update: true\n")
	if len(b.Packages) > 0 {
		sb.WriteString("packages:\n")
		for _, p := range b.Packages {
			sb.WriteString("  - " + q(p) + "\n")
		}
	}
	if len(b.Files) > 0 {
		paths := make([]string, 0, len(b.Files))
		for p := range b.Files {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		sb.WriteString("write_files:\n")
		for _, p := range paths {
			sb.WriteString("  - path: " + q(p) + "\n    permissions: \"0700\"\n    content: " + q(b.Files[p]) + "\n")
		}
	}
	sb.WriteString("runcmd:\n")
	for _, c := range append(append([]string{}, b.Pre...), b.Run) {
		sb.WriteString("  - [ sh, -c, " + q(c) + " ]\n")
	}
	return sb.String()
}
