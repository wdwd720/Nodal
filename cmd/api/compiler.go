package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/agents"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/provider/compilersandbox"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/strategy"
	"github.com/nodal/controlplane/internal/strategy/ir"
)

// The strategy compiler's registry (ADR-0029's second half).
//
// ADR-0029 declared two seams — a CompilerBackend and a RefsLoader — and said
// both must be present before a compile is attempted, because a compiler with
// an EMPTY registry would reject every instrument a user named and blame them
// for it. The backend seam had an implementation from the day it was declared.
// The RefsLoader had none, anywhere, so the pair could never be satisfied and
// the sentence "until both exist, a strategy cannot be compiled here" was true
// by construction rather than by configuration (F-256).
//
// This file is that implementation. It reads the registry every compile, at the
// instant the compile happens, through the caller's querier — never a cache,
// because an instrument that was halted a minute ago must not back a strategy
// compiled now.

// maxRegistryInstruments bounds one snapshot. The repository's own ceiling is a
// thousand; a deployment with more instruments than that has outgrown a
// compile-time snapshot and would need a lookup per named instrument instead,
// which is a different design and not this one.
const maxRegistryInstruments = 1000

// strategyRefs loads the registry snapshot the compiler validates against.
//
// It is a value type with two stateless collaborators, constructed once and
// safe to share: every method takes the querier it should read through.
type strategyRefs struct {
	instruments *instruments.Repository
	policies    *risk.Store
}

var _ agents.StructuredRefsLoader = strategyRefs{}

func newStrategyRefs() strategyRefs {
	return strategyRefs{instruments: instruments.NewRepository(), policies: risk.NewStore()}
}

// Refs is agents.RefsLoader: the snapshot strategy.Validate's TYPE and
// RISK_COMPAT stages check a document against.
//
// The policy is the GLOBAL one composed at `now` and nothing narrower. An
// ACCOUNT or AGENT policy tightens what an agent may DO at the moment it acts;
// a compiled version is a document, not an actor, and validating it against one
// account's tightening would produce a document that stops validating the day
// that tightening is lifted.
func (r strategyRefs) Refs(ctx context.Context, q db.Querier, now time.Time) (strategy.ValidationRefs, error) {
	list, err := r.instruments.List(ctx, q, maxRegistryInstruments)
	if err != nil {
		return strategy.ValidationRefs{}, fmt.Errorf("strategy refs: instruments: %w", err)
	}
	byID := make(map[string]instruments.Instrument, len(list))
	for _, inst := range list {
		byID[inst.ID.String()] = inst
	}

	venues, err := loadVenues(ctx, q)
	if err != nil {
		return strategy.ValidationRefs{}, err
	}
	tools, err := loadTools(ctx, q)
	if err != nil {
		return strategy.ValidationRefs{}, err
	}

	policy, _, err := r.policies.EffectivePolicy(ctx, q, "", "", now)
	if err != nil && err != risk.ErrNoPolicy { //nolint:errorlint // ErrNoPolicy is returned verbatim by EffectivePolicy
		return strategy.ValidationRefs{}, fmt.Errorf("strategy refs: risk policy: %w", err)
	}
	// A deployment with no GLOBAL policy hands the zero Policy through rather
	// than failing: Validate's RISK_COMPAT stage reports RISK_POLICY_MISSING,
	// which tells the user what is wrong with this deployment instead of
	// failing their compile with an internal error.
	refs := strategy.ValidationRefs{
		Instruments: byID,
		Venues:      venues,
		Tools:       tools,
		Policy:      policy,
		// AssetClasses is deliberately empty: this schema has no asset-class
		// registry, so there is no list to check an envelope against, and
		// inventing one here would be a constraint nobody decided. Validate
		// skips the check when the set is empty, and the structured compiler
		// declares no asset classes at all.
		AssetClasses: map[string]struct{}{},
		Now:          now,
	}
	if !policy.Missing() {
		refs.PolicyHash = policy.Hash()
	}
	return refs, nil
}

