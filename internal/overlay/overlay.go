// Package overlay controls per-project private networks (WireGuard or Nebula) between managed hosts.
// Hosts generate their own keys; the control plane only stores public keys and, for Nebula, signs host
// certificates with a per-project CA whose key is sealed with the platform KEK.
package overlay

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/munisp/spadeai/internal/secrets"
)

const (
	WireGuardPort = 51820
	NebulaPort    = 4242
)

// Iface is the host interface name for a project's overlay (<= 15 chars, Linux IFNAMSIZ).
func Iface(kind, projectID string) string {
	if kind == "nebula" {
		return "spn-" + projectID[:8]
	}
	return "spw-" + projectID[:8]
}

func Port(kind string) int {
	if kind == "nebula" {
		return NebulaPort
	}
	return WireGuardPort
}

type Peer struct {
	ServiceID, IP, PublicKey, Endpoint, HostCert string
}

// Payloads builds the overlay_apply payload for every peer: a full mesh for WireGuard; for Nebula the
// lowest-addressed peer is the lighthouse and every other peer learns it via static_host_map.
func Payloads(kind, projectID, cidr string, generation int64, caCrt string, peers []Peer) (map[string]map[string]any, error) {
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil {
		return nil, fmt.Errorf("overlay cidr: %w", err)
	}
	sorted := append([]Peer(nil), peers...)
	sort.Slice(sorted, func(i, j int) bool {
		a, _ := netip.ParseAddr(sorted[i].IP)
		b, _ := netip.ParseAddr(sorted[j].IP)
		return a.Less(b)
	})
	out := map[string]map[string]any{}
	for _, self := range sorted {
		p := map[string]any{
			"project":     "spade-" + self.ServiceID[:8],
			"kind":        kind,
			"iface":       Iface(kind, projectID),
			"address":     fmt.Sprintf("%s/%d", self.IP, prefix.Bits()),
			"listen_port": Port(kind),
			"generation":  generation,
		}
		switch kind {
		case "wireguard":
			var others []map[string]any
			for _, o := range sorted {
				if o.ServiceID != self.ServiceID {
					others = append(others, map[string]any{"public_key": o.PublicKey, "endpoint": o.Endpoint, "allowed_ips": o.IP + "/32"})
				}
			}
			p["peers"] = others
		case "nebula":
			if caCrt == "" || self.HostCert == "" {
				return nil, fmt.Errorf("nebula peer %s has no signed host certificate", self.ServiceID)
			}
			lh := sorted[0]
			p["ca"], p["cert"] = caCrt, self.HostCert
			p["am_lighthouse"] = lh.ServiceID == self.ServiceID
			p["lighthouses"] = []string{}
			p["static_host_map"] = map[string][]string{lh.IP: {lh.Endpoint}}
			if lh.ServiceID != self.ServiceID {
				p["lighthouses"] = []string{lh.IP}
			}
		default:
			return nil, fmt.Errorf("unknown overlay kind %q", kind)
		}
		out[self.ServiceID] = p
	}
	return out, nil
}

// Controller persists overlay membership in Postgres.
type Controller struct {
	Pool       *pgxpool.Pool
	KEK        *secrets.KEK // required for Nebula (CA key sealing)
	NebulaCert string       // path to nebula-cert; empty disables Nebula
}

var ErrNebulaUnavailable = errors.New("nebula controller unavailable: nebula-cert binary or KEK not configured")

// NebulaReady reports whether Nebula certificates can be issued.
func (c *Controller) NebulaReady() error {
	if c.KEK == nil || c.NebulaCert == "" {
		return ErrNebulaUnavailable
	}
	if _, err := exec.LookPath(c.NebulaCert); err != nil {
		return fmt.Errorf("%w: %v", ErrNebulaUnavailable, err)
	}
	return nil
}

// Register records a host's overlay public key and endpoint, allocating a stable overlay IP on first join
// and (Nebula) signing a host certificate. Returns the project ID.
func (c *Controller) Register(ctx context.Context, serviceID, publicKey, endpoint string) (string, error) {
	if _, err := netip.ParseAddrPort(endpoint); err != nil {
		return "", fmt.Errorf("invalid overlay endpoint %q: %w", endpoint, err)
	}
	tx, err := c.Pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	var projectID, kind, cidr string
	if err := tx.QueryRow(ctx, `SELECT p.id::text, p.overlay_kind, p.overlay_cidr::text FROM service s JOIN project p ON p.id=s.project_id
		WHERE s.id=$1 FOR UPDATE OF p`, serviceID).Scan(&projectID, &kind, &cidr); err != nil {
		return "", err
	}
	if kind == "nebula" {
		if err := c.NebulaReady(); err != nil {
			return "", err
		}
	}
	var ip string
	err = tx.QueryRow(ctx, `SELECT host(overlay_ip) FROM overlay_peer WHERE service_id=$1`, serviceID).Scan(&ip)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `SELECT host(network($1::inet) + n) FROM generate_series(2, (2^(32-masklen($1::inet)))::bigint - 2) n
			WHERE NOT EXISTS (SELECT 1 FROM overlay_peer o WHERE o.project_id=$2 AND o.overlay_ip = (host(network($1::inet) + n))::inet)
			ORDER BY n LIMIT 1`, cidr, projectID).Scan(&ip)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("overlay %s for project %s is full", cidr, projectID)
		}
	}
	if err != nil {
		return "", err
	}
	var hostCert *string
	if kind == "nebula" {
		crt, err := c.signNebula(ctx, tx, projectID, serviceID, ip, cidr, publicKey)
		if err != nil {
			return "", err
		}
		hostCert = &crt
	}
	if _, err := tx.Exec(ctx, `INSERT INTO overlay_peer(service_id,project_id,kind,overlay_ip,public_key,endpoint,host_cert) VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (service_id) DO UPDATE SET public_key=EXCLUDED.public_key, endpoint=EXCLUDED.endpoint, host_cert=EXCLUDED.host_cert, updated_at=now()`,
		serviceID, projectID, kind, ip, publicKey, endpoint, hostCert); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `UPDATE service SET overlay_ip=$2 WHERE id=$1`, serviceID, ip); err != nil {
		return "", err
	}
	return projectID, tx.Commit(ctx)
}

