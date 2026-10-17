// Package audit appends hash-chained, immutable audit events.
package audit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

const genesis = "0000000000000000000000000000000000000000000000000000000000000000"

type Event struct {
	Actor      string         `json:"actor"`
	Verb       string         `json:"verb"`
	TargetKind string         `json:"target_kind"`
	TargetID   string         `json:"target_id"`
	Detail     map[string]any `json:"detail"`
}

// Hash computes the chain hash for an event given its predecessor.
func Hash(prev string, occurredAt time.Time, e Event) (string, error) {
	body, err := json.Marshal(e) // map keys are sorted by encoding/json
	if err != nil {
		return "", err
	}
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|", prev, occurredAt.UTC().Format(time.RFC3339Nano))
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Append must run inside the caller's transaction so audit and state change commit atomically.
func Append(ctx context.Context, tx pgx.Tx, e Event) error {
	if e.Detail == nil {
		e.Detail = map[string]any{}
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7426002)`); err != nil {
		return err
	}
	prev := genesis
	err := tx.QueryRow(ctx, `SELECT hash FROM audit_event ORDER BY seq DESC LIMIT 1`).Scan(&prev)
	if err != nil && err != pgx.ErrNoRows {
		return err
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	hash, err := Hash(prev, now, e)
	if err != nil {
		return err
	}
	detail, _ := json.Marshal(e.Detail)
	_, err = tx.Exec(ctx, `INSERT INTO audit_event(occurred_at, actor, verb, target_kind, target_id, detail, prev_hash, hash)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, now, e.Actor, e.Verb, e.TargetKind, e.TargetID, detail, prev, hash)
	return err
}

// Verify walks the full chain and returns the first broken sequence number, or 0 if intact.
func Verify(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}) (int64, error) {
	rows, err := q.Query(ctx, `SELECT seq, occurred_at, actor, verb, target_kind, coalesce(target_id,''), detail, prev_hash, hash FROM audit_event ORDER BY seq`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	prev := genesis
	for rows.Next() {
		var seq int64
		var at time.Time
		var e Event
		var detail []byte
		var prevHash, hash string
		if err := rows.Scan(&seq, &at, &e.Actor, &e.Verb, &e.TargetKind, &e.TargetID, &detail, &prevHash, &hash); err != nil {
			return 0, err
		}
		if err := json.Unmarshal(detail, &e.Detail); err != nil {
			return seq, nil
		}
		want, err := Hash(prev, at, e)
		if err != nil {
			return 0, err
		}
		if prevHash != prev || hash != want {
			return seq, nil
		}
		prev = hash
	}
	return 0, rows.Err()
}
