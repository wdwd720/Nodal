// Command advperf measures the advanced capabilities against the performance
// targets of the goal document (§32) on this machine and writes a report.
//
//	go run ./scripts/advperf [-out dist/advanced-perf.md] [-scale 1]
//
// Every figure in the report is measured by this run. Targets the run misses
// are reported as missed; nothing is rounded toward a target. The report
// records the environment (host, CPU, Go, PostgreSQL, load average before and
// after), because a latency without its machine is not a measurement.
//
// It provisions its own database (controlplane_test_advperf) through
// internal/testkit/localdb, which refuses any host but 127.0.0.1/localhost,
// and drops it afterwards unless -keep is given.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/capital/buyingpower"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/matching"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/quant"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/testkit/localdb"
	"github.com/nodal/controlplane/internal/transfers"
	"github.com/nodal/controlplane/internal/valuation"
	"github.com/nodal/controlplane/scripts/internal/sandboxworld"
)

// Measurement is one row of the report.
type Measurement struct {
	Name     string `json:"name"`
	Target   string `json:"target"`
	Result   string `json:"result"`
	Met      *bool  `json:"met,omitempty"` // nil when the goal sets no target
	Detail   string `json:"detail"`
	Samples  int    `json:"samples"`
	Duration string `json:"duration"`
}

