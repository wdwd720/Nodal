package ledger

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync/atomic"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// PostMaxRetries is the deadlock/serialization retry budget of PostInTx.
// Entries are inserted in ledger_account_id order so deadlocks between
// posters are not expected; the budget is a safety net.
const PostMaxRetries = 5

// systemActorID is recorded as posted_by_actor_id when no principal is on
// the context (background workers posting on behalf of the system).
const systemActorID = "ledger-service"

// Service implements Poster and Reader against PostgreSQL.
type Service struct {
	clk          clock.Clock
	buildVersion string
	allowSeed    atomic.Bool
	caps         atomic.Pointer[CapabilityResolver]
}

// CapabilityResolver reports which value-domain conversion capabilities are
// currently ACTIVE. internal/gates supplies the production implementation.
//
// A Service with no resolver treats every capability as inactive, which is the
// correct reading for a process that has not been told otherwise: it permits
// every single-domain posting the system has ever made and refuses every
// cross-domain one. Fail closed is the default, not a configuration.
type CapabilityResolver interface {
	ActiveConversionCapabilities(ctx context.Context) (map[valuedomain.CapabilityKey]bool, error)
}

// SetCapabilityResolver installs the resolver. Only the composition root calls
// it.
func (s *Service) SetCapabilityResolver(r CapabilityResolver) {
	if r == nil {
		s.caps.Store(nil)
		return
	}
	s.caps.Store(&r)
}

func (s *Service) activeCapabilities(ctx context.Context) (map[valuedomain.CapabilityKey]bool, error) {
	p := s.caps.Load()
	if p == nil {
		return nil, nil
	}
	return (*p).ActiveConversionCapabilities(ctx)
}

var (
	_ Poster = (*Service)(nil)
	_ Reader = (*Service)(nil)
)

// NewService returns a Service. buildVersion is recorded on every
// transaction it posts. The clock must not be nil.
func NewService(clk clock.Clock, buildVersion string) *Service {
	if clk == nil {
		panic("ledger: NewService requires a clock")
	}
	return &Service{clk: clk, buildVersion: buildVersion}
}

// AllowSeedPostings enables KindSeed. Only the composition root calls it,
// and only when config.Environment is LOCAL or TEST; by default SEED
// postings fail with FORBIDDEN.
func (s *Service) AllowSeedPostings() { s.allowSeed.Store(true) }

const ledgerAccountColumns = `id, owner_type, owner_id::text, code, asset_id, normal_side, allow_negative, status, value_domain, created_at`

func scanLedgerAccount(row pgx.Row) (LedgerAccount, error) {
	var a LedgerAccount
	if err := row.Scan(&a.ID, &a.Ref.OwnerType, &a.Ref.OwnerID, &a.Ref.Code, &a.Ref.AssetID, &a.NormalSide, &a.AllowNegative, &a.Status, &a.Domain, &a.CreatedAt); err != nil {
		return LedgerAccount{}, err
	}
	a.CreatedAt = a.CreatedAt.UTC()
	return a, nil
}

// Account returns the ledger account for ref, or NOT_FOUND if it has never
// been touched by a posting.
func (s *Service) Account(ctx context.Context, q db.Querier, ref AccountRef) (LedgerAccount, error) {
	if err := ref.Validate(); err != nil {
		return LedgerAccount{}, err
	}
	acct, found, err := lookupAccount(ctx, q, ref.normalized())
	if err != nil {
		return LedgerAccount{}, MapError(err)
	}
	if !found {
		return LedgerAccount{}, errs.New(errs.CodeNotFound, "ledger account not found").WithField("account", ref.String())
	}
	return acct, nil
}

func lookupAccount(ctx context.Context, q db.Querier, ref AccountRef) (LedgerAccount, bool, error) {
	acct, err := scanLedgerAccount(q.QueryRow(ctx,
		`SELECT `+ledgerAccountColumns+` FROM ledger_accounts WHERE owner_type = $1 AND owner_id = $2::uuid AND code = $3 AND asset_id = $4`,
		ref.OwnerType, ref.OwnerID, ref.Code, ref.AssetID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return LedgerAccount{}, false, nil
		}
		return LedgerAccount{}, false, err
	}
	return acct, true, nil
}

