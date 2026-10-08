package orders

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/identity"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/legalrouter"
	"github.com/nodal/controlplane/internal/matching"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuation"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// VerificationSource reports an account's verification level, reading through
// the querier the caller supplies (the command's own transaction).
type VerificationSource interface {
	Level(ctx context.Context, q db.Querier, account accounts.AccountID) (valuedomain.VerificationLevel, error)
}

// identityVerification is the default VerificationSource: what Nodal can
// establish by itself (internal/identity), read inside the caller's
// transaction rather than through the pool.
type identityVerification struct{}

func (identityVerification) Level(ctx context.Context, q db.Querier, account accounts.AccountID) (valuedomain.VerificationLevel, error) {
	r, err := identity.NewVerificationResolver(q)
	if err != nil {
		return valuedomain.VerificationNone, err
	}
	return r.Level(ctx, account)
}

// SessionCalendar answers whether an instrument's trading calendar is open.
type SessionCalendar interface {
	IsOpen(calendar string, at time.Time) bool
}

// alwaysOpen24x7 is the default calendar: "24x7" is always open and every
// other calendar is closed until a real calendar is configured. It fails
// closed rather than inventing exchange hours.
type alwaysOpen24x7 struct{}

func (alwaysOpen24x7) IsOpen(calendar string, _ time.Time) bool { return calendar == "24x7" }

// Config wires a Service. Every dependency except the optional ones is
// required; a service that could not reach one of them would skip a control.
type Config struct {
	Clock        clock.Clock
	Ledger       *ledger.Service
	Capital      *capital.Service
	Intents      *econ.IntentStore
	Effects      *econ.EffectStore
	Authorizer   *econ.Authorizer
	Registry     *econ.Registry
	Risk         *risk.Store
	RiskPolicy   risk.AdvancedPolicy
	Router       *legalrouter.Router
	Instruments  *instruments.Repository
	Assets       *assets.Repository
	Prices       valuation.PriceSource
	KillSwitches econ.KillSwitchReader

	// Verification defaults to the identity resolver read in the caller's
	// transaction; Sessions defaults to "24x7 only".
	Verification VerificationSource
	Sessions     SessionCalendar
	// ReservationTTL bounds how long a reservation waits for its order to
	// reach the book (default 15 minutes). A resting order locks its
	// reservation, so expiry never touches it.
	ReservationTTL time.Duration
	// MarkLookback is how far back a mark is looked for when deciding whether
	// "a mark exists" (default 30 days). Whether it is fresh enough is the
	// risk kernel's decision, not this window's.
	MarkLookback time.Duration
	// Actor is the deterministic service that applies effects (default
	// SYSTEM "orders-service"). It is never an agent.
	Actor econ.Actor
}

// Service is the internal order book. It holds no financial state: the
// database is the truth, and the in-memory engines are a cache of the
// journal that is checked against it on every command.
type Service struct {
	clk       clock.Clock
	ledger    *ledger.Service
	capital   *capital.Service
	intents   *econ.IntentStore
	effects   *econ.EffectStore
	auth      *econ.Authorizer
	registry  *econ.Registry
	risk      *risk.Store
	policy    risk.AdvancedPolicy
	router    *legalrouter.Router
	instr     *instruments.Repository
	assets    *assets.Repository
	prices    valuation.PriceSource
	kills     econ.KillSwitchReader
	verify    VerificationSource
	sessions  SessionCalendar
	resTTL    time.Duration
	lookback  time.Duration
	actor     econ.Actor
	engines   *engineCache
	configsMu sync.Mutex
	configs   map[BookID]*bookConfig
}

