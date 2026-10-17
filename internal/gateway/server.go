package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Server struct {
	Pool    *pgxpool.Pool
	CA      *CA
	DistDir string // directory containing spade-agent-linux-<arch> binaries
}

type agentKey struct{}

func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	r.Get("/agent/v1/ca.pem", func(w http.ResponseWriter, _ *http.Request) { w.Write(s.CA.CertPEM) })
	r.Get("/agent/v1/install.sh", s.install)
	r.Get("/agent/v1/download/{file}", s.download)
	r.Post("/agent/v1/enrol", s.enrol)
	r.Group(func(r chi.Router) {
		r.Use(s.mtls)
		r.Post("/agent/v1/heartbeat", s.heartbeat)
		r.Get("/agent/v1/commands/next", s.next)
		r.Post("/agent/v1/commands/{id}/result", s.result)
	})
	return r
}

func jsonOut(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) enrol(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token  string `json:"token"`
		CSRPEM string `json:"csr_pem"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in); err != nil || in.Token == "" || in.CSRPEM == "" {
		jsonOut(w, 400, map[string]string{"error": "token and csr_pem required"})
		return
	}
	sum := sha256.Sum256([]byte(in.Token))
	tx, err := s.Pool.Begin(r.Context())
	if err != nil {
		jsonOut(w, 503, map[string]string{"error": "db unavailable"})
		return
	}
	defer tx.Rollback(r.Context())
	var agentID string
	err = tx.QueryRow(r.Context(), `SELECT id::text FROM agent WHERE enrol_token_sha256=$1 AND enrol_expires_at > now() FOR UPDATE`, hex.EncodeToString(sum[:])).Scan(&agentID)
	if err != nil {
		jsonOut(w, 401, map[string]string{"error": "invalid or expired enrolment token"})
		return
	}
	certPEM, fp, err := s.CA.SignAgent([]byte(in.CSRPEM), agentID, 30*24*time.Hour)
	if err != nil {
		jsonOut(w, 400, map[string]string{"error": "bad csr: " + err.Error()})
		return
	}
	if _, err := tx.Exec(r.Context(), `UPDATE agent SET enrol_token_sha256=NULL, enrol_expires_at=NULL, cert_fingerprint=$2, last_seen_at=now() WHERE id=$1`, agentID, fp); err != nil {
		jsonOut(w, 503, map[string]string{"error": "db error"})
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		jsonOut(w, 503, map[string]string{"error": "db error"})
		return
	}
	jsonOut(w, 200, map[string]string{"agent_id": agentID, "cert_pem": string(certPEM), "ca_pem": string(s.CA.CertPEM)})
}

// mtls requires a CA-signed client cert whose fingerprint matches the agent's current enrolment (replacement = revocation).
func (s *Server) mtls(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			jsonOut(w, 401, map[string]string{"error": "client certificate required"})
			return
		}
		c := r.TLS.PeerCertificates[0]
		fp := sha256.Sum256(c.Raw)
		var ok bool
		err := s.Pool.QueryRow(r.Context(), `SELECT cert_fingerprint=$2 FROM agent WHERE id::text=$1`, c.Subject.CommonName, hex.EncodeToString(fp[:])).Scan(&ok)
		if err != nil || !ok {
			jsonOut(w, 403, map[string]string{"error": "certificate revoked or unknown agent"})
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), agentKey{}, c.Subject.CommonName)))
	})
}

func agentID(r *http.Request) string { return r.Context().Value(agentKey{}).(string) }

func (s *Server) heartbeat(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Version string `json:"version"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in)
	_, err := s.Pool.Exec(r.Context(), `UPDATE agent SET last_seen_at=now(), agent_version=nullif($2,'') WHERE id::text=$1`, agentID(r), in.Version)
	if err != nil {
		jsonOut(w, 503, map[string]string{"error": "db error"})
		return
	}
	w.WriteHeader(204)
}

