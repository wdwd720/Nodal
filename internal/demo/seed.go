package demo

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/credit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/nativeasset"
	"github.com/nodal/controlplane/internal/nativemarket"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// Issuer is the identity-provider name every demo user carries.
//
// It is not any deployment's OIDC issuer, and `users` is unique on
// (idp_issuer, idp_subject), so a demo row can never collide with — or be
// mistaken for — a real identity, and no session can resolve to one.
const Issuer = "demo"

// Prefix is what every demo object's name begins with, so the label is legible
// even where the demo_seed_rows join is not available.
const Prefix = "DEMO"

// SeedEpoch is the instant every demo posting is dated.
//
// Fixed, not the wall clock: internal/ledger includes effective_at in a
// posting's content hash, so a moving timestamp under a fixed idempotency key
// would be refused as INVALID_IDEMPOTENCY_REUSE on the second run. The same
// seed should describe the same financial fact.
var SeedEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// Deps are the domain services the seeder drives. Every one is required: a
// seeder that could run with half of them would create half a market.
type Deps struct {
	DB      *db.DB
	Assets  *nativeasset.Service
	Markets *nativemarket.Service
	Credits *credit.Service
	Clock   clock.Clock
	// CreditAssetID is THE Credit asset. It is passed rather than looked up so
	// the seeder cannot create one: a deployment with no Credit asset has not
	// been provisioned, and seeding one would be provisioning it by accident.
	CreditAssetID assets.AssetID
	// Environment is CP_ENV, recorded on every demo row.
	Environment string
	// SandboxTier is cfg.SandboxTier(). False refuses everything.
	SandboxTier bool
}

// Seeder loads the demo data.
type Seeder struct {
	deps Deps
}

// NewSeeder returns a Seeder, or an error saying why this deployment may not
// have one.
func NewSeeder(d Deps) (*Seeder, error) {
	env := strings.ToUpper(strings.TrimSpace(d.Environment))
	if env == "" {
		return nil, errors.New("demo: the environment must be named; a seeder that does not know where it is refuses to run")
	}
	if env == "PROD" {
		return nil, errors.New("demo: refusing to seed PROD; demo data exists only on a sandbox tier (ADR-0023)")
	}
	if !d.SandboxTier {
		return nil, errors.New("demo: this deployment is not a sandbox tier, so it has no place to put simulated data; " +
			"set CP_API_LEGAL_POLICY=SANDBOX on a non-production deployment")
	}
	if d.DB == nil || d.Assets == nil || d.Markets == nil || d.Credits == nil || d.Clock == nil {
		return nil, errors.New("demo: a seeder needs the database and the asset, market and credit services")
	}
	if d.CreditAssetID.IsZero() {
		return nil, errors.New("demo: this deployment has no Credit asset; provision one before seeding demo markets")
	}
	d.Environment = env
	return &Seeder{deps: d}, nil
}

// Result says what one run did.
type Result struct {
	// Created lists the seed keys this run created. Empty on a second run.
	Created []string
	// Existing lists the seed keys that were already present.
	Existing []string
	// Markets is every demo market after the run, created or not.
	Markets []nativemarket.MarketID
}

// Seed loads the demo data, once.
func (s *Seeder) Seed(ctx context.Context) (Result, error) {
	// The seeder acts as itself, in its own name. Every transition row, audit
	// event and credit lot it produces carries this actor, so the demo data is
	// attributable to the thing that made it rather than to a person.
	ctx = security.WithPrincipal(ctx, security.Principal{
		SubjectID: "config:demodata", ActorType: security.ActorSystem, AuthTime: s.deps.Clock.Now(),
	})

	var res Result
	creator, err := s.account(ctx, &res, "creator")
	if err != nil {
		return res, err
	}
	traders := make([]accounts.AccountID, 0, len(traderNames))
	for _, name := range traderNames {
		t, terr := s.account(ctx, &res, name)
		if terr != nil {
			return res, terr
		}
		traders = append(traders, t)
		if ferr := s.fund(ctx, &res, name, t); ferr != nil {
			return res, ferr
		}
	}

	for _, spec := range specs {
		marketID, cerr := s.market(ctx, &res, spec, creator)
		if cerr != nil {
			return res, fmt.Errorf("demo asset %s: %w", spec.symbol, cerr)
		}
		res.Markets = append(res.Markets, marketID)
		for i, tr := range spec.trades {
			if terr := s.trade(ctx, &res, spec, i, tr, marketID, traders); terr != nil {
				return res, fmt.Errorf("demo trade %s#%d: %w", spec.symbol, i, terr)
			}
		}
	}
	return res, nil
}

