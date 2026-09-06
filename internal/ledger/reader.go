package ledger

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/money"
)

// Pagination bounds for ListTransactions.
const (
	DefaultListLimit = 50
	MaxListLimit     = 500
)

// Balance returns the normal-side balance of ref. An account that has never
// been posted to has balance zero.
func (s *Service) Balance(ctx context.Context, q db.Querier, ref AccountRef) (money.Quantity, error) {
	if err := ref.Validate(); err != nil {
		return money.Quantity{}, err
	}
	ref = ref.normalized()
	var text string
	err := q.QueryRow(ctx,
		`SELECT COALESCE(b.balance, 0)::text
		   FROM ledger_accounts a
		   LEFT JOIN ledger_balances b ON b.ledger_account_id = a.id
		  WHERE a.owner_type = $1 AND a.owner_id = $2::uuid AND a.code = $3 AND a.asset_id = $4`,
		ref.OwnerType, ref.OwnerID, ref.Code, ref.AssetID).Scan(&text)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return money.Quantity{}, nil
		}
		return money.Quantity{}, MapError(err)
	}
	return scanQuantity(text)
}

func scanQuantity(text string) (money.Quantity, error) {
	qty, err := money.ScanQuantity(text)
	if err != nil {
		return money.Quantity{}, errs.Wrap(err, errs.CodeInternal, "ledger: stored balance is not an integer")
	}
	return qty, nil
}

// BalancesForOwner returns every ledger account of the owner with its
// balance, ordered by code then asset.
func (s *Service) BalancesForOwner(ctx context.Context, q db.Querier, ownerType OwnerType, ownerID string) ([]AccountBalance, error) {
	if !ownerType.Valid() {
		return nil, errs.Newf(errs.CodeValidationFailed, "unknown ledger owner type %q", ownerType)
	}
	ownerID = strings.ToLower(strings.TrimSpace(ownerID))
	if _, err := id.ParseAny(ownerID); err != nil {
		return nil, errs.New(errs.CodeValidationFailed, "owner id must be a canonical uuid")
	}
	rows, err := q.Query(ctx,
		`SELECT a.id, a.owner_type, a.owner_id::text, a.code, a.asset_id,
		        COALESCE(b.balance, 0)::text, COALESCE(b.entry_count, 0), COALESCE(b.version, 0), COALESCE(b.updated_at, a.created_at)
		   FROM ledger_accounts a
		   LEFT JOIN ledger_balances b ON b.ledger_account_id = a.id
		  WHERE a.owner_type = $1 AND a.owner_id = $2::uuid
		  ORDER BY a.code, a.asset_id`,
		ownerType, ownerID)
	if err != nil {
		return nil, MapError(err)
	}
	defer rows.Close()
	var out []AccountBalance
	for rows.Next() {
		var b AccountBalance
		var text string
		if err := rows.Scan(&b.LedgerAccountID, &b.Account.OwnerType, &b.Account.OwnerID, &b.Account.Code, &b.Account.AssetID, &text, &b.EntryCount, &b.Version, &b.UpdatedAt); err != nil {
			return nil, MapError(err)
		}
		if b.Balance, err = scanQuantity(text); err != nil {
			return nil, err
		}
		b.UpdatedAt = b.UpdatedAt.UTC()
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, MapError(err)
	}
	return out, nil
}

const transactionColumns = `t.id, t.kind, t.idempotency_key, t.reference_type, t.reference_id, t.reversal_of, t.effective_at, t.posted_at,
	t.description, COALESCE(t.correlation_id, ''), t.posted_by_actor_type, t.posted_by_actor_id, COALESCE(t.reason_code, ''),
	t.metadata, t.content_hash, COALESCE(t.build_version, '')`

