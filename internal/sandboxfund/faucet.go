package sandboxfund

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/observability"
	"github.com/nodal/controlplane/internal/security"
)

type grantKind struct{}

// GrantID identifies one faucet grant.
type GrantID = id.ID[grantKind]

// NewGrantID returns a fresh grant id.
func NewGrantID() GrantID { return id.New[grantKind]() }

// effectKind is the economic effect kind of a grant.
const effectKind = "SANDBOX_FUNDING"

// MaxIdempotencyKeyLen bounds a grant's idempotency key.
const MaxIdempotencyKeyLen = 200

// faucetActor applies the effect: a deterministic service, never the caller.
var faucetActor = econ.SystemActor("sandboxfund")

// AllowedActorTypes are the actor types that may ask for a grant, and the Go
// twin of the CHECK sandbox_fundings_actor_type_check. A USER may fund only
// an account they own; SERVICE and SYSTEM fund demonstration accounts. An
// AGENT never may, and neither may an OPERATOR: changing a customer's
// balance is an admin-plane act with its own controls.
func AllowedActorTypes() []security.ActorType {
	return []security.ActorType{security.ActorUser, security.ActorService, security.ActorSystem}
}

// Deps are the Faucet's collaborators.
type Deps struct {
	Clock       clock.Clock
	Ledger      *ledger.Service
	Effects     *econ.EffectStore
	Environment config.Environment
	// Enabled comes from deployment configuration. It is consulted only in
	// STAGING and PROD; in LOCAL, TEST and DEV the faucet always runs.
	Enabled bool
	Policy  Policy
	// KillSwitches stops the faucet with the global new-risk kill switch:
	// minting play money is new value, and the operator who pulled the
	// switch meant all of it. Required.
	KillSwitches econ.KillSwitchReader
}

// Faucet grants SIMULATED value to customer accounts.
type Faucet struct {
	clk        clock.Clock
	ledger     *ledger.Service
	effects    *econ.EffectStore
	env        config.Environment
	enabled    bool
	kills      econ.KillSwitchReader
	policy     Policy
	policyHash string
	accounts   *accounts.Repository
	assets     *assets.Repository
}

// New wires a Faucet.
func New(d Deps) (*Faucet, error) {
	if d.Clock == nil || d.Ledger == nil || d.Effects == nil || d.KillSwitches == nil {
		return nil, errs.New(errs.CodeValidationFailed, "sandboxfund: a clock, a ledger, an effect store and a kill-switch reader are required")
	}
	if !d.Environment.IsValid() {
		return nil, errs.Newf(errs.CodeValidationFailed, "sandboxfund: unknown environment %q", d.Environment)
	}
	if err := d.Policy.Validate(); err != nil {
		return nil, err
	}
	hash, err := d.Policy.Hash()
	if err != nil {
		return nil, err
	}
	return &Faucet{
		clk: d.Clock, ledger: d.Ledger, effects: d.Effects, env: d.Environment, enabled: d.Enabled, kills: d.KillSwitches,
		policy: d.Policy, policyHash: hash, accounts: accounts.NewRepository(), assets: assets.NewRepository(),
	}, nil
}

// Grant is a sandbox_fundings row.
type Grant struct {
	ID                   GrantID
	AccountID            accounts.AccountID
	AssetID              assets.AssetID
	Quantity             money.Quantity
	IdempotencyKey       string
	RequestHash          []byte
	PolicyVersion        string
	PolicyHash           string
	Environment          config.Environment
	ActorType            security.ActorType
	ActorID              string
	EffectID             econ.EffectID
	JournalTransactionID ledger.TransactionID
	EvidenceRef          string
	EvidenceSHA256       []byte
	CorrelationID        string
	GrantedAt            time.Time
	// Replayed is true when this is the grant an earlier identical request
	// made; nothing new was written.
	Replayed bool
}