// EnsureAccount resolves the ledger account for ref, creating it with the
// normal side and allow_negative of its code if it does not exist yet
// (INSERT ... ON CONFLICT DO NOTHING, then select). A stored definition that
// disagrees with the chart of accounts fails with INTERNAL: it means the
// row was created by something other than this package.
func (s *Service) EnsureAccount(ctx context.Context, q db.Querier, ref AccountRef) (LedgerAccount, error) {
	if err := ref.Validate(); err != nil {
		return LedgerAccount{}, err
	}
	ref = ref.normalized()
	acct, found, err := lookupAccount(ctx, q, ref)
	if err != nil {
		return LedgerAccount{}, MapError(err)
	}
	if found {
		return checkDefinition(acct)
	}
	acct, err = scanLedgerAccount(q.QueryRow(ctx,
		`INSERT INTO ledger_accounts (id, owner_type, owner_id, code, asset_id, normal_side, allow_negative, status, created_at)
		 VALUES ($1, $2, $3::uuid, $4, $5, $6, $7, 'OPEN', $8)
		 ON CONFLICT (owner_type, owner_id, code, asset_id) DO NOTHING
		 RETURNING `+ledgerAccountColumns,
		NewLedgerAccountID(), ref.OwnerType, ref.OwnerID, ref.Code, ref.AssetID, ref.Code.NormalSide(), ref.Code.AllowsNegative(), s.clk.Now()))
	if err == nil {
		return checkDefinition(acct)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return LedgerAccount{}, MapError(err)
	}
	// A concurrent transaction created it first; it is committed by now.
	acct, found, err = lookupAccount(ctx, q, ref)
	if err != nil {
		return LedgerAccount{}, MapError(err)
	}
	if !found {
		return LedgerAccount{}, errs.New(errs.CodeInternal, "ledger account disappeared after a conflicting insert").WithField("account", ref.String())
	}
	return checkDefinition(acct)
}

func checkDefinition(acct LedgerAccount) (LedgerAccount, error) {
	if acct.NormalSide != acct.Ref.Code.NormalSide() || acct.AllowNegative != acct.Ref.Code.AllowsNegative() {
		return LedgerAccount{}, errs.New(errs.CodeInternal, "ledger account definition disagrees with the chart of accounts").
			WithField("account", acct.Ref.String()).
			WithField("stored_normal_side", string(acct.NormalSide)).
			WithField("stored_allow_negative", acct.AllowNegative)
	}
	return acct, nil
}

// actorFrom resolves who is posting. Agents are refused outright: the ledger
// is written by deterministic services, never by a proposal generator.
func actorFrom(ctx context.Context) (security.ActorType, string, error) {
	p, ok := security.PrincipalFrom(ctx)
	if !ok {
		return security.ActorSystem, systemActorID, nil
	}
	if p.IsAgent() {
		return "", "", errs.New(errs.CodeForbidden, "agent principals cannot post to the ledger")
	}
	if p.SubjectID == "" || !p.ActorType.Valid() {
		return "", "", errs.New(errs.CodeForbidden, "invalid principal on context")
	}
	return p.ActorType, p.SubjectID, nil
}

// entryRow is an entry bound to its resolved account, ready to insert.
type entryRow struct {
	account LedgerAccount
	entry   Entry
	// rank orders entries on the same account so increases apply before
	// decreases; a valid posting never trips the negative-balance trigger
	// on a transient state.
	rank int
}