// NewService validates the configuration and returns a Service.
func NewService(c Config) (*Service, error) {
	if c.Clock == nil || c.Ledger == nil || c.Capital == nil || c.Intents == nil || c.Effects == nil ||
		c.Authorizer == nil || c.Registry == nil || c.Risk == nil || c.Router == nil || c.Instruments == nil ||
		c.Assets == nil || c.Prices == nil || c.KillSwitches == nil {
		return nil, errs.New(errs.CodeValidationFailed, "orders: every dependency of the order service is required")
	}
	if c.RiskPolicy.Missing() {
		return nil, errs.New(errs.CodeValidationFailed, "orders: a risk policy is required")
	}
	if c.Verification == nil {
		c.Verification = identityVerification{}
	}
	if c.Sessions == nil {
		c.Sessions = alwaysOpen24x7{}
	}
	if c.ReservationTTL <= 0 {
		c.ReservationTTL = 15 * time.Minute
	}
	if c.ReservationTTL > capital.MaxReservationTTL {
		return nil, errs.New(errs.CodeValidationFailed, "orders: reservation TTL exceeds the capital maximum")
	}
	if c.MarkLookback <= 0 {
		c.MarkLookback = 30 * 24 * time.Hour
	}
	if c.Actor == (econ.Actor{}) {
		c.Actor = econ.SystemActor("orders-service")
	}
	if !isServiceActor(c.Actor) {
		return nil, errs.New(errs.CodeValidationFailed, "orders: the applying actor must be a non-agent actor")
	}
	return &Service{
		clk: c.Clock, ledger: c.Ledger, capital: c.Capital, intents: c.Intents, effects: c.Effects, auth: c.Authorizer,
		registry: c.Registry, risk: c.Risk, policy: c.RiskPolicy, router: c.Router, instr: c.Instruments, assets: c.Assets,
		prices: c.Prices, kills: c.KillSwitches, verify: c.Verification, sessions: c.Sessions, resTTL: c.ReservationTTL,
		lookback: c.MarkLookback, actor: c.Actor, engines: newEngineCache(), configs: map[BookID]*bookConfig{},
	}, nil
}

