// Command spade-hadrill is the load generator for the Lagos/Abuja failover drill (scripts/e2e-ha.sh).
// It writes to Postgres and TigerBeetle continuously while the script kills nodes, counting only writes
// the cluster acknowledged, then verifies that every acknowledged write survived the failover.
//
//	spade-hadrill run -for 60s -out drill.json   # write until the deadline, record acknowledged writes
//	spade-hadrill verify -in drill.json          # every acknowledged row and transfer is present
//
// With -temporal host:port, run also starts a workflow whose durable timer spans the outage and
// requires it to complete afterwards.
package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	tb "github.com/tigerbeetle/tigerbeetle-go"

	"github.com/munisp/spadeai/internal/config"
)

type result struct {
	Debit, Credit string        `json:"-"`
	DebitID       [16]byte      `json:"debit_id"`
	CreditID      [16]byte      `json:"credit_id"`
	PGAcked       []int64       `json:"pg_acked"`
	PGErrors      int           `json:"pg_errors"`
	TBAcked       uint64        `json:"tb_acked"`
	TBErrors      int           `json:"tb_errors"`
	MaxPGGap      time.Duration `json:"max_pg_gap_ns"`
	MaxTBGap      time.Duration `json:"max_tb_gap_ns"`
	LastPGErr     string        `json:"last_pg_error,omitempty"`
	LastTBErr     string        `json:"last_tb_error,omitempty"`
	WorkflowID    string        `json:"workflow_id,omitempty"`
	WorkflowOut   string        `json:"workflow_result,omitempty"`
	WorkflowErr   string        `json:"workflow_error,omitempty"`
}

func randID() tb.Uint128 {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		log.Fatal(err)
	}
	b[15] &= 0x7f
	return tb.BytesToUint128(b)
}

func main() {
	if len(os.Args) < 2 {
		log.Fatal("usage: spade-hadrill run|verify [flags]")
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	dur := fs.Duration("for", 60*time.Second, "how long to write")
	out := fs.String("out", "drill.json", "result file (run)")
	in := fs.String("in", "drill.json", "result file (verify)")
	tHost := fs.String("temporal", "", "Temporal frontend host:port; empty skips the workflow check")
	_ = fs.Parse(os.Args[2:])
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, config.DatabaseURL())
	if err != nil {
		log.Fatal(err)
	}
	client, err := tb.NewClient(tb.ToUint128(0), config.TBAddresses())
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()
	switch os.Args[1] {
	case "run":
		r, err := run(ctx, pool, client, *dur, *tHost)
		if err != nil {
			log.Fatal(err)
		}
		b, _ := json.Marshal(r)
		if err := os.WriteFile(*out, b, 0o600); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("acknowledged: postgres=%d tigerbeetle=%d; errors while failing over: postgres=%d tigerbeetle=%d; longest write gap: postgres=%s tigerbeetle=%s\n",
			len(r.PGAcked), r.TBAcked, r.PGErrors, r.TBErrors, r.MaxPGGap.Round(time.Millisecond), r.MaxTBGap.Round(time.Millisecond))
		if r.WorkflowID != "" {
			fmt.Printf("temporal workflow %s: result=%q error=%q\n", r.WorkflowID, r.WorkflowOut, r.WorkflowErr)
		}
	case "verify":
		b, err := os.ReadFile(*in)
		if err != nil {
			log.Fatal(err)
		}
		var r result
		if err := json.Unmarshal(b, &r); err != nil {
			log.Fatal(err)
		}
		if err := verify(ctx, pool, client, &r); err != nil {
			log.Fatal("FAIL ", err)
		}
		fmt.Printf("no acknowledged write lost: %d postgres rows, %d tigerbeetle transfers\n", len(r.PGAcked), r.TBAcked)
		if r.WorkflowID != "" {
			fmt.Printf("temporal workflow %s completed across the failover\n", r.WorkflowID)
		}
	default:
		log.Fatal("usage: spade-hadrill run|verify [flags]")
	}
}

