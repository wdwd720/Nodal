package wallet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
)

// Repository persists wallets. Every method takes a Querier or Tx so the
// caller owns the transaction boundary.
type Repository struct{}

// NewRepository returns a Repository.
func NewRepository() *Repository { return &Repository{} }

const walletColumns = `id, account_id, provider, provider_wallet_id, chain, address, kind, status,
	delegation_ref, delegation_verified_at, signing_policy_version, capabilities, created_at, updated_at`

func scanWallet(row pgx.Row) (Wallet, error) {
	var (
		w             Wallet
		delegationRef *string
		policyVersion *string
		caps          []byte
	)
	if err := row.Scan(&w.ID, &w.AccountID, &w.Provider, &w.ProviderWalletID, &w.Chain, &w.Address, &w.Kind, &w.Status,
		&delegationRef, &w.DelegationVerifiedAt, &policyVersion, &caps, &w.CreatedAt, &w.UpdatedAt); err != nil {
		return Wallet{}, err
	}
	if delegationRef != nil {
		w.DelegationRef = *delegationRef
	}
	if policyVersion != nil {
		w.SigningPolicyVersion = *policyVersion
	}
	if len(caps) > 0 {
		w.Capabilities = json.RawMessage(caps)
	}
	if w.DelegationVerifiedAt != nil {
		t := w.DelegationVerifiedAt.UTC()
		w.DelegationVerifiedAt = &t
	}
	w.CreatedAt, w.UpdatedAt = w.CreatedAt.UTC(), w.UpdatedAt.UTC()
	return w, nil
}

// Create inserts a wallet. A zero ID is assigned. Duplicate (provider,
// provider_wallet_id) or (chain, address) → CONFLICT.
func (r *Repository) Create(ctx context.Context, q db.Querier, w Wallet) (Wallet, error) {
	if err := w.Validate(); err != nil {
		return Wallet{}, err
	}
	if w.ID.IsZero() {
		w.ID = NewWalletID()
	}
	caps := w.Capabilities
	if len(caps) == 0 {
		caps = json.RawMessage(`{}`)
	}
	row := q.QueryRow(ctx, `INSERT INTO wallets (id, account_id, provider, provider_wallet_id, chain, address, kind, status,
			delegation_ref, delegation_verified_at, signing_policy_version, capabilities)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,''),$10,NULLIF($11,''),$12)
		RETURNING `+walletColumns,
		w.ID, w.AccountID, w.Provider, w.ProviderWalletID, w.Chain, w.Address, string(w.Kind), string(w.Status),
		w.DelegationRef, w.DelegationVerifiedAt, w.SigningPolicyVersion, []byte(caps))
	out, err := scanWallet(row)
	if err != nil {
		if db.IsUniqueViolation(err) {
			return Wallet{}, errs.Wrap(err, errs.CodeConflict, "wallet already exists for this provider id or address")
		}
		if db.IsForeignKeyViolation(err) {
			return Wallet{}, errs.Wrap(err, errs.CodeNotFound, "account not found")
		}
		return Wallet{}, fmt.Errorf("wallet: create: %w", err)
	}
	return out, nil
}

// Get returns a wallet by id.
func (r *Repository) Get(ctx context.Context, q db.Querier, walletID WalletID) (Wallet, error) {
	w, err := scanWallet(q.QueryRow(ctx, `SELECT `+walletColumns+` FROM wallets WHERE id = $1`, walletID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Wallet{}, errs.New(errs.CodeNotFound, "wallet not found").WithField("wallet_id", walletID.String())
		}
		return Wallet{}, fmt.Errorf("wallet: get: %w", err)
	}
	return w, nil
}

// GetForUpdate returns a wallet by id under a row lock (inside tx).
func (r *Repository) GetForUpdate(ctx context.Context, tx pgx.Tx, walletID WalletID) (Wallet, error) {
	w, err := scanWallet(tx.QueryRow(ctx, `SELECT `+walletColumns+` FROM wallets WHERE id = $1 FOR UPDATE`, walletID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Wallet{}, errs.New(errs.CodeNotFound, "wallet not found").WithField("wallet_id", walletID.String())
		}
		return Wallet{}, fmt.Errorf("wallet: lock: %w", err)
	}
	return w, nil
}

// GetByProviderWalletID returns a wallet by (provider, provider_wallet_id).
func (r *Repository) GetByProviderWalletID(ctx context.Context, q db.Querier, provider, providerWalletID string) (Wallet, error) {
	w, err := scanWallet(q.QueryRow(ctx, `SELECT `+walletColumns+` FROM wallets WHERE provider = $1 AND provider_wallet_id = $2`, provider, providerWalletID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Wallet{}, errs.New(errs.CodeNotFound, "wallet not found")
		}
		return Wallet{}, fmt.Errorf("wallet: get by provider id: %w", err)
	}
	return w, nil
}

