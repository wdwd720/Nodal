package orders

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/agentauthority"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/feeledger"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/legalrouter"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/settlement"
	"github.com/nodal/controlplane/internal/valuation"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// The paper venue: external simulated liquidity.
//
// A market order fills immediately, in full, against the reference mark (the
// freshest valuation price of base in quote; a mark older than the venue's
// maximum age refuses the fill). It is priced through the instrument's
// published fees.Schedule -- the fee-ledger version in force under the
// instrument's key (PaperScheduleKey) at the fill instant, which an operator
// publishes and versions through the fee ledger -- whose explicit spread
// moves the execution price away from the mark: everything the customer pays
// beyond the mark is an itemized, disclosed component, recomputed by
// fees.VerifyQuote and fees.VerifyDisclosure before anything is posted. The
// customer's side is a SIM_TRADE at the mark, the spread is FEES_SPREAD /
// SPREAD_REVENUE, the platform and strategy fees are FEES_PLATFORM /
// PLATFORM_FEE_REVENUE (each its own pair), a venue pass-through is FEES_VENUE
// and a quote-asset network cost FEES_NETWORK: exactly the disclosure's
// charges (feeledger.ChargeEntries). The venue's own side is external and not
// booked, like the V1 spot venue. The disclosure is recorded in the fee ledger
// for the payer and booked against the fill's journal and effect, and the full
// disclosure and the schedule it was priced under are stored with the fill as
// well, so every unit of spread and fee is discoverable from either record.

// Reasons of the paper venue.
const (
	ReasonPaperNoSchedule         = "ORDER_PAPER_NO_PUBLISHED_SCHEDULE"
	ReasonPaperNetworkAsset       = "ORDER_PAPER_NETWORK_FEE_IN_OTHER_ASSET"
	ReasonPaperProtection         = "ORDER_PAPER_PROTECTION_PRICE_BREACHED"
	ReasonPaperIntentNotApproved  = "ORDER_PAPER_INTENT_NOT_APPROVED"
	ReasonPaperIntentShape        = "ORDER_PAPER_INTENT_NOT_EXECUTABLE"
	ReasonPaperDisclosureMismatch = "ORDER_PAPER_DISCLOSURE_DOES_NOT_VERIFY"
)

// IntentExecutor executes an economic intent against simulated liquidity in
// the caller's transaction. It never moves the intent through its lifecycle:
// the caller owns that, and has routed and risk-approved the intent before
// calling. *PaperVenue implements it; the strategy marketplace's paper-follow
// executes through it.
type IntentExecutor interface {
	ExecuteIntent(ctx context.Context, tx pgx.Tx, intent econ.Intent) (PaperFill, error)
}

var _ IntentExecutor = (*PaperVenue)(nil)

// PaperConfig wires a PaperVenue.
type PaperConfig struct {
	Clock        clock.Clock
	Ledger       *ledger.Service
	Capital      *capital.Service
	Intents      *econ.IntentStore
	Effects      *econ.EffectStore
	Authorizer   *econ.Authorizer
	Risk         *risk.Store
	RiskPolicy   risk.AdvancedPolicy
	Router       *legalrouter.Router
	Instruments  *instruments.Repository
	Assets       *assets.Repository
	Prices       valuation.PriceSource
	KillSwitches econ.KillSwitchReader
	Verification VerificationSource
	Sessions     SessionCalendar
	// FeeLedger is where every instrument's schedule is published and every
	// fill's charges are disclosed and booked. An instrument with no schedule
	// version in force under its key is refused.
	FeeLedger *feeledger.Service
	// ScheduleKeyPrefix names the fee-ledger keys the venue prices by: an
	// instrument's schedule is the version in force under
	// PaperScheduleKey(ScheduleKeyPrefix, instrument) (default
	// PaperScheduleKeyPrefix). A caller with its own published schedules (the
	// strategy marketplace) uses its own prefix.
	ScheduleKeyPrefix string
	// MaxMarkAge bounds the age of the mark a fill uses (default 30s); an
	// instrument's own oracle maximum age applies when it is stricter.
	MaxMarkAge time.Duration
	// MarkLookback is how far back the risk stage looks for a mark at all
	// (default 30 days).
	MarkLookback time.Duration
	Actor        econ.Actor
}

// PaperVenue fills market orders against a reference mark.
type PaperVenue struct {
	clk       clock.Clock
	ledger    *ledger.Service
	capital   *capital.Service
	intents   *econ.IntentStore
	effects   *econ.EffectStore
	auth      *econ.Authorizer
	risk      *risk.Store
	policy    risk.AdvancedPolicy
	router    *legalrouter.Router
	instr     *instruments.Repository
	assets    *assets.Repository
	prices    valuation.PriceSource
	kills     econ.KillSwitchReader
	verify    VerificationSource
	sessions  SessionCalendar
	feeLedger *feeledger.Service
	keyPrefix string
	maxAge    time.Duration
	lookback  time.Duration
	actor     econ.Actor
}

// NewPaperVenue validates the configuration and returns a PaperVenue.
func NewPaperVenue(c PaperConfig) (*PaperVenue, error) {
	if c.Clock == nil || c.Ledger == nil || c.Capital == nil || c.Intents == nil || c.Effects == nil || c.Authorizer == nil ||
		c.Risk == nil || c.Router == nil || c.Instruments == nil || c.Assets == nil || c.Prices == nil || c.KillSwitches == nil ||
		c.FeeLedger == nil {
		return nil, errs.New(errs.CodeValidationFailed, "orders: every dependency of the paper venue is required")
	}
	if c.RiskPolicy.Missing() {
		return nil, errs.New(errs.CodeValidationFailed, "orders: a risk policy is required")
	}
	if c.ScheduleKeyPrefix == "" {
		c.ScheduleKeyPrefix = PaperScheduleKeyPrefix
	}
	if !paperKeyPrefixPattern.MatchString(c.ScheduleKeyPrefix) {
		return nil, errs.New(errs.CodeValidationFailed, "orders: a schedule key prefix is lower-case [a-z0-9_.:/-], at most 91 bytes")
	}
	if c.Verification == nil {
		c.Verification = identityVerification{}
	}
	if c.Sessions == nil {
		c.Sessions = alwaysOpen24x7{}
	}
	if c.MaxMarkAge <= 0 {
		c.MaxMarkAge = 30 * time.Second
	}
	if c.MarkLookback <= 0 {
		c.MarkLookback = 30 * 24 * time.Hour
	}
	if c.Actor == (econ.Actor{}) {
		c.Actor = econ.SystemActor("orders-paper-venue")
	}
	if !isServiceActor(c.Actor) {
		return nil, errs.New(errs.CodeValidationFailed, "orders: the applying actor must be a non-agent actor")
	}
	return &PaperVenue{
		clk: c.Clock, ledger: c.Ledger, capital: c.Capital, intents: c.Intents, effects: c.Effects, auth: c.Authorizer,
		risk: c.Risk, policy: c.RiskPolicy, router: c.Router, instr: c.Instruments, assets: c.Assets, prices: c.Prices,
		kills: c.KillSwitches, verify: c.Verification, sessions: c.Sessions, feeLedger: c.FeeLedger,
		keyPrefix: c.ScheduleKeyPrefix, maxAge: c.MaxMarkAge, lookback: c.MarkLookback, actor: c.Actor,
	}, nil
}