func scanTransaction(row pgx.Row) (Transaction, error) {
	var t Transaction
	var reversalOf TransactionID
	var metadata []byte
	if err := row.Scan(&t.ID, &t.Kind, &t.IdempotencyKey, &t.Reference.Type, &t.Reference.ID, &reversalOf, &t.EffectiveAt, &t.PostedAt,
		&t.Description, &t.CorrelationID, &t.PostedByActorType, &t.PostedByActorID, &t.ReasonCode,
		&metadata, &t.ContentHash, &t.BuildVersion); err != nil {
		return Transaction{}, err
	}
	// pgx returns timestamptz in the process-local zone; the ledger speaks UTC.
	t.EffectiveAt = t.EffectiveAt.UTC()
	t.PostedAt = t.PostedAt.UTC()
	if !reversalOf.IsZero() {
		t.ReversalOf = &reversalOf
	}
	if len(metadata) > 0 {
		dec := json.NewDecoder(bytes.NewReader(metadata))
		dec.UseNumber() // numbers stay json.Number: never a binary conversion
		if err := dec.Decode(&t.Metadata); err != nil {
			return Transaction{}, fmt.Errorf("ledger: decode metadata: %w", err)
		}
	}
	return t, nil
}

// Transaction returns a posted transaction with its entries, or NOT_FOUND.
func (s *Service) Transaction(ctx context.Context, q db.Querier, txID TransactionID) (Transaction, error) {
	if txID.IsZero() {
		return Transaction{}, errs.New(errs.CodeValidationFailed, "transaction id is required")
	}
	t, err := scanTransaction(q.QueryRow(ctx, `SELECT `+transactionColumns+` FROM journal_transactions t WHERE t.id = $1`, txID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Transaction{}, errs.New(errs.CodeNotFound, "journal transaction not found").WithField("transaction_id", txID.String())
		}
		return Transaction{}, MapError(err)
	}
	entries, err := s.entriesFor(ctx, q, []TransactionID{txID})
	if err != nil {
		return Transaction{}, err
	}
	t.Entries = entries[txID]
	return t, nil
}