// Fund grants quantity base units of a SIMULATED asset to an account:
// SANDBOX_FUNDING, Dr WALLET / Cr CAPITAL, with an economic effect, evidence
// and a grant record, all in tx. It is refused outside the environments
// Allowed permits, for any asset that is not SIMULATED, for a prediction
// market's outcome share and for any asset whose supply a controller owns (a
// platform stable asset, a bridged twin), for an account that is not an
// ACTIVE customer account, and beyond the policy's rolling cap. A
// supply-controlled asset is refused through econ.RefuseSupplyControlled
// (ASSET_RESTRICTED, refusal SUPPLY_CONTROLLED_ASSET). It is idempotent on
// (account, idempotency key).
func (f *Faucet) Fund(ctx context.Context, tx pgx.Tx, p security.Principal, accountID accounts.AccountID, assetID assets.AssetID, quantity money.Quantity, idempotencyKey string) (Grant, error) {
	if tx == nil {
		return Grant{}, errs.New(errs.CodeInternal, "sandboxfund: Fund requires a transaction")
	}
	if !Allowed(f.env, f.enabled) {
		return Grant{}, errs.Newf(errs.CodeCapabilityNotApproved, "the sandbox faucet is not enabled in %s", f.env).
			WithField("environment", string(f.env))
	}
	if err := f.authorize(ctx, p, accountID); err != nil {
		return Grant{}, err
	}
	snap, err := f.kills.Snapshot(ctx, tx)
	if err != nil {
		return Grant{}, err
	}
	if snap.Global() {
		return Grant{}, errs.New(errs.CodeKillSwitchActive, "the global new-risk kill switch stops the sandbox faucet")
	}
	if !quantity.IsPositive() {
		return Grant{}, errs.New(errs.CodeValidationFailed, "a grant is a positive quantity of base units")
	}
	if k := strings.TrimSpace(idempotencyKey); k == "" || k != idempotencyKey || len(k) > MaxIdempotencyKeyLen || !utf8.ValidString(k) {
		return Grant{}, errs.New(errs.CodeValidationFailed, "idempotency key must be 1..200 bytes of UTF-8 without surrounding whitespace")
	}
	request := map[string]string{"account_id": accountID.String(), "asset_id": assetID.String(), "quantity": quantity.String()}
	reqHash, err := econ.RequestHash(request)
	if err != nil {
		return Grant{}, err
	}

	// One grant decision at a time per account, then replay.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "sandboxfund.account:"+accountID.String()); err != nil {
		return Grant{}, dbErr("lock account for funding", err)
	}
	if g, found, err := grantByKey(ctx, tx, accountID, idempotencyKey); err != nil {
		return Grant{}, err
	} else if found {
		if !bytes.Equal(g.RequestHash, reqHash) {
			return Grant{}, errs.New(errs.CodeInvalidIdempotencyReuse, "this idempotency key was already used for a different grant").
				WithField("grant_id", g.ID.String())
		}
		g.Replayed = true
		return g, nil
	}

	acct, err := f.accounts.Get(ctx, tx, accountID)
	if err != nil {
		if errs.CodeOf(err) == errs.CodeNotFound {
			return Grant{}, errs.New(errs.CodeNotFound, "account not found")
		}
		return Grant{}, dbErr("load account", err)
	}
	if acct.Kind != accounts.KindCustomer {
		return Grant{}, errs.New(errs.CodeForbidden, "the faucet funds customer accounts only")
	}
	switch acct.Status {
	case accounts.StatusActive:
	case accounts.StatusFrozen:
		return Grant{}, errs.New(errs.CodeAccountFrozen, "the account is frozen")
	default:
		return Grant{}, errs.Newf(errs.CodeForbidden, "the account is %s", acct.Status)
	}
	asset, err := f.assets.Get(ctx, tx, assetID)
	if err != nil {
		if errs.CodeOf(err) == errs.CodeNotFound {
			return Grant{}, errs.New(errs.CodeNotFound, "asset not found")
		}
		return Grant{}, dbErr("load asset", err)
	}
	if asset.Kind != assets.KindSimulated {
		return Grant{}, errs.New(errs.CodeForbidden, "the sandbox faucet grants SIMULATED assets only").
			WithField("asset_id", asset.ID.String()).WithField("asset_kind", string(asset.Kind))
	}
	if asset.Status != assets.StatusActive {
		return Grant{}, errs.Newf(errs.CodeAssetRestricted, "asset is %s", asset.Status)
	}
	if share, err := isPredictionShare(ctx, tx, asset.ID); err != nil {
		return Grant{}, err
	} else if share {
		return Grant{}, errs.New(errs.CodeForbidden,
			"the sandbox faucet never grants a prediction-market outcome share: a share exists only as its market issues it, "+
				"in complete sets against collateral").
			WithField("asset_id", asset.ID.String()).WithField("refusal", RefusalPredictionShare)
	}
	// One granted unit of a supply-controlled asset would be a unit no
	// issuance backs: the stable asset's holdings would exceed what its
	// issuer issued and its supply reconciliation would pause issuance, a
	// twin's would exceed what its bridge locked. The database refuses the
	// grant's journal anyway (SU001, migration 00763); the shared check names
	// the reason before anything is written.
	if err := econ.RefuseSupplyControlled(ctx, tx, asset.ID, "sandboxfund: a faucet grant",
		"a granted unit would be one no issuance backs, and the database refuses a SANDBOX_FUNDING journal for such an "+
			"asset (SU001, migration 00763); it reaches a customer only through its controller (a mint, a bridge release) "+
			"or from another holder"); err != nil {
		return Grant{}, err
	}

	now := f.clk.Now().UTC()
	used, err := UsedInWindow(ctx, tx, accountID, assetID, now.Add(-f.policy.Window()))
	if err != nil {
		return Grant{}, err
	}
	if err := f.policy.Admit(used, quantity, asset.Decimals); err != nil {
		return Grant{}, err
	}

	gid := NewGrantID()
	correlation := observability.CorrelationID(ctx)
	eff, replay, err := f.effects.Begin(ctx, tx, econ.BeginRequest{
		Key: "sandbox-funding:" + accountID.String() + ":" + idempotencyKey, Subsystem: econ.SubsystemSandboxFunding,
		Kind: effectKind, AccountID: accountID.String(), Request: request, PostsLedger: true, Actor: faucetActor,
		CorrelationID: correlation,
	})
	if err != nil {
		return Grant{}, err
	}
	if replay {
		return Grant{}, errs.New(errs.CodeConflict, "a funding effect exists for this key without its grant").
			WithField("effect_id", eff.ID.String())
	}
	post, err := f.ledger.Post(ctx, tx, ledger.Posting{
		Kind: ledger.KindSandboxFunding, IdempotencyKey: "sandbox-funding:" + gid.String(),
		Reference:   ledger.FinancialEventReference{Type: "sandbox_funding", ID: gid.String()},
		EffectiveAt: now, Description: "sandbox faucet grant of simulated value", CorrelationID: correlation,
		Entries: []ledger.Entry{
			{Account: ledger.CustomerAccount(accountID, ledger.CodeWallet, assetID), Side: ledger.Debit, Quantity: quantity},
			{Account: ledger.CustomerAccount(accountID, ledger.CodeCapital, assetID), Side: ledger.Credit, Quantity: quantity},
		},
	})
	if err != nil {
		return Grant{}, err
	}
	actorType, actorID := p.ActorType, p.SubjectID
	ref, err := econ.RecordEvidence(ctx, tx, econ.Evidence{Key: "sandboxfund/" + gid.String(), Document: grantEvidence{
		Schema: "nodal.sandboxfund.grant/v1", GrantID: gid.String(), AccountID: accountID.String(),
		AssetID: assetID.String(), AssetSymbol: asset.Symbol, AssetKind: string(asset.Kind), Quantity: quantity,
		UsedBefore: used, Cap: f.policy.Cap(asset.Decimals), Policy: f.policy, PolicyHash: f.policyHash,
		Environment: string(f.env), Enabled: f.enabled, ActorType: string(actorType), ActorID: actorID,
		JournalTransactionID: post.TransactionID.String(), GrantedAt: now,
	}})
	if err != nil {
		return Grant{}, err
	}
	if _, err := f.effects.Apply(ctx, tx, eff.ID, econ.ApplyRequest{
		JournalTransactionID: &post.TransactionID, Evidence: &ref, Actor: faucetActor, Reason: "sandbox faucet grant posted",
	}); err != nil {
		return Grant{}, err
	}
	g, err := scanGrant(tx.QueryRow(ctx, `INSERT INTO sandbox_fundings
		(id, account_id, asset_id, quantity, idempotency_key, request_hash, policy_version, policy_hash, environment,
		 actor_type, actor_id, effect_id, journal_transaction_id, evidence_ref, evidence_sha256, correlation_id, granted_at)
		VALUES ($1,$2,$3,$4::numeric,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,NULLIF($16,''),$17)
		RETURNING `+grantColumns,
		gid, accountID, assetID, quantity.String(), idempotencyKey, reqHash, f.policy.Version, f.policyHash, string(f.env),
		string(actorType), actorID, eff.ID, post.TransactionID, ref.URI, ref.SHA256, correlation, now))
	if err != nil {
		return Grant{}, dbErr("record grant", err)
	}
	return g, nil
}

