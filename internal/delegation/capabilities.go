package delegation

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

// GrantRequest is a customer's grant of a capability to one of the
// account's agents. The environment is the deployment's, never the caller's;
// the strategy is the agent's own.
type GrantRequest struct {
	AccountID             accounts.AccountID
	AgentID               string
	Mode                  gates.Mode
	InstrumentClasses     []InstrumentClass
	ActionClasses         []ActionClass
	MaxNotional           money.USD
	MaxDailyNotional      money.USD
	MaxSingleTrade        money.USD
	DestinationAllowlist  []devnet.PublicKey
	ChainAllowlist        []devnet.ChainID
	ExpiresAt             time.Time
	RequiresHumanApproval bool
	IdempotencyKey        string
}

// GrantCapability records a grant and registers the account's delegated
// wallet key on every chain the grant allows. Only the account's owner,
// freshly strongly authenticated, may grant; the capability is born ACTIVE
// with a unique nonce and a content hash. It is idempotent on (account,
// key): the same terms return the existing grant (existing=true), different
// terms are INVALID_IDEMPOTENCY_REUSE.
//
// The keys are registered here, in the grant's transaction, because this is
// where the signer is: the delegated signer derives each wallet's public key
// and registers it once (DelegatedSigner.EnsureKey), and a key already
// registered for the account that is not the one it derives refuses the grant
// (DELEGATION_SIGNER_KEY_MISMATCH). An agent's admission only reads the
// registered key (admittedWallet), so it needs no signer, and every chain a
// capability can be used on has its key from the moment of the grant.
func (s *Service) GrantCapability(ctx context.Context, tx pgx.Tx, req GrantRequest) (Capability, bool, error) {
	c, existing, err := s.recordGrant(ctx, tx, req)
	if err != nil {
		return Capability{}, false, err
	}
	if err := s.registerWalletKeys(ctx, tx, c.Terms); err != nil {
		return Capability{}, false, err
	}
	return c, existing, nil
}

// registerWalletKeys registers the account's delegated wallet public key on
// each chain terms allow. A replayed grant registers nothing new: the keys
// are already there.
func (s *Service) registerWalletKeys(ctx context.Context, tx pgx.Tx, terms CapabilityTerms) error {
	for _, chain := range terms.ChainAllowlist {
		if _, err := s.signer.EnsureKey(ctx, tx, terms.AccountID, chain); err != nil {
			return err
		}
	}
	return nil
}

