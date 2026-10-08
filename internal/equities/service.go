package equities

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/feeledger"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/identity"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/legalrouter"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuation"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// VerificationReader reports how thoroughly the person behind an account has
// been verified, inside the caller's transaction.
type VerificationReader interface {
	Level(ctx context.Context, q db.Querier, account accounts.AccountID) (valuedomain.VerificationLevel, error)
}

// RiskSubject is what a risk-section reader is asked about: the order's
// economic intent, the USD notional it puts at risk and the evaluation time.
type RiskSubject struct {
	Intent      econ.Intent
	NotionalUSD money.USD
	At          time.Time
}

// AuthorityReader supplies the risk kernel's authority section for an order
// an agent proposed: whether the agent holds delegated authority over the
// account that is unexpired, unrevoked, within its limits and in scope for
// this order. Equities keeps no delegation register of its own.
type AuthorityReader interface {
	Authority(ctx context.Context, q db.Querier, s RiskSubject) (risk.AuthoritySnapshot, error)
}

// StrategyReader supplies the risk kernel's strategy section for a
// strategy-originated order: whether the strategy is approved and its
// manifest permits this order.
type StrategyReader interface {
	Strategy(ctx context.Context, q db.Querier, s RiskSubject) (risk.StrategySnapshot, error)
}

// identityVerification reads the level internal/identity establishes, over
// the caller's querier.
type identityVerification struct{}

func (identityVerification) Level(ctx context.Context, q db.Querier, account accounts.AccountID) (valuedomain.VerificationLevel, error) {
	r, err := identity.NewVerificationResolver(q)
	if err != nil {
		return valuedomain.VerificationNone, err
	}
	return r.Level(ctx, account)
}

// Config wires a Service. Every field is required unless it says otherwise.
type Config struct {
	Clock        clock.Clock
	Intents      *econ.IntentStore
	Effects      *econ.EffectStore
	Authorizer   *econ.Authorizer
	KillSwitches econ.KillSwitchReader
	Router       *legalrouter.Router
	RiskPolicy   risk.AdvancedPolicy
	Capital      *capital.Service
	Ledger       *ledger.Service
	Prices       valuation.PriceSource
	Audit        audit.Writer
	Adapter      EquitiesExecutionAdapter
	// Schedules are the fee schedules, one per settlement asset. An equity
	// whose settlement asset has no schedule cannot be listed or traded.
	Schedules []fees.Schedule
	// FeeLedger is where every fill's spread and fees are disclosed and
	// booked (feebooking.go): a settlement asset's schedule is made the
	// version in force under its equities key, and a fill that charges
	// anything records its payer's disclosure there and registers its charges
	// against the fill's own journal and effect.
	FeeLedger *feeledger.Service
	// MarketCollarBPS bounds a market order: the protection price is the
	// venue's indicative price moved by this much against the customer, and
	// the order's capital reservation is sized at it.
	MarketCollarBPS money.BPS
	// MinOrderNotional is the smallest order, valued at par in its
	// settlement asset (a sandbox dollar).
	MinOrderNotional money.USD
	// Verification is optional; the default reads internal/identity.
	Verification VerificationReader
	// Authority is optional. Without it the risk kernel's authority section
	// is missing, so every order an agent proposes is refused (fail closed).
	Authority AuthorityReader
	// Strategies is optional. Without it the strategy section is missing, so
	// every strategy-originated order is refused (fail closed).
	Strategies StrategyReader
}

// Service is the equities subsystem. It holds no mutable state; every
// mutating method runs in the caller's transaction.
type Service struct {
	clk          clock.Clock
	intents      *econ.IntentStore
	effects      *econ.EffectStore
	auth         *econ.Authorizer
	kills        econ.KillSwitchReader
	router       *legalrouter.Router
	riskStore    *risk.Store
	riskPolicy   risk.AdvancedPolicy
	capital      *capital.Service
	ledger       *ledger.Service
	prices       valuation.PriceSource
	audit        audit.Writer
	adapter      EquitiesExecutionAdapter
	schedules    map[assets.AssetID]fees.Schedule
	feeLedger    *feeledger.Service
	collar       money.BPS
	minNotional  money.USD
	verification VerificationReader
	authority    AuthorityReader
	strategies   StrategyReader
	instruments  *instruments.Repository
	assets       *assets.Repository
	accounts     *accounts.Repository
	actor        econ.Actor
}

