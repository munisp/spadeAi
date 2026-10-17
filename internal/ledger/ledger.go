// Package ledger maps SpadeAi money movements onto TigerBeetle.
// Postgres holds the account map and the transfer outbox (transfer_intent); TigerBeetle holds balances.
package ledger

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	tb "github.com/tigerbeetle/tigerbeetle-go"

	"github.com/munisp/spadeai/internal/ids"
)

const LedgerNGN uint32 = 566 // ISO 4217 numeric

const (
	CodeWallet   uint16 = 1
	CodeRevenue  uint16 = 2
	CodeVAT      uint16 = 3
	CodeClearing uint16 = 4
	CodeWriteoff uint16 = 5
)

var kindCode = map[string]uint16{
	"org_wallet": CodeWallet, "platform_revenue": CodeRevenue, "vat_liability": CodeVAT,
	"payment_clearing": CodeClearing, "writeoff": CodeWriteoff,
}

type Ledger struct {
	TB   tb.Client
	Pool *pgxpool.Pool
}

func Connect(clusterID uint64, addresses []string) (tb.Client, error) {
	return tb.NewClient(tb.ToUint128(clusterID), addresses)
}

func ledgerFor(currency string) (uint32, error) {
	if currency == "NGN" {
		return LedgerNGN, nil
	}
	return 0, fmt.Errorf("currency %s not enabled", currency)
}

// EnsureAccount creates (idempotently) a TigerBeetle account and its Postgres mapping.
func (l *Ledger) EnsureAccount(ctx context.Context, kind, ownerID, currency string, prepaid bool) (*big.Int, error) {
	code, ok := kindCode[kind]
	if !ok {
		return nil, fmt.Errorf("unknown account kind %q", kind)
	}
	led, err := ledgerFor(currency)
	if err != nil {
		return nil, err
	}
	id := ids.U128("account", kind, ownerID, currency)
	flags := tb.AccountFlags{History: true}
	if kind == "org_wallet" && prepaid {
		flags.DebitsMustNotExceedCredits = true
	}
	res, err := l.TB.CreateAccounts([]tb.Account{{ID: tb.BigIntToUint128(id), Ledger: led, Code: code, Flags: flags.ToUint16()}})
	if err != nil {
		return nil, fmt.Errorf("tigerbeetle create account: %w", err)
	}
	for _, r := range res {
		if r.Status != tb.AccountCreated && r.Status != tb.AccountExists {
			return nil, fmt.Errorf("tigerbeetle create account: %s", r.Status)
		}
	}
	var owner any
	if ownerID != "" {
		owner = ownerID
	}
	_, err = l.Pool.Exec(ctx, `INSERT INTO ledger_account_map(owner_kind, owner_id, currency, tb_account_id, tb_ledger)
		VALUES ($1,$2,$3,$4::text::numeric,$5) ON CONFLICT DO NOTHING`, kind, owner, currency, id.String(), led)
	return id, err
}

func (l *Ledger) PlatformAccounts(ctx context.Context, currency string) (revenue, vat, clearing, writeoff *big.Int, err error) {
	if revenue, err = l.EnsureAccount(ctx, "platform_revenue", "", currency, false); err != nil {
		return
	}
	if vat, err = l.EnsureAccount(ctx, "vat_liability", "", currency, false); err != nil {
		return
	}
	if clearing, err = l.EnsureAccount(ctx, "payment_clearing", "", currency, false); err != nil {
		return
	}
	writeoff, err = l.EnsureAccount(ctx, "writeoff", "", currency, false)
	return
}

func WalletID(orgID, currency string) *big.Int { return ids.U128("account", "org_wallet", orgID, currency) }

type Balance struct {
	Currency  string `json:"currency"`
	Available int64  `json:"available_minor"`
	Pending   int64  `json:"pending_debits_minor"`
	Posted    int64  `json:"posted_balance_minor"`
}

func (l *Ledger) WalletBalance(orgID, currency string) (*Balance, error) {
	accts, err := l.TB.LookupAccounts([]tb.Uint128{tb.BigIntToUint128(WalletID(orgID, currency))})
	if err != nil {
		return nil, err
	}
	if len(accts) == 0 {
		return nil, errors.New("wallet not found")
	}
	a := accts[0]
	cp, dp, dpend := a.CreditsPosted.BigInt(), a.DebitsPosted.BigInt(), a.DebitsPending.BigInt()
	posted := new(big.Int).Sub(cp, dp)
	avail := new(big.Int).Sub(posted, dpend)
	return &Balance{Currency: currency, Available: avail.Int64(), Pending: dpend.Int64(), Posted: posted.Int64()}, nil
}

// Intent is a row destined for transfer_intent.
type Intent struct {
	ID            *big.Int
	Kind          string
	OrgID         string
	Debit, Credit *big.Int
	Amount        int64
	Currency      string
	Reference     string
}

// Enqueue writes intents into the outbox inside the caller's transaction. Duplicate IDs are ignored (idempotent).
func Enqueue(ctx context.Context, tx pgx.Tx, in ...Intent) error {
	for _, i := range in {
		if i.Amount <= 0 {
			continue
		}
		if _, err := tx.Exec(ctx, `INSERT INTO transfer_intent(tb_transfer_id, kind, org_id, debit_account, credit_account, amount_minor, currency, reference)
			VALUES ($1::text::numeric,$2,$3,$4::text::numeric,$5::text::numeric,$6,$7,$8) ON CONFLICT (tb_transfer_id) DO NOTHING`,
			i.ID.String(), i.Kind, i.OrgID, i.Debit.String(), i.Credit.String(), i.Amount, i.Currency, i.Reference); err != nil {
			return err
		}
	}
	return nil
}

