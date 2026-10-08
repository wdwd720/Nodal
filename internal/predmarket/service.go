package predmarket

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/legalrouter"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/reconciliation"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// serviceActor is who applies every economic effect: a deterministic
// service, never the agent or person who asked for it.
const serviceActorID = "predmarket"

func serviceActor() econ.Actor { return econ.SystemActor(serviceActorID) }

// AccountFacts are the account facts Risk V2 and the legal router key on.
type AccountFacts struct {
	Status       accounts.Status
	Verification valuedomain.VerificationLevel
	// Jurisdiction is empty when the platform has not established one; the
	// legal router then keys on UNKNOWN, which the simulation rule permits.
	Jurisdiction string
}

// AccountFactsReader resolves AccountFacts inside the caller's transaction.
type AccountFactsReader interface {
	Facts(ctx context.Context, q db.Querier, account accounts.AccountID) (AccountFacts, error)
}

// DBAccountFacts reads AccountFacts from accounts and users in the caller's
// transaction. Verification follows identity.NodalIdentityResolver exactly
// -- NODAL_IDENTITY when the owning user is ACTIVE and the identity provider
// asserted a verified email, NONE otherwise -- but reads through the caller's
// querier, so it never needs a second pooled connection while a transaction
// holds one.
type DBAccountFacts struct{}

// Facts implements AccountFactsReader. A missing account is NOT_FOUND.
func (DBAccountFacts) Facts(ctx context.Context, q db.Querier, account accounts.AccountID) (AccountFacts, error) {
	var (
		acct      accounts.Status
		user      string
		haveEmail bool
	)
	err := q.QueryRow(ctx, `SELECT a.status, u.status, (u.email_hash IS NOT NULL)
		FROM accounts a JOIN users u ON u.id = a.owner_user_id WHERE a.id = $1`, account).Scan(&acct, &user, &haveEmail)
	if errors.Is(err, pgx.ErrNoRows) {
		return AccountFacts{}, errs.New(errs.CodeNotFound, "predmarket: account not found").WithField("account_id", account.String())
	}
	if err != nil {
		return AccountFacts{}, dbErr("read account facts", err)
	}
	f := AccountFacts{Status: acct, Verification: valuedomain.VerificationNone}
	if user == "ACTIVE" && haveEmail {
		f.Verification = valuedomain.VerificationNodalIdentity
	}
	return f, nil
}

// Config wires a Service. Every dependency except Oracle is required.
type Config struct {
	Clock          clock.Clock
	Intents        *econ.IntentStore
	Effects        *econ.EffectStore
	Authorizer     *econ.Authorizer
	Registry       *econ.Registry
	KillSwitches   econ.KillSwitchReader
	Ledger         *ledger.Service
	Capital        *capital.Service
	Risk           *risk.Store
	RiskPolicy     risk.AdvancedPolicy
	Router         *legalrouter.Router
	Instruments    *instruments.Repository
	Assets         *assets.Repository
	Reconciliation *reconciliation.Repository
	Audit          audit.Writer
	// Accounts defaults to DBAccountFacts.
	Accounts AccountFactsReader
	// Oracle is the external provider FetchReport asks for a market whose
	// oracle is not the sandbox oracle. Optional: a deployment that receives
	// reports by push decodes them with DecodeOracleReport instead.
	Oracle PredictionOracleProvider
	// SandboxOracle signs the observations operators record with
	// RecordObservation; every observation is stored, and FetchReport reads
	// the latest stored one back, verified. Optional: without it no
	// observation can be recorded.
	SandboxOracle *SandboxOracle
	// Books cancels resting orders on a market's outcome instruments at
	// close, void and settlement. Optional: without it a settlement or void
	// that finds outcome shares still reserved is refused, naming the
	// reservations.
	Books OutcomeBooks
}

// Service runs prediction markets. It holds no mutable state: every method
// runs in the caller's transaction and all truth is in the database.
type Service struct {
	clk      clock.Clock
	intents  *econ.IntentStore
	effects  *econ.EffectStore
	authz    *econ.Authorizer
	registry *econ.Registry
	kills    econ.KillSwitchReader
	ledger   *ledger.Service
	capital  *capital.Service
	risk     *risk.Store
	policy   risk.AdvancedPolicy
	router   *legalrouter.Router
	instr    *instruments.Repository
	assets   *assets.Repository
	recon    *reconciliation.Repository
	audit    audit.Writer
	facts    AccountFactsReader
	oracle   PredictionOracleProvider
	sandbox  *SandboxOracle
	books    OutcomeBooks
}