// ScheduleKey is the fee-ledger key whose version in force prices this
// venue's fills of an instrument.
func (v *PaperVenue) ScheduleKey(inst instruments.InstrumentID) string {
	return PaperScheduleKey(v.keyPrefix, inst)
}

// EnsureSchedule makes s the version in force under an instrument's key
// (feeledger.EnsureSchedule) and returns it: for a caller that charges its own
// fixed schedule through the venue (the strategy marketplace's published
// strategy-fill schedule). Another version in force is refused.
func (v *PaperVenue) EnsureSchedule(ctx context.Context, tx pgx.Tx, inst instruments.InstrumentID, s fees.Schedule, reason string,
	actor econ.Actor) (feeledger.PublishedSchedule, error) {
	return v.feeLedger.EnsureSchedule(ctx, tx, feeledger.PublishRequest{Key: v.ScheduleKey(inst), Schedule: s, Reason: reason, Actor: actor})
}

// PaperFill is a paper_fills row: one immediate fill and its audit record.
type PaperFill struct {
	ID                   id.ID[id.Any]
	IntentID             econ.IntentID
	AccountID            accounts.AccountID
	InstrumentID         instruments.InstrumentID
	VenueLabel           string
	Side                 Side
	BaseAsset            assets.AssetID
	QuoteAsset           assets.AssetID
	BaseQuantity         money.Quantity
	NotionalAtReference  money.Quantity
	SpreadAmount         money.Quantity
	PlatformFeeAmount    money.Quantity
	VenueFeeAmount       money.Quantity
	NetworkFeeAmount     money.Quantity
	TotalQuote           money.Quantity
	ScheduleVersion      string
	ScheduleHash         string
	Schedule             []byte
	Disclosure           fees.Disclosure
	DisclosureBytes      []byte
	DisclosureHash       string
	ReservationID        string
	JournalTransactionID ledger.TransactionID
	EffectID             econ.EffectID
	EvidenceRef          string
	EvidenceSHA256       []byte
	CreatedAt            time.Time
	// Replayed is true when the intent had already been filled and the
	// stored fill was returned.
	Replayed bool
}

// paperMarket is everything the venue needs about one instrument.
type paperMarket struct {
	inst          instruments.Instrument
	base, quote   assets.Asset
	maxAge        time.Duration
	calendar      string
	expiry        *time.Time
	scheduleKey   string
	schedule      fees.Schedule
	scheduleBytes []byte
	scheduleHash  string
}

// market resolves an instrument, its assets and the schedule version in force
// under its key at an instant.
func (v *PaperVenue) market(ctx context.Context, q db.Querier, instrumentID instruments.InstrumentID, at time.Time) (paperMarket, error) {
	inst, err := v.instr.Get(ctx, q, instrumentID)
	if err != nil {
		return paperMarket{}, err
	}
	m := paperMarket{inst: inst, maxAge: v.maxAge, calendar: "24x7"}
	var baseID, quoteID assets.AssetID
	terms, err := v.instr.CurrentTerms(ctx, q, inst.ID)
	switch {
	case err == nil:
		if terms.Terms.BaseAssetID == nil {
			return paperMarket{}, rejection(ReasonPaperIntentShape, "the instrument names no base asset")
		}
		baseID, quoteID = *terms.Terms.BaseAssetID, terms.Terms.SettlementAssetID
		if terms.Terms.QuoteAssetID != nil {
			quoteID = *terms.Terms.QuoteAssetID
		}
		m.calendar, m.expiry = terms.Terms.TradingCalendar, terms.Terms.Expiry
		if age := time.Duration(terms.Terms.OracleMaxAgeMS) * time.Millisecond; age > 0 && age < m.maxAge {
			m.maxAge = age
		}
	case errs.CodeOf(err) == errs.CodeNotFound && inst.Type == instruments.TypeSpotPair && inst.BaseAssetID != nil && inst.QuoteAssetID != nil:
		baseID, quoteID = *inst.BaseAssetID, *inst.QuoteAssetID
	case err != nil && errs.CodeOf(err) != errs.CodeNotFound:
		return paperMarket{}, err
	default:
		return paperMarket{}, rejection(ReasonPaperIntentShape, "the instrument has no terms the paper venue can fill")
	}
	if m.base, err = v.assets.Get(ctx, q, baseID); err != nil {
		return paperMarket{}, err
	}
	if m.quote, err = v.assets.Get(ctx, q, quoteID); err != nil {
		return paperMarket{}, err
	}
	for _, a := range []assets.Asset{m.base, m.quote} {
		if a.Kind != assets.KindSimulated || a.ValueDomain != valuedomain.Simulated {
			return paperMarket{}, errs.Newf(errs.CodeForbidden, "orders: asset %s is not SIMULATED; the paper venue moves only simulated value", a.ID)
		}
	}
	m.scheduleKey = v.ScheduleKey(inst.ID)
	pub, err := v.feeLedger.EffectiveSchedule(ctx, q, m.scheduleKey, at)
	if feeledger.ReasonOf(err) == feeledger.ReasonScheduleNotFound {
		return paperMarket{}, rejection(ReasonPaperNoSchedule, "the instrument has no fee schedule published and in force").
			WithField("schedule_key", m.scheduleKey)
	}
	if err != nil {
		return paperMarket{}, err
	}
	s := pub.Schedule
	if s.QuoteAsset != m.quote.ID || s.QuoteDecimals != m.quote.Decimals {
		return paperMarket{}, rejection(ReasonPaperNoSchedule, "the published schedule is not quoted in the instrument's quote asset")
	}
	if s.NetworkFee.Amount.IsPositive() && s.NetworkFee.Asset != m.quote.ID {
		return paperMarket{}, rejection(ReasonPaperNetworkAsset, "a network cost in another asset cannot be charged on a paper fill")
	}
	m.schedule, m.scheduleBytes, m.scheduleHash = s, pub.Canonical, pub.Hash
	return m, nil
}

// paperTerms are the typed terms of a paper order intent.
type paperTerms struct {
	Venue           string          `json:"venue"`
	VenueLabel      string          `json:"venue_label"`
	Side            Side            `json:"side"`
	Quantity        string          `json:"quantity"`
	LiquidityIntent LiquidityIntent `json:"liquidity_intent"`
	Protection      *priceDoc       `json:"paper_protection_price,omitempty"`
	Risk            *RiskContext    `json:"risk_context,omitempty"`
}

// priceDoc is a price in an intent's terms: mantissa as a string.
type priceDoc struct {
	Mantissa string `json:"mantissa"`
	Scale    int32  `json:"scale"`
}