// Registry is agents.StructuredRefsLoader: the name-resolution half, which
// ValidationRefs does not carry because Validate checks a document that already
// names identifiers and a person names an instrument the way it is written on
// the screen.
func (r strategyRefs) Registry(ctx context.Context, q db.Querier, _ time.Time) (agents.StructuredRegistry, error) {
	list, err := r.instruments.List(ctx, q, maxRegistryInstruments)
	if err != nil {
		return agents.StructuredRegistry{}, fmt.Errorf("strategy registry: instruments: %w", err)
	}
	byName := make(map[string]string, len(list))
	for _, inst := range list {
		byName[inst.CanonicalName] = inst.ID.String()
	}

	// Only listings that may take new actions. A venue that once listed an
	// instrument and has since been disabled is not a venue a new strategy may
	// name, and the refusal says so by name rather than at execution time.
	rows, err := q.Query(ctx, `
		SELECT l.instrument_id::text, v.code
		  FROM venue_listings l JOIN venues v ON v.id = l.venue_id
		 WHERE l.status IN ('ACTIVE','DEGRADED') AND v.status IN ('ACTIVE','DEGRADED')
		 ORDER BY l.instrument_id, v.code`)
	if err != nil {
		return agents.StructuredRegistry{}, fmt.Errorf("strategy registry: venue listings: %w", err)
	}
	defer rows.Close()
	venuesByInstrument := map[string][]string{}
	for rows.Next() {
		var instrumentID, code string
		if serr := rows.Scan(&instrumentID, &code); serr != nil {
			return agents.StructuredRegistry{}, fmt.Errorf("strategy registry: scan listing: %w", serr)
		}
		venuesByInstrument[instrumentID] = append(venuesByInstrument[instrumentID], code)
	}
	if err := rows.Err(); err != nil {
		return agents.StructuredRegistry{}, fmt.Errorf("strategy registry: venue listings: %w", err)
	}
	return agents.StructuredRegistry{
		InstrumentIDsByCanonicalName: byName,
		VenueCodesByInstrumentID:     venuesByInstrument,
	}, nil
}

func loadVenues(ctx context.Context, q db.Querier) (map[string]instruments.Venue, error) {
	rows, err := q.Query(ctx, `SELECT code, status FROM venues ORDER BY code`)
	if err != nil {
		return nil, fmt.Errorf("strategy refs: venues: %w", err)
	}
	defer rows.Close()
	out := map[string]instruments.Venue{}
	for rows.Next() {
		var code, status string
		if serr := rows.Scan(&code, &status); serr != nil {
			return nil, fmt.Errorf("strategy refs: scan venue: %w", serr)
		}
		out[code] = instruments.Venue{Code: code, Status: instruments.VenueStatus(status)}
	}
	return out, rows.Err()
}

func loadTools(ctx context.Context, q db.Querier) (map[string]strategy.Tool, error) {
	rows, err := q.Query(ctx, `SELECT code, version, effect, status, environments FROM tools ORDER BY code, version`)
	if err != nil {
		return nil, fmt.Errorf("strategy refs: tools: %w", err)
	}
	defer rows.Close()
	out := map[string]strategy.Tool{}
	for rows.Next() {
		var (
			code, effect, status string
			version              int
			environments         []string
		)
		if serr := rows.Scan(&code, &version, &effect, &status, &environments); serr != nil {
			return nil, fmt.Errorf("strategy refs: scan tool: %w", serr)
		}
		out[strategy.ToolKey(code, version)] = strategy.Tool{
			Code: code, Version: version, Effect: ir.Effect(effect), Status: status, Environments: environments,
		}
	}
	return out, rows.Err()
}

// sandboxStrategyCompiler returns the structured compiler when this deployment
// is a sandbox tier, and nil on every other one.
//
// Nil is not a missing dependency, it is the declared state ADR-0029 describes:
// a deployment with no compiler records every compile attempt with
// COMPILER_UNAVAILABLE and says exactly that. The condition is
// cfg.SandboxTier(), which is CP_API_LEGAL_POLICY=SANDBOX — a value
// config.Validate already refuses in PROD — so there is no second switch to
// forget and no new environment variable to set.
func sandboxStrategyCompiler(cfg *config.Config, clk clock.Clock, log *slog.Logger) (agents.StructuredCompilerBackend, error) {
	if !cfg.SandboxTier() {
		return nil, nil
	}
	c, err := compilersandbox.New(cfg.Env, clk, config.BuildVersion)
	if err != nil {
		return nil, err
	}
	log.Warn("strategies are compiled by the sandbox structured compiler: it reads the fields you state and never your description",
		"compiler", c.CompilerName(), "environment", string(cfg.Env),
		"consequence", "every strategy version compiled here is a rehearsal, is labelled one, and cannot exist in PROD")
	return c, nil
}