// NewService validates the configuration and returns a Service.
func NewService(cfg Config) (*Service, error) {
	missing := []string{}
	need := func(ok bool, name string) {
		if !ok {
			missing = append(missing, name)
		}
	}
	need(cfg.Clock != nil, "Clock")
	need(cfg.Intents != nil, "Intents")
	need(cfg.Effects != nil, "Effects")
	need(cfg.Authorizer != nil, "Authorizer")
	need(cfg.Registry != nil, "Registry")
	need(cfg.KillSwitches != nil, "KillSwitches")
	need(cfg.Ledger != nil, "Ledger")
	need(cfg.Capital != nil, "Capital")
	need(cfg.Risk != nil, "Risk")
	// A missing policy would make Risk V2 refuse everything; that is safe, and
	// still a wiring mistake to report here rather than on the first intent.
	need(!cfg.RiskPolicy.Missing(), "RiskPolicy")
	need(cfg.Router != nil, "Router")
	need(cfg.Instruments != nil, "Instruments")
	need(cfg.Assets != nil, "Assets")
	need(cfg.Reconciliation != nil, "Reconciliation")
	need(cfg.Audit != nil, "Audit")
	if len(missing) > 0 {
		return nil, errs.New(errs.CodeInternal, "predmarket: the service is missing dependencies").WithField("missing", missing)
	}
	facts := cfg.Accounts
	if facts == nil {
		facts = DBAccountFacts{}
	}
	return &Service{
		clk: cfg.Clock, intents: cfg.Intents, effects: cfg.Effects, authz: cfg.Authorizer, registry: cfg.Registry,
		kills: cfg.KillSwitches, ledger: cfg.Ledger, capital: cfg.Capital, risk: cfg.Risk, policy: cfg.RiskPolicy,
		router: cfg.Router, instr: cfg.Instruments, assets: cfg.Assets, recon: cfg.Reconciliation, audit: cfg.Audit,
		facts: facts, oracle: cfg.Oracle, sandbox: cfg.SandboxOracle, books: cfg.Books,
	}, nil
}

func (s *Service) now() time.Time { return s.clk.Now().UTC() }

// atomically runs fn in a savepoint of tx and keeps what fn did only when it
// returns nil. It is what makes an operation all-or-nothing whatever the
// caller then does with tx: an error rolls back to the savepoint, so even a
// caller that commits after an error commits none of the operation's partial
// state, and the transaction is usable again (a failed statement no longer
// aborts it).
func atomically(ctx context.Context, tx pgx.Tx, fn func(sp pgx.Tx) error) error {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return dbErr("open savepoint", err)
	}
	if err := fn(sp); err != nil {
		_ = sp.Rollback(ctx) // the outer transaction reports anything worse
		return err
	}
	if err := sp.Commit(ctx); err != nil {
		return dbErr("release savepoint", err)
	}
	return nil
}

// --- actors --------------------------------------------------------------------

// canonicalOperator validates an operator actor and returns it with its id in
// canonical form, so dual-control comparisons cannot be defeated by case.
func canonicalOperator(a econ.Actor) (econ.Actor, error) {
	if a.Type != security.ActorOperator {
		return econ.Actor{}, errs.Newf(errs.CodeForbidden, "predmarket: this is an operator action; %s actors cannot take it", a.Type).
			WithField("actor_type", string(a.Type))
	}
	parsed, err := id.ParseAny(strings.TrimSpace(a.ID))
	if err != nil {
		return econ.Actor{}, errs.New(errs.CodeValidationFailed, "predmarket: an operator is identified by their user id (a canonical uuid)")
	}
	return econ.Actor{Type: security.ActorOperator, ID: parsed.String()}, nil
}

// canonicalLifecycleActor accepts an operator or the system scheduler.
func canonicalLifecycleActor(a econ.Actor) (econ.Actor, error) {
	if a.Type == security.ActorSystem {
		if !validText(a.ID, 200) {
			return econ.Actor{}, errs.New(errs.CodeValidationFailed, "predmarket: a system actor needs a component name")
		}
		return a, nil
	}
	return canonicalOperator(a)
}

