package treasury

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/orders"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuation"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// Config wires a Service. Every dependency is required: a treasury that could
// not reach one of them would skip a control.
type Config struct {
	Clock        clock.Clock
	Ledger       *ledger.Service
	Effects      *econ.EffectStore
	Authorizer   *econ.Authorizer
	Registry     *econ.Registry
	Orders       *orders.Service
	Prices       valuation.PriceSource
	Assets       *assets.Repository
	Instruments  *instruments.Repository
	KillSwitches econ.KillSwitchReader
	Audit        audit.Writer
	// RiskPolicy is the Risk Kernel V2 policy house orders are evaluated
	// under (risk.DefaultAdvancedPolicy is the sandbox one).
	RiskPolicy risk.AdvancedPolicy
	// FundingMarkMaxAge bounds the mark that values non-cash funding when the
	// book has no risk policy yet (default 60s); a policy's MaxMarkAge
	// applies once there is one.
	FundingMarkMaxAge time.Duration
}

// Service is the treasury. It holds no financial state: the database is the
// truth, and every command runs in the caller's transaction under the house
// book's row lock.
type Service struct {
	clk            clock.Clock
	ledger         *ledger.Service
	effects        *econ.EffectStore
	auth           *econ.Authorizer
	registry       *econ.Registry
	orders         *orders.Service
	prices         valuation.PriceSource
	assets         *assets.Repository
	instr          *instruments.Repository
	kills          econ.KillSwitchReader
	audit          audit.Writer
	policy         risk.AdvancedPolicy
	fundingMarkAge time.Duration
}

// NewService validates the configuration and returns a Service.
func NewService(c Config) (*Service, error) {
	if c.Clock == nil || c.Ledger == nil || c.Effects == nil || c.Authorizer == nil || c.Registry == nil || c.Orders == nil ||
		c.Prices == nil || c.Assets == nil || c.Instruments == nil || c.KillSwitches == nil || c.Audit == nil {
		return nil, errs.New(errs.CodeValidationFailed, "treasury: every dependency of the treasury is required")
	}
	if c.RiskPolicy.Missing() {
		return nil, errs.New(errs.CodeValidationFailed, "treasury: a Risk Kernel V2 policy is required")
	}
	if c.FundingMarkMaxAge <= 0 {
		c.FundingMarkMaxAge = time.Minute
	}
	return &Service{
		clk: c.Clock, ledger: c.Ledger, effects: c.Effects, auth: c.Authorizer, registry: c.Registry, orders: c.Orders,
		prices: c.Prices, assets: c.Assets, instr: c.Instruments, kills: c.KillSwitches, audit: c.Audit,
		policy: c.RiskPolicy, fundingMarkAge: c.FundingMarkMaxAge,
	}, nil
}

// Book is a treasury book: a house book with its treasury registration.
type Book struct {
	HouseAccountID econ.HouseAccountID
	Name           string
	Strategy       econ.HouseStrategy
	Class          Class
	Status         econ.HouseStatus
	ValuationAsset assets.AssetID
	Version        int64
	RegisteredBy   econ.Actor
	RegisteredAt   time.Time
}

// account is the ledger reference of one of the book's accounts.
func (b Book) account(code ledger.Code, asset assets.AssetID) ledger.AccountRef {
	return ledger.AccountRef{OwnerType: ledger.OwnerHouse, OwnerID: b.HouseAccountID.String(), Code: code, AssetID: asset}
}

const bookColumns = `tb.house_account_id, ha.name, ha.strategy, tb.class, ha.status, tb.valuation_asset_id, tb.version,
	tb.registered_by_actor_type, tb.registered_by_actor_id, tb.registered_at`

func scanBook(row pgx.Row) (Book, error) {
	var b Book
	if err := row.Scan(&b.HouseAccountID, &b.Name, &b.Strategy, &b.Class, &b.Status, &b.ValuationAsset, &b.Version,
		&b.RegisteredBy.Type, &b.RegisteredBy.ID, &b.RegisteredAt); err != nil {
		return Book{}, err
	}
	b.RegisteredAt = b.RegisteredAt.UTC()
	return b, nil
}

// GetBook returns a treasury book.
func (s *Service) GetBook(ctx context.Context, q db.Querier, house econ.HouseAccountID) (Book, error) {
	b, err := scanBook(q.QueryRow(ctx, `SELECT `+bookColumns+` FROM treasury_books tb JOIN house_accounts ha ON ha.id = tb.house_account_id
		WHERE tb.house_account_id = $1`, house))
	if err != nil {
		return Book{}, notFound(err, "treasury book", house.String())
	}
	return b, nil
}