// --- the demo objects -------------------------------------------------------

var traderNames = []string{"trader-a", "trader-b"}

// traderCredits is what each demo trader is given: 50,000 Credits at the Credit
// asset's six decimal places.
const traderCredits = "50000000000"

// tradeSpec is one demo trade. Trader is an index into traderNames.
type tradeSpec struct {
	Trader int
	Side   nativemarket.Side
	// Amount is Credits for a BUY and asset base units for a SELL.
	Amount string
}

// assetSpec is one demo asset, its market and the trades that give it a price
// history.
//
// The numbers are chosen to stay inside the conservative market-safety policy:
// each buy is a small fraction of a 30,000-Credit virtual reserve, so no single
// trade approaches the 25% price-impact limit and no sequence approaches the
// circuit breaker's 90%. A seeder that tripped the breaker would leave STAGING
// with a paused market and a mystery.
type assetSpec struct {
	key               string
	name              string
	symbol            string
	description       string
	maxSupply         string
	creatorAllocation string
	virtualReserve    string
	platformBPS       money.BPS
	creatorBPS        money.BPS
	trades            []tradeSpec
}

// specs is the demo catalogue. It is a fixed table, so the same run produces
// the same markets in the same order on every deployment.
var specs = []assetSpec{
	{
		key: "orbital", name: "DEMO Orbital Freight", symbol: "DEMOORB",
		description: "Demo data. This asset exists to make the STAGING product testable and represents nothing.",
		maxSupply:   "1000000000000000", creatorAllocation: "100000000000000",
		virtualReserve: "30000000000", platformBPS: 100, creatorBPS: 50,
		trades: []tradeSpec{
			{Trader: 0, Side: nativemarket.Buy, Amount: "2000000000"},
			{Trader: 1, Side: nativemarket.Buy, Amount: "1500000000"},
			{Trader: 0, Side: nativemarket.Buy, Amount: "1000000000"},
		},
	},
	{
		key: "harbour", name: "DEMO Harbour Index", symbol: "DEMOHBR",
		description: "Demo data. A simulated index-style asset for exercising the markets page; it tracks nothing.",
		maxSupply:   "500000000000000", creatorAllocation: "25000000000",
		virtualReserve: "45000000000", platformBPS: 50, creatorBPS: 25,
		trades: []tradeSpec{
			{Trader: 1, Side: nativemarket.Buy, Amount: "3000000000"},
			{Trader: 0, Side: nativemarket.Buy, Amount: "2500000000"},
		},
	},
	{
		key: "lantern", name: "DEMO Lantern Works", symbol: "DEMOLTN",
		description: "Demo data. A thin market, on purpose: it is here so low-liquidity warnings have something to warn about.",
		maxSupply:   "200000000000000", creatorAllocation: "0",
		virtualReserve: "5000000000", platformBPS: 200, creatorBPS: 100,
		trades: []tradeSpec{
			{Trader: 0, Side: nativemarket.Buy, Amount: "200000000"},
		},
	},
	{
		key: "quiet", name: "DEMO Quiet Foundry", symbol: "DEMOQFY",
		description: "Demo data. A market with no trades, so the empty-chart and no-history states have a subject.",
		maxSupply:   "750000000000000", creatorAllocation: "50000000000",
		virtualReserve: "20000000000", platformBPS: 100, creatorBPS: 0,
	},
}

// Specs returns the demo catalogue's keys, for tests and for tools that report
// what a seeded deployment should contain.
func Specs() []string {
	out := make([]string, 0, len(specs))
	for _, s := range specs {
		out = append(out, s.symbol)
	}
	return out
}

// account creates (or finds) one demo account.
func (s *Seeder) account(ctx context.Context, res *Result, name string) (accounts.AccountID, error) {
	key := s.key("account", name)
	if id, found, err := s.existing(ctx, "ACCOUNT", key); err != nil {
		return accounts.AccountID{}, err
	} else if found {
		res.Existing = append(res.Existing, key)
		return accounts.ParseAccountID(id)
	}
	repo := accounts.NewRepository()
	var out accounts.AccountID
	err := s.deps.DB.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		user, uerr := repo.GetUserBySubject(ctx, tx, Issuer, name)
		if uerr != nil {
			if errs.CodeOf(uerr) != errs.CodeNotFound {
				return uerr
			}
			user, uerr = repo.CreateUser(ctx, tx, Issuer, name, nil)
			if uerr != nil {
				return uerr
			}
		}
		owned, oerr := repo.ListByOwner(ctx, tx, user.ID)
		if oerr != nil {
			return oerr
		}
		if len(owned) > 0 {
			out = owned[0].ID
		} else {
			acct, aerr := repo.CreateAccount(ctx, tx, user.ID, accounts.KindCustomer)
			if aerr != nil {
				return aerr
			}
			out = acct.ID
		}
		return s.record(ctx, tx, "ACCOUNT", key, out.String(), Prefix+" "+name)
	})
	if err != nil {
		return accounts.AccountID{}, err
	}
	res.Created = append(res.Created, key)
	return out, nil
}