// requireOtherThan enforces dual control: who may not take this action.
func requireOtherThan(actor econ.Actor, other econ.Actor, rule string) error {
	if strings.EqualFold(actor.ID, other.ID) && actor.Type == other.Type {
		return errs.New(errs.CodeForbidden, "predmarket: dual control: "+rule).WithField("actor_id", actor.ID)
	}
	return nil
}

// --- authorization, kill switches, risk ----------------------------------------

func (s *Service) authorize(ctx context.Context, tx pgx.Tx, mode gates.Mode) error {
	if !supportedMode(mode) {
		return errs.Newf(errs.CodeCapabilityNotApproved, "predmarket: prediction markets run in PAPER or SANDBOX, not %s", mode).
			WithField("mode", string(mode))
	}
	return s.authz.Require(ctx, tx, gates.PredictionMarkets, mode)
}

func (s *Service) killSwitches(ctx context.Context, tx pgx.Tx) ([]risk.KillSwitch, error) {
	snap, err := s.kills.Snapshot(ctx, tx)
	if err != nil {
		return nil, err
	}
	out := make([]risk.KillSwitch, 0, len(snap.Switches))
	for _, sw := range snap.Switches {
		if sw.Active {
			out = append(out, risk.KillSwitch{Kind: string(sw.Kind), Scope: sw.ScopeID})
		}
	}
	return out, nil
}

// usd converts collateral (or share) base units to simulated USD. One
// collateral unit of the sandbox dollar is one simulated USD, and one share
// is valued at the one unit it can at most pay. The conversion is monotone in
// q for either rounding mode, so comparing two amounts converted the same way
// never refuses a comparison the exact amounts would pass; the exact checks
// are the capital reservation's and the ledger's.
func usd(q money.Quantity, mode money.RoundingMode) (money.USD, error) {
	u, err := money.QuoteQuantityToUSD(q, ShareDecimals, mode)
	if err != nil {
		return money.USD{}, errs.Wrap(err, errs.CodeOverflow, "predmarket: amount is out of range for a USD valuation")
	}
	return u, nil
}

// evaluate runs Risk V2 and persists the decision exactly as produced.
func (s *Service) evaluate(ctx context.Context, tx pgx.Tx, in risk.AdvancedInput, correlationID string) (risk.AdvancedDecision, error) {
	d := risk.EvaluateAdvanced(s.policy, in)
	if _, err := s.risk.RecordAdvancedDecision(ctx, tx, in, d, correlationID); err != nil {
		return risk.AdvancedDecision{}, dbErr("record risk decision", err)
	}
	return d, nil
}

// dbErr classifies a database error: retryable errors pass through for
// db.InTx, errs errors keep their code, ledger and domain SQLSTATEs map to
// stable codes, and anything else is INTERNAL.
func dbErr(op string, err error) error {
	if err == nil {
		return nil
	}
	if db.IsRetryable(err) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if ee, ok := errs.As(err); ok {
		return ee
	}
	switch db.SQLState(err) {
	case "PM001", "PM003", "PM007":
		return errs.Wrap(err, errs.CodeInvalidStateTransition, "predmarket: "+op)
	case "PM002", "PM005":
		return errs.Wrap(err, errs.CodeConflict, "predmarket: "+op)
	case "PM004", "PM006", "PM008":
		return errs.Wrap(err, errs.CodeForbidden, "predmarket: "+op)
	case "PM009":
		return errs.Wrap(err, errs.CodeReconciliationRequired, "predmarket: "+op)
	case "PM010":
		return errs.Wrap(err, errs.CodeValidationFailed, "predmarket: "+op)
	}
	if db.IsUniqueViolation(err) {
		return errs.Wrap(err, errs.CodeConflict, "predmarket: "+op)
	}
	if mapped := ledger.MapError(err); mapped != nil {
		if ee, ok := errs.As(mapped); ok && ee.Code != errs.CodeInternal {
			return ee
		}
	}
	return errs.Wrap(err, errs.CodeInternal, "predmarket: "+op)
}