// authorize decides who may ask: never an agent or an operator; a user only
// for an account they own; a service only when it holds sandbox:fund; the
// system (internal background work, unreachable from HTTP) for any account.
func (f *Faucet) authorize(ctx context.Context, p security.Principal, accountID accounts.AccountID) error {
	if err := p.Validate(); err != nil {
		return errs.Wrap(err, errs.CodeForbidden, "invalid principal")
	}
	switch p.ActorType {
	case security.ActorUser:
		if err := security.RequireAccountOwner(security.WithPrincipal(ctx, p), accountID.String()); err != nil {
			if errors.Is(err, security.ErrUnauthenticated) {
				return errs.Wrap(err, errs.CodeUnauthenticated, "authentication required")
			}
			return errs.Wrap(err, errs.CodeForbidden, "a customer funds only an account they own")
		}
		return nil
	case security.ActorService:
		if !p.Has(security.PermSandboxFund, f.clk.Now()) {
			return errs.New(errs.CodeForbidden, "a service funds an account only when it holds sandbox:fund").
				WithField("actor_type", string(p.ActorType))
		}
		return nil
	case security.ActorSystem:
		return nil
	}
	return errs.Newf(errs.CodeForbidden, "a %s does not use the sandbox faucet", p.ActorType).
		WithField("actor_type", string(p.ActorType))
}