// protectionOf reads an optional paper protection price from any intent's
// terms; terms written by another subsystem simply have none.
func protectionOf(raw json.RawMessage, quote assets.AssetID) (*money.Price, error) {
	var t struct {
		Protection *priceDoc `json:"paper_protection_price"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &t) != nil || t.Protection == nil {
		return nil, nil
	}
	m, err := money.ParseQuantity(t.Protection.Mantissa)
	if err != nil {
		return nil, rejection(ReasonPriceNotPositive, "the protection price is not a decimal integer mantissa")
	}
	p := money.Price{Mantissa: m, Scale: t.Protection.Scale, QuoteAsset: quote.String(), Source: "protection", At: time.Unix(0, 0).UTC()}
	if err := p.Validate(); err != nil || !m.IsPositive() {
		return nil, rejection(ReasonPriceNotPositive, "the protection price must be a positive price")
	}
	return &p, nil
}

// priceCmp compares two prices exactly: -1, 0 or +1.
func priceCmp(a, b money.Price) int {
	return a.Mantissa.Mul(pow10(int(b.Scale))).Cmp(b.Mantissa.Mul(pow10(int(a.Scale))))
}

// ExecuteIntent fills an ACQUIRE_EXPOSURE (buy) or REDUCE_EXPOSURE (sell)
// intent with a stated base quantity on the paper venue, in the caller's
// transaction. The intent must be stored, unchanged, and already risk
// approved (RISK_APPROVED, RESERVED or EXECUTING); ExecuteIntent does not move
// it. Every refusal happens before anything is written, so a caller can record
// the refusal and commit. A second call for the same intent returns the first
// fill.
func (v *PaperVenue) ExecuteIntent(ctx context.Context, tx pgx.Tx, intent econ.Intent) (PaperFill, error) {
	stored, err := v.intents.Get(ctx, tx, intent.ID)
	if err != nil {
		return PaperFill{}, err
	}
	if !bytes.Equal(stored.ContentHash, intent.ContentHash) {
		return PaperFill{}, rejection(ReasonPaperIntentShape, "the intent is not the stored intent")
	}
	if prior, found, err := v.fillByIntent(ctx, tx, stored.ID); err != nil {
		return PaperFill{}, err
	} else if found {
		prior.Replayed = true
		return prior, nil
	}
	switch stored.Status {
	case econ.StatusRiskApproved, econ.StatusReserved, econ.StatusExecuting:
	default:
		return PaperFill{}, rejection(ReasonPaperIntentNotApproved, "the intent is "+string(stored.Status)+"; only a risk-approved intent executes")
	}
	side := SideBuy
	switch stored.Kind {
	case econ.KindAcquireExposure:
	case econ.KindReduceExposure:
		side = SideSell
	default:
		return PaperFill{}, rejection(ReasonPaperIntentShape, "the paper venue fills ACQUIRE_EXPOSURE and REDUCE_EXPOSURE intents")
	}
	if stored.Quantity == nil || !stored.Quantity.IsPositive() || stored.InstrumentID == "" {
		return PaperFill{}, rejection(ReasonPaperIntentShape, "a paper fill needs an instrument and a base quantity")
	}
	if !stored.Mode.Simulated() || stored.Mode == gates.ModeDevnet {
		return PaperFill{}, rejection(ReasonPaperIntentShape, "the paper venue runs in PAPER or SANDBOX")
	}
	if err := v.auth.Require(ctx, tx, gates.SpreadEngine, stored.Mode); err != nil {
		return PaperFill{}, err
	}
	instrumentID, err := instruments.ParseInstrumentID(stored.InstrumentID)
	if err != nil {
		return PaperFill{}, rejection(ReasonPaperIntentShape, "the intent's instrument is not an instrument id")
	}
	now := v.clk.Now().UTC()
	m, err := v.market(ctx, tx, instrumentID, now)
	if err != nil {
		return PaperFill{}, err
	}
	mark, err := v.prices.Latest(ctx, tx, m.base.ID, m.quote.ID, m.maxAge, now)
	if err != nil {
		return PaperFill{}, err
	}
	input := fees.QuoteInput{
		Side: fees.Side(side), BaseQty: *stored.Quantity, BaseDecimals: m.base.Decimals, QuoteDecimals: m.quote.Decimals,
		ReferencePrice: mark, StrategyOriginated: stored.StrategyRef != "",
	}
	quote, err := m.schedule.Quote(input)
	if err != nil {
		return PaperFill{}, err
	}
	// The auditor's checks run before the fill, not after it: a quote that
	// charges anything its schedule and input do not account for is never
	// posted.
	if err := fees.VerifyQuote(m.schedule, input, quote); err != nil {
		return PaperFill{}, errs.Wrap(err, errs.CodeInternal, "orders: a paper quote does not verify").WithField("reason", ReasonPaperDisclosureMismatch)
	}
	disclosure := quote.Disclosure()
	if err := fees.VerifyDisclosure(m.schedule, disclosure); err != nil {
		return PaperFill{}, errs.Wrap(err, errs.CodeInternal, "orders: a paper disclosure does not verify").WithField("reason", ReasonPaperDisclosureMismatch)
	}
	if protection, err := protectionOf(stored.Terms, m.quote.ID); err != nil {
		return PaperFill{}, err
	} else if protection != nil {
		c := priceCmp(quote.ExecutionPrice, *protection)
		if (side == SideBuy && c > 0) || (side == SideSell && c < 0) {
			return PaperFill{}, rejection(ReasonPaperProtection, "the execution price is beyond the protection price")
		}
	}
	reserveAsset, reserveAmount := m.quote.ID, quote.TotalDebit
	if side == SideSell {
		reserveAsset, reserveAmount = m.base.ID, quote.Input.BaseQty
	}
	// The reservation names the economic intent it serves (00733).
	reservation, err := v.capital.Reserve(ctx, tx, capital.ReserveRequest{
		AccountID: stored.AccountID.String(), AssetID: reserveAsset, Quantity: reserveAmount, EconomicIntentID: stored.ID.String(),
		ActorType: v.actor.Type, ActorID: v.actor.ID, IdempotencyKey: "orders:paper:reserve:" + stored.ID.String(),
		TTL: time.Minute, Reason: "paper fill (economic intent " + stored.ID.String() + ")",
	})
	if err != nil {
		return PaperFill{}, err
	}
	discBytes, err := disclosure.Canonical()
	if err != nil {
		return PaperFill{}, err
	}
	discHash, err := disclosure.Hash()
	if err != nil {
		return PaperFill{}, err
	}
	// The payer's disclosure is recorded in the fee ledger under the very
	// version that priced the fill, and must be byte for byte the one the
	// venue computed; what is posted and booked is exactly its charges. A
	// fill that charges nothing books nothing.
	charges := feeledger.ChargesOf(disclosure)
	var feeQuote *feeledger.StoredQuote
	if !charges.IsZero() {
		account, base := stored.AccountID, m.base.ID
		q, err := v.feeLedger.RecordQuote(ctx, tx, feeledger.QuoteRequest{
			ScheduleKey: m.scheduleKey, At: now, Input: input, Subsystem: econ.SubsystemOrders,
			IdempotencyKey: "orders:paper:" + stored.ID.String(), AccountID: &account, BaseAsset: &base, Mode: stored.Mode,
			Actor: v.actor,
		})
		if err != nil {
			return PaperFill{}, writtenFailure{err}
		}
		if q.DisclosureHash != discHash || q.ScheduleHash != m.scheduleHash {
			return PaperFill{}, errs.New(errs.CodeInternal, "orders: the fee ledger disclosed another quote than the paper venue priced").
				WithField("reason", ReasonPaperDisclosureMismatch).WithField("disclosure_id", q.ID.String())
		}
		feeQuote, charges = &q, q.Charges
	}
	fill := PaperFill{
		ID: id.New[id.Any](), IntentID: stored.ID, AccountID: stored.AccountID, InstrumentID: m.inst.ID, VenueLabel: VenueLabelPaper,
		Side: side, BaseAsset: m.base.ID, QuoteAsset: m.quote.ID, BaseQuantity: quote.Input.BaseQty,
		NotionalAtReference: quote.NotionalAtReference, SpreadAmount: charges.Spread,
		PlatformFeeAmount: charges.Platform.Add(charges.Strategy), VenueFeeAmount: charges.Venue, TotalQuote: quote.Total(),
		ScheduleVersion: m.schedule.Version, ScheduleHash: m.scheduleHash, Schedule: m.scheduleBytes,
		Disclosure: disclosure, DisclosureBytes: discBytes, DisclosureHash: discHash, ReservationID: reservation.ID.String(),
	}
	if charges.NetworkInQuote {
		fill.NetworkFeeAmount = charges.Network
	}
	iid := stored.ID
	key := "orders:paper:" + stored.ID.String()
	eff, existed, err := v.effects.Begin(ctx, tx, econ.BeginRequest{
		Key: key, Subsystem: econ.SubsystemOrders, Kind: "PAPER_FILL", IntentID: &iid, AccountID: stored.AccountID.String(),
		Request:     map[string]any{"intent_id": stored.ID.String(), "disclosure_hash": discHash, "schedule_hash": m.scheduleHash},
		PostsLedger: true, Actor: v.actor, CorrelationID: stored.CorrelationID,
	})
	if err != nil {
		return PaperFill{}, err
	}
	if existed {
		return PaperFill{}, errs.New(errs.CodeConflict, "orders: this intent's paper fill effect was already claimed").WithField("effect_id", eff.ID.String())
	}
	posted, err := v.ledger.Post(security.WithPrincipal(ctx, security.Principal{SubjectID: v.actor.ID, ActorType: security.ActorSystem}), tx, ledger.Posting{
		Kind: ledger.KindSimTrade, IdempotencyKey: key,
		Reference:   ledger.FinancialEventReference{Type: "paper_fill", ID: fill.ID.String()},
		EffectiveAt: now, Description: "simulated paper-venue fill at a reference mark with a disclosed spread",
		Entries: paperEntries(stored.AccountID, side, m.base.ID, m.quote.ID, quote, charges),
		Metadata: map[string]any{
			"venue_label": VenueLabelPaper, "intent_id": stored.ID.String(), "disclosure_hash": discHash,
			"schedule_version": m.schedule.Version, "schedule_hash": m.scheduleHash,
			"spread": fill.SpreadAmount.String(), "platform_fee": fill.PlatformFeeAmount.String(),
		},
	})
	if err != nil {
		return PaperFill{}, err
	}
	fill.JournalTransactionID = posted.TransactionID
	if _, err := v.capital.ConsumeFinal(ctx, tx, reservation.ID, reserveAmount, 0, ""); err != nil {
		return PaperFill{}, err
	}
	var discDoc any
	if err := json.Unmarshal(discBytes, &discDoc); err != nil {
		return PaperFill{}, errs.Wrap(err, errs.CodeInternal, "orders: re-read disclosure")
	}
	evidence := map[string]any{
		"kind": "orders.paper_fill.v1", "venue_label": VenueLabelPaper, "intent_id": stored.ID.String(),
		"account_id": stored.AccountID.String(), "instrument_id": m.inst.ID.String(), "side": string(side),
		"schedule_key": m.scheduleKey, "schedule_version": m.schedule.Version, "schedule_hash": m.scheduleHash,
		"disclosure_hash": discHash, "disclosure": discDoc, "journal_transaction_id": posted.TransactionID.String(),
		"reservation_id": reservation.ID.String(),
	}
	if feeQuote != nil {
		evidence["fee_disclosure_id"] = feeQuote.ID.String()
	}
	ev, err := econ.RecordEvidence(ctx, tx, econ.Evidence{Key: "orders/paper/" + stored.ID.String(), Document: evidence})
	if err != nil {
		return PaperFill{}, err
	}
	fill.EvidenceRef, fill.EvidenceSHA256 = ev.URI, ev.SHA256
	applied, err := v.effects.Apply(ctx, tx, eff.ID, econ.ApplyRequest{
		JournalTransactionID: &posted.TransactionID, ReservationID: reservation.ID.String(), Evidence: &ev, Actor: v.actor,
		Reason: "paper fill posted",
	})
	if err != nil {
		return PaperFill{}, err
	}
	fill.EffectID = applied.ID
	if feeQuote != nil {
		if _, err := v.feeLedger.RecordEmbedded(ctx, tx, feeledger.EmbeddedRequest{
			DisclosureID: feeQuote.ID, AccountID: stored.AccountID, JournalTransactionID: posted.TransactionID, EffectID: applied.ID,
			IntentID: &iid, Mode: stored.Mode, CorrelationID: stored.CorrelationID, Actor: v.actor,
		}); err != nil {
			return PaperFill{}, writtenFailure{err}
		}
	}
	if err := tx.QueryRow(ctx, `INSERT INTO paper_fills (id, intent_id, account_id, instrument_id, side, base_asset_id, quote_asset_id,
		base_quantity, notional_at_reference, spread_amount, platform_fee_amount, venue_fee_amount, network_fee_amount, total_quote,
		schedule_version, schedule_hash, schedule, disclosure, disclosure_hash, reservation_id, journal_transaction_id, effect_id,
		evidence_ref, evidence_sha256)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8::numeric,$9::numeric,$10::numeric,$11::numeric,$12::numeric,$13::numeric,$14::numeric,
		        $15,$16,$17,$18,$19,$20,$21,$22,$23,$24) RETURNING created_at`,
		fill.ID, fill.IntentID, fill.AccountID, fill.InstrumentID, fill.Side, fill.BaseAsset, fill.QuoteAsset,
		fill.BaseQuantity.String(), fill.NotionalAtReference.String(), fill.SpreadAmount.String(), fill.PlatformFeeAmount.String(),
		fill.VenueFeeAmount.String(), fill.NetworkFeeAmount.String(), fill.TotalQuote.String(), fill.ScheduleVersion,
		fill.ScheduleHash, fill.Schedule, fill.DisclosureBytes, fill.DisclosureHash, reservation.ID, posted.TransactionID,
		applied.ID, ev.URI, ev.SHA256).Scan(&fill.CreatedAt); err != nil {
		return PaperFill{}, dbErr("record paper fill", err)
	}
	return fill, nil
}

// paperEntries is the customer side of a paper fill (the venue's side is
// external and not booked): SIM_TRADE at the reference notional, then every
// disclosed charge on its own pair of entries -- exactly the fee ledger's
// entries for the disclosure (feeledger.ChargeEntries), which is what lets
// the fee ledger register them against this journal.
func paperEntries(account accounts.AccountID, side Side, base, quote assets.AssetID, q fees.Quote, charges feeledger.Charges) []ledger.Entry {
	qty, n := q.Input.BaseQty, q.NotionalAtReference
	var out []ledger.Entry
	if side == SideBuy {
		out = append(out,
			ledger.Entry{Account: ledger.CustomerAccount(account, ledger.CodeWallet, quote), Side: ledger.Credit, Quantity: n},
			ledger.Entry{Account: ledger.CustomerAccount(account, ledger.CodeTradingOutflow, quote), Side: ledger.Debit, Quantity: n},
			ledger.Entry{Account: ledger.CustomerAccount(account, ledger.CodeWallet, base), Side: ledger.Debit, Quantity: qty},
			ledger.Entry{Account: ledger.CustomerAccount(account, ledger.CodeTradingInflow, base), Side: ledger.Credit, Quantity: qty})
	} else {
		out = append(out,
			ledger.Entry{Account: ledger.CustomerAccount(account, ledger.CodeWallet, base), Side: ledger.Credit, Quantity: qty},
			ledger.Entry{Account: ledger.CustomerAccount(account, ledger.CodeTradingOutflow, base), Side: ledger.Debit, Quantity: qty},
			ledger.Entry{Account: ledger.CustomerAccount(account, ledger.CodeWallet, quote), Side: ledger.Debit, Quantity: n},
			ledger.Entry{Account: ledger.CustomerAccount(account, ledger.CodeTradingInflow, quote), Side: ledger.Credit, Quantity: n})
	}
	return append(out, feeledger.ChargeEntries(account, charges)...)
}

// writtenFailure is a failure after the fill has written (its reservation,
// its disclosure): the transaction must roll back, so it is never a refusal a
// stage records and commits (paperRefusal), whatever its code says.
type writtenFailure struct{ error }

func (w writtenFailure) Unwrap() error { return w.error }

const paperColumns = `id, intent_id, account_id, instrument_id, side, base_asset_id, quote_asset_id, base_quantity::text,
	notional_at_reference::text, spread_amount::text, platform_fee_amount::text, venue_fee_amount::text,
	network_fee_amount::text, total_quote::text, schedule_version, schedule_hash, schedule, disclosure, disclosure_hash,
	reservation_id::text, journal_transaction_id, effect_id, evidence_ref, evidence_sha256, created_at`

func scanFill(row pgx.Row) (PaperFill, error) {
	var f PaperFill
	var base, notional, spread, platform, venue, network, total string
	if err := row.Scan(&f.ID, &f.IntentID, &f.AccountID, &f.InstrumentID, &f.Side, &f.BaseAsset, &f.QuoteAsset, &base, &notional,
		&spread, &platform, &venue, &network, &total, &f.ScheduleVersion, &f.ScheduleHash, &f.Schedule, &f.DisclosureBytes,
		&f.DisclosureHash, &f.ReservationID, &f.JournalTransactionID, &f.EffectID, &f.EvidenceRef, &f.EvidenceSHA256,
		&f.CreatedAt); err != nil {
		return PaperFill{}, err
	}
	for _, p := range []struct {
		dst *money.Quantity
		src string
	}{{&f.BaseQuantity, base}, {&f.NotionalAtReference, notional}, {&f.SpreadAmount, spread}, {&f.PlatformFeeAmount, platform},
		{&f.VenueFeeAmount, venue}, {&f.NetworkFeeAmount, network}, {&f.TotalQuote, total}} {
		v, err := money.ScanQuantity(p.src)
		if err != nil {
			return PaperFill{}, err
		}
		*p.dst = v
	}
	if err := json.Unmarshal(f.DisclosureBytes, &f.Disclosure); err != nil {
		return PaperFill{}, errs.Wrap(err, errs.CodeInternal, "orders: stored disclosure is unreadable")
	}
	f.VenueLabel = VenueLabelPaper
	f.CreatedAt = f.CreatedAt.UTC()
	return f, nil
}

func (v *PaperVenue) fillByIntent(ctx context.Context, q db.Querier, iid econ.IntentID) (PaperFill, bool, error) {
	f, err := scanFill(q.QueryRow(ctx, `SELECT `+paperColumns+` FROM paper_fills WHERE intent_id = $1`, iid))
	if errors.Is(err, pgx.ErrNoRows) {
		return PaperFill{}, false, nil
	}
	if err != nil {
		return PaperFill{}, false, dbErr("read paper fill", err)
	}
	return f, true, nil
}

// Fill returns the paper fill of an intent.
func (v *PaperVenue) Fill(ctx context.Context, q db.Querier, iid econ.IntentID) (PaperFill, error) {
	f, found, err := v.fillByIntent(ctx, q, iid)
	if err != nil {
		return PaperFill{}, err
	}
	if !found {
		return PaperFill{}, errs.New(errs.CodeNotFound, "orders: no paper fill for this intent")
	}
	return f, nil
}

// VerifyFillRecord is what an auditor runs on a stored paper fill's record
// alone: the schedule bytes must be a canonical schedule; the disclosure bytes
// must be canonical, must hash to the recorded hash, and must be exactly the
// disclosure the schedule produces for the disclosed input
// (fees.VerifyDisclosure). A disclosure edited in any way -- one unit of
// spread, a rate, a rounding mode -- fails.
func VerifyFillRecord(schedule, disclosure []byte, disclosureHash string) (fees.Disclosure, error) {
	s, err := DecodeSchedule(schedule)
	if err != nil {
		return fees.Disclosure{}, err
	}
	sum := sha256.Sum256(disclosure)
	if hex.EncodeToString(sum[:]) != disclosureHash {
		return fees.Disclosure{}, rejection(ReasonPaperDisclosureMismatch, "the stored disclosure does not match its hash")
	}
	var d fees.Disclosure
	dec := json.NewDecoder(bytes.NewReader(disclosure))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return fees.Disclosure{}, errs.Wrap(err, errs.CodeValidationFailed, "orders: the stored disclosure is not a disclosure").
			WithField("reason", ReasonPaperDisclosureMismatch)
	}
	again, err := d.Canonical()
	if err != nil {
		return fees.Disclosure{}, err
	}
	if !bytes.Equal(again, disclosure) {
		return fees.Disclosure{}, rejection(ReasonPaperDisclosureMismatch, "the stored disclosure is not in canonical form")
	}
	if err := fees.VerifyDisclosure(s, d); err != nil {
		return fees.Disclosure{}, errs.Wrap(err, errs.CodeValidationFailed, "orders: the stored disclosure does not verify against its schedule").
			WithField("reason", ReasonPaperDisclosureMismatch)
	}
	return d, nil
}

// VerifyFill proves a stored paper fill end to end: its record verifies
// (VerifyFillRecord), the amounts on the fill are the disclosure's, and the
// journal it names posted exactly those amounts to their own accounts -- the
// spread to FEES_SPREAD / SPREAD_REVENUE and nothing else anywhere. Every
// unit of spread the customer paid is therefore discoverable from the record.
func (v *PaperVenue) VerifyFill(ctx context.Context, q db.Querier, iid econ.IntentID) error {
	f, err := v.Fill(ctx, q, iid)
	if err != nil {
		return err
	}
	d, err := VerifyFillRecord(f.Schedule, f.DisclosureBytes, f.DisclosureHash)
	if err != nil {
		return err
	}
	if !d.Spread.Amount.Equal(f.SpreadAmount) || !d.NotionalAtReference.Equal(f.NotionalAtReference) || !d.Input.BaseQty.Equal(f.BaseQuantity) {
		return rejection(ReasonPaperDisclosureMismatch, "the fill's amounts are not its disclosure's")
	}
	total := d.TotalDebit
	if f.Side == SideSell {
		total = d.TotalCredit
	}
	if !total.Equal(f.TotalQuote) {
		return rejection(ReasonPaperDisclosureMismatch, "the fill's total is not its disclosure's")
	}
	jt, err := v.ledger.Transaction(ctx, q, f.JournalTransactionID)
	if err != nil {
		return err
	}
	sum := map[ledger.Code]money.Quantity{}
	for _, e := range jt.Entries {
		if e.Account.AssetID != f.QuoteAsset {
			continue
		}
		key := e.Account.Code
		if (e.Account.OwnerType == ledger.OwnerCustomer && e.Side == ledger.Debit) || (e.Account.OwnerType == ledger.OwnerPlatform && e.Side == ledger.Credit) {
			sum[key] = sum[key].Add(e.Quantity)
		}
	}
	checks := []struct {
		code ledger.Code
		want money.Quantity
	}{
		{ledger.CodeFeesSpread, f.SpreadAmount}, {ledger.CodeSpreadRevenue, f.SpreadAmount},
		{ledger.CodeFeesPlatform, f.PlatformFeeAmount}, {ledger.CodePlatformFeeRevenue, f.PlatformFeeAmount},
		{ledger.CodeFeesVenue, f.VenueFeeAmount}, {ledger.CodeFeesNetwork, f.NetworkFeeAmount},
	}
	for _, c := range checks {
		if !sum[c.code].Equal(c.want) {
			return rejection(ReasonPaperDisclosureMismatch, "the journal's "+string(c.code)+" is not the disclosed amount")
		}
	}
	charged := f.SpreadAmount.Add(f.PlatformFeeAmount).Add(f.VenueFeeAmount).Add(f.NetworkFeeAmount)
	if !charged.Equal(d.Spread.Amount.Add(d.Fees.Total).Add(networkInTotal(d))) {
		return rejection(ReasonPaperDisclosureMismatch, "the charges posted are not the charges disclosed")
	}
	return nil
}

func networkInTotal(d fees.Disclosure) money.Quantity {
	if d.Network.InQuoteAsset {
		return d.Network.Amount
	}
	return money.Quantity{}
}

// PaperOrderRequest is a customer market order on the paper venue.
type PaperOrderRequest struct {
	InstrumentID instruments.InstrumentID
	AccountID    accounts.AccountID
	// Mode is PAPER (default) or SANDBOX.
	Mode     gates.Mode
	Side     Side
	Quantity money.Quantity
	// ProtectionPrice optionally bounds the execution price: a buy never
	// executes above it, a sell never below it.
	ProtectionPrice *money.Price
	IdempotencyKey  string
	ActorType       security.ActorType
	ActorID         string
	AgentID         string
	AgentAuthority  agentauthority.Level
	StrategyRef     string
	CorrelationID   string
	Jurisdiction    string
	Risk            RiskContext
}

// PaperOutcome is the state of a paper order after a stage.
type PaperOutcome struct {
	Intent    econ.Intent
	Fill      *PaperFill
	Rejection error
}

// paperAccept records the paper order's intent and routes it (RECEIVED ->
// ROUTED, or REJECTED). It is idempotent on (account, idempotency key).
func (v *PaperVenue) paperAccept(ctx context.Context, tx pgx.Tx, r PaperOrderRequest) (PaperOutcome, error) {
	if r.AccountID.IsZero() || !r.Side.Valid() || !r.Quantity.IsPositive() {
		return PaperOutcome{}, rejection(ReasonInvalidRequest, "a paper order needs an account, a side and a positive quantity")
	}
	if err := validKey(r.IdempotencyKey); err != nil {
		return PaperOutcome{}, err
	}
	mode := r.Mode
	if mode == "" {
		mode = gates.ModePaper
	}
	m, err := v.market(ctx, tx, r.InstrumentID, v.clk.Now().UTC())
	if err != nil {
		return PaperOutcome{}, err
	}
	terms := paperTerms{Venue: "PAPER", VenueLabel: VenueLabelPaper, Side: r.Side, Quantity: r.Quantity.String(), LiquidityIntent: TakeLiquidity}
	if r.ProtectionPrice != nil {
		if r.ProtectionPrice.QuoteAsset != m.quote.ID.String() || !r.ProtectionPrice.Mantissa.IsPositive() {
			return PaperOutcome{}, rejection(ReasonPriceQuoteAsset, "the protection price must be a positive price in the quote asset")
		}
		terms.Protection = &priceDoc{Mantissa: r.ProtectionPrice.Mantissa.String(), Scale: r.ProtectionPrice.Scale}
	}
	if r.Risk.Authority != nil || r.Risk.Strategy != nil || r.Risk.Sandbox != nil {
		rc := r.Risk
		terms.Risk = &rc
	}
	raw, err := json.Marshal(terms)
	if err != nil {
		return PaperOutcome{}, errs.Wrap(err, errs.CodeInternal, "orders: encode paper terms")
	}
	kind := econ.KindAcquireExposure
	if r.Side == SideSell {
		kind = econ.KindReduceExposure
	}
	base, qty := m.base.ID, r.Quantity
	in, existed, err := v.intents.Submit(ctx, tx, econ.Intent{
		AccountID: r.AccountID, Kind: kind, Mode: mode, Capability: gates.SpreadEngine, ActorType: r.ActorType, ActorID: r.ActorID,
		AgentID: r.AgentID, StrategyRef: r.StrategyRef, InstrumentID: m.inst.ID.String(), AssetID: &base, Quantity: &qty,
		Terms: raw, IdempotencyKey: r.IdempotencyKey, CorrelationID: r.CorrelationID,
	})
	if err != nil {
		return PaperOutcome{}, err
	}
	if existed {
		return v.paperSettled(ctx, tx, in)
	}
	level, err := v.verify.Level(ctx, tx, r.AccountID)
	if err != nil {
		return PaperOutcome{}, err
	}
	route, err := econ.Route(econ.RouteRequest{
		Intent:         in,
		Subject:        settlement.Subject{Type: settlement.SubjectInstrument, ID: m.inst.ID.String(), AssetID: base.String()},
		AgentAuthority: r.AgentAuthority, Jurisdiction: r.Jurisdiction, Verification: level,
	}, v.router, v.clk.Now().UTC())
	if err != nil {
		return PaperOutcome{}, err
	}
	if !route.MayExecuteNow() {
		in, err = v.intents.Transition(ctx, tx, in.ID, econ.StatusRejected, v.actor, "route refused", ReasonRouteRefused)
		if err != nil {
			return PaperOutcome{}, err
		}
		return PaperOutcome{Intent: in, Rejection: rejection(ReasonRouteRefused, "the settlement compiler refused the paper order")}, nil
	}
	in, err = v.intents.Transition(ctx, tx, in.ID, econ.StatusRouted, v.actor, "routed: "+route.Product, "")
	if err != nil {
		return PaperOutcome{}, err
	}
	return PaperOutcome{Intent: in}, nil
}

func (v *PaperVenue) paperSettled(ctx context.Context, q db.Querier, in econ.Intent) (PaperOutcome, error) {
	out := PaperOutcome{Intent: in}
	if f, found, err := v.fillByIntent(ctx, q, in.ID); err != nil {
		return PaperOutcome{}, err
	} else if found {
		f.Replayed = true
		out.Fill = &f
	}
	if in.Status == econ.StatusRejected {
		out.Rejection = rejection(in.RejectionCode, "the paper order was rejected: "+in.RejectionCode)
	}
	return out, nil
}

// paperApprove authorizes SPREAD_ENGINE in the intent's mode and evaluates
// Risk Kernel V2 (ROUTED -> RISK_APPROVED, or REJECTED).
func (v *PaperVenue) paperApprove(ctx context.Context, tx pgx.Tx, iid econ.IntentID) (PaperOutcome, error) {
	in, err := lockIntent(ctx, tx, v.intents, iid)
	if err != nil {
		return PaperOutcome{}, err
	}
	if in.Status != econ.StatusRouted {
		return v.paperSettled(ctx, tx, in)
	}
	reject := func(code string, rej error) (PaperOutcome, error) {
		in, err := v.intents.Transition(ctx, tx, in.ID, econ.StatusRejected, v.actor, "rejected: "+code, code)
		if err != nil {
			return PaperOutcome{}, err
		}
		return PaperOutcome{Intent: in, Rejection: rej}, nil
	}
	if err := v.auth.Require(ctx, tx, gates.SpreadEngine, in.Mode); err != nil {
		code := errs.CodeOf(err)
		if code == errs.CodeCapabilityNotApproved || code == errs.CodeKillSwitchActive {
			return reject(string(code), err)
		}
		return PaperOutcome{}, err
	}
	input, err := v.riskInput(ctx, tx, in)
	if err != nil {
		return PaperOutcome{}, err
	}
	d := risk.EvaluateAdvanced(v.policy, input)
	if _, err := v.risk.RecordAdvancedDecision(ctx, tx, input, d, in.CorrelationID); err != nil {
		return PaperOutcome{}, dbErr("record risk decision", err)
	}
	if d.Verdict != risk.Allow {
		return reject(string(errs.CodeRiskRejected), d.RejectionError())
	}
	in, err = v.intents.Transition(ctx, tx, in.ID, econ.StatusRiskApproved, v.actor, "risk approved: "+d.Hash, "")
	if err != nil {
		return PaperOutcome{}, err
	}
	return PaperOutcome{Intent: in}, nil
}

// riskInput is the paper order's Risk Kernel V2 input: identity, account,
// instrument, the oracle for a buy, and the balance valued at the current
// mark when one exists.
func (v *PaperVenue) riskInput(ctx context.Context, q db.Querier, in econ.Intent) (risk.AdvancedInput, error) {
	now := v.clk.Now().UTC()
	instrumentID, err := instruments.ParseInstrumentID(in.InstrumentID)
	if err != nil {
		return risk.AdvancedInput{}, rejection(ReasonPaperIntentShape, "the intent's instrument is not an instrument id")
	}
	m, err := v.market(ctx, q, instrumentID, now)
	if err != nil {
		return risk.AdvancedInput{}, err
	}
	level, err := v.verify.Level(ctx, q, in.AccountID)
	if err != nil {
		return risk.AdvancedInput{}, err
	}
	acct, err := accounts.NewRepository().Get(ctx, q, in.AccountID)
	if err != nil {
		return risk.AdvancedInput{}, err
	}
	var terms paperTerms
	if err := json.Unmarshal(in.Terms, &terms); err != nil {
		return risk.AdvancedInput{}, errs.Wrap(err, errs.CodeInternal, "orders: paper terms")
	}
	riskIncreasing := in.Kind == econ.KindAcquireExposure
	status := string(m.inst.Status)
	if !m.inst.IsActiveAt(now) {
		status = "OUTSIDE_ACTIVE_WINDOW"
	}
	input := risk.AdvancedInput{
		Stage: risk.StagePreTrade,
		Intent: risk.AdvancedIntent{
			ID: in.ID.String(), Kind: string(in.Kind), AccountID: in.AccountID.String(), ActorType: string(in.ActorType),
			AgentID: in.AgentID, StrategyRef: in.StrategyRef, Capability: string(in.Capability), Mode: string(in.Mode),
			InstrumentID: in.InstrumentID, InstrumentKind: string(m.inst.Type), RiskIncreasing: riskIncreasing,
			SandboxOriginated: terms.Risk != nil && terms.Risk.Sandbox != nil,
		},
		Identity: &risk.IdentitySnapshot{Verified: level.AtLeast(valuedomain.VerificationNodalIdentity)},
		Account:  &risk.AdvancedAccountSnapshot{Status: string(acct.Status)},
		Instrument: &risk.InstrumentSnapshot{Status: status, SessionOpen: v.sessions.IsOpen(m.calendar, now),
			Expired: m.expiry != nil && !now.Before(*m.expiry)},
		Now: now,
	}
	if riskIncreasing {
		if input.Oracle, err = oracleSection(ctx, v.prices, q, m.base.ID, m.quote.ID, v.lookback, now); err != nil {
			return risk.AdvancedInput{}, err
		}
	}
	if mark, err := v.prices.Latest(ctx, q, m.base.ID, m.quote.ID, v.lookback, now); err == nil {
		qin := fees.QuoteInput{Side: fees.Side(terms.Side), BaseQty: *in.Quantity, BaseDecimals: m.base.Decimals,
			QuoteDecimals: m.quote.Decimals, ReferencePrice: mark, StrategyOriginated: in.StrategyRef != ""}
		if quote, err := m.schedule.Quote(qin); err == nil {
			if input.Intent.NotionalUSD, err = money.QuoteQuantityToUSD(quote.NotionalAtReference, m.quote.Decimals, money.RoundUp); err != nil {
				return risk.AdvancedInput{}, errs.Wrap(err, errs.CodeOverflow, "orders: paper notional in USD")
			}
			if input.Balance, err = v.balance(ctx, q, in, m, terms.Side, quote, mark, now); err != nil {
				return risk.AdvancedInput{}, err
			}
		}
	} else if errs.CodeOf(err) != errs.CodeStaleMarketData {
		return risk.AdvancedInput{}, err
	}
	if terms.Risk != nil {
		input.Authority, input.Strategy, input.Sandbox = terms.Risk.Authority, terms.Risk.Strategy, terms.Risk.Sandbox
	}
	if input.KillSwitches, err = activeKillSwitches(ctx, v.kills, q); err != nil {
		return risk.AdvancedInput{}, err
	}
	return input, nil
}

func (v *PaperVenue) balance(ctx context.Context, q db.Querier, in econ.Intent, m paperMarket, side Side, quote fees.Quote, mark money.Price, now time.Time) (*risk.BalanceSnapshot, error) {
	asset := m.quote.ID
	if side == SideSell {
		asset = m.base.ID
	}
	avail, err := v.capital.Availability(ctx, q, in.AccountID.String(), asset, now)
	if err != nil {
		return nil, err
	}
	availQuote, required := avail.Available, quote.TotalDebit
	if side == SideSell {
		if availQuote, err = money.Notional(avail.Available, m.base.Decimals, mark, m.quote.Decimals, money.RoundDown); err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "orders: value holdings")
		}
		required = quote.NotionalAtReference
	}
	a, err := money.QuoteQuantityToUSD(availQuote, m.quote.Decimals, money.RoundDown)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeOverflow, "orders: available balance in USD")
	}
	r, err := money.QuoteQuantityToUSD(required, m.quote.Decimals, money.RoundUp)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeOverflow, "orders: required balance in USD")
	}
	return &risk.BalanceSnapshot{Available: a, Required: r}, nil
}

// paperFillStage fills a risk-approved paper intent (RISK_APPROVED ->
// EXECUTING) or records why it could not be filled (-> REJECTED).
func (v *PaperVenue) paperFillStage(ctx context.Context, tx pgx.Tx, iid econ.IntentID) (PaperOutcome, error) {
	in, err := lockIntent(ctx, tx, v.intents, iid)
	if err != nil {
		return PaperOutcome{}, err
	}
	if in.Status != econ.StatusRiskApproved {
		return v.paperSettled(ctx, tx, in)
	}
	fill, err := v.ExecuteIntent(ctx, tx, in)
	if err != nil {
		if code, ok := paperRefusal(err); ok {
			in, terr := v.intents.Transition(ctx, tx, in.ID, econ.StatusRejected, v.actor, "paper fill refused: "+code, code)
			if terr != nil {
				return PaperOutcome{}, terr
			}
			return PaperOutcome{Intent: in, Rejection: err}, nil
		}
		return PaperOutcome{}, err
	}
	in, err = v.intents.Transition(ctx, tx, in.ID, econ.StatusExecuting, v.actor, "filled on the paper venue", "")
	if err != nil {
		return PaperOutcome{}, err
	}
	return PaperOutcome{Intent: in, Fill: &fill}, nil
}

// paperRefusal classifies the refusals ExecuteIntent makes before writing
// anything, which a stage records instead of rolling back.
func paperRefusal(err error) (string, bool) {
	if errors.As(err, new(writtenFailure)) {
		return "", false
	}
	if r := ReasonOf(err); r != "" && r != ReasonPaperDisclosureMismatch {
		return r, true
	}
	switch c := errs.CodeOf(err); c {
	case errs.CodeStaleMarketData, errs.CodeInsufficientBuyingPower, errs.CodeCapabilityNotApproved, errs.CodeKillSwitchActive,
		errs.CodeAccountFrozen, errs.CodeAssetRestricted:
		return string(c), true
	}
	return "", false
}

// paperComplete moves a filled paper intent to COMPLETED.
func (v *PaperVenue) paperComplete(ctx context.Context, tx pgx.Tx, iid econ.IntentID) (PaperOutcome, error) {
	in, err := lockIntent(ctx, tx, v.intents, iid)
	if err != nil {
		return PaperOutcome{}, err
	}
	if in.Status == econ.StatusExecuting {
		if in, err = v.intents.Transition(ctx, tx, in.ID, econ.StatusCompleted, v.actor, "paper fill complete", ""); err != nil {
			return PaperOutcome{}, err
		}
	}
	return v.paperSettled(ctx, tx, in)
}

// Submit runs a paper order's stages, each in its own transaction.
func (v *PaperVenue) Submit(ctx context.Context, d *db.DB, r PaperOrderRequest) (PaperOutcome, error) {
	if d == nil {
		return PaperOutcome{}, errs.New(errs.CodeValidationFailed, "orders: the paper pipeline needs the database")
	}
	run := func(fn func(context.Context, pgx.Tx) (PaperOutcome, error)) (PaperOutcome, error) {
		var out PaperOutcome
		err := d.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted, MaxRetries: PipelineMaxRetries}, func(ctx context.Context, tx pgx.Tx) error {
			o, err := fn(ctx, tx)
			out = o
			return err
		})
		return out, err
	}
	out, err := run(func(ctx context.Context, tx pgx.Tx) (PaperOutcome, error) { return v.paperAccept(ctx, tx, r) })
	if err != nil || out.Rejection != nil {
		return out, firstErr(err, out.Rejection)
	}
	iid := out.Intent.ID
	var filled *PaperFill
	for _, stage := range []func(context.Context, pgx.Tx, econ.IntentID) (PaperOutcome, error){v.paperApprove, v.paperFillStage, v.paperComplete} {
		out, err = run(func(ctx context.Context, tx pgx.Tx) (PaperOutcome, error) { return stage(ctx, tx, iid) })
		if err != nil || out.Rejection != nil {
			return out, firstErr(err, out.Rejection)
		}
		if out.Fill != nil {
			filled = out.Fill
		}
	}
	if out.Fill == nil {
		out.Fill = filled
	}
	return out, nil
}

func firstErr(errsIn ...error) error {
	for _, e := range errsIn {
		if e != nil {
			return e
		}
	}
	return nil
}