// ListByAccount returns the account's wallets in creation order.
func (r *Repository) ListByAccount(ctx context.Context, q db.Querier, accountID string) ([]Wallet, error) {
	acct, err := id.ParseAny(accountID)
	if err != nil {
		return nil, errs.New(errs.CodeValidationFailed, "wallet: account id must be a canonical uuid")
	}
	rows, err := q.Query(ctx, `SELECT `+walletColumns+` FROM wallets WHERE account_id = $1 ORDER BY created_at, id`, acct)
	if err != nil {
		return nil, fmt.Errorf("wallet: list by account: %w", err)
	}
	defer rows.Close()
	var out []Wallet
	for rows.Next() {
		w, err := scanWallet(rows)
		if err != nil {
			return nil, fmt.Errorf("wallet: list scan: %w", err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// Transition applies a status change under a row lock and records the
// wallet_status_transitions row in the same transaction. AGENT actors and
// empty reasons are rejected; illegal transitions fail with
// INVALID_STATE_TRANSITION.
func (r *Repository) Transition(ctx context.Context, tx pgx.Tx, walletID WalletID, ch StatusChange, now time.Time) (Wallet, error) {
	if err := ch.Validate(); err != nil {
		return Wallet{}, err
	}
	cur, err := r.GetForUpdate(ctx, tx, walletID)
	if err != nil {
		return Wallet{}, err
	}
	if !CanTransition(cur.Status, ch.To) {
		return Wallet{}, errs.Newf(errs.CodeInvalidStateTransition, "wallet status %s -> %s is not allowed", cur.Status, ch.To).
			WithField("from", string(cur.Status)).WithField("to", string(ch.To))
	}
	if _, err := tx.Exec(ctx, `INSERT INTO wallet_status_transitions (id, wallet_id, from_status, to_status, actor_type, actor_id, reason, correlation_id, occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''),$9)`,
		id.New[id.Any](), walletID, string(cur.Status), string(ch.To), string(ch.ActorType), ch.ActorID, ch.Reason, ch.CorrelationID, now.UTC()); err != nil {
		return Wallet{}, fmt.Errorf("wallet: record transition: %w", err)
	}
	// The INSERT above IS the status change. 00745 revoked UPDATE on wallets
	// from cp_app and granted back only the four delegation columns, so there is
	// no statement this function could issue that writes `status` -- and the
	// trigger on wallet_status_transitions has already written it from the row.
	updated, err := scanWallet(tx.QueryRow(ctx, `SELECT `+walletColumns+` FROM wallets WHERE id = $1`, walletID))
	if err != nil {
		return Wallet{}, fmt.Errorf("wallet: read back status: %w", err)
	}
	return updated, nil
}

// MarkDelegationVerified records a successful delegation verification:
// delegation_ref, signing_policy_version and delegation_verified_at, plus
// the capability probe as JSON. It is not a status change and needs no
// transition row. An unverified status clears nothing: revocation is a
// status transition to REVOKED.
func (r *Repository) MarkDelegationVerified(ctx context.Context, tx pgx.Tx, walletID WalletID, st DelegationStatus, capability Capability, now time.Time) (Wallet, error) {
	if !st.Verified() {
		return Wallet{}, errs.Newf(errs.CodeDelegationNotVerified, "delegation state is %s", st.State)
	}
	caps, err := json.Marshal(map[string]any{
		"delegated_signing":  string(capability.DelegatedSigning),
		"verification_label": string(capability.VerificationLabel),
		"policy_id":          st.PolicyID,
		"signer_id":          st.SignerID,
		"allowed_programs":   st.AllowedPrograms,
		"probed_at":          capability.ProbedAt.UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		return Wallet{}, fmt.Errorf("wallet: encode capabilities: %w", err)
	}
	w, err := scanWallet(tx.QueryRow(ctx, `UPDATE wallets SET delegation_ref = NULLIF($2,''), signing_policy_version = NULLIF($3,''),
			delegation_verified_at = $4, capabilities = $5 WHERE id = $1 RETURNING `+walletColumns,
		walletID, st.EvidenceRef, st.PolicyVersion, now.UTC(), caps))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Wallet{}, errs.New(errs.CodeNotFound, "wallet not found")
		}
		return Wallet{}, fmt.Errorf("wallet: mark delegation verified: %w", err)
	}
	return w, nil
}
