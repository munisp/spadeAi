// Package proxmox drives Proxmox VE VMs cloned from an Ubuntu 24.04 template that has the QEMU guest agent
// installed. Proxmox cannot take cloud-init user data over the API (cicustom needs a snippet file on the
// node), so first boot is written and run through the guest agent and must exit 0; only then is the VM
// tagged spade-ready. Unready VMs carry no attempt, so reconciliation removes them.
package proxmox

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/munisp/spadeai/internal/providers/cloud"
)

type Client struct {
	BaseURL           string // https://pve.example:8006/api2/json
	Token             string // API token: user@realm!tokenid=secret
	Node              string
	Template          int // vmid of the template to clone
	FullClone         bool
	Storage           string // target storage for full clones
	Pool              string
	CAFile            string // PEM CA for the node's certificate; system roots otherwise
	InNigeriaDeclared bool
	BootWait          time.Duration // guest agent + first boot budget; default 15m
	Poll              time.Duration // default 2s
	HTTP              *http.Client
}

func (c *Client) Name() string { return "proxmox" }
func (c *Client) TokenEnv() string {
	return "SPADE_PROXMOX_API/SPADE_PROXMOX_TOKEN/SPADE_PROXMOX_NODE/SPADE_PROXMOX_TEMPLATE"
}
func (c *Client) Configured() bool {
	return c != nil && c.BaseURL != "" && c.Token != "" && c.Node != "" && c.Template > 0
}
func (c *Client) InNigeria() bool { return c.InNigeriaDeclared }

const (
	readyTag  = "spade-ready"
	bootPath  = "/root/spade-boot.sh"
	maxScript = 60 << 10 // guest agent file-write limit
)

func svcTag(id string) string { return "spade-svc-" + id }
func attTag(a string) string  { return "spade-att-" + a }

var tagRe = regexp.MustCompile(`^[a-z0-9_][a-z0-9_.+-]*$`)

func (c *Client) do(ctx context.Context, method, path string, body, out any) (int, error) {
	if !c.Configured() {
		return 0, fmt.Errorf("proxmox: %s not configured", c.TokenEnv())
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
		if c.CAFile != "" {
			pem, err := os.ReadFile(c.CAFile)
			if err != nil {
				return 0, fmt.Errorf("proxmox: CA file: %w", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return 0, fmt.Errorf("proxmox: CA file %s has no certificates", c.CAFile)
			}
			hc.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
		}
	}
	a := cloud.API{Provider: "proxmox", TokenEnv: c.TokenEnv(), BaseURL: c.BaseURL, Token: c.Token, HTTP: hc}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	code, err := a.Do(ctx, method, path, map[string]string{"Authorization": "PVEAPIToken=" + c.Token}, body, &env)
	if err != nil {
		return code, err
	}
	if out != nil && len(env.Data) > 0 && string(env.Data) != "null" {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return code, fmt.Errorf("proxmox %s %s: decode: %w", method, path, err)
		}
	}
	return code, nil
}

func gone(code int, err error) bool {
	return err != nil && (code == http.StatusNotFound || (code == http.StatusInternalServerError && strings.Contains(err.Error(), "does not exist")))
}

func tagsOf(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ';' || r == ',' || r == ' ' })
}

func (c *Client) List(ctx context.Context, serviceID string) ([]cloud.Server, error) {
	var vms []struct {
		VMID     int    `json:"vmid"`
		Node     string `json:"node"`
		Name     string `json:"name"`
		Status   string `json:"status"`
		Tags     string `json:"tags"`
		Template int    `json:"template"`
	}
	if _, err := c.do(ctx, http.MethodGet, "/cluster/resources?type=vm", nil, &vms); err != nil {
		return nil, err
	}
	var res []cloud.Server
	for _, v := range vms {
		tags := tagsOf(v.Tags)
		if v.Template == 1 || !slices.Contains(tags, svcTag(serviceID)) {
			continue
		}
		s := cloud.Server{ID: fmt.Sprintf("%s/%d", v.Node, v.VMID), Name: v.Name, Status: v.Status}
		if slices.Contains(tags, readyTag) {
			for _, t := range tags {
				if a, ok := strings.CutPrefix(t, "spade-att-"); ok {
					s.Attempt = a
				}
			}
		}
		res = append(res, s)
	}
	return res, nil
}

