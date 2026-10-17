// Package backups manages per-service logins on the tenant restic REST server and runs retention from
// the control plane. Tenant hosts only ever get an append-only, private-repo login for their own
// repository; deleting snapshots needs the admin endpoint, which never leaves the control plane.
package backups

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/bcrypt"
)

var userRe = regexp.MustCompile(`^s-[0-9a-f-]{36}$`)

// User is the service's login on the tenant server; with --private-repos it can only reach /<user>.
func User(serviceID string) (string, error) {
	u := "s-" + strings.ToLower(serviceID)
	if !userRe.MatchString(u) {
		return "", fmt.Errorf("invalid service id for backup login: %q", serviceID)
	}
	return u, nil
}

// Repo turns a credential-free rest: base URL into the repository URL for one login.
// Plain HTTP is refused unless allowPlaintext, because the URL carries the password.
func Repo(base, user, password string, allowPlaintext bool) (string, error) {
	raw, ok := strings.CutPrefix(base, "rest:")
	if !ok {
		return "", errors.New("backup repository must be a rest: URL")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", errors.New("backup repository must be rest:https://host[:port][/path]")
	}
	if u.User != nil {
		return "", errors.New("backup repository URL must not carry credentials; per-service logins are injected")
	}
	if u.Scheme == "http" && !allowPlaintext {
		return "", errors.New("backup repository would send credentials over plain HTTP; use rest:https://")
	}
	u.User = url.UserPassword(user, password)
	u.Path = path.Join("/", u.Path, user)
	return "rest:" + u.String(), nil
}

// Htpasswd maintains the tenant server's htpasswd file (bcrypt). rest-server reloads it when it changes,
// so mount its directory (not the file) into the server: updates are atomic renames.
type Htpasswd struct {
	Path string
	mu   sync.Mutex
}

// Set ensures user has password; it reports whether the file changed.
func (h *Htpasswd) Set(user, password string) (bool, error) {
	if !userRe.MatchString(user) {
		return false, fmt.Errorf("invalid backup login %q", user)
	}
	return h.update(user, func(old []byte) ([]byte, error) {
		if old != nil && bcrypt.CompareHashAndPassword(old, []byte(password)) == nil {
			return old, nil
		}
		return bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	})
}

// Remove deletes user's login; removing a missing user is a no-op.
func (h *Htpasswd) Remove(user string) error {
	if !userRe.MatchString(user) {
		return fmt.Errorf("invalid backup login %q", user)
	}
	_, err := h.update(user, func([]byte) ([]byte, error) { return nil, nil })
	return err
}

func (h *Htpasswd) update(user string, next func(old []byte) ([]byte, error)) (bool, error) {
	if h == nil || h.Path == "" {
		return false, errors.New("backups unavailable: SPADE_BACKUP_HTPASSWD not configured")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	lf, err := os.OpenFile(h.Path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return false, err
	}
	defer lf.Close()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		return false, err
	}
	data, err := os.ReadFile(h.Path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	var out bytes.Buffer
	var old []byte
	sc := bufio.NewScanner(bytes.NewReader(data))
	var others []string
	for sc.Scan() {
		line := sc.Text()
		if name, hash, ok := strings.Cut(line, ":"); ok && name == user {
			old = []byte(hash)
			continue
		}
		if strings.TrimSpace(line) != "" {
			others = append(others, line)
		}
	}
	hash, err := next(old)
	if err != nil {
		return false, err
	}
	if bytes.Equal(hash, old) {
		return false, nil
	}
	for _, l := range others {
		out.WriteString(l + "\n")
	}
	if hash != nil {
		out.WriteString(user + ":" + string(hash) + "\n")
	}
	tmp, err := os.CreateTemp(filepath.Dir(h.Path), ".htpasswd-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(out.Bytes()); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	return true, os.Rename(tmp.Name(), h.Path)
}

// WaitLogin polls the tenant server until it accepts user's login (rest-server reloads htpasswd
// periodically), so the host's first restic call does not race the reload.
func WaitLogin(ctx context.Context, hc *http.Client, base, user, password string, timeout time.Duration) error {
	repo, err := Repo(base, user, password, true)
	if err != nil {
		return err
	}
	u, _ := url.Parse(strings.TrimPrefix(repo, "rest:"))
	pw, _ := u.User.Password()
	u.User = nil
	u.Path += "/config"
	deadline := time.Now().Add(timeout)
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodHead, u.String(), nil)
		req.SetBasicAuth(user, pw)
		resp, err := hc.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNotFound {
				return nil
			}
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("backup login %s not usable: %w", user, err)
			}
			return fmt.Errorf("backup login %s not accepted by the repository server (HTTP %d)", user, resp.StatusCode)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// Pruner applies retention through the admin endpoint (not append-only), which only the control
// plane can reach. It returns the snapshot ids still present afterwards.
type Pruner struct {
	AdminRepo string // rest:http(s)://admin:pw@host:port, no path
	Restic    string
}

func (p *Pruner) Configured() bool { return p != nil && p.AdminRepo != "" && p.Restic != "" }

func (p *Pruner) Prune(ctx context.Context, user, repoPassword, tag string, keepDaily int) ([]string, error) {
	if !p.Configured() {
		return nil, errors.New("backup retention unavailable: SPADE_BACKUP_ADMIN_REPO or restic not configured")
	}
	if !userRe.MatchString(user) || keepDaily < 1 {
		return nil, fmt.Errorf("invalid prune request for %q", user)
	}
	repo, err := adminRepo(p.AdminRepo, "SPADE_BACKUP_ADMIN_REPO", user)
	if err != nil {
		return nil, err
	}
	run := resticRunner(ctx, p.Restic, "RESTIC_REPOSITORY="+repo, "RESTIC_PASSWORD="+repoPassword)
	if _, err := run("forget", "--tag", tag, "--keep-daily", fmt.Sprint(keepDaily), "--prune"); err != nil {
		return nil, err
	}
	out, err := run("snapshots", "--json", "--tag", tag)
	if err != nil {
		return nil, err
	}
	return snapshotIDs(out)
}

func snapshotIDs(out []byte) ([]string, error) {
	var snaps []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(out, &snaps); err != nil {
		return nil, fmt.Errorf("restic snapshots: %w", err)
	}
	ids := make([]string, 0, len(snaps))
	for _, s := range snaps {
		ids = append(ids, s.ID)
	}
	return ids, nil
}

// adminRepo turns an admin endpoint (rest:http(s)://user:pw@host[:port], from env var name) into the
// repository URL of one service login. Plain HTTP is refused unless the endpoint is on loopback.
func adminRepo(raw, name, user string) (string, error) {
	u, err := url.Parse(strings.TrimPrefix(raw, "rest:"))
	if err != nil || !strings.HasPrefix(raw, "rest:") || u.User == nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("%s must be rest:http(s)://user:password@host[:port]", name)
	}
	if u.Scheme == "http" && !loopback(u.Hostname()) {
		return "", fmt.Errorf("%s over plain HTTP is only allowed on loopback", name)
	}
	u.Path = path.Join("/", u.Path, user)
	return "rest:" + u.String(), nil
}