// lockBook takes the row lock that serializes every command of a house book
// and advances its version. FOR NO KEY UPDATE conflicts with itself and with
// every update of the row, which is all the serialization needs.
func (s *Service) lockBook(ctx context.Context, tx pgx.Tx, house econ.HouseAccountID) (Book, error) {
	b, err := scanBook(tx.QueryRow(ctx, `SELECT `+bookColumns+` FROM treasury_books tb JOIN house_accounts ha ON ha.id = tb.house_account_id
		WHERE tb.house_account_id = $1 FOR NO KEY UPDATE OF tb`, house))
	if err != nil {
		return Book{}, notFound(err, "treasury book", house.String())
	}
	if _, err := tx.Exec(ctx, `UPDATE treasury_books SET version = version + 1 WHERE house_account_id = $1`, house); err != nil {
		return Book{}, dbErr("advance treasury book version", err)
	}
	b.Version++
	return b, nil
}

// RegisterRequest creates a house book and registers it with the treasury.
type RegisterRequest struct {
	Name           string
	Strategy       econ.HouseStrategy
	ValuationAsset assets.AssetID
	// Signing is the book's first signing policy.
	Signing        SigningPolicy
	IdempotencyKey string
	Reason         string
	Actor          econ.Actor
}

// Register creates a house book (econ.Registry.CreateHouseAccount), registers
// it with its class, valuation asset and first signing policy, and records the
// REGISTER action. Only an operator registers a house book. The valuation
// asset must be a SIMULATED, USD-pegged asset (the sandbox dollar normally):
// risk limits are stated in USD at parity.
func (s *Service) Register(ctx context.Context, tx pgx.Tx, r RegisterRequest) (Book, error) {
	if r.Actor.Type != security.ActorOperator || strings.TrimSpace(r.Actor.ID) == "" {
		return Book{}, rejection(errs.CodeForbidden, ReasonNotAuthorized, "only an operator registers a house book")
	}
	if err := validKey(r.IdempotencyKey); err != nil {
		return Book{}, err
	}
	if strings.TrimSpace(r.Reason) == "" {
		return Book{}, errs.New(errs.CodeValidationFailed, "treasury: a registration needs a reason")
	}
	class, ok := ClassOf(r.Strategy)
	if !ok {
		return Book{}, errs.Newf(errs.CodeValidationFailed, "treasury: unknown house strategy %q", r.Strategy)
	}
	if err := r.Signing.Validate(); err != nil {
		return Book{}, err
	}
	asset, err := s.assets.Get(ctx, tx, r.ValuationAsset)
	if err != nil {
		return Book{}, err
	}
	if asset.Kind != assets.KindSimulated || asset.ValueDomain != valuedomain.Simulated || !asset.IsStablecoin ||
		asset.PegCurrency != valuation.USDPeg {
		return Book{}, rejection(errs.CodeValidationFailed, ReasonValuationAsset,
			"a house book is valued in a SIMULATED, USD-pegged asset")
	}
	registrationHash := func(house econ.HouseAccountID) ([]byte, error) {
		doc, err := r.Signing.Document(house)
		if err != nil {
			return nil, err
		}
		return econ.RequestHash(map[string]any{
			"name": strings.TrimSpace(r.Name), "strategy": string(r.Strategy), "valuation_asset": asset.ID.String(),
			"signing_policy_hash": hashHex(doc),
		})
	}
	// A replay of a registration that committed returns the book it created.
	var existing econ.HouseAccountID
	switch err := tx.QueryRow(ctx, `SELECT id FROM house_accounts WHERE name = $1`, strings.TrimSpace(r.Name)).Scan(&existing); {
	case err == nil:
		prior, found, err := s.actionByKey(ctx, tx, existing, r.IdempotencyKey)
		if err != nil {
			return Book{}, err
		}
		if !found {
			return Book{}, errs.New(errs.CodeConflict, "treasury: a house book with this name already exists")
		}
		want, err := registrationHash(existing)
		if err != nil {
			return Book{}, err
		}
		if err := sameRequest(prior, ActionRegister, want); err != nil {
			return Book{}, err
		}
		return s.GetBook(ctx, tx, existing)
	case !errors.Is(err, pgx.ErrNoRows):
		return Book{}, dbErr("look up house book", err)
	}
	house, err := s.registry.CreateHouseAccount(ctx, tx, r.Name, r.Strategy, r.Actor)
	if err != nil {
		return Book{}, err
	}
	now := s.clk.Now().UTC()
	if _, err := tx.Exec(ctx, `INSERT INTO treasury_books (house_account_id, class, valuation_asset_id, registered_by_actor_type,
		registered_by_actor_id, registered_at) VALUES ($1,$2,$3,$4,$5,$6)`,
		house.ID, class, asset.ID, r.Actor.Type, r.Actor.ID, now); err != nil {
		return Book{}, dbErr("register treasury book", err)
	}
	sp, err := s.insertSigningPolicy(ctx, tx, house.ID, 1, r.Signing, r.Actor, r.Reason, now)
	if err != nil {
		return Book{}, err
	}
	reqHash, err := registrationHash(house.ID)
	if err != nil {
		return Book{}, err
	}
	book, err := s.lockBook(ctx, tx, house.ID)
	if err != nil {
		return Book{}, err
	}
	a := Action{
		ID: id.New[actionKind](), HouseAccountID: house.ID, Kind: ActionRegister, Outcome: OutcomeApplied, Actor: r.Actor,
		SigningPolicyVersion: sp.Version, IdempotencyKey: r.IdempotencyKey, RequestHash: reqHash, SigningPolicyID: &sp.ID,
		OccurredAt: now,
	}
	if err := s.recordAction(ctx, tx, book, &a, r.Reason, map[string]any{
		"class": string(class), "strategy": string(r.Strategy), "valuation_asset": asset.ID.String(),
		"signing_policy_version": sp.Version, "signing_policy_hash": sp.Hash, "key_custody": KeyCustodyNone,
	}); err != nil {
		return Book{}, err
	}
	return book, nil
}

