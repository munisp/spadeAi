// Package capability records honest, probe-based readiness for every dependency.
package capability

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type State struct {
	Component string    `json:"component"`
	Status    string    `json:"status"` // available | degraded | unavailable
	Reason    string    `json:"reason"`
	CheckedAt time.Time `json:"checked_at"`
}

type Probe func(ctx context.Context) (status, reason string)

type Registry struct {
	mu     sync.RWMutex
	probes map[string]Probe
	last   map[string]State
	pool   *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Registry {
	return &Registry{probes: map[string]Probe{}, last: map[string]State{}, pool: pool}
}

func (r *Registry) Register(name string, p Probe) { r.mu.Lock(); r.probes[name] = p; r.mu.Unlock() }

func FromErr(err error) (string, string) {
	if err != nil {
		return "unavailable", err.Error()
	}
	return "available", ""
}

func (r *Registry) RunOnce(ctx context.Context) {
	r.mu.RLock()
	probes := make(map[string]Probe, len(r.probes))
	for k, v := range r.probes {
		probes[k] = v
	}
	r.mu.RUnlock()
	for name, p := range probes {
		cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
		status, reason := p(cctx)
		cancel()
		s := State{Component: name, Status: status, Reason: reason, CheckedAt: time.Now().UTC()}
		r.mu.Lock()
		r.last[name] = s
		r.mu.Unlock()
		if r.pool != nil {
			_, _ = r.pool.Exec(ctx, `INSERT INTO capability_state(component,status,reason,checked_at) VALUES ($1,$2,$3,$4)
				ON CONFLICT (component) DO UPDATE SET status=EXCLUDED.status, reason=EXCLUDED.reason, checked_at=EXCLUDED.checked_at`,
				s.Component, s.Status, s.Reason, s.CheckedAt)
		}
	}
}

func (r *Registry) Loop(ctx context.Context, every time.Duration) {
	r.RunOnce(ctx)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.RunOnce(ctx)
		}
	}
}

func (r *Registry) Snapshot() []State {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]State, 0, len(r.last))
	for _, s := range r.last {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Component < out[j].Component })
	return out
}

func (r *Registry) Status(name string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if s, ok := r.last[name]; ok {
		return s.Status
	}
	return "unavailable"
}
