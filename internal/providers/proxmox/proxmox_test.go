package proxmox

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/munisp/spadeai/internal/providers/cloud"
	"github.com/munisp/spadeai/internal/providers/cloud/cloudtest"
)

type vm struct {
	tags, status, name, script string
	template                   int
	cores, mem                 int
}

type fake struct {
	mu        sync.Mutex
	vms       map[int]*vm
	next      int
	agentDown bool
	bootExit  int
}

func newFake() *fake {
	return &fake{vms: map[int]*vm{9000: {template: 1, status: "stopped", name: "ubuntu"}}, next: 100}
}

func (f *fake) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		reply := func(v any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": v}) }
		if r.Header.Get("Authorization") != "PVEAPIToken=root@pam!spade=good" {
			w.WriteHeader(401)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		p := strings.TrimPrefix(r.URL.Path, "/api2/json")
		if p == "/cluster/nextid" {
			reply(strconv.Itoa(f.next))
			f.next++
			return
		}
		if p == "/cluster/resources" {
			var out []map[string]any
			for id, v := range f.vms {
				out = append(out, map[string]any{"vmid": id, "node": "pve", "name": v.name, "status": v.status, "tags": v.tags, "template": v.template})
			}
			reply(out)
			return
		}
		if strings.HasPrefix(p, "/nodes/pve/tasks/") {
			reply(map[string]any{"status": "stopped", "exitstatus": "OK"})
			return
		}
		rest, ok := strings.CutPrefix(p, "/nodes/pve/qemu/")
		if !ok {
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			w.WriteHeader(404)
			return
		}
		idStr, op, _ := strings.Cut(rest, "/")
		id, _ := strconv.Atoi(idStr)
		v := f.vms[id]
		if v == nil {
			w.WriteHeader(500)
			fmt.Fprintf(w, `{"data":null,"message":"Configuration file 'nodes/pve/qemu-server/%d.conf' does not exist"}`, id)
			return
		}
		switch r.Method + " " + op {
		case "POST clone":
			n := int(body["newid"].(float64))
			if v.template != 1 || f.vms[n] != nil {
				w.WriteHeader(500)
				return
			}
			f.vms[n] = &vm{status: "stopped", name: body["name"].(string)}
			reply("UPID:clone")
		case "GET config":
			reply(map[string]any{"tags": v.tags, "template": v.template})
		case "PUT config":
			if s, ok := body["tags"].(string); ok {
				v.tags = s
			}
			if c, ok := body["cores"].(float64); ok {
				v.cores, v.mem = int(c), int(body["memory"].(float64))
			}
		case "POST status/start":
			v.status = "running"
			reply("UPID:start")
		case "POST status/stop":
			v.status = "stopped"
			reply("UPID:stop")
		case "GET status/current":
			reply(map[string]any{"status": v.status})
		case "POST agent/ping":
			if f.agentDown || v.status != "running" {
				w.WriteHeader(500)
				return
			}
			reply(map[string]any{})
		case "POST agent/file-write":
			v.script, _ = body["content"].(string)
		case "POST agent/exec":
			if fmt.Sprint(body["command"]) != "[sh /root/spade-boot.sh]" || !strings.HasPrefix(v.script, "#!/bin/sh") {
				t.Errorf("bad exec %v / script %q", body["command"], v.script)
			}
			reply(map[string]any{"pid": 42})
		case "GET agent/exec-status":
			reply(map[string]any{"exited": 1, "exitcode": f.bootExit, "err-data": "apt failed"})
		case "DELETE ":
			if v.status != "stopped" {
				w.WriteHeader(500)
				return
			}
			delete(f.vms, id)
			reply("UPID:destroy")
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			w.WriteHeader(404)
		}
	})
}

func client(url, token string) *Client {
	return &Client{BaseURL: url + "/api2/json", Token: token, Node: "pve", Template: 9000, Poll: time.Millisecond, BootWait: 200 * time.Millisecond}
}

func TestContract(t *testing.T) {
	f := newFake()
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	cloudtest.ContractSize(t, "proxmox:MEDIUM-2C-4G", client(srv.URL, "root@pam!spade=good"), client(srv.URL, "root@pam!spade=bad"), client(srv.URL, ""))
	for id, v := range f.vms {
		if id != 9000 && (v.cores != 2 || v.mem != 4096) {
			t.Fatalf("VM %d sized %d/%d", id, v.cores, v.mem)
		}
	}
}

func TestFailedBootDestroysVM(t *testing.T) {
	for name, set := range map[string]func(*fake){"agent down": func(f *fake) { f.agentDown = true }, "boot exits 1": func(f *fake) { f.bootExit = 1 }} {
		t.Run(name, func(t *testing.T) {
			f := newFake()
			set(f)
			srv := httptest.NewServer(f.handler(t))
			defer srv.Close()
			c := client(srv.URL, "root@pam!spade=good")
			o := cloud.CreateOpts{ServiceID: "svc", Attempt: "a1", Size: "2C-4G", Script: cloud.Script(cloud.Boot{Run: "true"})}
			if _, _, err := cloud.Ensure(t.Context(), c, o); err == nil {
				t.Fatal("create must fail")
			}
			if len(f.vms) != 1 {
				t.Fatalf("failed VM must be destroyed, %d VMs left", len(f.vms))
			}
		})
	}
}

func TestRefusals(t *testing.T) {
	f := newFake()
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	c := client(srv.URL, "root@pam!spade=good")
	if err := c.Destroy(t.Context(), "pve/9000"); err == nil || f.vms[9000] == nil {
		t.Fatalf("an unmanaged VM must not be deleted: %v", err)
	}
	if err := c.Destroy(t.Context(), "pve/555"); err != nil {
		t.Fatalf("a missing VM is already gone: %v", err)
	}
	for _, o := range []cloud.CreateOpts{
		{ServiceID: "svc", Attempt: "a1", Size: "huge", Script: "#!/bin/sh"},
		{ServiceID: "svc", Attempt: "a1", Size: "2C-4G", Script: strings.Repeat("x", maxScript+1)},
		{ServiceID: "Bad;Tag", Attempt: "a1", Size: "2C-4G", Script: "#!/bin/sh"},
	} {
		if _, err := c.Create(t.Context(), o); err == nil {
			t.Fatalf("must refuse %+v", o.ServiceID+" "+o.Size)
		}
	}
	if len(f.vms) != 1 {
		t.Fatal("refused creates must not clone")
	}
}