// SetSigningPolicy records the next version of a house book's signing policy.
// Only an operator changes it.
func (s *Service) SetSigningPolicy(ctx context.Context, tx pgx.Tx, house econ.HouseAccountID, p SigningPolicy, idempotencyKey,
	reason string, actor econ.Actor) (SigningPolicyVersion, error) {
	if err := validKey(idempotencyKey); err != nil {
		return SigningPolicyVersion{}, err
	}
	if strings.TrimSpace(reason) == "" {
		return SigningPolicyVersion{}, errs.New(errs.CodeValidationFailed, "treasury: a signing policy change needs a reason")
	}
	doc, err := p.Document(house)
	if err != nil {
		return SigningPolicyVersion{}, err
	}
	book, err := s.lockBook(ctx, tx, house)
	if err != nil {
		return SigningPolicyVersion{}, err
	}
	cur, err := s.currentSigningPolicy(ctx, tx, house)
	if err != nil {
		return SigningPolicyVersion{}, err
	}
	if err := cur.Authorize(actor, ActionSetSigningPolicy); err != nil {
		return SigningPolicyVersion{}, err
	}
	reqHash, err := econ.RequestHash(map[string]any{"signing_policy_hash": hashHex(doc)})
	if err != nil {
		return SigningPolicyVersion{}, err
	}
	if prior, found, err := s.actionByKey(ctx, tx, house, idempotencyKey); err != nil {
		return SigningPolicyVersion{}, err
	} else if found {
		if err := sameRequest(prior, ActionSetSigningPolicy, reqHash); err != nil {
			return SigningPolicyVersion{}, err
		}
		return s.signingPolicyByID(ctx, tx, *prior.SigningPolicyID)
	}
	now := s.clk.Now().UTC()
	next, err := s.insertSigningPolicy(ctx, tx, house, cur.Version+1, p, actor, reason, now)
	if err != nil {
		return SigningPolicyVersion{}, err
	}
	a := Action{
		ID: id.New[actionKind](), HouseAccountID: house, Kind: ActionSetSigningPolicy, Outcome: OutcomeApplied, Actor: actor,
		SigningPolicyVersion: next.Version, IdempotencyKey: idempotencyKey, RequestHash: reqHash, SigningPolicyID: &next.ID,
		OccurredAt: now,
	}
	if err := s.recordAction(ctx, tx, book, &a, reason, map[string]any{
		"signing_policy_version": next.Version, "signing_policy_hash": next.Hash, "previous_version": cur.Version,
	}); err != nil {
		return SigningPolicyVersion{}, err
	}
	return next, nil
}