// fund issues a demo trader's Credits.
//
// PROMOTIONAL and UNFUNDED, deliberately. Nobody paid for these, and the
// provenance says so: valuedomain.DefaultPolicy permits no origin to be
// withdrawn at all, and valuedomain.SandboxPolicy — the one a sandbox tier
// runs — permits PURCHASED and earned value and never PROMOTIONAL. So these
// Credits can be spent inside the product and can never leave it, which is
// exactly what demo money should be.
func (s *Seeder) fund(ctx context.Context, res *Result, name string, account accounts.AccountID) error {
	key := s.key("credits", name)
	if _, found, err := s.existing(ctx, "ACCOUNT", key); err != nil {
		return err
	} else if found {
		res.Existing = append(res.Existing, key)
		return nil
	}
	amount, err := money.ParseQuantity(traderCredits)
	if err != nil {
		return err
	}
	err = s.deps.DB.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		if _, ierr := s.deps.Credits.Issue(ctx, tx, credit.IssueRequest{
			AccountID: account, Quantity: amount,
			Origin:   valuedomain.OriginPromotional,
			Finality: valuedomain.FinalityUnfunded,
			Reference: credit.Reference{
				Type: "demo_seed", ID: key,
			},
			IdempotencyKey: key,
			Reason:         Prefix + " seed: simulated Credits, never real value and never withdrawable",
			EffectiveAt:    SeedEpoch,
		}); ierr != nil {
			return ierr
		}
		return s.record(ctx, tx, "ACCOUNT", key, account.String(), Prefix+" credits for "+name)
	})
	if err != nil {
		return err
	}
	res.Created = append(res.Created, key)
	return nil
}

// market creates one demo asset and opens its market, in one transaction.
//
// One transaction because a half-created market — an asset that is ACTIVE with
// no market, or a mint with no market row — is worse than none: the supply
// would exist with nowhere to trade it.
func (s *Seeder) market(ctx context.Context, res *Result, spec assetSpec, creator accounts.AccountID) (nativemarket.MarketID, error) {
	key := s.key("market", spec.key)
	if id, found, err := s.existing(ctx, "NATIVE_MARKET", key); err != nil {
		return nativemarket.MarketID{}, err
	} else if found {
		res.Existing = append(res.Existing, key)
		return nativemarket.ParseMarketID(id)
	}

	maxSupply, err := money.ParseQuantity(spec.maxSupply)
	if err != nil {
		return nativemarket.MarketID{}, err
	}
	creatorAllocation, err := money.ParseQuantity(spec.creatorAllocation)
	if err != nil {
		return nativemarket.MarketID{}, err
	}
	reserve, err := money.ParseQuantity(spec.virtualReserve)
	if err != nil {
		return nativemarket.MarketID{}, err
	}

	var market nativemarket.Market
	err = s.deps.DB.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		asset, verdict, cerr := s.deps.Assets.CreateDraft(ctx, tx, nativeasset.CreateRequest{
			CreatorAccountID: creator,
			Name:             spec.name,
			Symbol:           spec.symbol,
			Description:      spec.description,
			Supply: nativeasset.SupplyModel{
				MaxSupply:         maxSupply,
				CreatorAllocation: creatorAllocation,
			},
		})
		if cerr != nil {
			return cerr
		}
		// The verdict is the screener's, not the seeder's. If the demo copy
		// ever stops passing on its own, the seeder stops rather than
		// approving its own content.
		if !verdict.State.PermitsActivation() {
			return errs.Newf(errs.CodeValidationFailed,
				"demo asset %s did not pass content screening (%s); fix the demo copy rather than the verdict: %s",
				spec.symbol, verdict.State, verdict.Explain())
		}
		if _, serr := s.deps.Assets.SetStatus(ctx, tx, asset.AssetID, nativeasset.StatusPendingReview,
			Prefix+" seed: submitted by its creator"); serr != nil {
			return serr
		}
		live, aerr := s.deps.Assets.Activate(ctx, tx, asset.AssetID, Prefix+" seed: sandbox demo asset")
		if aerr != nil {
			return aerr
		}
		activatedAt := SeedEpoch
		if live.ActivatedAt != nil {
			activatedAt = live.ActivatedAt.UTC()
		}
		m, merr := s.deps.Markets.Create(ctx, tx, nativemarket.CreateRequest{
			AssetID:              asset.AssetID,
			CreditAssetID:        s.deps.CreditAssetID,
			CreatorID:            creator,
			PoolSupply:           live.Supply.PoolSupply(),
			CreatorAllocation:    live.Supply.CreatorAllocation,
			TreasuryAllocation:   live.Supply.TreasuryAllocation,
			VirtualCreditReserve: reserve,
			Fees:                 nativemarket.Fees{PlatformBPS: spec.platformBPS, CreatorBPS: spec.creatorBPS},
			IdempotencyKey:       key,
			EffectiveAt:          activatedAt,
		})
		if merr != nil {
			return merr
		}
		open, oerr := s.deps.Markets.SetStatus(ctx, tx, m.ID, nativemarket.StatusActive, Prefix+" seed: market opened")
		if oerr != nil {
			return oerr
		}
		market = open
		if rerr := s.record(ctx, tx, "NATIVE_ASSET", s.key("asset", spec.key), asset.AssetID.String(), spec.name); rerr != nil {
			return rerr
		}
		return s.record(ctx, tx, "NATIVE_MARKET", key, open.ID.String(), spec.name)
	})
	if err != nil {
		return nativemarket.MarketID{}, err
	}
	res.Created = append(res.Created, key)
	return market.ID, nil
}