// resolveEntries ensures every account (in one global order, so concurrent
// creators cannot deadlock), refuses closed accounts, and returns the rows in
// insertion order: ledger_account_id ascending, then normal side first.
func (s *Service) resolveEntries(ctx context.Context, tx pgx.Tx, entries []Entry) ([]entryRow, error) {
	byKey := make(map[string]AccountRef, len(entries))
	for _, e := range entries {
		byKey[e.Account.key()] = e.Account.normalized()
	}
	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	resolved := make(map[string]LedgerAccount, len(keys))
	for _, k := range keys {
		acct, err := s.EnsureAccount(ctx, tx, byKey[k])
		if err != nil {
			return nil, err
		}
		if acct.Status != AccountOpen {
			return nil, errs.New(errs.CodeLedgerAccountClosed, "ledger account is closed").WithField("account", acct.Ref.String())
		}
		resolved[k] = acct
	}
	rows := make([]entryRow, 0, len(entries))
	for _, e := range entries {
		acct := resolved[e.Account.key()]
		rank := 0
		if e.Side != acct.NormalSide {
			rank = 1
		}
		rows = append(rows, entryRow{account: acct, entry: e, rank: rank})
	}
	slices.SortStableFunc(rows, func(a, b entryRow) int {
		if c := id.Compare(a.account.ID, b.account.ID); c != 0 {
			return c
		}
		return cmp.Compare(a.rank, b.rank)
	})
	return rows, nil
}