// Plan bumps the project's overlay generation and returns the overlay_apply payload per member service.
func (c *Controller) Plan(ctx context.Context, projectID string) (map[string]map[string]any, error) {
	tx, err := c.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var kind, cidr string
	var gen int64
	if err := tx.QueryRow(ctx, `UPDATE project SET overlay_generation=overlay_generation+1 WHERE id=$1 RETURNING overlay_kind, overlay_cidr::text, overlay_generation`,
		projectID).Scan(&kind, &cidr, &gen); err != nil {
		return nil, err
	}
	var ca string
	if kind == "nebula" {
		_ = tx.QueryRow(ctx, `SELECT ca_crt FROM overlay_ca WHERE project_id=$1`, projectID).Scan(&ca)
	}
	rows, err := tx.Query(ctx, `SELECT service_id::text, host(overlay_ip), public_key, endpoint, coalesce(host_cert,'') FROM overlay_peer WHERE project_id=$1`, projectID)
	if err != nil {
		return nil, err
	}
	var peers []Peer
	for rows.Next() {
		var p Peer
		if err := rows.Scan(&p.ServiceID, &p.IP, &p.PublicKey, &p.Endpoint, &p.HostCert); err != nil {
			rows.Close()
			return nil, err
		}
		peers = append(peers, p)
	}
	rows.Close()
	out, err := Payloads(kind, projectID, cidr, gen, ca, peers)
	if err != nil {
		return nil, err
	}
	return out, tx.Commit(ctx)
}

// Remove drops a service from its project's overlay. Returns the project ID ("" if it was not a member).
func (c *Controller) Remove(ctx context.Context, serviceID string) (string, error) {
	var projectID string
	err := c.Pool.QueryRow(ctx, `DELETE FROM overlay_peer WHERE service_id=$1 RETURNING project_id::text`, serviceID).Scan(&projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err == nil {
		_, err = c.Pool.Exec(ctx, `UPDATE service SET overlay_ip=NULL WHERE id=$1`, serviceID)
	}
	return projectID, err
}

func (c *Controller) signNebula(ctx context.Context, tx pgx.Tx, projectID, serviceID, ip, cidr, pub string) (string, error) {
	dir, err := os.MkdirTemp("", "spade-nebula-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	caCrt, caKey := filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key")
	aad := "overlay_ca:" + projectID
	var crt string
	var sealed []byte
	err = tx.QueryRow(ctx, `SELECT ca_crt, ca_key_sealed FROM overlay_ca WHERE project_id=$1`, projectID).Scan(&crt, &sealed)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if out, err := exec.CommandContext(ctx, c.NebulaCert, "ca", "-name", "spade-"+projectID[:8], "-duration", "87600h",
			"-out-crt", caCrt, "-out-key", caKey).CombinedOutput(); err != nil {
			return "", fmt.Errorf("nebula-cert ca: %v: %s", err, out)
		}
		crtB, _ := os.ReadFile(caCrt)
		keyB, _ := os.ReadFile(caKey)
		if sealed, err = c.KEK.Seal(keyB, aad); err != nil {
			return "", err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO overlay_ca(project_id,ca_crt,ca_key_sealed) VALUES ($1,$2,$3)`, projectID, string(crtB), sealed); err != nil {
			return "", err
		}
	case err != nil:
		return "", err
	default:
		key, err := c.KEK.Open(sealed, aad)
		if err != nil {
			return "", fmt.Errorf("unseal nebula CA: %w", err)
		}
		if err := os.WriteFile(caCrt, []byte(crt), 0o600); err != nil {
			return "", err
		}
		if err := os.WriteFile(caKey, key, 0o600); err != nil {
			return "", err
		}
	}
	prefix, _ := netip.ParsePrefix(cidr)
	pubF, hostCrt := filepath.Join(dir, "host.pub"), filepath.Join(dir, "host.crt")
	if err := os.WriteFile(pubF, []byte(pub), 0o600); err != nil {
		return "", err
	}
	if out, err := exec.CommandContext(ctx, c.NebulaCert, "sign", "-ca-crt", caCrt, "-ca-key", caKey, "-in-pub", pubF,
		"-name", "svc-"+serviceID[:8], "-ip", fmt.Sprintf("%s/%d", ip, prefix.Bits()), "-out-crt", hostCrt).CombinedOutput(); err != nil {
		return "", fmt.Errorf("nebula-cert sign: %v: %s", err, out)
	}
	b, err := os.ReadFile(hostCrt)
	return string(b), err
}