// priceToolAtBoot registers the price tool a structured strategy reads through,
// on a sandbox tier that has none.
//
// # Why this exists
//
// `tools` is the registry every declared data dependency is checked against
// (PARTS 66, 174, 176) and `strategy_dependencies` has a foreign key into it.
// Nothing in any deployment ever wrote a row: `scripts/seed` registers assets,
// venues, instruments, listings, policies and prices, and no tool, so the TYPE
// stage of every compile would answer UNKNOWN_TOOL for whatever a strategy
// named. That is the same F-26 shape the risk policy had before D-067, and the
// fix is the same shape too: the tier that exists for rehearsing the product
// registers the row it needs, through SQL that says what the row is.
//
// # Why PROD is refused
//
// A tool row is a statement that an adapter exists, that these egress hosts are
// the ones it may dial, and that its answers may back a financial decision. On
// a sandbox tier that statement is a rehearsal and the name says so. In PROD it
// would be a claim about a data supplier nobody contracted, which is exactly
// the fabricated-provider-reference the goal forbids.
func priceToolAtBoot(ctx context.Context, database *db.DB, cfg *config.Config, log *slog.Logger) error {
	if !cfg.SandboxTier() || cfg.Env == config.EnvProd {
		return nil
	}
	// The output schema is the contract a dependency's field reads are typed
	// against: one exact decimal, `mid`, at USD scale. It is deliberately the
	// smallest schema that supports the one rule this compiler can express.
	const outputSchema = `{
	  "type": "object",
	  "required": ["mid"],
	  "properties": {
	    "mid": {"type": "string", "description": "exact mid price in USD minor units, scale 2"}
	  }
	}`
	tag, err := database.Exec(ctx, `
		INSERT INTO tools (id, code, version, effect, provider, description, egress_hosts,
		                   input_schema_version, output_schema_version, output_schema,
		                   cost_per_call_usd_minor, max_calls_per_minute, max_calls_per_run, timeout_ms,
		                   status, environments, created_by_actor_type, created_by_actor_id)
		VALUES ($1, $2, 1, 'READ_MARKET_DATA', 'sandbox',
		        'Spot mid price for a registered instrument. A sandbox tier row: it names no contracted data supplier and dials no host.',
		        '{}'::text[], 1, 1, $3::jsonb, 0, 60, 1, 2000, 'ACTIVE', ARRAY[$4]::text[], 'SYSTEM', 'api-boot')
		ON CONFLICT (code, version) DO NOTHING`,
		id.New[id.Any](), compilersandbox.PreferredPriceTool, outputSchema, string(cfg.Env))
	if err != nil {
		return fmt.Errorf("price tool at boot: %w", err)
	}
	if tag.RowsAffected() > 0 {
		log.Warn("registered a sandbox price tool at boot: it names no contracted data supplier",
			"tool", compilersandbox.PreferredPriceTool, "environment", string(cfg.Env),
			"consequence", "a strategy compiled on this tier declares a dependency on this row; nothing evaluates it and no adapter is deployed")
	}
	return nil
}