// isServiceActor reports whether a may write a transition row: a typed,
// named actor that is not an agent.
func isServiceActor(a econ.Actor) bool {
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

// postingContext attributes every ledger posting to the deterministic
// service: an agent proposes intents and never posts, and a user's request is
// recorded on the order and the intent, not as the poster of the journal.
func (s *Service) postingContext(ctx context.Context) context.Context {
	return security.WithPrincipal(ctx, security.Principal{SubjectID: s.actor.ID, ActorType: security.ActorSystem})
}

// bookConfig is the immutable part of a book: frozen in SQL (OB008) once the
// book is opened, so it can be cached for the life of the process.
type bookConfig struct {
	ID                BookID
	InstrumentID      instruments.InstrumentID
	EngineInstrument  string
	Mode              gates.Mode
	Terms             BookTerms
	TermsSource       TermsSource
	TermsVersion      int
	TermsHash         []byte
	MakerSchedule     fees.Schedule
	MakerScheduleHash string
	TakerSchedule     fees.Schedule
	TakerScheduleHash string
	SnapshotInterval  int64
	GenesisHash       [32]byte
	CreatedAt         time.Time
}

func (c *bookConfig) engineConfig() matching.Config {
	return matching.Config{Instrument: c.EngineInstrument, MaxPriceTicks: c.Terms.MaxPriceTicks}
}

// bookCursor is the mutable part of a book, read under its row lock.
type bookCursor struct {
	Status         BookStatus
	LastCommandSeq int64
	LastEventSeq   int64
	LastTradeSeq   int64
	StateHash      [32]byte
}

// OpenBookRequest opens the book of one instrument.
type OpenBookRequest struct {
	InstrumentID instruments.InstrumentID
	// Mode is PAPER or SANDBOX.
	Mode gates.Mode
	// SpotTerms are required for a SPOT_PAIR instrument, which has no
	// canonical terms row, and refused for every other instrument, whose
	// terms are its current canonical version.
	SpotTerms *instruments.Terms
	// MakerSchedule and TakerSchedule price the explicit fees of every fill
	// (ValidateBookSchedule says what they may contain).
	MakerSchedule fees.Schedule
	TakerSchedule fees.Schedule
	// SnapshotInterval is how many commands pass between snapshots
	// (default 64).
	SnapshotInterval int
	Actor            econ.Actor
}

// Book is a book_states row as callers see it.
type Book struct {
	ID                BookID
	InstrumentID      instruments.InstrumentID
	VenueLabel        string
	Mode              gates.Mode
	Status            BookStatus
	Terms             BookTerms
	TermsSource       TermsSource
	TermsVersion      int
	MakerScheduleHash string
	TakerScheduleHash string
	SnapshotInterval  int64
	LastCommandSeq    int64
	LastEventSeq      int64
	LastTradeSeq      int64
	StateHash         [32]byte
}

func bookView(c *bookConfig, cur bookCursor) Book {
	return Book{
		ID: c.ID, InstrumentID: c.InstrumentID, VenueLabel: VenueLabelInternalBook, Mode: c.Mode, Status: cur.Status,
		Terms: c.Terms, TermsSource: c.TermsSource, TermsVersion: c.TermsVersion, MakerScheduleHash: c.MakerScheduleHash,
		TakerScheduleHash: c.TakerScheduleHash, SnapshotInterval: c.SnapshotInterval, LastCommandSeq: cur.LastCommandSeq,
		LastEventSeq: cur.LastEventSeq, LastTradeSeq: cur.LastTradeSeq, StateHash: cur.StateHash,
	}
}

// OpenBook opens the book of an instrument in the caller's transaction. It
// is idempotent: opening an instrument's book again with the same terms and
// schedules returns the existing book, and anything else is a CONFLICT.
func (s *Service) OpenBook(ctx context.Context, tx pgx.Tx, r OpenBookRequest) (Book, error) {
	if !isServiceActor(r.Actor) {
		return Book{}, errs.New(errs.CodeForbidden, "orders: a book is opened by a person or a service, never by an agent")
	}
	if r.Mode != gates.ModePaper && r.Mode != gates.ModeSandbox {
		return Book{}, errs.Newf(errs.CodeValidationFailed, "orders: a book runs in PAPER or SANDBOX, not %q", r.Mode)
	}
	if err := s.auth.Require(ctx, tx, gates.InternalMatching, r.Mode); err != nil {
		return Book{}, err
	}
	interval := int64(r.SnapshotInterval)
	if interval == 0 {
		interval = 64
	}
	if interval < 1 || interval > 100_000 {
		return Book{}, errs.New(errs.CodeValidationFailed, "orders: snapshot interval must be within 1..100000")
	}
	inst, err := s.instr.Get(ctx, tx, r.InstrumentID)
	if err != nil {
		return Book{}, err
	}
	source, version, terms, err := s.bookInstrumentTerms(ctx, tx, inst, r.SpotTerms)
	if err != nil {
		return Book{}, err
	}
	if terms.BaseAssetID == nil {
		return Book{}, errs.New(errs.CodeValidationFailed, "orders: a book's instrument names its base asset")
	}
	base, err := s.assets.Get(ctx, tx, *terms.BaseAssetID)
	if err != nil {
		return Book{}, err
	}
	quoteID := terms.SettlementAssetID
	if terms.QuoteAssetID != nil {
		quoteID = *terms.QuoteAssetID
	}
	quote, err := s.assets.Get(ctx, tx, quoteID)
	if err != nil {
		return Book{}, err
	}
	// Risk limits are stated in USD; the book's quote asset is valued at USD
	// parity for them, which is only honest for a USD-pegged asset.
	if !quote.IsStablecoin || quote.PegCurrency != "USD" {
		return Book{}, errs.New(errs.CodeValidationFailed,
			"orders: a book's quote asset must be a USD-pegged simulated stable asset; risk limits are stated in USD")
	}
	bt, err := DeriveBookTerms(inst, terms, base, quote)
	if err != nil {
		return Book{}, err
	}
	for _, sch := range []fees.Schedule{r.MakerSchedule, r.TakerSchedule} {
		if err := ValidateBookSchedule(sch, bt); err != nil {
			return Book{}, err
		}
	}
	makerBytes, err := r.MakerSchedule.Canonical()
	if err != nil {
		return Book{}, err
	}
	takerBytes, err := r.TakerSchedule.Canonical()
	if err != nil {
		return Book{}, err
	}
	makerHash, err := r.MakerSchedule.Hash()
	if err != nil {
		return Book{}, err
	}
	takerHash, err := r.TakerSchedule.Hash()
	if err != nil {
		return Book{}, err
	}
	termsJSON, err := terms.CanonicalJSON()
	if err != nil {
		return Book{}, errs.Wrap(err, errs.CodeInternal, "orders: encode terms")
	}
	termsHash, err := terms.ContentHash()
	if err != nil {
		return Book{}, errs.Wrap(err, errs.CodeInternal, "orders: hash terms")
	}
	genesis, err := matching.NewBook(matching.Config{Instrument: inst.ID.String(), MaxPriceTicks: bt.MaxPriceTicks})
	if err != nil {
		return Book{}, err
	}
	gh := genesis.StateHash()
	var termsVersion any
	if source == TermsCanonical {
		termsVersion = version
	}
	bid := id.New[bookKind]()
	tag, err := tx.Exec(ctx, `INSERT INTO book_states (id, instrument_id, engine_instrument, mode, status, instrument_kind,
		base_asset_id, quote_asset_id, base_decimals, quote_decimals, price_scale, tick_size, lot_size, min_quantity,
		notional_per_tick_lot, max_price_ticks, terms_source, terms_version, terms, terms_hash, maker_schedule,
		maker_schedule_hash, taker_schedule, taker_schedule_hash, snapshot_interval, genesis_state_hash, state_hash,
		created_by_actor_type, created_by_actor_id)
		VALUES ($1,$2,$3,$4,'OPEN',$5,$6,$7,$8,$9,$10,$11::numeric,$12::numeric,$13::numeric,$14::numeric,$15,$16,$17,$18,$19,
		        $20,$21,$22,$23,$24,$25,$25,$26,$27)
		ON CONFLICT (instrument_id) DO NOTHING`,
		bid, inst.ID, inst.ID.String(), r.Mode, terms.Kind, bt.BaseAsset, bt.QuoteAsset, int16(bt.BaseDecimals),
		int16(bt.QuoteDecimals), bt.PriceScale, bt.TickSize.String(), bt.LotSize.String(), bt.MinQuantity.String(),
		bt.NotionalPerTickLot.String(), bt.MaxPriceTicks, source, termsVersion, termsJSON, termsHash, makerBytes, makerHash,
		takerBytes, takerHash, interval, gh[:], r.Actor.Type, r.Actor.ID)
	if err != nil {
		return Book{}, dbErr("open book", err)
	}
	cfg, cur, err := s.readBook(ctx, tx, inst.ID, false)
	if err != nil {
		return Book{}, err
	}
	if tag.RowsAffected() == 0 {
		same := cfg.Mode == r.Mode && bytes.Equal(cfg.TermsHash, termsHash) && cfg.MakerScheduleHash == makerHash &&
			cfg.TakerScheduleHash == takerHash && cfg.SnapshotInterval == interval
		if !same {
			return Book{}, errs.New(errs.CodeConflict, "orders: this instrument's book is already open with different terms").
				WithField("book_id", cfg.ID.String())
		}
	}
	return bookView(cfg, cur), nil
}

// bookInstrumentTerms resolves the terms a book is opened with.
func (s *Service) bookInstrumentTerms(ctx context.Context, q db.Querier, inst instruments.Instrument, spot *instruments.Terms) (TermsSource, int, instruments.Terms, error) {
	v, err := s.instr.CurrentTerms(ctx, q, inst.ID)
	switch {
	case err == nil:
		if spot != nil {
			return "", 0, instruments.Terms{}, errs.New(errs.CodeValidationFailed,
				"orders: this instrument has canonical terms; a book does not restate them")
		}
		return TermsCanonical, v.Version, v.Terms, nil
	case errs.CodeOf(err) == errs.CodeNotFound:
		if inst.Type != instruments.TypeSpotPair || spot == nil {
			return "", 0, instruments.Terms{}, errs.New(errs.CodeValidationFailed,
				"orders: only a SPOT_PAIR may state its terms when its book is opened; other instruments need canonical terms")
		}
		if spot.Kind != instruments.TypeSpotPair {
			return "", 0, instruments.Terms{}, errs.New(errs.CodeValidationFailed, "orders: spot terms must be SPOT_PAIR terms")
		}
		return TermsBookSpot, 0, *spot, nil
	default:
		return "", 0, instruments.Terms{}, err
	}
}

const bookColumns = `id, instrument_id, engine_instrument, mode, status, instrument_kind, base_asset_id, quote_asset_id,
	base_decimals, quote_decimals, price_scale, tick_size::text, lot_size::text, min_quantity::text,
	notional_per_tick_lot::text, max_price_ticks, terms_source, coalesce(terms_version, 0), terms, terms_hash,
	maker_schedule, maker_schedule_hash, taker_schedule, taker_schedule_hash, snapshot_interval, genesis_state_hash,
	last_command_seq, last_event_seq, last_trade_seq, state_hash, created_at`

// bookLock is the row lock that serializes a book's commands. It is FOR NO KEY
// UPDATE, not FOR UPDATE: it conflicts with itself and with every update of
// the row, which is all the serialization needs, but not with the FOR KEY
// SHARE lock a foreign-key check takes. Inserting an order (whose book_id
// references the book) in another transaction therefore never waits for a
// command, which is what keeps a submission that already holds its
// account's audit stream from deadlocking against a command that needs it.
const bookLock = ` FOR NO KEY UPDATE`

// readBook loads a book by instrument, optionally locking its row. The
// immutable configuration is decoded once and cached; the cursor is always
// read from the row.
func (s *Service) readBook(ctx context.Context, q db.Querier, instrumentID instruments.InstrumentID, lock bool) (*bookConfig, bookCursor, error) {
	sql := `SELECT ` + bookColumns + ` FROM book_states WHERE instrument_id = $1`
	if lock {
		sql += bookLock
	}
	return s.scanBook(q.QueryRow(ctx, sql, instrumentID))
}

// readBookByID is readBook keyed by book id.
func (s *Service) readBookByID(ctx context.Context, q db.Querier, bookID BookID, lock bool) (*bookConfig, bookCursor, error) {
	sql := `SELECT ` + bookColumns + ` FROM book_states WHERE id = $1`
	if lock {
		sql += bookLock
	}
	return s.scanBook(q.QueryRow(ctx, sql, bookID))
}

func (s *Service) scanBook(row pgx.Row) (*bookConfig, bookCursor, error) {
	var (
		c                                      bookConfig
		cur                                    bookCursor
		kind                                   string
		baseDec, quoteDec                      int16
		tick, lot, minQty, nptl                string
		termsRaw, makerRaw, takerRaw, genesis  []byte
		stateHash                              []byte
		instrumentID                           instruments.InstrumentID
		baseAsset, quoteAsset                  assets.AssetID
		priceScale                             int32
		maxTicks                               int64
		termsSource                            string
		termsVersion                           int
		makerHash, takerHash, status, modeText string
	)
	err := row.Scan(&c.ID, &instrumentID, &c.EngineInstrument, &modeText, &status, &kind, &baseAsset, &quoteAsset,
		&baseDec, &quoteDec, &priceScale, &tick, &lot, &minQty, &nptl, &maxTicks, &termsSource, &termsVersion,
		&termsRaw, &c.TermsHash, &makerRaw, &makerHash, &takerRaw, &takerHash, &c.SnapshotInterval, &genesis,
		&cur.LastCommandSeq, &cur.LastEventSeq, &cur.LastTradeSeq, &stateHash, &c.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, bookCursor{}, errs.New(errs.CodeNotFound, "orders: no book is open for this instrument")
	}
	if err != nil {
		return nil, bookCursor{}, dbErr("read book", err)
	}
	cur.Status = BookStatus(status)
	copy(cur.StateHash[:], stateHash)
	s.configsMu.Lock()
	cached, ok := s.configs[c.ID]
	s.configsMu.Unlock()
	if ok {
		return cached, cur, nil
	}
	c.InstrumentID = instrumentID
	c.Mode = gates.Mode(modeText)
	c.TermsSource = TermsSource(termsSource)
	c.TermsVersion = termsVersion
	c.MakerScheduleHash, c.TakerScheduleHash = makerHash, takerHash
	copy(c.GenesisHash[:], genesis)
	var terms instruments.Terms
	if err := json.Unmarshal(termsRaw, &terms); err != nil {
		return nil, bookCursor{}, errs.Wrap(err, errs.CodeInternal, "orders: stored book terms are unreadable")
	}
	if h, err := terms.ContentHash(); err != nil || !bytes.Equal(h, c.TermsHash) {
		return nil, bookCursor{}, errs.New(errs.CodeInternal, "orders: stored book terms do not match their hash")
	}
	if c.MakerSchedule, err = DecodeSchedule(makerRaw); err != nil {
		return nil, bookCursor{}, err
	}
	if c.TakerSchedule, err = DecodeSchedule(takerRaw); err != nil {
		return nil, bookCursor{}, err
	}
	bt := BookTerms{
		InstrumentID: instrumentID, Kind: instruments.InstrumentType(kind), BaseAsset: baseAsset, QuoteAsset: quoteAsset,
		BaseDecimals: uint8(baseDec), QuoteDecimals: uint8(quoteDec), PriceScale: priceScale, MaxPriceTicks: maxTicks, // #nosec G115 -- CHECKed 0..18
		OracleMaxAge:    time.Duration(terms.OracleMaxAgeMS) * time.Millisecond,
		TradingCalendar: terms.TradingCalendar, Expiry: terms.Expiry,
	}
	for _, p := range []struct {
		dst *money.Quantity
		src string
	}{{&bt.TickSize, tick}, {&bt.LotSize, lot}, {&bt.MinQuantity, minQty}, {&bt.NotionalPerTickLot, nptl}} {
		v, err := money.ScanQuantity(p.src)
		if err != nil {
			return nil, bookCursor{}, errs.Wrap(err, errs.CodeInternal, "orders: stored book terms are not integers")
		}
		*p.dst = v
	}
	c.Terms = bt
	s.configsMu.Lock()
	if prior, ok := s.configs[c.ID]; ok {
		s.configsMu.Unlock()
		return prior, cur, nil
	}
	s.configs[c.ID] = &c
	s.configsMu.Unlock()
	return &c, cur, nil
}

// lockBook takes the row lock that serializes every command of a book, and
// returns its configuration and cursor.
func (s *Service) lockBook(ctx context.Context, tx pgx.Tx, instrumentID instruments.InstrumentID) (*bookConfig, bookCursor, error) {
	return s.readBook(ctx, tx, instrumentID, true)
}

// GetBook returns a book by instrument.
func (s *Service) GetBook(ctx context.Context, q db.Querier, instrumentID instruments.InstrumentID) (Book, error) {
	c, cur, err := s.readBook(ctx, q, instrumentID, false)
	if err != nil {
		return Book{}, err
	}
	return bookView(c, cur), nil
}

// SetBookStatus halts, resumes or closes a book. A halted book accepts only
// cancels; a book can be closed only once nothing rests on it.
func (s *Service) SetBookStatus(ctx context.Context, tx pgx.Tx, instrumentID instruments.InstrumentID, to BookStatus, actor econ.Actor, reason string) (Book, error) {
	if !isServiceActor(actor) {
		return Book{}, errs.New(errs.CodeForbidden, "orders: an agent never changes a book's status")
	}
	if strings.TrimSpace(reason) == "" {
		return Book{}, errs.New(errs.CodeValidationFailed, "orders: a book status change needs a reason")
	}
	if !to.Valid() {
		return Book{}, errs.Newf(errs.CodeValidationFailed, "orders: unknown book status %q", to)
	}
	c, cur, err := s.lockBook(ctx, tx, instrumentID)
	if err != nil {
		return Book{}, err
	}
	if cur.Status == to {
		return bookView(c, cur), nil
	}
	if !CanTransitionBook(cur.Status, to) {
		return Book{}, errs.Newf(errs.CodeInvalidStateTransition, "orders: a book cannot move %s -> %s", cur.Status, to)
	}
	if to == BookClosed {
		var resting bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM book_orders WHERE book_id = $1 AND status IN ('OPEN','PARTIALLY_FILLED','PENDING'))`,
			c.ID).Scan(&resting); err != nil {
			return Book{}, dbErr("check resting orders", err)
		}
		if resting {
			return Book{}, errs.New(errs.CodeInvalidStateTransition, "orders: a book with working orders cannot be closed; cancel them first")
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO book_state_transitions (id, book_id, from_status, to_status, actor_type, actor_id, reason, occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, id.New[id.Any](), c.ID, cur.Status, to, actor.Type, actor.ID, reason, s.clk.Now().UTC()); err != nil {
		return Book{}, dbErr("record book transition", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE book_states SET status = $2 WHERE id = $1`, c.ID, to); err != nil {
		return Book{}, dbErr("update book status", err)
	}
	cur.Status = to
	return bookView(c, cur), nil
}