// SetPolicy records the next version of a house book's risk policy. Only an
// operator changes it; the policy must be valued in the book's valuation
// asset.
func (s *Service) SetPolicy(ctx context.Context, tx pgx.Tx, house econ.HouseAccountID, p Policy, idempotencyKey, reason string,
	actor econ.Actor) (PolicyVersion, error) {
	if err := validKey(idempotencyKey); err != nil {
		return PolicyVersion{}, err
	}
	if strings.TrimSpace(reason) == "" {
		return PolicyVersion{}, errs.New(errs.CodeValidationFailed, "treasury: a policy change needs a reason")
	}
	doc, err := p.Document(house)
	if err != nil {
		return PolicyVersion{}, err
	}
	book, err := s.lockBook(ctx, tx, house)
	if err != nil {
		return PolicyVersion{}, err
	}
	if p.ValuationAsset != book.ValuationAsset {
		return PolicyVersion{}, rejection(errs.CodeValidationFailed, ReasonValuationAsset,
			"a policy is stated in the book's valuation asset")
	}
	sp, err := s.currentSigningPolicy(ctx, tx, house)
	if err != nil {
		return PolicyVersion{}, err
	}
	if err := sp.Authorize(actor, ActionSetPolicy); err != nil {
		return PolicyVersion{}, err
	}
	reqHash, err := econ.RequestHash(map[string]any{"policy_hash": hashHex(doc)})
	if err != nil {
		return PolicyVersion{}, err
	}
	if prior, found, err := s.actionByKey(ctx, tx, house, idempotencyKey); err != nil {
		return PolicyVersion{}, err
	} else if found {
		if err := sameRequest(prior, ActionSetPolicy, reqHash); err != nil {
			return PolicyVersion{}, err
		}
		return s.policyByID(ctx, tx, *prior.PolicyID)
	}
	cur, found, err := s.currentPolicy(ctx, tx, house)
	if err != nil {
		return PolicyVersion{}, err
	}
	version := 1
	if found {
		version = cur.Version + 1
	}
	now := s.clk.Now().UTC()
	pv := PolicyVersion{
		ID: id.New[policyKind](), HouseAccountID: house, Version: version, Policy: p, Document: doc, Hash: hashHex(doc),
		Actor: actor, Reason: reason, EffectiveAt: now,
	}
	if _, err := tx.Exec(ctx, `INSERT INTO treasury_policies (id, house_account_id, version, label, valuation_asset_id,
		daily_loss_limit, max_mark_age_ms, policy, policy_hash, actor_type, actor_id, reason, effective_at)
		VALUES ($1,$2,$3,$4,$5,$6::numeric,$7,$8,$9,$10,$11,$12,$13)`,
		pv.ID, house, version, p.Label, p.ValuationAsset, p.DailyLossLimit.String(), p.MaxMarkAge.Milliseconds(), doc, pv.Hash,
		actor.Type, actor.ID, reason, now); err != nil {
		return PolicyVersion{}, dbErr("record treasury policy", err)
	}
	a := Action{
		ID: id.New[actionKind](), HouseAccountID: house, Kind: ActionSetPolicy, Outcome: OutcomeApplied, Actor: actor,
		SigningPolicyVersion: sp.Version, RiskPolicyVersion: version, IdempotencyKey: idempotencyKey, RequestHash: reqHash,
		PolicyID: &pv.ID, OccurredAt: now,
	}
	if err := s.recordAction(ctx, tx, book, &a, reason, map[string]any{
		"policy_version": version, "policy_hash": pv.Hash, "label": p.Label,
	}); err != nil {
		return PolicyVersion{}, err
	}
	return pv, nil
}

// requireCapability asks the Authorizer whether the house book may exercise a
// capability in a mode, and turns a refusal into a stable reason.
func (s *Service) requireCapability(ctx context.Context, tx pgx.Tx, c gates.Capability, m gates.Mode) error {
	err := s.auth.Require(ctx, tx, c, m)
	if err == nil {
		return nil
	}
	ee, ok := errs.As(err)
	if !ok {
		return err
	}
	reason := ReasonCapability
	if ee.Code == errs.CodeKillSwitchActive {
		reason = ReasonKillSwitch
	}
	return ee.WithField("reason", reason)
}

// postingContext attributes a posting to the actor that took the house
// action: never an agent (the signing policy has already refused one).
func postingContext(ctx context.Context, actor econ.Actor) context.Context {
	return security.WithPrincipal(ctx, security.Principal{SubjectID: actor.ID, ActorType: actor.Type})
}