// next long-polls (up to 25s) for the oldest queued command and marks it delivered.
func (s *Server) next(w http.ResponseWriter, r *http.Request) {
	deadline := time.Now().Add(25 * time.Second)
	id := agentID(r)
	_, _ = s.Pool.Exec(r.Context(), `UPDATE agent SET last_seen_at=now() WHERE id::text=$1`, id)
	for {
		var cmdID, verb string
		var payload []byte
		err := s.Pool.QueryRow(r.Context(), `UPDATE agent_command SET status='delivered', updated_at=now() WHERE id = (
			SELECT id FROM agent_command WHERE agent_id::text=$1 AND status='queued' AND expires_at > now() ORDER BY created_at LIMIT 1 FOR UPDATE SKIP LOCKED)
			RETURNING id::text, verb, payload`, id).Scan(&cmdID, &verb, &payload)
		if err == nil {
			jsonOut(w, 200, map[string]any{"id": cmdID, "verb": verb, "payload": json.RawMessage(payload)})
			return
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			jsonOut(w, 503, map[string]string{"error": "db error"})
			return
		}
		if time.Now().After(deadline) {
			w.WriteHeader(204)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(time.Second):
		}
	}
}

func (s *Server) result(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Status string          `json:"status"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil || (in.Status != "succeeded" && in.Status != "failed") {
		jsonOut(w, 400, map[string]string{"error": "status must be succeeded or failed"})
		return
	}
	if len(in.Result) == 0 {
		in.Result = json.RawMessage("{}")
	}
	tag, err := s.Pool.Exec(r.Context(), `UPDATE agent_command SET status=$3, result=$4, updated_at=now() WHERE id::text=$1 AND agent_id::text=$2 AND status='delivered'`,
		chi.URLParam(r, "id"), agentID(r), in.Status, []byte(in.Result))
	if err != nil {
		jsonOut(w, 503, map[string]string{"error": "db error"})
		return
	}
	if tag.RowsAffected() == 0 {
		jsonOut(w, 409, map[string]string{"error": "command not in delivered state for this agent"})
		return
	}
	w.WriteHeader(204)
}

func (s *Server) install(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/x-shellscript")
	fmt.Fprint(w, installScript)
}

func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	f := chi.URLParam(r, "file")
	if f != "spade-agent-linux-amd64" && f != "spade-agent-linux-arm64" {
		http.NotFound(w, r)
		return
	}
	p := filepath.Join(s.DistDir, f)
	if _, err := os.Stat(p); err != nil {
		http.Error(w, "agent binary not published on this gateway", 404)
		return
	}
	http.ServeFile(w, r, p)
}

const installScript = `#!/bin/sh
# SpadeAi agent bootstrap. Requires: SPADE_GATEWAY, SPADE_CA_SHA256, SPADE_ENROL_TOKEN. Docker + compose plugin must be present.
set -eu
: "${SPADE_GATEWAY:?}" "${SPADE_CA_SHA256:?}" "${SPADE_ENROL_TOKEN:?}"
command -v docker >/dev/null || { echo "docker is required" >&2; exit 1; }
ARCH=$(uname -m); case "$ARCH" in x86_64) ARCH=amd64;; aarch64) ARCH=arm64;; *) echo "unsupported arch $ARCH" >&2; exit 1;; esac
mkdir -p /opt/spade/bin /etc/spade
curl -fsSk "$SPADE_GATEWAY/agent/v1/ca.pem" -o /etc/spade/ca.pem
GOT=$(openssl x509 -in /etc/spade/ca.pem -outform DER | sha256sum | cut -d' ' -f1)
[ "$GOT" = "$SPADE_CA_SHA256" ] || { echo "CA pin mismatch" >&2; exit 1; }
curl -fsS --cacert /etc/spade/ca.pem "$SPADE_GATEWAY/agent/v1/download/spade-agent-linux-$ARCH" -o /opt/spade/bin/spade-agent
chmod 0755 /opt/spade/bin/spade-agent
/opt/spade/bin/spade-agent enrol --gateway "$SPADE_GATEWAY" --ca /etc/spade/ca.pem --token "$SPADE_ENROL_TOKEN" --state-dir /etc/spade
cat > /etc/systemd/system/spade-agent.service <<UNIT
[Unit]
Description=SpadeAi agent
After=network-online.target docker.service
[Service]
ExecStart=/opt/spade/bin/spade-agent run --gateway $SPADE_GATEWAY --state-dir /etc/spade
Restart=always
[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload && systemctl enable --now spade-agent
`
