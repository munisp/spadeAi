// Command spade is the SpadeAi CLI. It talks to the public API with an OIDC token.
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type cfg struct {
	API    string `json:"api"`
	Issuer string `json:"issuer"`
	Token  string `json:"token"`
}

func cfgPath() string {
	d, _ := os.UserConfigDir()
	return filepath.Join(d, "spade", "config.json")
}

func load() cfg {
	c := cfg{API: "http://localhost:8080", Issuer: "http://localhost:8180/realms/spadeai"}
	if b, err := os.ReadFile(cfgPath()); err == nil {
		_ = json.Unmarshal(b, &c)
	}
	if v := os.Getenv("SPADE_API"); v != "" {
		c.API = v
	}
	if v := os.Getenv("SPADE_TOKEN"); v != "" {
		c.Token = v
	}
	return c
}

func save(c cfg) error {
	if err := os.MkdirAll(filepath.Dir(cfgPath()), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	return os.WriteFile(cfgPath(), b, 0o600)
}

func call(c cfg, method, path string, body any, idem bool) (map[string]any, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, strings.TrimRight(c.API, "/")+path, rd)
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	if idem {
		k := make([]byte, 16)
		_, _ = rand.Read(k)
		req.Header.Set("Idempotency-Key", hex.EncodeToString(k))
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out map[string]any
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &out)
	if resp.StatusCode >= 300 {
		return out, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return out, nil
}

func login(c cfg, user, pass string) (cfg, error) {
	form := url.Values{"client_id": {"spade-cli"}, "grant_type": {"password"}, "username": {user}, "password": {pass}, "scope": {"openid"}}
	resp, err := http.PostForm(c.Issuer+"/protocol/openid-connect/token", form)
	if err != nil {
		return c, err
	}
	defer resp.Body.Close()
	var t struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error_description"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&t)
	if t.AccessToken == "" {
		return c, errors.New("login failed: " + t.Error)
	}
	c.Token = t.AccessToken
	return c, save(c)
}

func pp(v any) { b, _ := json.MarshalIndent(v, "", "  "); fmt.Println(string(b)) }

const usage = `spade — SpadeAi CLI
  spade login -u USER -p PASS
  spade caps
  spade orgs create NAME [--postpaid] [--ai self_hosted|external|hybrid] [--residency]
  spade orgs list
  spade wallet ORG_ID
  spade projects create ORG_ID NAME EMAIL [--overlay nebula|wireguard]
  spade templates
  spade deploy PROJECT_ID TEMPLATE NAME SIZE
  spade services PROJECT_ID
  spade backups list|create SERVICE_ID
  spade backups restore SERVICE_ID BACKUP_ID   (overwrites current data)
  spade overlay PROJECT_ID          (private network members, IPs, sync state)
  spade action SERVICE_ID start|stop|restart|delete
  spade op OPERATION_ID [--wait]
  spade audit verify`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(a []string) error {
	c := load()
	if len(a) == 0 {
		fmt.Println(usage)
		return nil
	}
	need := func(n int) error {
		if len(a) < n {
			return errors.New("missing arguments\n" + usage)
		}
		return nil
	}
	show := func(v map[string]any, err error) error {
		if err != nil {
			return err
		}
		pp(v)
		return nil
	}
	switch a[0] {
	case "login":
		fs := flag.NewFlagSet("login", flag.ContinueOnError)
		u, p := fs.String("u", "", "user"), fs.String("p", os.Getenv("SPADE_PASSWORD"), "password")
		if err := fs.Parse(a[1:]); err != nil {
			return err
		}
		_, err := login(c, *u, *p)
		if err == nil {
			fmt.Println("logged in")
		}
		return err
	case "caps":
		return show(call(c, "GET", "/v1/capabilities", nil, false))
	case "templates":
		return show(call(c, "GET", "/v1/templates", nil, false))
	case "orgs":
		if err := need(2); err != nil {
			return err
		}
		if a[1] == "list" {
			return show(call(c, "GET", "/v1/orgs", nil, false))
		}
		fs := flag.NewFlagSet("orgs", flag.ContinueOnError)
		post, ai, res := fs.Bool("postpaid", false, ""), fs.String("ai", "self_hosted", ""), fs.Bool("residency", false, "")
		if err := need(3); err != nil {
			return err
		}
		if err := fs.Parse(a[3:]); err != nil {
			return err
		}
		mode := "prepaid"
		if *post {
			mode = "postpaid"
		}
		return show(call(c, "POST", "/v1/orgs", map[string]any{"name": a[2], "billing_mode": mode, "ai_policy": *ai, "residency_locked": *res}, false))
	case "wallet":
		if err := need(2); err != nil {
			return err
		}
		return show(call(c, "GET", "/v1/orgs/"+a[1]+"/wallet", nil, false))
	case "projects":
		if err := need(5); err != nil {
			return err
		}
		fs := flag.NewFlagSet("projects", flag.ContinueOnError)
		ov := fs.String("overlay", "wireguard", "")
		if err := fs.Parse(a[5:]); err != nil {
			return err
		}
		return show(call(c, "POST", "/v1/orgs/"+a[2]+"/projects", map[string]any{"name": a[3], "technical_email": a[4], "overlay": *ov}, false))
	case "deploy":
		if err := need(5); err != nil {
			return err
		}
		return show(call(c, "POST", "/v1/projects/"+a[1]+"/services", map[string]any{"template": a[2], "display_name": a[3], "size": a[4]}, true))
	case "services":
		if err := need(2); err != nil {
			return err
		}
		return show(call(c, "GET", "/v1/projects/"+a[1]+"/services", nil, false))
	case "action":
		if err := need(3); err != nil {
			return err
		}
		return show(call(c, "POST", "/v1/services/"+a[1]+"/actions/"+a[2], nil, true))
	case "op":
		if err := need(2); err != nil {
			return err
		}
		for {
			v, err := call(c, "GET", "/v1/operations/"+a[1], nil, false)
			if err != nil || len(a) < 3 || a[2] != "--wait" || (v["status"] != "pending" && v["status"] != "running") {
				return show(v, err)
			}
			time.Sleep(2 * time.Second)
		}
	case "overlay":
		if err := need(2); err != nil {
			return err
		}
		return show(call(c, "GET", "/v1/projects/"+url.PathEscape(a[1])+"/overlay", nil, false))
	case "backups":
		if err := need(3); err != nil {
			return err
		}
		switch a[1] {
		case "list":
			return show(call(c, "GET", "/v1/services/"+url.PathEscape(a[2])+"/backups", nil, false))
		case "create":
			return show(call(c, "POST", "/v1/services/"+url.PathEscape(a[2])+"/backups", nil, true))
		case "restore":
			if err := need(4); err != nil {
				return err
			}
			return show(call(c, "POST", "/v1/services/"+url.PathEscape(a[2])+"/backups/"+url.PathEscape(a[3])+"/restore", nil, true))
		}
		return errors.New("usage: spade backups list|create SERVICE_ID | restore SERVICE_ID BACKUP_ID")
	case "audit":
		return show(call(c, "GET", "/v1/audit-events/verify", nil, false))
	}
	return errors.New("unknown command\n" + usage)
}
