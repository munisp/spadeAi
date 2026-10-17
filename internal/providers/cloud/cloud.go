// Package cloud is the common adapter for API cloud providers. A driver only lists, creates and destroys
// servers tagged for a service; idempotency, reconciliation and first-boot rendering live here so every
// provider gets the same guarantees.
package cloud

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

type Server struct {
	ID      string
	Name    string
	Status  string
	IPv4    string
	Attempt string // the enrolment attempt the server was created for
}

type CreateOpts struct {
	ServiceID, Attempt, Size string
	UserData                 string // cloud-config, for providers that take user data at create
	Script                   string // the same first boot as a shell script, for providers that run it in the guest
}

type Driver interface {
	Name() string
	TokenEnv() string
	Configured() bool
	// InNigeria reports whether servers are created in Nigeria; residency-locked orgs need it.
	InNigeria() bool
	Ping(ctx context.Context) error
	// List returns only servers tagged for serviceID, across all attempts.
	List(ctx context.Context, serviceID string) ([]Server, error)
	Create(ctx context.Context, o CreateOpts) (*Server, error)
	// Destroy returns nil when the server is already gone.
	Destroy(ctx context.Context, id string) error
}

// ServerName is a provider-safe hostname: letters, digits and dashes, under 32 characters.
func ServerName(o CreateOpts) string {
	return "spade-" + o.ServiceID[:min(8, len(o.ServiceID))] + "-" + o.Attempt[:min(6, len(o.Attempt))]
}

// Tag keys shared by tag-based providers.
func ServiceTag(serviceID string) string { return "spade-svc:" + serviceID }
func AttemptTag(attempt string) string   { return "spade-att:" + attempt }

const ManagedTag = "spade-managed"

// FromTags returns the attempt if tags carry the service tag, or ok=false.
func FromTags(tags []string, serviceID string) (attempt string, ok bool) {
	for _, t := range tags {
		if t == ServiceTag(serviceID) {
			ok = true
		}
		if a, found := strings.CutPrefix(t, "spade-att:"); found {
			attempt = a
		}
	}
	return attempt, ok
}

// Ensure returns the server for this attempt, creating it only if none exists.
func Ensure(ctx context.Context, d Driver, o CreateOpts) (*Server, bool, error) {
	servers, err := d.List(ctx, o.ServiceID)
	if err != nil {
		return nil, false, err
	}
	var mine []Server
	for _, s := range servers {
		if s.Attempt == o.Attempt {
			mine = append(mine, s)
		}
	}
	switch len(mine) {
	case 0:
	case 1:
		return &mine[0], false, nil
	default:
		return nil, false, fmt.Errorf("%s: %d servers for service %s attempt %s; refusing to guess", d.Name(), len(mine), o.ServiceID, o.Attempt)
	}
	s, err := d.Create(ctx, o)
	if err != nil {
		return nil, false, err
	}
	return s, true, nil
}

// DeleteOthers removes the service's servers from other attempts: they carry a stale enrolment token,
// or were created by a request whose response was lost.
func DeleteOthers(ctx context.Context, d Driver, serviceID, keepAttempt string) error {
	servers, err := d.List(ctx, serviceID)
	if err != nil {
		return err
	}
	for _, s := range servers {
		if s.Attempt == keepAttempt {
			continue
		}
		if err := d.Destroy(ctx, s.ID); err != nil {
			return err
		}
	}
	return nil
}

// DeleteAll removes every server of the service; deleted reports whether any existed.
func DeleteAll(ctx context.Context, d Driver, serviceID string) (bool, error) {
	servers, err := d.List(ctx, serviceID)
	if err != nil {
		return false, err
	}
	for _, s := range servers {
		if err := d.Destroy(ctx, s.ID); err != nil {
			return true, err
		}
	}
	return len(servers) > 0, nil
}

// API is the shared bearer-token JSON client.
type API struct {
	Provider string // used in error messages
	TokenEnv string
	BaseURL  string
	Token    string
	HTTP     *http.Client
}

// Do sends a request; on a non-2xx status it returns the status and an error with a bounded body excerpt.
func (a *API) Do(ctx context.Context, method, path string, hdr map[string]string, body, out any) (int, error) {
	if a.Token == "" {
		return 0, fmt.Errorf("%s: %s not configured", a.Provider, a.TokenEnv)
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(a.BaseURL, "/")+path, rd)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+a.Token)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	hc := a.HTTP
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
		return resp.StatusCode, fmt.Errorf("%s %s %s: %d %s", a.Provider, method, path, resp.StatusCode, strings.TrimSpace(string(data[:min(300, len(data))])))
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, fmt.Errorf("%s %s %s: decode: %w", a.Provider, method, path, err)
		}
	}
	return resp.StatusCode, nil
}

// Boot is what first boot does on a stock Ubuntu 24.04 image.
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

// Script renders Boot as a POSIX shell script for providers without user data (Proxmox guest agent).
// It stops at the first failing step; every value is single-quoted.
func Script(b Boot) string {
	var sb strings.Builder
	sb.WriteString("#!/bin/sh\nset -eu\nexport DEBIAN_FRONTEND=noninteractive\n")
	if len(b.Packages) > 0 {
		sb.WriteString("apt-get update -q\napt-get install -yq")
		for _, p := range b.Packages {
			sb.WriteString(" " + ShellQuote(p))
		}
		sb.WriteString("\n")
	}
	paths := make([]string, 0, len(b.Files))
	for p := range b.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		sb.WriteString("mkdir -p \"$(dirname " + ShellQuote(p) + ")\"\n")
		sb.WriteString("printf '%s' " + ShellQuote(base64.StdEncoding.EncodeToString([]byte(b.Files[p]))) + " | base64 -d > " + ShellQuote(p) + "\n")
		sb.WriteString("chmod 0700 " + ShellQuote(p) + "\n")
	}
	for _, c := range append(append([]string{}, b.Pre...), b.Run) {
		sb.WriteString("sh -c " + ShellQuote(c) + "\n")
	}
	return sb.String()
}

// ShellQuote single-quotes v for POSIX sh.
func ShellQuote(v string) string { return "'" + strings.ReplaceAll(v, "'", `'"'"'`) + "'" }