// RefusalPredictionShare names the faucet's refusal of a prediction-market
// outcome share (the refusal field of the error).
const RefusalPredictionShare = "PREDICTION_MARKET_SHARE"

// isPredictionShare reports whether an asset is a registered prediction-market
// outcome share (internal/predmarket's prediction_outcomes_registry). A share
// exists only as its market issues it: one granted unit would leave the
// market's customer holdings above its outstanding shares, and from then on
// its escrow would refuse every issuance, redemption, settlement and void for
// every holder. The database refuses the grant's journal anyway (PM008); this
// check names the reason before anything is written.
func isPredictionShare(ctx context.Context, q db.Querier, asset assets.AssetID) (bool, error) {
	var share bool
	if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM prediction_outcomes_registry WHERE share_asset_id = $1)`,
		asset).Scan(&share); err != nil {
		return false, dbErr("check prediction shares", err)
	}
	return share, nil
}

// UsedInWindow is what an account was granted of an asset after since.
func UsedInWindow(ctx context.Context, q db.Querier, account accounts.AccountID, asset assets.AssetID, since time.Time) (money.Quantity, error) {
	var text string
	if err := q.QueryRow(ctx, `SELECT coalesce(sum(quantity), 0)::text FROM sandbox_fundings
		WHERE account_id = $1 AND asset_id = $2 AND granted_at > $3`, account, asset, since).Scan(&text); err != nil {
		return money.Quantity{}, dbErr("read faucet usage", err)
	}
	v, err := money.ScanQuantity(text)
	if err != nil {
		return money.Quantity{}, errs.Wrap(err, errs.CodeInternal, "sandboxfund: stored grant total is not an integer")
	}
	return v, nil
}

// Policy returns the faucet policy in force and its hash.
func (f *Faucet) Policy() (Policy, string) { return f.policy, f.policyHash }

const grantColumns = `id, account_id, asset_id, quantity::text, idempotency_key, request_hash, policy_version, policy_hash,
	environment, actor_type, actor_id, effect_id, journal_transaction_id, evidence_ref, evidence_sha256,
	coalesce(correlation_id,''), granted_at`

func scanGrant(row pgx.Row) (Grant, error) {
	var g Grant
	var qty string
	if err := row.Scan(&g.ID, &g.AccountID, &g.AssetID, &qty, &g.IdempotencyKey, &g.RequestHash, &g.PolicyVersion,
		&g.PolicyHash, &g.Environment, &g.ActorType, &g.ActorID, &g.EffectID, &g.JournalTransactionID, &g.EvidenceRef,
		&g.EvidenceSHA256, &g.CorrelationID, &g.GrantedAt); err != nil {
		return Grant{}, err
	}
	v, err := money.ScanQuantity(qty)
	if err != nil {
		return Grant{}, err
	}
	g.Quantity = v
	g.GrantedAt = g.GrantedAt.UTC()
	return g, nil
}

func grantByKey(ctx context.Context, q db.Querier, account accounts.AccountID, key string) (Grant, bool, error) {
	g, err := scanGrant(q.QueryRow(ctx, `SELECT `+grantColumns+` FROM sandbox_fundings
		WHERE account_id = $1 AND idempotency_key = $2`, account, key))
	if errors.Is(err, pgx.ErrNoRows) {
		return Grant{}, false, nil
	}
	if err != nil {
		return Grant{}, false, dbErr("load grant", err)
	}
	return g, true, nil
}

// grantEvidence is the canonical evidence document of one grant.
type grantEvidence struct {
	Schema               string         `json:"schema"`
	GrantID              string         `json:"grant_id"`
	AccountID            string         `json:"account_id"`
	AssetID              string         `json:"asset_id"`
	AssetSymbol          string         `json:"asset_symbol"`
	AssetKind            string         `json:"asset_kind"`
	Quantity             money.Quantity `json:"quantity"`
	UsedBefore           money.Quantity `json:"used_in_window_before"`
	Cap                  money.Quantity `json:"cap"`
	Policy               Policy         `json:"policy"`
	PolicyHash           string         `json:"policy_hash"`
	Environment          string         `json:"environment"`
	Enabled              bool           `json:"enabled_by_configuration"`
	ActorType            string         `json:"actor_type"`
	ActorID              string         `json:"actor_id"`
	JournalTransactionID string         `json:"journal_transaction_id"`
	GrantedAt            time.Time      `json:"granted_at"`
}

func dbErr(op string, err error) error {
	if db.IsRetryable(err) {
		return err
	}
	if ee, ok := errs.As(err); ok {
		return ee
	}
	if mapped := ledger.MapError(err); mapped != nil {
		if ee, ok := errs.As(mapped); ok && ee.Code != errs.CodeInternal {
			return ee
		}
	}
	return errs.Wrap(err, errs.CodeInternal, "sandboxfund: "+op)
}