func run(ctx context.Context, pool *pgxpool.Pool, client tb.Client, dur time.Duration, temporalHost string) (*result, error) {
	if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS ha_drill (id bigint PRIMARY KEY, at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return nil, err
	}
	dr, cr := randID(), randID()
	res, err := client.CreateAccounts([]tb.Account{{ID: dr, Ledger: 999, Code: 1}, {ID: cr, Ledger: 999, Code: 1}})
	if err != nil {
		return nil, err
	}
	for _, x := range res {
		if x.Status != tb.AccountCreated {
			return nil, fmt.Errorf("create drill account: %s", x.Status)
		}
	}
	r := &result{DebitID: dr.Bytes(), CreditID: cr.Bytes()}
	var awaitWorkflow func(time.Duration) (string, error)
	if temporalHost != "" {
		id, await, err := temporalDrill(ctx, temporalHost, dur/2)
		if err != nil {
			return nil, fmt.Errorf("start temporal drill workflow: %w", err)
		}
		r.WorkflowID, awaitWorkflow = id, await
	}
	var seq [8]byte
	if _, err := rand.Read(seq[:]); err != nil {
		return nil, err
	}
	next := int64(binary.BigEndian.Uint64(seq[:]) >> 2)
	deadline := time.Now().Add(dur)
	lastPG, lastTB := time.Now(), time.Now()
	pgDone, tbDone := make(chan struct{}), make(chan struct{})
	go func() { // Postgres writer
		defer close(pgDone)
		for time.Now().Before(deadline) {
			id := next
			next++
			wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			_, err := pool.Exec(wctx, `INSERT INTO ha_drill (id) VALUES ($1)`, id)
			cancel()
			if err != nil {
				r.PGErrors++
				r.LastPGErr = err.Error()
				time.Sleep(200 * time.Millisecond)
				continue
			}
			r.PGAcked = append(r.PGAcked, id)
			if g := time.Since(lastPG); g > r.MaxPGGap {
				r.MaxPGGap = g
			}
			lastPG = time.Now()
			time.Sleep(50 * time.Millisecond)
		}
	}()
	go func() { // TigerBeetle writer: the client retries internally until a quorum commits the batch.
		defer close(tbDone)
		for time.Now().Before(deadline) {
			res, err := client.CreateTransfers([]tb.Transfer{{ID: randID(), DebitAccountID: dr, CreditAccountID: cr, Amount: tb.ToUint128(1), Ledger: 999, Code: 1}})
			switch {
			case err != nil:
				r.TBErrors++
				r.LastTBErr = err.Error()
				time.Sleep(200 * time.Millisecond)
				continue
			case len(res) != 1 || res[0].Status != tb.TransferCreated:
				r.TBErrors++
				r.LastTBErr = fmt.Sprint(res)
				continue
			}
			r.TBAcked++
			if g := time.Since(lastTB); g > r.MaxTBGap {
				r.MaxTBGap = g
			}
			lastTB = time.Now()
			time.Sleep(50 * time.Millisecond)
		}
	}()
	<-pgDone
	<-tbDone
	if awaitWorkflow != nil {
		out, err := awaitWorkflow(2 * time.Minute)
		r.WorkflowOut = out
		if err != nil {
			r.WorkflowErr = err.Error()
		}
	}
	if len(r.PGAcked) == 0 || r.TBAcked == 0 {
		return r, errors.New("no writes acknowledged at all: cluster never available")
	}
	return r, nil
}

func verify(ctx context.Context, pool *pgxpool.Pool, client tb.Client, r *result) error {
	if r.WorkflowID != "" && (r.WorkflowErr != "" || r.WorkflowOut != "survived") {
		return fmt.Errorf("temporal workflow %s did not complete across the failover: %s", r.WorkflowID, r.WorkflowErr)
	}
	var missing int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM unnest($1::bigint[]) a(id) WHERE NOT EXISTS (SELECT 1 FROM ha_drill d WHERE d.id = a.id)`, r.PGAcked).Scan(&missing); err != nil {
		return err
	}
	if missing > 0 {
		return fmt.Errorf("%d of %d acknowledged postgres rows lost", missing, len(r.PGAcked))
	}
	accts, err := client.LookupAccounts([]tb.Uint128{tb.BytesToUint128(r.CreditID)})
	if err != nil {
		return err
	}
	if len(accts) != 1 {
		return errors.New("drill credit account lost")
	}
	got := accts[0].CreditsPosted.BigInt()
	if !got.IsUint64() || got.Uint64() < r.TBAcked {
		return fmt.Errorf("tigerbeetle credits %s < %d acknowledged transfers", got, r.TBAcked)
	}
	return nil
}