// MaxMarketCollarBPS bounds the market order collar (20%).
const MaxMarketCollarBPS money.BPS = 2_000

// NewService validates the wiring. A missing dependency, an adapter that
// claims live, a fee schedule that carries a network cost or names the same
// settlement asset twice, and a policy that cannot be evaluated are refused.
func NewService(cfg Config) (*Service, error) {
	var missing []string
	for name, nilish := range map[string]bool{
		"clock": cfg.Clock == nil, "intents": cfg.Intents == nil, "effects": cfg.Effects == nil,
		"authorizer": cfg.Authorizer == nil, "kill switches": cfg.KillSwitches == nil, "legal router": cfg.Router == nil,
		"capital": cfg.Capital == nil, "ledger": cfg.Ledger == nil, "prices": cfg.Prices == nil, "audit": cfg.Audit == nil,
		"adapter": cfg.Adapter == nil, "fee ledger": cfg.FeeLedger == nil,
	} {
		if nilish {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, errs.New(errs.CodeInternal, "equities: incomplete wiring").WithField("missing", missing)
	}
	if cfg.Adapter.Mode() == config.ProviderModeLive {
		return nil, ErrLiveExecutionUnsupported()
	}
	if cfg.RiskPolicy.Missing() {
		return nil, errs.New(errs.CodeInternal, "equities: a risk policy is required")
	}
	if cfg.MarketCollarBPS <= 0 || cfg.MarketCollarBPS > MaxMarketCollarBPS {
		return nil, errs.Newf(errs.CodeInternal, "equities: the market collar must be within 1..%d bps", MaxMarketCollarBPS)
	}
	if !cfg.MinOrderNotional.IsPositive() {
		return nil, errs.New(errs.CodeInternal, "equities: a positive minimum order notional is required")
	}
	if len(cfg.Schedules) == 0 {
		return nil, errs.New(errs.CodeInternal, "equities: at least one fee schedule is required")
	}
	schedules := make(map[assets.AssetID]fees.Schedule, len(cfg.Schedules))
	for _, sch := range cfg.Schedules {
		if err := sch.Validate(); err != nil {
			return nil, err
		}
		if !sch.NetworkFee.Amount.IsZero() {
			return nil, errs.New(errs.CodeInternal, "equities: an equity fee schedule carries no network cost")
		}
		if _, dup := schedules[sch.QuoteAsset]; dup {
			return nil, errs.New(errs.CodeInternal, "equities: two fee schedules for one settlement asset")
		}
		schedules[sch.QuoteAsset] = sch
	}
	v := cfg.Verification
	if v == nil {
		v = identityVerification{}
	}
	return &Service{
		clk: cfg.Clock, intents: cfg.Intents, effects: cfg.Effects, auth: cfg.Authorizer, kills: cfg.KillSwitches,
		router: cfg.Router, riskStore: risk.NewStore(), riskPolicy: cfg.RiskPolicy, capital: cfg.Capital, ledger: cfg.Ledger,
		prices: cfg.Prices, audit: cfg.Audit, adapter: cfg.Adapter, schedules: schedules, feeLedger: cfg.FeeLedger,
		collar:      cfg.MarketCollarBPS,
		minNotional: cfg.MinOrderNotional, verification: v, authority: cfg.Authority, strategies: cfg.Strategies,
		instruments: instruments.NewRepository(), assets: assets.NewRepository(), accounts: accounts.NewRepository(),
		actor: econ.SystemActor("equities"),
	}, nil
}

// Adapter returns the venue the service trades on.
func (s *Service) Adapter() EquitiesExecutionAdapter { return s.adapter }

// ListingSpec describes a new equity or ETF listing.
type ListingSpec struct {
	Kind        instruments.InstrumentType
	Symbol      string
	Name        string
	ExchangeMIC string
	// CashAssetID is the settlement asset: a SIMULATED dollar.
	CashAssetID assets.AssetID
	// ShareDecimals is the fractional-share precision of the share asset.
	ShareDecimals uint8
	// PriceScale and TickSize define the price grid (2 and 1: cents).
	PriceScale int32
	TickSize   money.Quantity
	// LotSize and MinQuantity are in share base units.
	LotSize        money.Quantity
	MinQuantity    money.Quantity
	OracleMaxAgeMS int64
	OracleSources  []string
	RiskClass      assets.RiskClass
	Actor          econ.Actor
}

// Listing is an equity listing: the instrument, its current terms, the share
// asset and the settlement asset.
type Listing struct {
	Instrument instruments.Instrument
	Terms      instruments.Version
	ShareAsset assets.Asset
	CashAsset  assets.Asset
}

// ListEquity creates the share asset (one SIMULATED asset per listing) and
// the EQUITY or ETF instrument that trades it, in the caller's transaction.
func (s *Service) ListEquity(ctx context.Context, tx pgx.Tx, spec ListingSpec) (Listing, error) {
	if spec.Kind != instruments.TypeEquity && spec.Kind != instruments.TypeETF {
		return Listing{}, errs.New(errs.CodeValidationFailed, "equities: a listing is an EQUITY or an ETF")
	}
	if err := validActor(spec.Actor); err != nil {
		return Listing{}, err
	}
	if strings.TrimSpace(spec.Name) == "" {
		return Listing{}, errs.New(errs.CodeValidationFailed, "equities: a listing has a name")
	}
	cash, err := s.assets.Get(ctx, tx, spec.CashAssetID)
	if err != nil {
		return Listing{}, err
	}
	if err := checkCashAsset(cash); err != nil {
		return Listing{}, err
	}
	// Refused before the share asset is created (supply.go).
	if err := refuseSupplyControlledCash(ctx, tx, cash.ID); err != nil {
		return Listing{}, err
	}
	sch, ok := s.schedules[cash.ID]
	if !ok {
		return Listing{}, errs.New(errs.CodeValidationFailed, "equities: no fee schedule covers this settlement asset")
	}
	if sch.QuoteDecimals != cash.Decimals {
		return Listing{}, errs.New(errs.CodeValidationFailed, "equities: the fee schedule's decimals disagree with the settlement asset")
	}
	share, err := s.assets.Create(ctx, tx, assets.Asset{
		Chain: assets.SandboxChain, Kind: assets.KindSimulated, ValueDomain: valuedomain.Simulated,
		Symbol: spec.Symbol, Name: spec.Name + " (simulated share)", Decimals: spec.ShareDecimals,
		RiskClass: spec.RiskClass, Status: assets.StatusActive,
	})
	if err != nil {
		return Listing{}, err
	}
	shareID, cashID := share.ID, cash.ID
	terms := instruments.Terms{
		Kind: spec.Kind, Symbol: spec.Symbol, VenueCode: s.adapter.Name(), ExchangeMIC: spec.ExchangeMIC,
		BaseAssetID: &shareID, QuoteAssetID: &cashID, SettlementAssetID: cashID,
		ContractMultiplier: money.QuantityFromInt64(1), PriceScale: spec.PriceScale, TickSize: spec.TickSize,
		LotSize: spec.LotSize, MinQuantity: spec.MinQuantity, QuantityDecimals: spec.ShareDecimals,
		SettlementStyle: instruments.SettlePhysical, TradingCalendar: CalendarXNYS,
		OracleMaxAgeMS: spec.OracleMaxAgeMS, OracleSources: spec.OracleSources, MarginModel: instruments.MarginNone,
		CorporateActionBehavior: instruments.CorporateActionsAdjust, Transferability: instruments.Transferable,
	}
	inst, v, err := s.instruments.CreateAdvanced(ctx, tx, instruments.AdvancedSpec{
		Terms: terms, CanonicalName: spec.Symbol + " " + spec.ExchangeMIC + " " + share.ID.String(),
		RiskClass: spec.RiskClass, Status: assets.StatusActive, ActiveFrom: s.clk.Now().UTC(),
		ActorType: string(spec.Actor.Type), ActorID: spec.Actor.ID,
	})
	if err != nil {
		return Listing{}, err
	}
	if err := s.record(ctx, tx, audit.SystemStream, "equities.listing.created", "instrument", inst.ID.String(),
		"equity listed", map[string]string{"symbol": spec.Symbol, "exchange_mic": spec.ExchangeMIC, "share_asset_id": share.ID.String()}); err != nil {
		return Listing{}, err
	}
	return Listing{Instrument: inst, Terms: v, ShareAsset: share, CashAsset: cash}, nil
}

// checkCashAsset requires the settlement asset to be a SIMULATED dollar: the
// subsystem values it at par for USD notionals. It never creates one; the
// caller passes the platform's sandbox dollar.
func checkCashAsset(a assets.Asset) error {
	if a.Kind != assets.KindSimulated || a.ValueDomain != valuedomain.Simulated {
		return errs.New(errs.CodeForbidden, "equities: the settlement asset must be a SIMULATED asset")
	}
	if a.PegCurrency != "USD" {
		return errs.New(errs.CodeValidationFailed, "equities: the settlement asset must be a sandbox dollar pegged to USD")
	}
	return nil
}

// LoadListing reads an equity listing; anything that is not an EQUITY or ETF
// whose terms agree with its assets is refused.
func (s *Service) LoadListing(ctx context.Context, q db.Querier, iid instruments.InstrumentID) (Listing, error) {
	inst, err := s.instruments.Get(ctx, q, iid)
	if err != nil {
		return Listing{}, err
	}
	if inst.Type != instruments.TypeEquity && inst.Type != instruments.TypeETF {
		return Listing{}, errs.New(errs.CodeValidationFailed, "equities: the instrument is not an equity or ETF").
			WithField("instrument_id", iid.String())
	}
	v, err := s.instruments.CurrentTerms(ctx, q, iid)
	if err != nil {
		return Listing{}, err
	}
	t := v.Terms
	if t.BaseAssetID == nil || t.QuoteAssetID == nil || *t.QuoteAssetID != t.SettlementAssetID {
		return Listing{}, errs.New(errs.CodeInternal, "equities: an equity's terms name its share and its settlement asset")
	}
	share, err := s.assets.Get(ctx, q, *t.BaseAssetID)
	if err != nil {
		return Listing{}, err
	}
	cash, err := s.assets.Get(ctx, q, t.SettlementAssetID)
	if err != nil {
		return Listing{}, err
	}
	if share.Kind != assets.KindSimulated || share.Decimals != t.QuantityDecimals {
		return Listing{}, errs.New(errs.CodeInternal, "equities: the share asset disagrees with the instrument's terms")
	}
	if err := checkCashAsset(cash); err != nil {
		return Listing{}, err
	}
	return Listing{Instrument: inst, Terms: v, ShareAsset: share, CashAsset: cash}, nil
}

// Calendar builds a calendar with the holidays recorded for it.
func (s *Service) Calendar(ctx context.Context, q db.Querier, code string) (*Calendar, error) {
	hs, err := Holidays(ctx, q, code)
	if err != nil {
		return nil, err
	}
	return NewCalendar(code, hs)
}

// AddHoliday records a full-day closure. Recording the same closure twice is
// a replay; a different name for a recorded date is a conflict.
func (s *Service) AddHoliday(ctx context.Context, tx pgx.Tx, code string, h Holiday, actor econ.Actor) error {
	if code != CalendarXNYS {
		return errs.Newf(errs.CodeUnsupported, "equities: trading calendar %q is not implemented", code)
	}
	if err := validActor(actor); err != nil {
		return err
	}
	name := strings.TrimSpace(h.Name)
	if h.Date.IsZero() || name == "" {
		return errs.New(errs.CodeValidationFailed, "equities: a holiday has a date and a name")
	}
	if wd := h.Date.Weekday(); wd == time.Saturday || wd == time.Sunday {
		return errs.New(errs.CodeValidationFailed, "equities: a weekend is closed already")
	}
	tag, err := tx.Exec(ctx, `INSERT INTO equity_calendar_holidays (id, calendar_code, holiday_date, name, created_by_actor_type,
		created_by_actor_id) VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (calendar_code, holiday_date) DO NOTHING`,
		id.New[id.Any](), code, h.Date.Time(), name, string(actor.Type), actor.ID)
	if err != nil {
		return storeErr("record holiday", err)
	}
	if tag.RowsAffected() == 1 {
		return s.record(ctx, tx, audit.SystemStream, "equities.calendar.holiday_added", "equity_calendar", code,
			"holiday recorded", map[string]string{"date": h.Date.String(), "name": name})
	}
	var existing string
	if err := tx.QueryRow(ctx, `SELECT name FROM equity_calendar_holidays WHERE calendar_code = $1 AND holiday_date = $2`,
		code, h.Date.Time()).Scan(&existing); err != nil {
		return storeErr("load holiday", err)
	}
	if existing != name {
		return errs.Newf(errs.CodeConflict, "equities: %s is already recorded as %q", h.Date, existing)
	}
	return nil
}

// validActor refuses a missing actor and an agent: an agent proposes intents
// and never changes subsystem state.
func validActor(a econ.Actor) error {
	if !a.Type.Valid() || strings.TrimSpace(a.ID) == "" {
		return errs.New(errs.CodeValidationFailed, "equities: an actor type and id are required")
	}
	if a.Type == security.ActorAgent {
		return errs.New(errs.CodeForbidden, "equities: an agent proposes intents; it never changes equities state")
	}
	return nil
}

// AllTransitionActorTypes returns the actor types that may move an order or a
// corporate action (equity_order_transitions_actor_type_check and
// equity_corporate_action_transitions_actor_type_check, and the
// announced_by / created_by columns). An agent is never one.
func AllTransitionActorTypes() []security.ActorType {
	return []security.ActorType{security.ActorUser, security.ActorOperator, security.ActorService, security.ActorSystem}
}

// lockNamespace is the first key of every equities advisory lock.
const lockNamespace int32 = 743

// lockInstrument takes the instrument's transaction-scoped advisory lock:
// shared for anything that trades or settles, exclusive for a corporate
// action, so positions cannot move while an action reads and adjusts them.
func lockInstrument(ctx context.Context, tx pgx.Tx, iid instruments.InstrumentID, exclusive bool) error {
	fn := "pg_advisory_xact_lock_shared"
	if exclusive {
		fn = "pg_advisory_xact_lock"
	}
	if _, err := tx.Exec(ctx, `SELECT `+fn+`($1, hashtext($2))`, lockNamespace, iid.String()); err != nil {
		return storeErr("lock instrument", err)
	}
	return nil
}

// record appends an audit event in the caller's transaction.
func (s *Service) record(ctx context.Context, tx pgx.Tx, stream, action, resourceType, resourceID, reason string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "equities: encode audit payload")
	}
	_, err = s.audit.Append(ctx, tx, audit.Event{
		Stream: stream, ActorType: string(s.actor.Type), ActorID: s.actor.ID, Action: action, ResourceType: resourceType,
		ResourceID: resourceID, Reason: reason, Payload: body, OccurredAt: s.clk.Now().UTC(),
	})
	return err
}

// markFor returns the fresh mark of a listing, or nil when none is younger
// than the instrument's oracle bound. Only a fault is an error.
func (s *Service) markFor(ctx context.Context, q db.Querier, l Listing, now time.Time) (*Mark, error) {
	maxAge := time.Duration(l.Terms.Terms.OracleMaxAgeMS) * time.Millisecond
	p, err := s.prices.Latest(ctx, q, l.ShareAsset.ID, l.CashAsset.ID, maxAge, now)
	if err != nil {
		if errs.CodeOf(err) == errs.CodeStaleMarketData {
			return nil, nil
		}
		var ee *errs.Error
		if errors.As(err, &ee) {
			return nil, ee
		}
		return nil, errs.Wrap(err, errs.CodeInternal, "equities: read mark")
	}
	if !p.Mantissa.IsPositive() {
		return nil, nil
	}
	return &Mark{Mantissa: p.Mantissa, Scale: p.Scale, Source: p.Source, ObservedAt: p.At.UTC()}, nil
}