// entriesFor loads the entries of the given transactions, in seq order.
func (s *Service) entriesFor(ctx context.Context, q db.Querier, ids []TransactionID) (map[TransactionID][]JournalEntry, error) {
	out := make(map[TransactionID][]JournalEntry, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	texts := make([]string, len(ids))
	for i, x := range ids {
		texts[i] = x.String()
	}
	rows, err := q.Query(ctx,
		`SELECT e.id, e.transaction_id, e.seq, e.ledger_account_id, a.owner_type, a.owner_id::text, a.code, e.asset_id,
		        e.side, e.quantity::text, e.usd_value_minor, e.price_ref
		   FROM journal_entries e
		   JOIN ledger_accounts a ON a.id = e.ledger_account_id
		  WHERE e.transaction_id = ANY($1::uuid[])
		  ORDER BY e.transaction_id, e.seq`, texts)
	if err != nil {
		return nil, MapError(err)
	}
	defer rows.Close()
	for rows.Next() {
		var e JournalEntry
		var text string
		if err := rows.Scan(&e.ID, &e.TransactionID, &e.Seq, &e.LedgerAccountID, &e.Account.OwnerType, &e.Account.OwnerID, &e.Account.Code, &e.Account.AssetID,
			&e.Side, &text, &e.USDValueMinor, &e.PriceRef); err != nil {
			return nil, MapError(err)
		}
		if e.Quantity, err = scanQuantity(text); err != nil {
			return nil, err
		}
		out[e.TransactionID] = append(out[e.TransactionID], e)
	}
	if err := rows.Err(); err != nil {
		return nil, MapError(err)
	}
	return out, nil
}

// ListTransactions returns transactions matching filter in (posted_at, id)
// order, at most limit (default DefaultListLimit, capped at MaxListLimit),
// with an opaque cursor for the next page ("" when exhausted).
func (s *Service) ListTransactions(ctx context.Context, q db.Querier, filter TransactionFilter, cursor string, limit int) ([]Transaction, string, error) {
	if limit <= 0 {
		limit = DefaultListLimit
	}
	if limit > MaxListLimit {
		limit = MaxListLimit
	}
	var kind, ownerType, ownerID, assetID *string
	if filter.Kind != "" {
		if !filter.Kind.Valid() {
			return nil, "", errs.Newf(errs.CodeValidationFailed, "unknown journal kind %q", filter.Kind)
		}
		k := string(filter.Kind)
		kind = &k
	}
	if filter.OwnerType != "" {
		if !filter.OwnerType.Valid() {
			return nil, "", errs.Newf(errs.CodeValidationFailed, "unknown ledger owner type %q", filter.OwnerType)
		}
		o := string(filter.OwnerType)
		ownerType = &o
	}
	if filter.OwnerID != "" {
		o := strings.ToLower(strings.TrimSpace(filter.OwnerID))
		if _, err := id.ParseAny(o); err != nil {
			return nil, "", errs.New(errs.CodeValidationFailed, "owner id must be a canonical uuid")
		}
		ownerID = &o
	}
	if !filter.AssetID.IsZero() {
		a := filter.AssetID.String()
		assetID = &a
	}
	var since, until *time.Time
	if !filter.Since.IsZero() {
		t := filter.Since.UTC()
		since = &t
	}
	if !filter.Until.IsZero() {
		t := filter.Until.UTC()
		until = &t
	}
	if since != nil && until != nil && !until.After(*since) {
		return nil, "", errs.New(errs.CodeValidationFailed, "until must be after since")
	}
	var afterAt *time.Time
	var afterID *string
	if cursor != "" {
		at, txID, err := decodeCursor(cursor)
		if err != nil {
			return nil, "", err
		}
		s := txID.String()
		afterAt, afterID = &at, &s
	}

	rows, err := q.Query(ctx,
		`SELECT `+transactionColumns+`
		   FROM journal_transactions t
		  WHERE ($1::text IS NULL OR t.kind = $1)
		    AND ($2::timestamptz IS NULL OR t.posted_at >= $2)
		    AND ($3::timestamptz IS NULL OR t.posted_at < $3)
		    AND (($4::text IS NULL AND $5::uuid IS NULL AND $6::uuid IS NULL)
		         OR EXISTS (SELECT 1
		                      FROM journal_entries e
		                      JOIN ledger_accounts a ON a.id = e.ledger_account_id
		                     WHERE e.transaction_id = t.id
		                       AND ($4::text IS NULL OR a.owner_type = $4)
		                       AND ($5::uuid IS NULL OR a.owner_id = $5)
		                       AND ($6::uuid IS NULL OR a.asset_id = $6)))
		    AND ($7::timestamptz IS NULL OR (t.posted_at, t.id) > ($7, $8::uuid))
		  ORDER BY t.posted_at, t.id
		  LIMIT $9`,
		kind, since, until, ownerType, ownerID, assetID, afterAt, afterID, limit+1)
	if err != nil {
		return nil, "", MapError(err)
	}
	defer rows.Close()
	var page []Transaction
	for rows.Next() {
		t, err := scanTransaction(rows)
		if err != nil {
			return nil, "", MapError(err)
		}
		page = append(page, t)
	}
	if err := rows.Err(); err != nil {
		return nil, "", MapError(err)
	}
	next := ""
	if len(page) > limit {
		page = page[:limit]
		last := page[len(page)-1]
		next = encodeCursor(last.PostedAt, last.ID)
	}
	ids := make([]TransactionID, len(page))
	for i := range page {
		ids[i] = page[i].ID
	}
	entries, err := s.entriesFor(ctx, q, ids)
	if err != nil {
		return nil, "", err
	}
	for i := range page {
		page[i].Entries = entries[page[i].ID]
	}
	return page, next, nil
}

// Cursors are "v1|<posted_at RFC3339Nano UTC>|<transaction id>" in
// unpadded URL-safe base64. They are opaque to clients.
const cursorVersion = "v1"

func encodeCursor(postedAt time.Time, txID TransactionID) string {
	raw := cursorVersion + "|" + postedAt.UTC().Format(time.RFC3339Nano) + "|" + txID.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(s string) (time.Time, TransactionID, error) {
	bad := errs.New(errs.CodeValidationFailed, "invalid pagination cursor")
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, TransactionID{}, bad
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != 3 || parts[0] != cursorVersion {
		return time.Time{}, TransactionID{}, bad
	}
	at, err := time.Parse(time.RFC3339Nano, parts[1])
	if err != nil {
		return time.Time{}, TransactionID{}, bad
	}
	txID, err := ParseTransactionID(parts[2])
	if err != nil || txID.IsZero() {
		return time.Time{}, TransactionID{}, bad
	}
	return at.UTC(), txID, nil
}