// CrossingRule is one version of a book's house-versus-customer crossing
// rule. With no rule on record the default applies: crossing is forbidden.
type CrossingRule struct {
	BookID                     BookID
	Version                    int
	AllowHouseCustomerCrossing bool
	Reason                     string
	Actor                      econ.Actor
	CreatedAt                  time.Time
}

// SetCrossingRule records the next version of a book's crossing rule. Rules
// are append-only; the latest version is the one in force.
func (s *Service) SetCrossingRule(ctx context.Context, tx pgx.Tx, instrumentID instruments.InstrumentID, allow bool, actor econ.Actor, reason string) (CrossingRule, error) {
	if !isServiceActor(actor) {
		return CrossingRule{}, errs.New(errs.CodeForbidden, "orders: an agent never changes a crossing rule")
	}
	if strings.TrimSpace(reason) == "" {
		return CrossingRule{}, errs.New(errs.CodeValidationFailed, "orders: a crossing rule needs a reason")
	}
	c, _, err := s.lockBook(ctx, tx, instrumentID)
	if err != nil {
		return CrossingRule{}, err
	}
	cur, _, err := s.crossingRule(ctx, tx, c.ID)
	if err != nil {
		return CrossingRule{}, err
	}
	r := CrossingRule{BookID: c.ID, Version: cur.Version + 1, AllowHouseCustomerCrossing: allow, Reason: reason, Actor: actor}
	if err := tx.QueryRow(ctx, `INSERT INTO book_crossing_rules (id, book_id, version, allow_house_customer_crossing, reason, actor_type, actor_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING created_at`, id.New[id.Any](), c.ID, r.Version, allow, reason, actor.Type, actor.ID).
		Scan(&r.CreatedAt); err != nil {
		return CrossingRule{}, dbErr("record crossing rule", err)
	}
	return r, nil
}

// crossingRule returns the rule in force (version 0, forbidding, when none
// was ever recorded).
func (s *Service) crossingRule(ctx context.Context, q db.Querier, bookID BookID) (CrossingRule, bool, error) {
	r := CrossingRule{BookID: bookID}
	err := q.QueryRow(ctx, `SELECT version, allow_house_customer_crossing, reason, actor_type, actor_id, created_at
		FROM book_crossing_rules WHERE book_id = $1 ORDER BY version DESC LIMIT 1`, bookID).
		Scan(&r.Version, &r.AllowHouseCustomerCrossing, &r.Reason, &r.Actor.Type, &r.Actor.ID, &r.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return CrossingRule{BookID: bookID}, false, nil
	}
	if err != nil {
		return CrossingRule{}, false, dbErr("read crossing rule", err)
	}
	return r, true, nil
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
	return errs.Wrap(err, errs.CodeInternal, "orders: "+op)
}