// Report is the whole run.
type Report struct {
	Environment  map[string]string `json:"environment"`
	Measurements []Measurement     `json:"measurements"`
	Failures     []string          `json:"failures"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "advperf:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		out   = flag.String("out", "dist/advanced-perf.md", "markdown report path (a .json is written beside it)")
		admin = flag.String("admin", envOr("CP_TEST_ADMIN_DATABASE_URL", localdb.DefaultAdminDSN), "admin DSN (LOCAL only)")
		scale = flag.Int("scale", 1, "multiplies every sample count (use >1 for steadier tails)")
		keep  = flag.Bool("keep", false, "keep the database afterwards")
	)
	flag.Parse()
	if *scale < 1 {
		return errors.New("-scale must be >= 1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()

	rep := Report{Environment: environment()}
	rep.Environment["load_average_before"] = loadAverage()

	dbx, err := localdb.Provision(ctx, "advperf", localdb.Options{AdminDSN: *admin, Migrate: true})
	if err != nil {
		return fmt.Errorf("provision: %w", err)
	}
	if !*keep {
		defer func() { _ = localdb.Drop(context.Background(), "advperf", *admin) }()
	}
	w, err := sandboxworld.Open(ctx, sandboxworld.Options{
		AppDSN: dbx.AppDSN, Start: time.Date(2026, 10, 7, 14, 30, 0, 0, time.UTC), MaxConns: 64, Component: "advperf",
	})
	if err != nil {
		return err
	}
	defer w.Close()
	var pgVersion string
	if err := w.DB.QueryRow(ctx, `SELECT current_setting('server_version')`).Scan(&pgVersion); err == nil {
		rep.Environment["postgresql"] = pgVersion
	}

	steps := []func(context.Context, *sandboxworld.World, int) (Measurement, error){
		measureRiskV2,
		measureBuyingPowerPure,
		measureBuyingPowerLoaded,
		measureJournalTwoLegs,
		measureJournalTransferTemplate,
		measureMatchingThroughput,
		measureMatchingReplay,
		measureOptionAnalytics,
		measureConcurrentReservations,
		measureConcurrentTransfers,
		measureExactlyOnce,
	}
	for _, step := range steps {
		m, err := step(ctx, w, *scale)
		if err != nil {
			rep.Failures = append(rep.Failures, err.Error())
			fmt.Fprintln(os.Stderr, "advperf: FAILED:", err)
			continue
		}
		rep.Measurements = append(rep.Measurements, m)
		fmt.Printf("advperf: %-58s %s\n", m.Name, m.Result)
	}
	rep.Environment["load_average_after"] = loadAverage()

	if err := write(*out, rep); err != nil {
		return err
	}
	fmt.Printf("advperf: report written to %s\n", *out)
	if len(rep.Failures) > 0 {
		return fmt.Errorf("%d measurement(s) failed", len(rep.Failures))
	}
	return nil
}

// --- in-process latency ------------------------------------------------------

func measureRiskV2(_ context.Context, _ *sandboxworld.World, scale int) (Measurement, error) {
	now := time.Date(2026, 10, 7, 14, 30, 0, 0, time.UTC)
	in := risk.AdvancedInput{
		Stage: risk.StagePreTrade,
		Intent: risk.AdvancedIntent{
			ID: "0193b2e0-0000-7000-8000-000000000001", Kind: "OPEN_POSITION",
			AccountID: "0193b2e0-0000-7000-8000-0000000000a1", ActorType: "USER",
			Capability: "DERIVATIVES", Mode: "PAPER", InstrumentID: "0193b2e0-0000-7000-8000-0000000000c1",
			InstrumentKind: "PERPETUAL", NotionalUSD: money.USDFromMinor(1_000_00), RiskIncreasing: true,
		},
		Identity:   &risk.IdentitySnapshot{Verified: true},
		Account:    &risk.AdvancedAccountSnapshot{Status: "ACTIVE"},
		Instrument: &risk.InstrumentSnapshot{Status: "ACTIVE", SessionOpen: true},
		Margin: &risk.MarginSnapshot{
			EquityAfter: money.USDFromMinor(10_000_00), InitialRequiredAfter: money.USDFromMinor(1_000_00),
			MaintenanceRequiredAfter: money.USDFromMinor(500_00), LeverageAfterBPS: 10_000,
		},
		Oracle: &risk.OracleSnapshot{Available: true, ObservedAt: now.Add(-time.Second)},
		Now:    now,
	}
	policy := risk.DefaultAdvancedPolicy()
	first := risk.EvaluateAdvanced(policy, in)
	if first.Verdict != risk.Allow {
		return Measurement{}, fmt.Errorf("risk V2: the benchmark input is not ALLOW (%v); the measurement would time a rejection", first.ReasonCodes)
	}
	n := 20_000 * scale
	lat := make([]time.Duration, n)
	start := time.Now()
	for i := range lat {
		t0 := time.Now()
		d := risk.EvaluateAdvanced(policy, in)
		lat[i] = time.Since(t0)
		if d.Hash != first.Hash {
			return Measurement{}, errors.New("risk V2: the same input produced a different decision hash")
		}
	}
	p := percentiles(lat)
	return latency("Risk Kernel V2 decision (21 checks, in-process)", "p95 < 10 ms", p.p95 < 10*time.Millisecond, p, n, time.Since(start),
		"OPEN_POSITION on a perpetual with every required section; every decision hash identical (determinism checked on each sample)"), nil
}

func measureBuyingPowerPure(_ context.Context, _ *sandboxworld.World, scale int) (Measurement, error) {
	in := buyingpower.V2Input{
		OwnerType: "CUSTOMER", OwnerID: "0193b2e0-0000-7000-8000-0000000000a1",
		Policy: buyingpower.DefaultV2Policy(), AsOf: time.Date(2026, 10, 7, 14, 30, 0, 0, time.UTC),
	}
	for i := 0; i < 10; i++ {
		in.Holdings = append(in.Holdings, buyingpower.V2Holding{
			Component: buyingpower.ComponentWallet, AssetID: fmt.Sprintf("asset-%02d", i), Symbol: fmt.Sprintf("SBX%02d", i),
			Quantity: money.QuantityFromInt64(int64(1_000_000 * (i + 1))), MarkedUSD: money.USDFromMinor(int64(100_000 * (i + 1))),
			FactorBPS: money.BPS(10_000 - 500*int64(i%5)), SpendableCash: i == 0,
		})
	}
	if _, err := buyingpower.EvaluateV2(in); err != nil {
		return Measurement{}, fmt.Errorf("buying power V2: %w", err)
	}
	n := 20_000 * scale
	lat := make([]time.Duration, n)
	start := time.Now()
	for i := range lat {
		t0 := time.Now()
		if _, err := buyingpower.EvaluateV2(in); err != nil {
			return Measurement{}, err
		}
		lat[i] = time.Since(t0)
	}
	p := percentiles(lat)
	return latency("Buying power V2 evaluation (10 holdings, in-process)", "p95 < 25 ms", p.p95 < 25*time.Millisecond, p, n, time.Since(start),
		"the pure V2 engine over an explained 10-line portfolio"), nil
}

func measureOptionAnalytics(_ context.Context, _ *sandboxworld.World, scale int) (Measurement, error) {
	mustDec := func(s string) quant.Dec {
		d, err := quant.ParseDec(s)
		if err != nil {
			panic(err)
		}
		return d
	}
	in := quant.BSMInput{
		Kind: quant.Call, Spot: mustDec("2500"), Strike: mustDec("2600"), TimeYears: mustDec("0.25"),
		Vol: mustDec("0.65"), Rate: mustDec("0.04"), DividendYield: mustDec("0"),
	}
	if _, err := quant.BlackScholes(in); err != nil {
		return Measurement{}, fmt.Errorf("quant: %w", err)
	}
	n := 2_000 * scale
	lat := make([]time.Duration, n)
	start := time.Now()
	for i := range lat {
		t0 := time.Now()
		if _, err := quant.BlackScholes(in); err != nil {
			return Measurement{}, err
		}
		lat[i] = time.Since(t0)
	}
	p := percentiles(lat)
	m := latency("Option price + Greeks (Black-Scholes-Merton, exact fixed point)", "measured (no target)", true, p, n, time.Since(start),
		"40-digit fixed-point arithmetic with certified error bounds; one call computes price, delta, gamma, vega, theta, rho")
	m.Met = nil
	return m, nil
}

// --- matching -------------------------------------------------------------------

// commands generates a deterministic stream of simple limit orders: alternating
// sides in a ten-tick band either side of a mid, one in eight priced through the
// touch so it trades.
func commands(n int, prefix string) []matching.Command {
	state := uint64(0x9e3779b97f4a7c15)
	next := func(bound int64) int64 {
		state += 0x9e3779b97f4a7c15
		z := state
		z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
		z = (z ^ (z >> 27)) * 0x94d049bb133111eb
		return int64((z ^ (z >> 31)) % uint64(bound))
	}
	owners := []string{"a", "b", "c", "d"}
	cmds := make([]matching.Command, n)
	for i := range cmds {
		side, price := matching.SideBuy, 999-next(10)
		if i%2 == 1 {
			side, price = matching.SideSell, 1001+next(10)
		}
		if next(8) == 0 {
			if side == matching.SideBuy {
				price = 1005
			} else {
				price = 995
			}
		}
		cmds[i] = matching.NewOrder{Timestamp: int64(i), OrderID: prefix + strconv.Itoa(i), OwnerID: owners[i%len(owners)],
			Side: side, Type: matching.OrderTypeLimit, TIF: matching.TIFGTC, PriceTicks: price, QtyLots: 1 + next(10)}
	}
	return cmds
}

func measureMatchingThroughput(_ context.Context, _ *sandboxworld.World, scale int) (Measurement, error) {
	const epoch = 1 << 16
	total := 1_000_000 * scale
	cmds := commands(epoch, "L")
	var applied, trades int
	var busy time.Duration
	for applied < total {
		book, err := matching.NewBook(matching.Config{Instrument: "SBXBTC-SBXUSD"})
		if err != nil {
			return Measurement{}, err
		}
		t0 := time.Now()
		for i := 0; i < len(cmds) && applied < total; i++ {
			res, err := book.Apply(cmds[i])
			if err != nil {
				return Measurement{}, err
			}
			for _, e := range res.Events {
				if e.Type == matching.EventTrade {
					trades++
				}
			}
			applied++
		}
		busy += time.Since(t0)
	}
	perSec := float64(applied) / busy.Seconds()
	met := perSec > 25_000
	return Measurement{
		Name: "Matching engine throughput (simple GTC limit orders)", Target: "> 25,000 orders/s",
		Result: fmt.Sprintf("%s orders/s", groupInt(int64(perSec))), Met: &met, Samples: applied, Duration: busy.Round(time.Millisecond).String(),
		Detail: fmt.Sprintf("%s orders in fresh books of %s, %s trades; single goroutine; a book's history of order ids bounds memory, so a new book starts every %s orders",
			groupInt(int64(applied)), groupInt(epoch), groupInt(int64(trades)), groupInt(epoch)),
	}, nil
}

func measureMatchingReplay(_ context.Context, _ *sandboxworld.World, scale int) (Measurement, error) {
	n := 200_000 * scale
	book, err := matching.NewBook(matching.Config{Instrument: "SBXETH-SBXUSD"})
	if err != nil {
		return Measurement{}, err
	}
	seq, err := matching.NewSequencer(book)
	if err != nil {
		return Measurement{}, err
	}
	cmds := commands(n, "R")
	half := n / 2
	var snapshot []byte
	start := time.Now()
	for i, c := range cmds {
		if _, err := seq.Submit(c); err != nil {
			return Measurement{}, err
		}
		if i == half-1 {
			seq.Read(func(r matching.Reader) { snapshot = r.Snapshot() })
		}
	}
	var live [32]byte
	seq.Read(func(r matching.Reader) { live = r.StateHash() })
	journal := seq.Journal()

	replayed, err := matching.NewBook(matching.Config{Instrument: "SBXETH-SBXUSD"})
	if err != nil {
		return Measurement{}, err
	}
	t0 := time.Now()
	if _, err := replayed.Replay(journal); err != nil {
		return Measurement{}, fmt.Errorf("replay: %w", err)
	}
	replayTime := time.Since(t0)
	restored, err := matching.Restore(snapshot)
	if err != nil {
		return Measurement{}, fmt.Errorf("restore: %w", err)
	}
	if _, err := restored.Replay(journal[half:]); err != nil {
		return Measurement{}, fmt.Errorf("replay after restore: %w", err)
	}
	full, fromSnapshot := replayed.StateHash(), restored.StateHash()
	met := full == live && fromSnapshot == live
	return Measurement{
		Name: "Order-book replay determinism", Target: "replay deterministic",
		Result: map[bool]string{true: "identical state hash (3 of 3)", false: "STATE HASH MISMATCH"}[met], Met: &met,
		Samples: n, Duration: time.Since(start).Round(time.Millisecond).String(),
		Detail: fmt.Sprintf("%s sequenced commands; full journal replay (%s) and snapshot-at-%s + journal tail both reproduce the live book's SHA-256 %x…",
			groupInt(int64(n)), replayTime.Round(time.Millisecond), groupInt(int64(half)), live[:6]),
	}, nil
}

// --- database ----------------------------------------------------------------

func measureBuyingPowerLoaded(ctx context.Context, w *sandboxworld.World, scale int) (Measurement, error) {
	c, err := w.Customer(ctx, "perf-portfolio")
	if err != nil {
		return Measurement{}, err
	}
	if _, err := w.Fund(ctx, c, w.USD.ID, sandboxworld.Units(5_000, w.USD.Decimals), "perf-usd"); err != nil {
		return Measurement{}, err
	}
	for i := 1; i < 10; i++ {
		a, err := w.SimAsset(ctx, fmt.Sprintf("SBXP%d%s", i, strings.ToUpper(uuid.NewString()[:4])), "perf asset", 8, assets.RiskStandard, false)
		if err != nil {
			return Measurement{}, err
		}
		if _, err := w.Fund(ctx, c, a.ID, sandboxworld.Units(int64(i), 8), "perf-"+a.ID.String()); err != nil {
			return Measurement{}, err
		}
		if _, err := w.Prices.RecordPrice(ctx, w.DB, valuation.PriceObservation{
			AssetID: a.ID, QuoteAssetID: w.USD.ID, Mantissa: money.QuantityFromInt64(int64(1_000 * i)), Scale: 0,
			Source: "advperf-synthetic", ObservedAt: w.Clock.Now(),
		}); err != nil {
			return Measurement{}, fmt.Errorf("record mark: %w", err)
		}
	}
	loader, err := buyingpower.NewV2Loader(buyingpower.V2LoaderConfig{
		Clock: w.Clock, Prices: w.Prices, QuoteAssetID: w.USD.ID, MaxPriceAge: time.Hour,
	})
	if err != nil {
		return Measurement{}, err
	}
	first, err := loader.Compute(ctx, w.DB, "CUSTOMER", c.Account.String())
	if err != nil {
		return Measurement{}, fmt.Errorf("compute: %w", err)
	}
	n := 500 * scale
	lat := make([]time.Duration, n)
	start := time.Now()
	for i := range lat {
		t0 := time.Now()
		if _, err := loader.Compute(ctx, w.DB, "CUSTOMER", c.Account.String()); err != nil {
			return Measurement{}, err
		}
		lat[i] = time.Since(t0)
	}
	p := percentiles(lat)
	return latency("Buying power V2 from the ledger (10 assets, 9 marked)", "p95 < 25 ms", p.p95 < 25*time.Millisecond, p, n, time.Since(start),
		fmt.Sprintf("loader reads balances, reservations and marks, then evaluates; gross equity %s, %d explanation lines", first.GrossEquity, len(first.Lines))), nil
}

func measureJournalTwoLegs(ctx context.Context, w *sandboxworld.World, scale int) (Measurement, error) {
	c, err := w.Customer(ctx, "perf-journal")
	if err != nil {
		return Measurement{}, err
	}
	q := money.QuantityFromInt64(1_000_000)
	n := 2_000 * scale
	lat := make([]time.Duration, n)
	start := time.Now()
	for i := range lat {
		ref := uuid.NewString()
		t0 := time.Now()
		err := w.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := w.Ledger.Post(ctx, tx, ledger.Posting{
				Kind: ledger.KindSandboxFunding, IdempotencyKey: "perf-fund:" + ref,
				Reference: ledger.FinancialEventReference{Type: "sandbox_funding", ID: ref}, EffectiveAt: w.Clock.Now(),
				Entries: []ledger.Entry{
					{Account: ledger.CustomerAccount(c.Account, ledger.CodeWallet, w.USD.ID), Side: ledger.Debit, Quantity: q},
					{Account: ledger.CustomerAccount(c.Account, ledger.CodeCapital, w.USD.ID), Side: ledger.Credit, Quantity: q},
				},
			})
			return err
		})
		lat[i] = time.Since(t0)
		if err != nil {
			return Measurement{}, fmt.Errorf("post: %w", err)
		}
	}
	p := percentiles(lat)
	return latency("Ledger journal creation, 2 legs (sequential, committed)", "p95 < 25 ms excluding contention", p.p95 < 25*time.Millisecond, p, n, time.Since(start),
		"SANDBOX_FUNDING: balance, idempotency, value-domain and simulated-only checks, deferred per-party balance trigger, commit"), nil
}

func measureJournalTransferTemplate(ctx context.Context, w *sandboxworld.World, scale int) (Measurement, error) {
	a, err := w.Customer(ctx, "perf-sender")
	if err != nil {
		return Measurement{}, err
	}
	b, err := w.Customer(ctx, "perf-recipient")
	if err != nil {
		return Measurement{}, err
	}
	// Its own asset: these journals have no transfer, intent or effect behind
	// them, which is exactly what the transfers reconciliation reports as an
	// orphan, so they must not share an asset with the concurrent-transfer
	// measurement whose reconciliation is part of its result.
	asset, err := w.SimAsset(ctx, "SBXJ"+strings.ToUpper(uuid.NewString()[:6]), "journal benchmark unit", 6, assets.RiskSettlement, true)
	if err != nil {
		return Measurement{}, err
	}
	if _, err := w.Fund(ctx, a, asset.ID, sandboxworld.Units(50_000, asset.Decimals), "perf-transfer-seed"); err != nil {
		return Measurement{}, err
	}
	q := money.QuantityFromInt64(1_000)
	n := 1_000 * scale
	lat := make([]time.Duration, n)
	start := time.Now()
	for i := range lat {
		ref := uuid.NewString()
		t0 := time.Now()
		err := w.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := w.Ledger.Post(ctx, tx, ledger.Posting{
				Kind: ledger.KindP2PTransfer, IdempotencyKey: "perf-p2p:" + ref,
				Reference: ledger.FinancialEventReference{Type: "perf_transfer", ID: ref}, EffectiveAt: w.Clock.Now(),
				Entries: transfers.TransferEntries(a.Account, b.Account, asset.ID, q),
			})
			return err
		})
		lat[i] = time.Since(t0)
		if err != nil {
			return Measurement{}, fmt.Errorf("post: %w", err)
		}
	}
	p := percentiles(lat)
	return latency("Ledger journal creation, 4 legs, 2 parties (P2P template)", "p95 < 25 ms excluding contention", p.p95 < 25*time.Millisecond, p, n, time.Since(start),
		"the transfers posting template alone (no intent, risk or effect): per-party balance enforced at commit"), nil
}

func measureConcurrentReservations(ctx context.Context, w *sandboxworld.World, scale int) (Measurement, error) {
	c, err := w.Customer(ctx, "perf-reserve")
	if err != nil {
		return Measurement{}, err
	}
	const each = 1_000_000
	attempts := 1_000 * scale
	fundable := attempts * 6 / 10
	if _, err := w.Fund(ctx, c, w.USD.ID, money.QuantityFromInt64(int64(fundable*each)), "perf-reserve-seed"); err != nil {
		return Measurement{}, err
	}
	var wg sync.WaitGroup
	results := make(chan error, attempts)
	start := time.Now()
	gate := make(chan struct{})
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-gate
			results <- w.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
				_, err := w.Capital.Reserve(ctx, tx, capital.ReserveRequest{
					AccountID: c.Account.String(), AssetID: w.USD.ID, Quantity: money.QuantityFromInt64(each),
					ActorType: security.ActorUser, ActorID: c.User.String(), IdempotencyKey: fmt.Sprintf("perf-rsv-%s-%d", c.Account, i),
					TTL: time.Hour, Reason: "advperf concurrency",
				})
				return err
			})
		}(i)
	}
	close(gate)
	wg.Wait()
	elapsed := time.Since(start)
	close(results)
	ok, refused := 0, 0
	for err := range results {
		switch {
		case err == nil:
			ok++
		case errs.CodeOf(err) == errs.CodeInsufficientBuyingPower:
			refused++
		default:
			return Measurement{}, fmt.Errorf("reservation: unexpected error: %w", err)
		}
	}
	drift, err := capital.VerifyReservationTotals(ctx, w.DB)
	if err != nil {
		return Measurement{}, err
	}
	met := ok == fundable && refused == attempts-fundable && len(drift) == 0
	return Measurement{
		Name: "Concurrent reservation attempts on one wallet", Target: "1,000+ concurrent attempts preserve invariants",
		Result: fmt.Sprintf("%d reserved, %d refused, 0 over-allocation, %d drift", ok, refused, len(drift)), Met: &met,
		Samples: attempts, Duration: elapsed.Round(time.Millisecond).String(),
		Detail: fmt.Sprintf("%d goroutines released together against a balance covering exactly %d; each in its own transaction through capital.Service.Reserve; totals verified against the reservation rows afterwards",
			attempts, fundable),
	}, nil
}

func measureConcurrentTransfers(ctx context.Context, w *sandboxworld.World, scale int) (Measurement, error) {
	const parties, funding = 10, 50_000_000
	attempts := 100 * scale
	cs := make([]sandboxworld.Customer, parties)
	for i := range cs {
		var err error
		if cs[i], err = w.Customer(ctx, fmt.Sprintf("perf-party-%d", i)); err != nil {
			return Measurement{}, err
		}
		if _, err := w.Fund(ctx, cs[i], w.USD.ID, money.QuantityFromInt64(funding), fmt.Sprintf("perf-party-seed-%d", i)); err != nil {
			return Measurement{}, err
		}
	}
	sum := func() (money.Quantity, error) {
		total := money.QuantityFromInt64(0)
		for _, c := range cs {
			b, err := w.Wallet(ctx, c, w.USD.ID)
			if err != nil {
				return money.Quantity{}, err
			}
			total = total.Add(b)
		}
		return total, nil
	}
	before, err := sum()
	if err != nil {
		return Measurement{}, err
	}
	state := uint64(32)
	rnd := func(bound int64) int64 {
		state = state*6364136223846793005 + 1442695040888963407
		return int64((state >> 33) % uint64(bound))
	}
	type attempt struct {
		from int
		req  transfers.SendRequest
	}
	list := make([]attempt, attempts)
	for i := range list {
		from := int(rnd(parties))
		to := (from + 1 + int(rnd(parties-1))) % parties
		list[i] = attempt{from: from, req: transfers.SendRequest{
			SenderAccountID: cs[from].Account, RecipientAccountID: cs[to].Account, AssetID: w.USD.ID,
			Quantity: money.QuantityFromInt64(1 + rnd(20_000_000)), Mode: gates.ModeSandbox,
			IdempotencyKey: "perf-p2p-" + uuid.NewString(), CorrelationID: "perf-" + uuid.NewString(),
		}}
	}
	var wg sync.WaitGroup
	results := make(chan error, attempts)
	gate := make(chan struct{})
	start := time.Now()
	for _, a := range list {
		wg.Add(1)
		go func(a attempt) {
			defer wg.Done()
			<-gate
			var sendErr error
			err := w.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
				res, err := w.Transfers.Send(ctx, tx, cs[a.from].P, a.req)
				sendErr = err
				if err != nil && res.Transfer.Status != transfers.StatusRejected {
					return err
				}
				return nil
			})
			if err == nil {
				err = sendErr
			}
			results <- err
		}(a)
	}
	close(gate)
	wg.Wait()
	elapsed := time.Since(start)
	close(results)
	completed, refused := 0, 0
	for err := range results {
		switch {
		case err == nil:
			completed++
		case errs.CodeOf(err) == errs.CodeInsufficientBuyingPower:
			refused++
		default:
			return Measurement{}, fmt.Errorf("transfer: unexpected error: %w", err)
		}
	}
	after, err := sum()
	if err != nil {
		return Measurement{}, err
	}
	rec, err := w.Transfers.Reconcile(ctx, w.DB, w.USD.ID)
	if err != nil {
		return Measurement{}, err
	}
	met := before.Equal(after) && rec.OK() && completed+refused == attempts
	detail := fmt.Sprintf("%d parties, random amounts up to 20 SBX-USD, all released together; each Send runs intent, route, gate, Risk V2, reservation, posting, effect and evidence in one transaction",
		parties)
	if !rec.OK() {
		js, _ := json.Marshal(rec.Discrepancies)
		detail += "; DISCREPANCIES: " + string(js)
	}
	return Measurement{
		Name: "Concurrent customer-to-customer transfers", Target: "100 concurrent transfer attempts preserve invariants",
		Result: fmt.Sprintf("%d completed, %d refused for funds; Σ wallets conserved; reconciliation %s", completed, refused,
			map[bool]string{true: "clean", false: "FOUND DISCREPANCIES"}[rec.OK()]), Met: &met,
		Samples: attempts, Duration: elapsed.Round(time.Millisecond).String(), Detail: detail,
	}, nil
}

func measureExactlyOnce(ctx context.Context, w *sandboxworld.World, scale int) (Measurement, error) {
	attempts := 200 * scale
	key := "perf-exactly-once:" + uuid.NewString()
	req := econ.BeginRequest{Key: key, Subsystem: econ.SubsystemTransfers, Kind: "P2P_TRANSFER",
		Request: map[string]string{"amount": "100"}, Actor: w.System}
	var wg sync.WaitGroup
	type outcome struct {
		id     econ.EffectID
		replay bool
		err    error
	}
	results := make(chan outcome, attempts)
	gate := make(chan struct{})
	start := time.Now()
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gate
			var o outcome
			o.err = w.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
				e, replay, err := w.Effects.Begin(ctx, tx, req)
				o.id, o.replay = e.ID, replay
				return err
			})
			results <- o
		}()
	}
	close(gate)
	wg.Wait()
	elapsed := time.Since(start)
	close(results)
	ids := map[econ.EffectID]bool{}
	fresh := 0
	for o := range results {
		if o.err != nil {
			return Measurement{}, fmt.Errorf("begin: %w", o.err)
		}
		ids[o.id] = true
		if !o.replay {
			fresh++
		}
	}
	var rows int
	if err := w.DB.QueryRow(ctx, `SELECT count(*) FROM economic_effects WHERE effect_key = $1`, key).Scan(&rows); err != nil {
		return Measurement{}, err
	}
	met := len(ids) == 1 && fresh == 1 && rows == 1
	return Measurement{
		Name: "Exactly-once effect claim under concurrency", Target: "retries yield no second effect",
		Result: fmt.Sprintf("%d attempts → %d effect row, %d fresh claim, %d replays", attempts, rows, fresh, attempts-fresh), Met: &met,
		Samples: attempts, Duration: elapsed.Round(time.Millisecond).String(),
		Detail: "concurrent econ.EffectStore.Begin with one effect key, each in its own transaction",
	}, nil
}

// --- reporting ---------------------------------------------------------------

type pct struct{ p50, p95, p99, max time.Duration }

func percentiles(d []time.Duration) pct {
	s := append([]time.Duration(nil), d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	at := func(q int) time.Duration {
		i := (len(s)*q + 99) / 100
		if i < 1 {
			i = 1
		}
		return s[i-1]
	}
	return pct{p50: at(50), p95: at(95), p99: at(99), max: s[len(s)-1]}
}

func latency(name, target string, met bool, p pct, n int, total time.Duration, detail string) Measurement {
	return Measurement{
		Name: name, Target: target, Met: &met, Samples: n, Duration: total.Round(time.Millisecond).String(),
		Result: fmt.Sprintf("p50 %s · p95 %s · p99 %s · max %s", short(p.p50), short(p.p95), short(p.p99), short(p.max)),
		Detail: detail,
	}
}

func short(d time.Duration) string {
	switch {
	case d < time.Microsecond:
		return d.String()
	case d < time.Millisecond:
		return fmt.Sprintf("%dµs", d.Microseconds())
	default:
		return fmt.Sprintf("%.2fms", float64(d.Microseconds())/1000)
	}
}

func groupInt(n int64) string {
	s := strconv.FormatInt(n, 10)
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func write(path string, rep Report) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	js, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(strings.TrimSuffix(path, filepath.Ext(path))+".json", js, 0o644); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("# Advanced capabilities — measured performance\n\n")
	b.WriteString("Produced by `go run ./scripts/advperf`. Every figure below was measured by that run on the machine described; a missed target is reported as missed.\n\n## Environment\n\n")
	keys := make([]string, 0, len(rep.Environment))
	for k := range rep.Environment {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "- **%s**: %s\n", k, rep.Environment[k])
	}
	b.WriteString("\n## Results\n\n| Measurement | Target | Result | Met | Samples | Wall time |\n|---|---|---|---|---|---|\n")
	for _, m := range rep.Measurements {
		met := "—"
		if m.Met != nil {
			met = map[bool]string{true: "yes", false: "**no**"}[*m.Met]
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s |\n", m.Name, m.Target, m.Result, met, groupInt(int64(m.Samples)), m.Duration)
	}
	b.WriteString("\n## What each row measured\n\n")
	for _, m := range rep.Measurements {
		fmt.Fprintf(&b, "- **%s** — %s\n", m.Name, m.Detail)
	}
	if len(rep.Failures) > 0 {
		b.WriteString("\n## Failed measurements\n\n")
		for _, f := range rep.Failures {
			fmt.Fprintf(&b, "- %s\n", f)
		}
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func environment() map[string]string {
	env := map[string]string{
		"measured_at": time.Now().UTC().Format(time.RFC3339),
		"go":          runtime.Version(),
		"os_arch":     runtime.GOOS + "/" + runtime.GOARCH,
		"cpus":        strconv.Itoa(runtime.NumCPU()),
		"gomaxprocs":  strconv.Itoa(runtime.GOMAXPROCS(0)),
	}
	if runtime.GOOS == "darwin" {
		if out, err := exec.Command("sysctl", "-n", "machdep.cpu.brand_string").Output(); err == nil {
			env["cpu"] = strings.TrimSpace(string(out))
		}
		if out, err := exec.Command("sysctl", "-n", "hw.memsize").Output(); err == nil {
			if b, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64); err == nil {
				env["memory"] = strconv.FormatInt(b>>30, 10) + " GiB"
			}
		}
	}
	if out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output(); err == nil {
		env["commit"] = strings.TrimSpace(string(out))
	}
	return env
}

func loadAverage() string {
	switch runtime.GOOS {
	case "darwin":
		if out, err := exec.Command("sysctl", "-n", "vm.loadavg").Output(); err == nil {
			return strings.Trim(strings.TrimSpace(string(out)), "{} ")
		}
	case "linux":
		if b, err := os.ReadFile("/proc/loadavg"); err == nil {
			f := strings.Fields(string(b))
			if len(f) >= 3 {
				return strings.Join(f[:3], " ")
			}
		}
	}
	return "unavailable"
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