var sizeRe = regexp.MustCompile(`(\d+)C-(\d+)G$`)

// parseSize reads cores and memory from a size code ending in <n>C-<m>G.
func parseSize(code string) (cores, memMiB int, err error) {
	m := sizeRe.FindStringSubmatch(code)
	if m == nil {
		return 0, 0, fmt.Errorf("proxmox: size %q must end in <cores>C-<GiB>G", code)
	}
	cores, _ = strconv.Atoi(m[1])
	gib, _ := strconv.Atoi(m[2])
	if cores < 1 || cores > 128 || gib < 1 || gib > 1024 {
		return 0, 0, fmt.Errorf("proxmox: size %q out of range", code)
	}
	return cores, gib * 1024, nil
}

func (c *Client) wait(ctx context.Context, node, upid string) error {
	poll := c.Poll
	if poll == 0 {
		poll = 2 * time.Second
	}
	for {
		var st struct {
			Status     string `json:"status"`
			ExitStatus string `json:"exitstatus"`
		}
		if _, err := c.do(ctx, http.MethodGet, "/nodes/"+url.PathEscape(node)+"/tasks/"+url.PathEscape(upid)+"/status", nil, &st); err != nil {
			return err
		}
		if st.Status == "stopped" {
			if st.ExitStatus != "OK" {
				return fmt.Errorf("proxmox: task %s failed: %s", upid, st.ExitStatus)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

func (c *Client) Create(ctx context.Context, o cloud.CreateOpts) (*cloud.Server, error) {
	if !c.Configured() {
		return nil, fmt.Errorf("proxmox: %s not configured", c.TokenEnv())
	}
	if !tagRe.MatchString(svcTag(o.ServiceID)) || !tagRe.MatchString(attTag(o.Attempt)) {
		return nil, fmt.Errorf("proxmox: service %q / attempt %q are not valid tag values", o.ServiceID, o.Attempt)
	}
	if o.Script == "" || len(o.Script) > maxScript {
		return nil, fmt.Errorf("proxmox: first-boot script must be 1..%d bytes, got %d", maxScript, len(o.Script))
	}
	cores, mem, err := parseSize(o.Size)
	if err != nil {
		return nil, err
	}
	var next json.Number
	if _, err := c.do(ctx, http.MethodGet, "/cluster/nextid", nil, &next); err != nil {
		return nil, err
	}
	vmid, err := strconv.Atoi(next.String())
	if err != nil {
		return nil, fmt.Errorf("proxmox: nextid %q: %w", next, err)
	}
	node := url.PathEscape(c.Node)
	vm := fmt.Sprintf("/nodes/%s/qemu/%d", node, vmid)
	clone := map[string]any{"newid": vmid, "name": cloud.ServerName(o), "full": 0}
	if c.FullClone {
		clone["full"] = 1
		if c.Storage != "" {
			clone["storage"] = c.Storage
		}
	}
	if c.Pool != "" {
		clone["pool"] = c.Pool
	}
	var upid string
	if _, err := c.do(ctx, http.MethodPost, fmt.Sprintf("/nodes/%s/qemu/%d/clone", node, c.Template), clone, &upid); err != nil {
		return nil, err
	}
	fail := func(err error) (*cloud.Server, error) {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
		defer cancel()
		if derr := c.destroy(cctx, c.Node, vmid); derr != nil {
			return nil, fmt.Errorf("%w (cleanup of VM %d also failed: %v)", err, vmid, derr)
		}
		return nil, err
	}
	if err := c.wait(ctx, c.Node, upid); err != nil {
		return fail(err)
	}
	base := strings.Join([]string{svcTag(o.ServiceID), attTag(o.Attempt), cloud.ManagedTag}, ";")
	if _, err := c.do(ctx, http.MethodPut, vm+"/config", map[string]any{"tags": base, "cores": cores, "memory": mem, "agent": "1"}, nil); err != nil {
		return fail(err)
	}
	if _, err := c.do(ctx, http.MethodPost, vm+"/status/start", map[string]any{}, &upid); err != nil {
		return fail(err)
	}
	if err := c.wait(ctx, c.Node, upid); err != nil {
		return fail(err)
	}
	if err := c.boot(ctx, vm, o.Script); err != nil {
		return fail(err)
	}
	if _, err := c.do(ctx, http.MethodPut, vm+"/config", map[string]any{"tags": base + ";" + readyTag}, nil); err != nil {
		return fail(err)
	}
	return &cloud.Server{ID: fmt.Sprintf("%s/%d", c.Node, vmid), Name: cloud.ServerName(o), Status: "running", Attempt: o.Attempt}, nil
}

// boot waits for the guest agent, writes the first-boot script and runs it to completion.
func (c *Client) boot(ctx context.Context, vm, script string) error {
	budget := c.BootWait
	if budget == 0 {
		budget = 15 * time.Minute
	}
	poll := c.Poll
	if poll == 0 {
		poll = 2 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	sleep := func() error {
		select {
		case <-ctx.Done():
			return fmt.Errorf("proxmox: first boot did not finish within %s", budget)
		case <-time.After(poll):
			return nil
		}
	}
	for {
		if _, err := c.do(ctx, http.MethodPost, vm+"/agent/ping", map[string]any{}, nil); err == nil {
			break
		}
		if err := sleep(); err != nil {
			return fmt.Errorf("%w: guest agent never answered (template needs qemu-guest-agent)", err)
		}
	}
	if _, err := c.do(ctx, http.MethodPost, vm+"/agent/file-write", map[string]any{"file": bootPath, "content": script}, nil); err != nil {
		return err
	}
	var ex struct {
		PID int `json:"pid"`
	}
	if _, err := c.do(ctx, http.MethodPost, vm+"/agent/exec", map[string]any{"command": []string{"sh", bootPath}}, &ex); err != nil {
		return err
	}
	for {
		var st struct {
			Exited   int    `json:"exited"`
			ExitCode *int   `json:"exitcode"`
			Err      string `json:"err-data"`
		}
		if _, err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s/agent/exec-status?pid=%d", vm, ex.PID), nil, &st); err != nil {
			return err
		}
		if st.Exited == 1 {
			if st.ExitCode == nil || *st.ExitCode != 0 {
				return fmt.Errorf("proxmox: first boot failed: %s", strings.TrimSpace(st.Err[max(0, len(st.Err)-300):]))
			}
			return nil
		}
		if err := sleep(); err != nil {
			return err
		}
	}
}

// Destroy refuses VMs that SpadeAi did not create.
func (c *Client) Destroy(ctx context.Context, id string) error {
	node, v, ok := strings.Cut(id, "/")
	vmid, err := strconv.Atoi(v)
	if !ok || err != nil || node == "" {
		return fmt.Errorf("proxmox: bad server id %q", id)
	}
	var cfg struct {
		Tags string `json:"tags"`
	}
	code, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/nodes/%s/qemu/%d/config", url.PathEscape(node), vmid), nil, &cfg)
	if gone(code, err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !slices.Contains(tagsOf(cfg.Tags), cloud.ManagedTag) {
		return fmt.Errorf("proxmox: VM %s is not tagged %s; refusing to delete", id, cloud.ManagedTag)
	}
	return c.destroy(ctx, node, vmid)
}

func (c *Client) destroy(ctx context.Context, node string, vmid int) error {
	vm := fmt.Sprintf("/nodes/%s/qemu/%d", url.PathEscape(node), vmid)
	var st struct {
		Status string `json:"status"`
	}
	code, err := c.do(ctx, http.MethodGet, vm+"/status/current", nil, &st)
	if gone(code, err) {
		return nil
	}
	if err != nil {
		return err
	}
	var upid string
	if st.Status != "stopped" {
		if _, err := c.do(ctx, http.MethodPost, vm+"/status/stop", map[string]any{}, &upid); err != nil {
			return err
		}
		if err := c.wait(ctx, node, upid); err != nil {
			return err
		}
	}
	code, err = c.do(ctx, http.MethodDelete, vm+"?purge=1&destroy-unreferenced-disks=1", nil, &upid)
	if gone(code, err) {
		return nil
	}
	if err != nil {
		return err
	}
	return c.wait(ctx, node, upid)
}

// Ping checks the token and that the template exists on the node.
func (c *Client) Ping(ctx context.Context) error {
	var cfg struct {
		Template int `json:"template"`
	}
	if _, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/nodes/%s/qemu/%d/config", url.PathEscape(c.Node), c.Template), nil, &cfg); err != nil {
		return err
	}
	if cfg.Template != 1 {
		return fmt.Errorf("proxmox: VM %d on %s is not a template", c.Template, c.Node)
	}
	return nil
}