// sandboxVenuePolicyAtBoot lists the venues a sandbox tier actually has in its
// GLOBAL risk policy's venue allowlist (F-257).
//
// # The absence
//
// `risk.DefaultGlobalPolicyJSON` sets `"allowed_venues": []`, and its own
// comment says why: "the venue allowlist is empty, so no trade is allowed
// anywhere until venues are listed explicitly". An explicit empty list allows
// nothing, which is the correct fail-closed default and is exactly what
// `riskPolicyAtBoot` records on every non-PROD deployment.
//
// Nothing ever listed a venue. `scripts/riskpolicy -rules` is a person running
// a command with a file nobody has written, and the RISK_COMPAT stage of the
// compiler refuses any strategy whose envelope or intent names a venue outside
// the allowlist. So a compiled strategy version could never have existed on any
// deployment of this build -- not for want of a compiler, but because the
// policy in force permitted no venue, and the refusal would have arrived
// blaming the user's strategy for naming the only venue the deployment lists.
//
// # What this does
//
// On a sandbox tier, and nowhere else, it records a GLOBAL policy version whose
// allowlist is the venue codes the registry itself holds in a status that may
// take new actions. It widens nothing else: every limit, every ceiling and
// every other allowlist is copied from the policy already in force.
//
// It is a decision, and it is recorded as one -- a new, immutable, append-only
// policy version with a SYSTEM actor and a reason that says what it is, through
// the same `risk.Store.RecordPolicy` an operator's command calls. PROD is
// refused, and so is any deployment that is not a sandbox tier: a production
// venue allowlist is a risk-desk decision about where money may go, and a
// process that widened it at boot because it found a row in a table would be
// the control-that-looks-decided this whole audit is against.
func sandboxVenuePolicyAtBoot(ctx context.Context, database *db.DB, cfg *config.Config, clk clock.Clock, log *slog.Logger) error {
	if !cfg.SandboxTier() || cfg.Env == config.EnvProd {
		return nil
	}
	now := clk.Now().UTC()
	store := risk.NewStore()
	current, _, err := store.EffectivePolicy(ctx, database, "", "", now)
	if err != nil {
		if errors.Is(err, risk.ErrNoPolicy) {
			// Nothing to widen. riskPolicyAtBoot records the starter policy
			// before this runs; a deployment with none has a bigger problem
			// than its venue allowlist and says so on every trade.
			return nil
		}
		return fmt.Errorf("sandbox venue policy: %w", err)
	}

	venues, err := loadVenues(ctx, database)
	if err != nil {
		return err
	}
	codes := make([]string, 0, len(venues))
	for code, v := range venues {
		if v.Status.AllowsNewActions() {
			codes = append(codes, code)
		}
	}
	sort.Strings(codes)
	if len(codes) == 0 || coversAll(current.AllowedVenues, codes) {
		return nil
	}

	next := current
	next.AllowedVenues = codes
	if verr := next.Validate(risk.ScopeGlobal); verr != nil {
		return fmt.Errorf("sandbox venue policy: %w", verr)
	}
	rules, err := next.CanonicalJSON()
	if err != nil {
		return fmt.Errorf("sandbox venue policy: %w", err)
	}
	// Derived from the rules, like riskPolicyAtBoot's: the version names WHAT
	// was recorded, so a second boot with the same venues is a CONFLICT on the
	// unique version rather than a second row saying the same thing.
	version := "sandbox-venues-" + next.Hash()[:12]

	ctx = security.WithPrincipal(ctx, security.Principal{
		SubjectID: "config:bootstrap", ActorType: security.ActorSystem, AuthTime: now,
	})
	err = database.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		_, rerr := store.RecordPolicy(ctx, tx, risk.PolicyRecord{
			Scope:       risk.ScopeGlobal,
			Version:     version,
			Rules:       rules,
			EffectiveAt: now,
			ActorType:   security.ActorSystem,
			ActorID:     "config:bootstrap",
			Reason: "this is a sandbox tier and the GLOBAL policy's venue allowlist was empty, which permits no " +
				"venue at all, so no strategy could be compiled and no internal trade could name a venue. The " +
				"allowlist is now the venues this deployment's own registry lists. Every other limit is unchanged. " +
				"A production allowlist is a risk-desk decision recorded with `go run ./scripts/riskpolicy -rules`.",
		})
		return rerr
	})
	switch {
	case err == nil:
		log.Warn("widened the GLOBAL risk policy's venue allowlist to this sandbox tier's own venues",
			"version", version, "venues", strings.Join(codes, ","), "environment", string(cfg.Env),
			"consequence", "a strategy compiled here may name these venues; nothing else about the policy changed and PROD is never widened this way")
		return nil
	case errs.CodeOf(err) == errs.CodeConflict:
		return nil
	default:
		return fmt.Errorf("sandbox venue policy: %w", err)
	}
}

// coversAll reports whether every code is already in the allowlist. A nil
// allowlist means "unconstrained by this scope", which covers everything.
func coversAll(allowed, codes []string) bool {
	if allowed == nil {
		return true
	}
	have := make(map[string]struct{}, len(allowed))
	for _, a := range allowed {
		have[a] = struct{}{}
	}
	for _, c := range codes {
		if _, ok := have[c]; !ok {
			return false
		}
	}
	return true
}