// Post records p inside tx. See the package documentation for the contract.
// On error the transaction may be aborted; the caller must roll back (db.InTx
// does). Errors are *errs.Error except retryable transaction failures, which
// pass through so db.InTx can re-run the transaction.
func (s *Service) Post(ctx context.Context, tx pgx.Tx, p Posting) (PostResult, error) {
	if err := validatePosting(p); err != nil {
		return PostResult{}, err
	}
	if p.Kind == KindSeed && !s.allowSeed.Load() {
		return PostResult{}, errs.New(errs.CodeForbidden, "SEED postings are disabled in this environment")
	}
	actorType, actorID, err := actorFrom(ctx)
	if err != nil {
		return PostResult{}, err
	}
	if tx == nil {
		return PostResult{}, errs.New(errs.CodeInternal, "ledger: Post requires a transaction")
	}
	content, err := CanonicalContent(p)
	if err != nil {
		return PostResult{}, err
	}
	sum := sha256.Sum256(content)
	hash := sum[:]

	if res, found, err := s.findByKey(ctx, tx, p.IdempotencyKey, hash); err != nil || found {
		return res, err
	}
	if p.ReversalOf != nil {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM journal_transactions WHERE id = $1)`, *p.ReversalOf).Scan(&exists); err != nil {
			return PostResult{}, MapError(err)
		}
		if !exists {
			return PostResult{}, errs.New(errs.CodeValidationFailed, "reversal_of refers to an unknown journal transaction").
				WithField("reversal_of", p.ReversalOf.String())
		}
	}
	rows, err := s.resolveEntries(ctx, tx, p.Entries)
	if err != nil {
		return PostResult{}, err
	}
	if err := s.checkDomainIsolation(ctx, rows, p.Conversion); err != nil {
		return PostResult{}, err
	}
	metadata, err := canonicalMetadata(p.Metadata)
	if err != nil {
		return PostResult{}, err
	}

	txID := NewTransactionID()
	var reversalOf any
	if p.ReversalOf != nil {
		reversalOf = *p.ReversalOf
	}
	// The header goes in under a savepoint so a unique-key race (another
	// transaction committed the same idempotency key after our pre-check)
	// can be resolved by re-reading instead of aborting the caller's tx.
	sp, err := tx.Begin(ctx)
	if err != nil {
		return PostResult{}, MapError(err)
	}
	var convFrom, convTo any
	if p.Conversion != nil {
		convFrom, convTo = string(p.Conversion.From), string(p.Conversion.To)
	}
	_, err = sp.Exec(ctx,
		`INSERT INTO journal_transactions
		   (id, kind, idempotency_key, reference_type, reference_id, reversal_of, effective_at, posted_at,
		    description, correlation_id, posted_by_actor_type, posted_by_actor_id, reason_code, metadata, content_hash, build_version,
		    conversion_from, conversion_to)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NULLIF($10, ''), $11, $12, NULLIF($13, ''), $14, $15, NULLIF($16, ''), $17, $18)`,
		txID, p.Kind, p.IdempotencyKey, p.Reference.Type, p.Reference.ID, reversalOf, p.EffectiveAt.UTC(), s.clk.Now(),
		p.Description, p.CorrelationID, string(actorType), actorID, p.ReasonCode(), metadata, hash, s.buildVersion,
		convFrom, convTo)
	if err != nil {
		_ = sp.Rollback(ctx)
		if db.IsUniqueViolation(err) && db.ConstraintName(err) == constraintIdempotencyKey {
			res, found, ferr := s.findByKey(ctx, tx, p.IdempotencyKey, hash)
			if ferr != nil {
				return PostResult{}, ferr
			}
			if !found {
				return PostResult{}, errs.Wrap(err, errs.CodeInternal, "idempotency key conflict without a visible transaction")
			}
			return res, nil
		}
		return PostResult{}, MapError(err)
	}
	if err := sp.Commit(ctx); err != nil {
		return PostResult{}, MapError(err)
	}

	for i, r := range rows {
		seq := int32(i) // #nosec G115 -- bounded by MaxEntries
		if _, err := tx.Exec(ctx,
			`INSERT INTO journal_entries (id, transaction_id, seq, ledger_account_id, asset_id, side, quantity, usd_value_minor, price_ref)
			 VALUES ($1, $2, $3, $4, $5, $6, $7::numeric, $8, $9)`,
			NewJournalEntryID(), txID, seq, r.account.ID, r.account.Ref.AssetID, r.entry.Side, r.entry.Quantity.String(), r.entry.USDValueMinor, r.entry.PriceRef); err != nil {
			return PostResult{}, MapError(err)
		}
	}
	return PostResult{TransactionID: txID, ContentHash: hash}, nil
}

// findByKey looks the idempotency key up. found is true when a transaction
// with the same key and the same content hash exists (res describes it); a
// different hash fails with INVALID_IDEMPOTENCY_REUSE.
func (s *Service) findByKey(ctx context.Context, q db.Querier, key string, hash []byte) (PostResult, bool, error) {
	var existingID TransactionID
	var existingHash []byte
	err := q.QueryRow(ctx, `SELECT id, content_hash FROM journal_transactions WHERE idempotency_key = $1`, key).Scan(&existingID, &existingHash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PostResult{}, false, nil
		}
		return PostResult{}, false, MapError(err)
	}
	if !bytes.Equal(existingHash, hash) {
		return PostResult{}, false, errs.New(errs.CodeInvalidIdempotencyReuse, "idempotency key was already used for a different journal transaction").
			WithField("idempotency_key", key).
			WithField("existing_transaction_id", existingID.String())
	}
	return PostResult{TransactionID: existingID, ContentHash: existingHash, Existing: true}, true, nil
}

// PostInTx runs Post in its own READ COMMITTED transaction via db.InTx,
// retrying deadlocks and serialization failures up to PostMaxRetries, and
// maps the commit error (where the deferred balance triggers fire) with
// MapError. Use Post directly when the posting must share a transaction with
// other financial state changes and an outbox event.
func (s *Service) PostInTx(ctx context.Context, d *db.DB, p Posting) (PostResult, error) {
	if d == nil {
		return PostResult{}, errs.New(errs.CodeInternal, "ledger: PostInTx requires a database")
	}
	var res PostResult
	err := d.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted, MaxRetries: PostMaxRetries}, func(ctx context.Context, tx pgx.Tx) error {
		r, err := s.Post(ctx, tx, p)
		if err != nil {
			return err
		}
		res = r
		return nil
	})
	if err != nil {
		return PostResult{}, MapError(fmt.Errorf("ledger: post %s: %w", p.Kind, err))
	}
	return res, nil
}

// checkDomainIsolation is the application half of the value-domain invariant
// (gola.md PART IX); the deferred trigger installed by migration 00710 is the
// other half, and neither may be dropped because the other exists. This side
// produces the operator-facing error and is the only side that can evaluate
// capability activation, because a capability gate is keyed by environment and
// a database connection carries no environment the application could not
// simply assert.
func (s *Service) checkDomainIsolation(ctx context.Context, rows []entryRow, conv *valuedomain.ConversionKey) error {
	domains := make([]valuedomain.Domain, 0, len(rows))
	for _, r := range rows {
		domains = append(domains, r.account.Domain)
	}
	caps, err := s.activeCapabilities(ctx)
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "ledger: resolve active conversion capabilities")
	}
	return valuedomain.CheckPosting(domains, conv, caps)
}