// trade executes one demo trade through the real engine.
func (s *Seeder) trade(ctx context.Context, res *Result, spec assetSpec, i int, tr tradeSpec, marketID nativemarket.MarketID, traders []accounts.AccountID) error {
	key := s.key("trade", fmt.Sprintf("%s-%d", spec.key, i))
	if _, found, err := s.existing(ctx, "NATIVE_TRADE", key); err != nil {
		return err
	} else if found {
		res.Existing = append(res.Existing, key)
		return nil
	}
	if tr.Trader < 0 || tr.Trader >= len(traders) {
		return errs.Newf(errs.CodeInternal, "demo: trade %s names trader %d, which does not exist", key, tr.Trader)
	}
	amount, err := money.ParseQuantity(tr.Amount)
	if err != nil {
		return err
	}
	err = s.deps.DB.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		out, eerr := s.deps.Markets.Execute(ctx, tx, nativemarket.ExecuteRequest{
			MarketID:  marketID,
			AccountID: traders[tr.Trader],
			Side:      tr.Side,
			Amount:    amount,
			// The seeder takes whatever the curve gives: it is not protecting
			// anybody's money, and a minimum it had to guess would make the
			// seed fail on a market that had already moved.
			MinOutput:      money.Quantity{},
			IdempotencyKey: key,
			EffectiveAt:    SeedEpoch,
			CorrelationID:  key,
		})
		if eerr != nil {
			return eerr
		}
		return s.record(ctx, tx, "NATIVE_TRADE", key, out.FillID.String(),
			Prefix+" "+string(tr.Side)+" on "+spec.symbol)
	})
	if err != nil {
		return err
	}
	res.Created = append(res.Created, key)
	return nil
}

// --- the register -----------------------------------------------------------

// key is the deterministic seed key of one object.
func (s *Seeder) key(kind, name string) string {
	return "demo:" + kind + ":" + name
}

// existing reports whether this seed key has already produced an object.
func (s *Seeder) existing(ctx context.Context, kind, key string) (string, bool, error) {
	var (
		refID   string
		gotKind string
	)
	err := s.deps.DB.QueryRow(ctx,
		`SELECT ref_id::text, kind FROM demo_seed_rows WHERE seed_key = $1`, key).Scan(&refID, &gotKind)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, errs.Wrap(err, errs.CodeInternal, "demo: the seed register could not be read")
	}
	if gotKind != kind {
		return "", false, errs.Newf(errs.CodeConflict,
			"demo: seed key %s is registered as a %s, not a %s", key, gotKind, kind)
	}
	return refID, true, nil
}

// record labels one object as demo data, in the same transaction that created
// it. A label written afterwards could be missing for an object that exists.
func (s *Seeder) record(ctx context.Context, tx pgx.Tx, kind, key, refID, label string) error {
	if _, err := tx.Exec(ctx,
		`INSERT INTO demo_seed_rows (seed_key, kind, ref_id, environment, label)
		 VALUES ($1,$2,$3::uuid,$4,$5)`,
		key, kind, refID, s.deps.Environment, label); err != nil {
		return errs.Wrap(err, errs.CodeInternal, "demo: the seed register could not be written")
	}
	return nil
}