// RelayResult summarises one outbox relay pass.
type RelayResult struct {
	Committed  int
	Unpaid     []string // org ids whose prepaid debit was rejected with exceeds_credits
	Failed     int
}

// Relay submits pending intents to TigerBeetle. Deterministic IDs make retries safe (TransferExists == committed).
func (l *Ledger) Relay(ctx context.Context, limit int) (*RelayResult, error) {
	tx, err := l.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT tb_transfer_id::text, kind, org_id::text, debit_account::text, credit_account::text, amount_minor, currency
		FROM transfer_intent WHERE status='pending' ORDER BY created_at LIMIT $1 FOR UPDATE SKIP LOCKED`, limit)
	if err != nil {
		return nil, err
	}
	type row struct{ id, kind, org, dr, cr, cur string; amt int64 }
	var batch []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.kind, &r.org, &r.dr, &r.cr, &r.amt, &r.cur); err != nil {
			rows.Close()
			return nil, err
		}
		batch = append(batch, r)
	}
	rows.Close()
	out := &RelayResult{}
	if len(batch) == 0 {
		return out, tx.Commit(ctx)
	}
	transfers := make([]tb.Transfer, len(batch))
	for i, r := range batch {
		led, err := ledgerFor(r.cur)
		if err != nil {
			return nil, err
		}
		transfers[i] = tb.Transfer{ID: u(r.id), DebitAccountID: u(r.dr), CreditAccountID: u(r.cr), Amount: tb.ToUint128(uint64(r.amt)), Ledger: led, Code: 1}
	}
	res, err := l.TB.CreateTransfers(transfers)
	if err != nil {
		return nil, fmt.Errorf("tigerbeetle create transfers: %w", err)
	}
	seenUnpaid := map[string]bool{}
	for i, r := range res {
		b := batch[i]
		status := "failed"
		switch r.Status {
		case tb.TransferCreated, tb.TransferExists:
			status = "committed"
			out.Committed++
		case tb.TransferExceedsCredits:
			if b.kind != "usage" && b.kind != "vat" {
				out.Failed++
				break
			}
			status = "unpaid"
			if !seenUnpaid[b.org] {
				seenUnpaid[b.org] = true
				out.Unpaid = append(out.Unpaid, b.org)
			}
		default:
			out.Failed++
		}
		if _, err := tx.Exec(ctx, `UPDATE transfer_intent SET status=$2, tb_result=$3, updated_at=now() WHERE tb_transfer_id=$1::text::numeric`, b.id, status, r.Status.String()); err != nil {
			return nil, err
		}
	}
	return out, tx.Commit(ctx)
}

// Hold places a pending transfer (funds check only) that must later be voided.
func (l *Ledger) Hold(orgID, currency, reference string, amount int64, timeoutSec uint32) (*big.Int, error) {
	led, err := ledgerFor(currency)
	if err != nil {
		return nil, err
	}
	id := ids.U128("hold", reference)
	revenue := ids.U128("account", "platform_revenue", "", currency)
	res, err := l.TB.CreateTransfers([]tb.Transfer{{ID: tb.BigIntToUint128(id), DebitAccountID: tb.BigIntToUint128(WalletID(orgID, currency)),
		CreditAccountID: tb.BigIntToUint128(revenue), Amount: tb.ToUint128(uint64(amount)), Ledger: led, Code: 2,
		Timeout: timeoutSec, Flags: tb.TransferFlags{Pending: true}.ToUint16()}})
	if err != nil {
		return nil, err
	}
	for _, r := range res {
		switch r.Status {
		case tb.TransferCreated, tb.TransferExists:
		case tb.TransferExceedsCredits:
			return nil, ErrInsufficientFunds
		default:
			return nil, fmt.Errorf("hold rejected: %s", r.Status)
		}
	}
	return id, nil
}

var ErrInsufficientFunds = errors.New("insufficient prepaid balance")

// VoidHold releases a hold; already-voided or expired holds are treated as released.
func (l *Ledger) VoidHold(currency, reference string) error {
	led, err := ledgerFor(currency)
	if err != nil {
		return err
	}
	pending := ids.U128("hold", reference)
	res, err := l.TB.CreateTransfers([]tb.Transfer{{ID: tb.BigIntToUint128(ids.U128("hold_void", reference)), PendingID: tb.BigIntToUint128(pending),
		Ledger: led, Code: 2, Flags: tb.TransferFlags{VoidPendingTransfer: true}.ToUint16()}})
	if err != nil {
		return err
	}
	for _, r := range res {
		switch r.Status {
		case tb.TransferCreated, tb.TransferExists, tb.TransferPendingTransferAlreadyVoided, tb.TransferPendingTransferExpired, tb.TransferPendingTransferNotFound:
		default:
			return fmt.Errorf("void rejected: %s", r.Status)
		}
	}
	return nil
}

func u(s string) tb.Uint128 {
	n, _ := new(big.Int).SetString(s, 10)
	return tb.BigIntToUint128(n)
}
