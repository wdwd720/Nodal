package feeledger

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/security"
)

// Config wires a Service. Every dependency is required.
type Config struct {
	Clock      clock.Clock
	Ledger     *ledger.Service
	Capital    *capital.Service
	Effects    *econ.EffectStore
	Authorizer *econ.Authorizer
	Assets     *assets.Repository
	Audit      audit.Writer
	// QuoteTTL is how long a recorded quote can be booked (default two
	// minutes). A booking of an expired quote is refused.
	QuoteTTL time.Duration
	// Actor is the deterministic service that applies the effects this
	// package claims (default SYSTEM "fee-ledger"). It is never an agent.
	Actor econ.Actor
}

// Service is the fee ledger. It holds no financial state: every method runs
// in the caller's transaction.
type Service struct {
	clk      clock.Clock
	ledger   *ledger.Service
	capital  *capital.Service
	effects  *econ.EffectStore
	auth     *econ.Authorizer
	assets   *assets.Repository
	audit    audit.Writer
	quoteTTL time.Duration
	actor    econ.Actor
}

// NewService validates the configuration and returns a Service.
func NewService(c Config) (*Service, error) {
	if c.Clock == nil || c.Ledger == nil || c.Capital == nil || c.Effects == nil || c.Authorizer == nil || c.Assets == nil || c.Audit == nil {
		return nil, errs.New(errs.CodeValidationFailed, "feeledger: every dependency of the fee ledger is required")
	}
	if c.QuoteTTL <= 0 {
		c.QuoteTTL = 2 * time.Minute
	}
	if c.Actor == (econ.Actor{}) {
		c.Actor = econ.SystemActor("fee-ledger")
	}
	if !nonAgent(c.Actor) {
		return nil, errs.New(errs.CodeValidationFailed, "feeledger: the applying actor must be a named, non-agent actor")
	}
	return &Service{
		clk: c.Clock, ledger: c.Ledger, capital: c.Capital, effects: c.Effects, auth: c.Authorizer, assets: c.Assets,
		audit: c.Audit, quoteTTL: c.QuoteTTL, actor: c.Actor,
	}, nil
}

// nonAgent reports whether a may act here: a typed, named actor that is never
// an agent (econ.CreatorActorTypes).
func nonAgent(a econ.Actor) bool {
	if strings.TrimSpace(a.ID) == "" {
		return false
	}
	for _, t := range econ.CreatorActorTypes() {
		if t == a.Type {
			return true
		}
	}
	return false
}

func requireActor(a econ.Actor) error {
	if !nonAgent(a) {
		return errs.New(errs.CodeForbidden, "feeledger: a person or a service acts here, never an agent").
			WithField("actor_type", string(a.Type))
	}
	return nil
}

// requireMode authorizes a capability in a mode the fee ledger runs in.
func (s *Service) requireMode(ctx context.Context, tx pgx.Tx, c gates.Capability, m gates.Mode) error {
	if !supportedMode(m) {
		return errs.Newf(errs.CodeValidationFailed, "feeledger: %s runs in PAPER or SANDBOX here, not %q", c, m)
	}
	return s.auth.Require(ctx, tx, c, m)
}

// requireCharge authorizes SPREAD_ENGINE for a disclosed charge. A charge
// that accompanies a risk-reducing operation (a liquidation's fee) is not
// stopped by a kill switch or by an inactive gate: those stop new activity,
// and refusing the charge would stop the risk reduction it belongs to, which
// the subsystem itself never stops for them. An unsupported mode, LIVE or an
// unknown environment refuses it as it refuses everything.
func (s *Service) requireCharge(ctx context.Context, tx pgx.Tx, m gates.Mode, riskReducing bool) error {
	if !riskReducing {
		return s.requireMode(ctx, tx, gates.SpreadEngine, m)
	}
	if !supportedMode(m) {
		return errs.Newf(errs.CodeValidationFailed, "feeledger: %s runs in PAPER or SANDBOX here, not %q", gates.SpreadEngine, m)
	}
	d, err := s.auth.Decide(ctx, tx, gates.SpreadEngine, m)
	if err != nil {
		return err
	}
	for _, r := range d.Reasons {
		if r != gates.ReasonCapabilityKillSwitched && r != gates.ReasonGateNotActive {
			return s.auth.Require(ctx, tx, gates.SpreadEngine, m)
		}
	}
	return nil
}

// postingContext attributes a posting to the fee ledger's applying service.
func (s *Service) postingContext(ctx context.Context) context.Context {
	return security.WithPrincipal(ctx, security.Principal{SubjectID: s.actor.ID, ActorType: s.actor.Type})
}

func validKey(k string) error {
	if t := strings.TrimSpace(k); t == "" || t != k || len(k) > 200 {
		return errs.New(errs.CodeValidationFailed, "feeledger: an idempotency key of 1..200 bytes without surrounding whitespace is required")
	}
	return nil
}

// hashHex is the lowercase hex sha256 of b.
func hashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// lockKey takes a transaction-scoped advisory lock on a name, serializing the
// writers of one schedule key or one performance chain.
func lockKey(ctx context.Context, tx pgx.Tx, name string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "feeledger:"+name); err != nil {
		return dbErr("lock "+name, err)
	}
	return nil
}

// dbErr keeps retryable failures recognisable to db.InTx and maps the rest.
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
	return errs.Wrap(err, errs.CodeInternal, "feeledger: "+op)
}
