package workflows

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"go.temporal.io/sdk/client"

	"github.com/munisp/spadeai/internal/ids"
	"github.com/munisp/spadeai/internal/ledger"
)

// VATBasisPoints is Nigeria's VAT rate (7.5%).
const VATBasisPoints = 750

// Rater turns meter readings into TigerBeetle transfers, settles arrears and triggers suspend/resume.
type Rater struct {
	A        *Activities
	Temporal client.Client
	Log      *slog.Logger
}

func (r *Rater) Loop(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := r.Once(ctx); err != nil {
			r.Log.Error("rating pass failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Price converts milli-units × milli-kobo-per-unit into kobo, rounding half up.
func Price(quantityMilli, priceMilliKobo int64) int64 {
	return (quantityMilli*priceMilliKobo + 500_000) / 1_000_000
}

func VAT(net int64) int64 { return (net*VATBasisPoints + 5_000) / 10_000 }

func (r *Rater) Once(ctx context.Context) error {
	pool := r.A.Pool
	revenue, vat, _, _, err := r.A.Ledger.PlatformAccounts(ctx, "NGN")
	if err != nil {
		return err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `
		SELECT m.service_id::text, m.sku, m.period_start, m.quantity_milli, p.org_id::text, coalesce(pi.price_minor, -1)
		FROM meter_reading m JOIN service s ON s.id=m.service_id JOIN project p ON p.id=s.project_id
		LEFT JOIN LATERAL (SELECT i.price_minor FROM price_book b JOIN price_item i ON i.price_book_version=b.version AND i.sku=m.sku AND i.currency='NGN'
		   WHERE b.status IN ('effective','retired') AND b.effective_from <= m.period_start ORDER BY b.effective_from DESC LIMIT 1) pi ON true
		WHERE m.rated_at IS NULL ORDER BY m.created_at LIMIT 2000 FOR UPDATE OF m SKIP LOCKED`)
	if err != nil {
		return err
	}
	type reading struct {
		svc, sku, org string
		at            time.Time
		q, price      int64
	}
	var rs []reading
	for rows.Next() {
		var x reading
		if err := rows.Scan(&x.svc, &x.sku, &x.at, &x.q, &x.org, &x.price); err != nil {
			rows.Close()
			return err
		}
		rs = append(rs, x)
	}
	rows.Close()
	for _, x := range rs {
		if x.price < 0 {
			r.Log.Warn("unpriced sku left unrated", "sku", x.sku)
			continue
		}
		key := x.svc + "|" + x.sku + "|" + x.at.UTC().Format(time.RFC3339)
		net := Price(x.q, x.price)
		wallet := ledger.WalletID(x.org, "NGN")
		if err := ledger.Enqueue(ctx, tx,
			ledger.Intent{ID: ids.U128("usage", key), Kind: "usage", OrgID: x.org, Debit: wallet, Credit: revenue, Amount: net, Currency: "NGN", Reference: "usage:" + key},
			ledger.Intent{ID: ids.U128("vat", key), Kind: "vat", OrgID: x.org, Debit: wallet, Credit: vat, Amount: VAT(net), Currency: "NGN", Reference: "vat:" + key},
		); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE meter_reading SET rated_at=now() WHERE service_id=$1 AND sku=$2 AND period_start=$3`, x.svc, x.sku, x.at); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	res, err := r.A.Ledger.Relay(ctx, 8000)
	if err != nil {
		return err
	}
	for _, org := range res.Unpaid {
		r.start(ctx, "suspend-"+org, SuspendOrg, org)
	}
	if err := r.settleArrears(ctx); err != nil {
		return err
	}
	return nil
}

// settleArrears re-charges unpaid intents oldest first once the wallet can cover them.
// TigerBeetle permanently reserves IDs of transfers that failed with exceeds_credits, so each
// settlement attempt uses a new deterministic ID derived from (original id, attempt).
func (r *Rater) settleArrears(ctx context.Context) error {
	pool := r.A.Pool
	rows, err := pool.Query(ctx, `SELECT DISTINCT org_id::text FROM transfer_intent WHERE status='unpaid'`)
	if err != nil {
		return err
	}
	var orgs []string
	for rows.Next() {
		var o string
		_ = rows.Scan(&o)
		orgs = append(orgs, o)
	}
	rows.Close()
	for _, org := range orgs {
		bal, err := r.A.Ledger.WalletBalance(org, "NGN")
		if err != nil {
			return err
		}
		avail := bal.Available
		tx, err := pool.Begin(ctx)
		if err != nil {
			return err
		}
		q, err := tx.Query(ctx, `SELECT tb_transfer_id::text, debit_account::text, credit_account::text, amount_minor, reference,
			(SELECT count(*) FROM transfer_intent s WHERE s.settles=t.tb_transfer_id) FROM transfer_intent t
			WHERE org_id=$1 AND status='unpaid' ORDER BY created_at FOR UPDATE`, org)
		if err != nil {
			tx.Rollback(ctx)
			return err
		}
		type unpaid struct {
			id, dr, cr, ref string
			amt, attempts   int64
		}
		var us []unpaid
		for q.Next() {
			var u unpaid
			_ = q.Scan(&u.id, &u.dr, &u.cr, &u.amt, &u.ref, &u.attempts)
			us = append(us, u)
		}
		q.Close()
		for _, u := range us {
			if u.amt > avail {
				break // strictly oldest first
			}
			avail -= u.amt
			sid := ids.U128("arrears", u.id, itoa(u.attempts))
			if _, err := tx.Exec(ctx, `INSERT INTO transfer_intent(tb_transfer_id,kind,org_id,debit_account,credit_account,amount_minor,currency,reference,settles)
				VALUES ($1::text::numeric,'arrears_settlement',$2,$3::text::numeric,$4::text::numeric,$5,'NGN',$6,$7::text::numeric) ON CONFLICT DO NOTHING`,
				sid.String(), org, u.dr, u.cr, u.amt, "arrears:"+u.ref, u.id); err != nil {
				tx.Rollback(ctx)
				return err
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	if _, err := r.A.Ledger.Relay(ctx, 8000); err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, `UPDATE transfer_intent o SET status='settled', updated_at=now() FROM transfer_intent s
		WHERE s.settles=o.tb_transfer_id AND s.status='committed' AND o.status='unpaid'`); err != nil {
		return err
	}
	rows, err = pool.Query(ctx, `SELECT id::text FROM organization o WHERE suspended_at IS NOT NULL
		AND NOT EXISTS (SELECT 1 FROM transfer_intent t WHERE t.org_id=o.id AND t.status='unpaid')`)
	if err != nil {
		return err
	}
	var resume []string
	for rows.Next() {
		var o string
		_ = rows.Scan(&o)
		resume = append(resume, o)
	}
	rows.Close()
	for _, org := range resume {
		r.start(ctx, "resume-"+org+"-"+time.Now().UTC().Format("20060102T15"), ResumeOrg, org)
	}
	return nil
}

func (r *Rater) start(ctx context.Context, id string, wf any, arg string) {
	_, err := r.Temporal.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: id, TaskQueue: TaskQueue}, wf, arg)
	if err != nil {
		r.Log.Warn("workflow start", "id", id, "err", err)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