func resticRunner(ctx context.Context, bin string, env ...string) func(args ...string) ([]byte, error) {
	full := append(os.Environ(), env...)
	return func(args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Env = full
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("restic %s: %v: %s", args[0], err, strings.TrimSpace(stderr.String()))
		}
		return out, nil
	}
}

// Replicator copies each service's repository from the primary site's admin endpoint to an off-site
// (e.g. Abuja) restic REST server. The copy target runs --append-only, so nothing that reached it can be
// deleted through the platform; its retention is an operator decision.
type Replicator struct {
	From   string // SPADE_BACKUP_ADMIN_REPO
	To     string // SPADE_BACKUP_REPLICA_REPO
	Restic string
}

func (r *Replicator) Configured() bool {
	return r != nil && r.From != "" && r.To != "" && r.Restic != ""
}

// Copy brings the off-site repository up to date with every snapshot carrying tag and returns the
// snapshot IDs now present off-site. The off-site repository uses the same password and chunker
// parameters, so copies deduplicate.
func (r *Replicator) Copy(ctx context.Context, user, repoPassword, tag string) ([]string, error) {
	if !r.Configured() {
		return nil, errors.New("backup replication unavailable: SPADE_BACKUP_REPLICA_REPO not configured")
	}
	if !userRe.MatchString(user) {
		return nil, fmt.Errorf("invalid backup login %q", user)
	}
	from, err := adminRepo(r.From, "SPADE_BACKUP_ADMIN_REPO", user)
	if err != nil {
		return nil, err
	}
	to, err := adminRepo(r.To, "SPADE_BACKUP_REPLICA_REPO", user)
	if err != nil {
		return nil, err
	}
	run := resticRunner(ctx, r.Restic, "RESTIC_REPOSITORY="+to, "RESTIC_PASSWORD="+repoPassword,
		"RESTIC_FROM_REPOSITORY="+from, "RESTIC_FROM_PASSWORD="+repoPassword)
	if _, err := run("cat", "config"); err != nil {
		if _, ierr := run("init", "--copy-chunker-params"); ierr != nil {
			return nil, fmt.Errorf("off-site repository: %v (after %v)", ierr, err)
		}
	}
	if _, err := run("copy", "--tag", tag); err != nil {
		return nil, err
	}
	out, err := run("snapshots", "--json", "--tag", tag)
	if err != nil {
		return nil, err
	}
	return snapshotIDs(out)
}

// ProbeAdmin checks that an admin endpoint (name is its env var) accepts its credentials.
func ProbeAdmin(ctx context.Context, hc *http.Client, raw, name string) error {
	if _, err := adminRepo(raw, name, "s-00000000-0000-0000-0000-000000000000"); err != nil {
		return err
	}
	u, _ := url.Parse(strings.TrimPrefix(raw, "rest:"))
	pw, _ := u.User.Password()
	user := u.User.Username()
	u.User = nil
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(user, pw)
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("%s unreachable: %w", name, err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("%s credentials rejected (HTTP %d)", name, resp.StatusCode)
	}
	return nil
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