// recordGrant is GrantCapability up to the record of the grant: the owner,
// the agent, the terms, the idempotency key, the capability row, its
// transition and its audit event -- and no key.
func (s *Service) recordGrant(ctx context.Context, tx pgx.Tx, req GrantRequest) (Capability, bool, error) {
	if k := req.IdempotencyKey; strings.TrimSpace(k) != k || k == "" || len(k) > 200 {
		return Capability{}, false, errs.New(errs.CodeValidationFailed, "delegation: idempotency key must be 1..200 bytes without surrounding whitespace")
	}
	p, err := s.ownerFor(ctx, tx, req.AccountID, s.params.StepUpMaxAge)
	if err != nil {
		return Capability{}, false, err
	}
	agent, found, err := getAgent(ctx, tx, req.AgentID)
	if err != nil {
		return Capability{}, false, err
	}
	if !found || agent.AccountID != req.AccountID {
		return Capability{}, false, errs.New(errs.CodeForbidden, "delegation: a capability is granted only to one of the account's own agents").
			WithField("reason", ReasonAgentMismatch)
	}
	if !agent.usable() {
		return Capability{}, false, errs.New(errs.CodeValidationFailed, "delegation: the agent is "+agent.State).WithField("reason", ReasonAgentNotActive)
	}
	now := dbTime(s.clk.Now())
	terms := CapabilityTerms{
		AccountID: req.AccountID, AgentID: agent.ID, StrategyID: agent.StrategyID, Environment: s.env, Mode: req.Mode,
		InstrumentClasses: req.InstrumentClasses, ActionClasses: req.ActionClasses, MaxNotional: req.MaxNotional,
		MaxDailyNotional: req.MaxDailyNotional, MaxSingleTrade: req.MaxSingleTrade,
		DestinationAllowlist: req.DestinationAllowlist, ChainAllowlist: req.ChainAllowlist, ExpiresAt: req.ExpiresAt,
		RequiresHumanApproval: req.RequiresHumanApproval,
	}
	if err := terms.Validate(now); err != nil {
		return Capability{}, false, err
	}
	terms = terms.Normalize()
	key, err := termsKey(terms)
	if err != nil {
		return Capability{}, false, err
	}
	existing, err := scanCapability(tx.QueryRow(ctx, `SELECT `+capabilityColumns+` FROM delegated_capabilities
		WHERE account_id = $1 AND idempotency_key = $2`, req.AccountID, req.IdempotencyKey))
	switch {
	case err == nil:
		got, kerr := termsKey(existing.Terms)
		if kerr != nil {
			return Capability{}, false, kerr
		}
		if got != key {
			return Capability{}, false, errs.New(errs.CodeInvalidIdempotencyReuse,
				"delegation: this idempotency key was already used for a different grant").WithField("capability_id", existing.ID.String())
		}
		return existing, true, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return Capability{}, false, dbErr("look up grant", err)
	}
	var nonce [32]byte
	if _, err := io.ReadFull(s.random, nonce[:]); err != nil {
		return Capability{}, false, errs.Wrap(err, errs.CodeInternal, "delegation: draw the capability nonce")
	}
	c := Capability{
		ID: id.New[capabilityKind](), Terms: terms, GrantedAt: now, Nonce: hex.EncodeToString(nonce[:]),
		IdempotencyKey: req.IdempotencyKey, Status: CapabilityActive,
		GrantedBy: Actor{Type: security.ActorUser, ID: p.SubjectID},
	}
	if c.ContentHash, err = c.ComputeContentHash(); err != nil {
		return Capability{}, false, err
	}
	dests := make([]string, 0, len(terms.DestinationAllowlist))
	for _, d := range terms.DestinationAllowlist {
		dests = append(dests, d.String())
	}
	if _, err := tx.Exec(ctx, `INSERT INTO delegated_capabilities
		(id, account_id, agent_id, strategy_id, environment, mode, instrument_classes, action_classes, max_notional_minor,
		 max_daily_notional_minor, max_single_trade_minor, destination_allowlist, chain_allowlist, granted_at, expires_at,
		 nonce, requires_human_approval, content_hash, idempotency_key, status, granted_by_actor_type, granted_by_actor_id)
		VALUES ($1,$2,$3::uuid,$4::uuid,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,'ACTIVE',$20,$21)`,
		c.ID, terms.AccountID, terms.AgentID, terms.StrategyID, terms.Environment, terms.Mode, strs(terms.InstrumentClasses),
		strs(terms.ActionClasses), terms.MaxNotional.Minor(), terms.MaxDailyNotional.Minor(), terms.MaxSingleTrade.Minor(),
		dests, strs(terms.ChainAllowlist), c.GrantedAt, terms.ExpiresAt, c.Nonce, terms.RequiresHumanApproval,
		c.ContentHash[:], c.IdempotencyKey, c.GrantedBy.Type, c.GrantedBy.ID); err != nil {
		return Capability{}, false, dbErr("record grant", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO delegated_capability_transitions
		(id, capability_id, from_status, to_status, actor_type, actor_id, reason)
		VALUES ($1,$2,'NONE','ACTIVE',$3,$4,'granted')`, id.New[id.Any](), c.ID, c.GrantedBy.Type, c.GrantedBy.ID); err != nil {
		return Capability{}, false, dbErr("record grant transition", err)
	}
	if err := s.auditEvent(ctx, tx, c.GrantedBy, "delegation.capability.granted", "delegated_capability", c.ID.String(),
		c.Terms.AccountID, "capability granted", map[string]any{"capability_id": c.ID.String(), "agent_id": terms.AgentID,
			"content_hash": c.HashHex(), "expires_at": terms.ExpiresAt, "mode": string(terms.Mode)}); err != nil {
		return Capability{}, false, err
	}
	stored, err := getCapability(ctx, tx, c.ID, false)
	if err != nil {
		return Capability{}, false, err
	}
	return stored, false, nil
}

// RevokeRequest revokes a capability.
type RevokeRequest struct {
	CapabilityID CapabilityID
	Reason       string
}

// RevokeCapability revokes a capability: the account's owner (strongly
// authenticated) or an operator. Revocation is the one change a capability
// ever makes; revoking a revoked capability returns it unchanged. Nothing
// not yet signed under it can be signed afterwards: the provider, the
// database and the pending-work sweep all refuse.
func (s *Service) RevokeCapability(ctx context.Context, tx pgx.Tx, req RevokeRequest) (Capability, error) {
	reason := strings.TrimSpace(req.Reason)
	if reason == "" || len(reason) > 500 {
		return Capability{}, errs.New(errs.CodeValidationFailed, "delegation: a revocation needs a reason of 1..500 characters")
	}
	c, err := getCapability(ctx, tx, req.CapabilityID, true)
	if err != nil {
		return Capability{}, err
	}
	p, ok := security.PrincipalFrom(ctx)
	if !ok {
		return Capability{}, errs.New(errs.CodeUnauthenticated, "delegation: a revocation needs an authenticated customer or operator")
	}
	var actor Actor
	switch {
	case p.IsAgent():
		return Capability{}, errs.New(errs.CodeForbidden, "delegation: an agent never revokes, widens or extends a capability")
	case p.ActorType == security.ActorOperator:
		actor = Actor{Type: security.ActorOperator, ID: p.SubjectID}
	default:
		if _, err := s.ownerFor(ctx, tx, c.Terms.AccountID, s.params.StepUpMaxAge); err != nil {
			return Capability{}, err
		}
		actor = Actor{Type: security.ActorUser, ID: p.SubjectID}
	}
	if c.Status == CapabilityRevoked {
		return c, nil
	}
	now := dbTime(s.clk.Now())
	if _, err := tx.Exec(ctx, `INSERT INTO delegated_capability_transitions
		(id, capability_id, from_status, to_status, actor_type, actor_id, reason) VALUES ($1,$2,'ACTIVE','REVOKED',$3,$4,$5)`,
		id.New[id.Any](), c.ID, actor.Type, actor.ID, reason); err != nil {
		return Capability{}, dbErr("record revocation transition", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE delegated_capabilities SET status = 'REVOKED', revoked_by_actor_type = $2,
		revoked_by_actor_id = $3, revocation_reason = $4, revoked_at = $5 WHERE id = $1`,
		c.ID, actor.Type, actor.ID, reason, now); err != nil {
		return Capability{}, dbErr("revoke capability", err)
	}
	if err := s.auditEvent(ctx, tx, actor, "delegation.capability.revoked", "delegated_capability", c.ID.String(),
		c.Terms.AccountID, reason, map[string]any{"capability_id": c.ID.String(), "content_hash": c.HashHex()}); err != nil {
		return Capability{}, err
	}
	return getCapability(ctx, tx, c.ID, false)
}

// GetCapability returns one capability.
func (s *Service) GetCapability(ctx context.Context, q db.Querier, cid CapabilityID) (Capability, error) {
	return getCapability(ctx, q, cid, false)
}

// ListCapabilities returns an account's capabilities, newest first.
func (s *admitter) ListCapabilities(ctx context.Context, q db.Querier, account accounts.AccountID) ([]Capability, error) {
	rows, err := q.Query(ctx, `SELECT `+capabilityColumns+` FROM delegated_capabilities WHERE account_id = $1
		ORDER BY created_at DESC, id DESC LIMIT 200`, account)
	if err != nil {
		return nil, dbErr("list capabilities", err)
	}
	defer rows.Close()
	var out []Capability
	for rows.Next() {
		c, err := scanCapability(rows)
		if err != nil {
			return nil, dbErr("scan capability", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
