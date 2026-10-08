package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/marketplace"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/sandboxrt"
	nodalsdk "github.com/nodal/controlplane/internal/sandboxrt/sdk"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/scripts/internal/sandboxworld"
)

func init() {
	register("E", "Strategy sandbox: a creator's Go strategy is scanned, built to WASM and cleared by an operator; a follower's paper run in the wazero sandbox proposes an intent that Risk V2 allows and the paper venue fills, with its evidence and paper performance on the books", scenarioStrategy)
}

// breakoutSource is the strategy the creator submits: for every instrument it
// is shown, it buys one unit when the latest mark is above the average of the
// last four closes, and holds whatever it already holds.
const breakoutSource = `// Command breakout buys one unit of an instrument whose mark is above the
// average of its last four closes, and holds what it already holds.
package main

import (
	nodalsdk "nodal.dev/sdk"
)

func main() {
	nodalsdk.Run(func(s *nodalsdk.Session) error {
		units, ok := s.Config("order_units")
		if !ok {
			units = "1000000"
		}
		for _, ins := range s.Instruments() {
			if len(ins.Closes) < 4 {
				continue
			}
			if !s.Position(ins.ID).IsZero() {
				if err := s.Log("hold", map[string]string{"instrument": ins.ID, "mark": ins.Mark.String()}); err != nil {
					return err
				}
				continue
			}
			average, err := nodalsdk.SMA(ins.Closes, 4, 2, nodalsdk.RoundHalfEven)
			if err != nil {
				return err
			}
			if ins.Mark.Cmp(average) <= 0 {
				continue
			}
			if err := s.EmitIntent("ACQUIRE_EXPOSURE", ins.ID, "BUY", units, ""); err != nil {
				return err
			}
			if err := s.Log("breakout", map[string]string{"instrument": ins.ID, "mark": ins.Mark.String(), "average": average.String()}); err != nil {
				return err
			}
		}
		return nil
	})
}
`

// peekSource reads a file of the host it runs on. The scanner refuses it
// before anything is compiled.
const peekSource = `// Command peek reads a file of the host it runs on.
package main

import (
	"os"

	nodalsdk "nodal.dev/sdk"
)

func main() {
	nodalsdk.Run(func(s *nodalsdk.Session) error {
		_, err := os.ReadFile("/etc/hosts")
		return err
	})
}
`

func scenarioStrategy(ctx context.Context, w *sandboxworld.World, rec *recorder) error {
	st, err := w.OpenStrategies(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close(context.WithoutCancel(ctx)) }()
	market := st.Market
	usd := func(n int64) money.Quantity { return sandboxworld.Units(n, w.USD.Decimals) }
	showUSD := func(q money.Quantity) string { return q.ToDecimalString(w.USD.Decimals) + " SBX-USD" }

	// --- the market the strategy trades --------------------------------------
	nova, err := w.SimAsset(ctx, "SBX-NOVA", "Sandbox nova (simulated)", 6, assets.RiskMajor, false)
	if err != nil {
		return err
	}
	showNova := func(q money.Quantity) string { return q.ToDecimalString(nova.Decimals) + " SBX-NOVA" }
	var inst instruments.Instrument
	if err := w.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		inst, err = w.Instruments.CreateSpotPair(ctx, tx, instruments.SpotPairSpec{
			Base: nova.ID, Quote: w.USD.ID, Settlement: w.USD.ID, CanonicalName: "SBX-NOVA/SBX-USD",
			RiskClass: assets.RiskMajor, Status: assets.StatusActive, ActiveFrom: w.Clock.Now().Add(-time.Hour),
		})
		return err
	}); err != nil {
		return fmt.Errorf("open the SBX-NOVA pair: %w", err)
	}
	for i, px := range []string{"92.00", "94.00", "95.50", "97.00", "98.50", "100.00"} {
		if i > 0 {
			w.Clock.Advance(time.Minute)
		}
		if _, err := w.Mark(ctx, nova.ID, px, "demo-reference-feed"); err != nil {
			return err
		}
	}
	rec.step("a SIMULATED spot pair SBX-NOVA/SBX-USD opens; the sandbox reference feed marks it once a minute: 92.00, 94.00, 95.50, 97.00, 98.50, 100.00",
		"instrument", inst.ID.String(), "base_asset", nova.ID.String())

	alba, err := w.Customer(ctx, "alba-strategy-creator")
	if err != nil {
		return err
	}
	creator := econ.Actor{Type: security.ActorUser, ID: alba.User.String()}
	reviewer, err := w.OperatorActor(ctx, "olga-strategy-reviewer")
	if err != nil {
		return err
	}
	manifest := sandboxrt.Manifest{
		AllowedIntentKinds: []econ.IntentKind{econ.KindAcquireExposure}, AllowedInstrumentIDs: []string{inst.ID.String()},
		MaxNotionalPerIntent: "250.00", MaxIntentsPerRun: 1, MinIntervalSeconds: 60,
	}.Canonicalize()
	version := func(source, commit, description string) marketplace.VersionRequest {
		return marketplace.VersionRequest{
			Description: description, RiskClass: marketplace.RiskModerate, SupportedInstrumentIDs: []string{inst.ID.String()},
			Manifest: manifest, Source: sandboxrt.Source{"main.go": []byte(source)}, SourceCommit: commit,
			DataDependencies: []string{"closes", "marks"}, Config: map[string]string{"order_units": "1000000"},
		}
	}
	draft := func(title string, req marketplace.VersionRequest) (marketplace.Listing, marketplace.Version, error) {
		var l marketplace.Listing
		var v marketplace.Version
		err := w.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			if l, v, err = market.CreateListing(ctx, tx, marketplace.ListingRequest{
				CreatorAccountID: alba.Account, Creator: creator, Title: title, Version: req,
			}); err != nil {
				return err
			}
			v, err = market.Submit(ctx, tx, v.ID, creator)
			return err
		})
		return l, v, err
	}

	// --- the real strategy: submitted, scanned and built ---------------------
	listing, v, err := draft("Breakout", version(breakoutSource, "demo-e-breakout-v1",
		"buys one unit when the mark is above the average of the last four closes, and holds it"))
	if err != nil {
		return fmt.Errorf("submit the breakout strategy: %w", err)
	}
	rec.step("Alba (a customer) publishes the Breakout listing with a Go source file and a manifest: ACQUIRE_EXPOSURE on SBX-NOVA/SBX-USD only, at most 250.00 per intent and one intent per run, at most one run a minute; Alba submits it",
		"listing", listing.ID.String(), "version", v.ID.String(), "status", string(v.Status),
		"manifest_hash", hex.EncodeToString(v.ManifestHash), "source_digest", hex.EncodeToString(v.SourceDigest))
	v, val, err := market.RunValidation(ctx, w.DB, v.ID, reviewer)
	if err != nil {
		return fmt.Errorf("validate the breakout strategy: %w", err)
	}
	var wasmSize int
	var artifactOK, artifactLogOK bool
	var validationLogDigest []byte
	if err := w.DB.QueryRow(ctx, `SELECT a.wasm_size, sha256(a.wasm) = a.artifact_digest, a.build_log_digest = m.build_log_digest,
		m.build_log_digest FROM strategy_artifacts a JOIN marketplace_version_validations m ON m.artifact_digest = a.artifact_digest
		WHERE m.version_id = $1`, v.ID).Scan(&wasmSize, &artifactOK, &artifactLogOK, &validationLogDigest); err != nil {
		return fmt.Errorf("read the artifact: %w", err)
	}
	logSum := sha256.Sum256(val.BuildLog)
	rec.step("an operator (Olga) runs the validation: the source is scanned, then compiled to wasip1 by the local Go toolchain through sandboxrt.Builder, in a fresh directory whose environment carries nothing from this process (GOPROXY=off, GOTOOLCHAIN=local, the embedded SDK its only dependency) and under the builder's default bounds on time, module size and memory, as cmd/api builds",
		"toolchain", val.ToolchainVersion, "sdk", val.SDKVersion, "runtime", val.RuntimeVersion,
		"artifact_sha256", hex.EncodeToString(val.ArtifactDigest), "wasm_bytes", fmt.Sprint(wasmSize),
		"build_log_sha256", hex.EncodeToString(validationLogDigest), "build_environment", strings.Join(sandboxrt.BuildEnvironmentKeys(), ","))
	rec.check("the scan was clean and the build produced the artifact: VALIDATED, and the artifact is the sha256 of its wasm bytes",
		v.Status == marketplace.StatusValidated && val.Outcome == marketplace.ValidationPassed && len(val.ScanCodes) == 0 &&
			artifactOK && wasmSize > 0 && wasmSize <= 32<<20,
		"version %s, validation %s, scan codes [%s], sha256(wasm) = artifact digest %t, %d bytes within the 32 MiB bound",
		v.Status, val.Outcome, strings.Join(val.ScanCodes, ","), artifactOK, wasmSize)
	rec.check("the build log's hash is recorded with the validation and the artifact, and it is the sha256 of the stored log",
		artifactLogOK && bytes.Equal(validationLogDigest, logSum[:]),
		"validation and artifact agree %t; sha256 of the %d-byte stored log matches %t", artifactLogOK, len(val.BuildLog),
		bytes.Equal(validationLogDigest, logSum[:]))

	// --- a strategy that reaches for the host is refused at scan -----------
	peekListing, peek, err := draft("Host peek", version(peekSource, "demo-e-peek-v1", "reads a file of the host it runs on"))
	if err != nil {
		return fmt.Errorf("submit the peek strategy: %w", err)
	}
	peekV, peekVal, err := market.RunValidation(ctx, w.DB, peek.ID, reviewer)
	if err != nil {
		return fmt.Errorf("validate the peek strategy: %w", err)
	}
	storedPeek, recorded, err := market.ValidationOf(ctx, w.DB, peek.ID)
	if err != nil {
		return err
	}
	rec.step("Alba submits a second strategy that imports os to read /etc/hosts; the validation scans it before anything is compiled",
		"version", peek.ID.String(), "status", string(peekV.Status), "rejection_code", peekV.RejectionCode,
		"scan_codes", strings.Join(peekVal.ScanCodes, ","))
	rec.check("a strategy importing a forbidden package is refused at scan, the reason recorded and nothing built",
		peekV.Status == marketplace.StatusRejected && peekV.RejectionCode == sandboxrt.BuildFailureScan && recorded &&
			storedPeek.Outcome == marketplace.ValidationFailed && storedPeek.FailureReason == sandboxrt.BuildFailureScan &&
			strings.Join(storedPeek.ScanCodes, ",") == string(sandboxrt.ScanImportForbidden) &&
			len(storedPeek.ArtifactDigest) == 0 && len(storedPeek.BuildLog) == 0,
		"version %s (%s); recorded validation %s %s, scan codes [%s], artifact %d bytes, build log %d bytes",
		peekV.Status, peekV.RejectionCode, storedPeek.Outcome, storedPeek.FailureReason, strings.Join(storedPeek.ScanCodes, ","),
		len(storedPeek.ArtifactDigest), len(storedPeek.BuildLog))

	// --- an operator who is not the creator clears it ------------------------
	if err := w.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		v, err = market.StartReview(ctx, tx, v.ID, reviewer)
		return err
	}); err != nil {
		return fmt.Errorf("start review: %w", err)
	}
	selfErr := w.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := market.Approve(ctx, tx, v.ID, creator, "SELF_APPROVAL", "the author approving their own strategy")
		return err
	})
	rec.check("the strategy's own creator cannot approve it", errs.CodeOf(selfErr) == errs.CodeForbidden, "%v", selfErr)
	if err := w.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		v, err = market.Approve(ctx, tx, v.ID, reviewer, "MEETS_POLICY", "narrow manifest: one instrument, one intent per run")
		return err
	}); err != nil {
		return fmt.Errorf("approve: %w", err)
	}
	var reviewerType, reviewerID, decision string
	if err := w.DB.QueryRow(ctx, `SELECT reviewer_actor_type, reviewer_actor_id, decision FROM marketplace_reviews WHERE version_id = $1`,
		v.ID).Scan(&reviewerType, &reviewerID, &decision); err != nil {
		return err
	}
	catalogue, _, err := marketplace.Catalogue(ctx, w.DB, nil, 100)
	if err != nil {
		return err
	}
	published, peekListed := false, false
	for _, item := range catalogue {
		switch item.Listing.ID {
		case listing.ID:
			published = item.Latest.ID == v.ID
		case peekListing.ID:
			peekListed = true
		}
	}
	rec.step("Olga takes the version into review and approves it (MEETS_POLICY); approval clears its moderation, and the listing is published in the catalogue",
		"strategy_ref", v.StrategyRef(), "status", string(v.Status), "moderation", string(v.Moderation))
	rec.check("the version is APPROVED and CLEARED by an operator who is not its creator, and published: the catalogue lists it and not the refused one",
		v.Status == marketplace.StatusApproved && v.Moderation == marketplace.ModerationCleared && v.Followable() &&
			decision == string(marketplace.DecisionApprove) && reviewerType == string(security.ActorOperator) &&
			reviewerID == reviewer.ID && reviewerID != creator.ID && published && !peekListed,
		"%s/%s; review %s by %s, the creator is a %s; in the catalogue %t, the refused listing %t", v.Status, v.Moderation, decision,
		reviewerType, creator.Type, published, peekListed)

	// --- a customer follows it in paper mode ---------------------------------
	bea, err := w.Customer(ctx, "bea-strategy-follower")
	if err != nil {
		return err
	}
	follower := econ.Actor{Type: security.ActorUser, ID: bea.User.String()}
	if _, err := w.Fund(ctx, bea, w.USD.ID, usd(1_000), "demo-e-fund-bea"); err != nil {
		return fmt.Errorf("fund bea: %w", err)
	}
	cashBefore, err := w.Wallet(ctx, bea, w.USD.ID)
	if err != nil {
		return err
	}
	heldBefore, err := w.Wallet(ctx, bea, nova.ID)
	if err != nil {
		return err
	}
	var install marketplace.Install
	if err := w.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		install, _, err = market.InstallVersion(ctx, tx, marketplace.InstallRequest{
			VersionID: v.ID, AccountID: bea.Account, Mode: gates.ModePaper, Actor: follower,
		})
		return err
	}); err != nil {
		return fmt.Errorf("install: %w", err)
	}
	rec.step("Bea (another customer) holds 1,000 SBX-USD from the faucet and installs Breakout in PAPER mode; the install's paper book starts with its paper capital",
		"install", install.ID.String(), "mode", string(install.Mode), "paper_capital", install.PaperCapital)

	follow := func() (marketplace.FollowRun, error) {
		var run marketplace.FollowRun
		err := w.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			run, err = market.PaperFollow(ctx, tx, marketplace.FollowRequest{
				InstallID: install.ID, Trigger: marketplace.TriggerExplicit, Actor: follower,
			})
			return err
		})
		return run, err
	}
	run1, err := follow()
	if err != nil {
		return fmt.Errorf("first paper follow: %w", err)
	}
	rec.step("Bea follows it now: the service builds the run's input from the paper book and the reference prices it reads itself, and runs the artifact in the wazero sandbox",
		"follow_run", run1.ID.String(), "sandbox_run", run1.SandboxRunID.String(), "termination", string(run1.Termination),
		"input_sha256", hex.EncodeToString(run1.InputDigest), "run_digest", hex.EncodeToString(run1.RunDigest))
	sbRun, err := st.Sandbox.Run(ctx, w.DB, run1.SandboxRunID)
	if err != nil {
		return err
	}
	var charges int
	var chargedAccount, purpose, budget string
	var cpuMS int64
	if err := w.DB.QueryRow(ctx, `SELECT count(*) OVER (), account_id::text, purpose, cpu_ms, budget_version FROM sandbox_usage
		WHERE run_id = $1`, run1.SandboxRunID).Scan(&charges, &chargedAccount, &purpose, &cpuMS, &budget); err != nil {
		return fmt.Errorf("read the sandbox charge: %w", err)
	}
	limits := sandboxrt.DefaultLimits()
	rec.check("the sandbox run is recorded: OK, the verified artifact, one proposal accepted and no violation",
		sbRun.Termination == sandboxrt.TerminationOK && sbRun.ArtifactVerified && bytes.Equal(sbRun.ArtifactDigest, val.ArtifactDigest) &&
			sbRun.ProposalsCount == 1 && sbRun.AcceptedCount == 1 && sbRun.ViolationsCount == 0 && sbRun.StrategyRef == v.StrategyRef() &&
			sbRun.LimitsVersion == limits.Version,
		"termination %s, artifact verified %t and the validated one %t, %d proposed, %d accepted, %d violations, limits %s",
		sbRun.Termination, sbRun.ArtifactVerified, bytes.Equal(sbRun.ArtifactDigest, val.ArtifactDigest), sbRun.ProposalsCount,
		sbRun.AcceptedCount, sbRun.ViolationsCount, sbRun.LimitsVersion)
	rec.check("the run is charged to the follower's account under the sandbox budget (the database refuses an uncharged run, SX002)",
		charges == 1 && chargedAccount == bea.Account.String() && purpose == string(sandboxrt.PurposeFollow) &&
			cpuMS == limits.WallTimeMS && budget == st.Sandbox.Budget().Version,
		"%d charge to Bea's account %t, purpose %s, %d ms of CPU granted (the run's wall-time limit), budget %s",
		charges, chargedAccount == bea.Account.String(), purpose, cpuMS, budget)

	if len(run1.Intents) != 1 {
		return fmt.Errorf("the first follow produced %d intents, want 1", len(run1.Intents))
	}
	fi := run1.Intents[0]
	notional, _ := nodalsdk.ParseDecimal(fi.Notional)
	refPrice, _ := nodalsdk.ParseDecimal(fi.ReferencePrice)
	hundred := nodalsdk.IntDecimal(100)
	rec.step("its one proposal, BUY 1.000000 SBX-NOVA, becomes an econ intent (SERVICE, PAPER, sandbox-originated) valued at the reference price the service read, never a price the strategy chose",
		"intent", fi.IntentID.String(), "kind", string(fi.Kind), "notional", fi.Notional, "reference_price", fi.ReferencePrice)
	var intentActor, intentMode, intentStrategy, intentStatus string
	if err := w.DB.QueryRow(ctx, `SELECT actor_type, mode, coalesce(strategy_ref, ''), status FROM economic_intents WHERE id = $1`,
		fi.IntentID).Scan(&intentActor, &intentMode, &intentStrategy, &intentStatus); err != nil {
		return fmt.Errorf("read the follow intent: %w", err)
	}
	rec.check("the proposal is one ACQUIRE_EXPOSURE intent of 100.00 at the 100.00 reference price, the marketplace SERVICE's in PAPER for this strategy, dispatched and completed",
		fi.Kind == econ.KindAcquireExposure && notional.Cmp(hundred) == 0 && refPrice.Cmp(hundred) == 0 &&
			fi.Outcome == marketplace.OutcomeDispatched && run1.IntentsDispatched == 1 && run1.ViolationsCount == 0 &&
			intentActor == string(security.ActorService) && intentMode == string(gates.ModePaper) &&
			intentStrategy == v.StrategyRef() && intentStatus == string(econ.StatusCompleted),
		"%s %s at %s by %s in %s for the strategy %t, outcome %s, intent %s", fi.Kind, notional, refPrice, intentActor, intentMode,
		intentStrategy == v.StrategyRef(), fi.Outcome, intentStatus)

	// --- Risk V2 allowed it, with the sections only the marketplace vouches for
	var verdict string
	var decisionDoc []byte
	if err := w.DB.QueryRow(ctx, `SELECT verdict, decision FROM advanced_risk_decisions WHERE decision_hash = $1 AND intent_id = $2`,
		fi.DecisionHash, fi.IntentID).Scan(&verdict, &decisionDoc); err != nil {
		return fmt.Errorf("read the risk decision: %w", err)
	}
	var riskDecision risk.AdvancedDecision
	if err := json.Unmarshal(decisionDoc, &riskDecision); err != nil {
		return fmt.Errorf("decode the risk decision: %w", err)
	}
	sections := map[risk.AdvancedCategory]risk.CheckResult{}
	var applied []string
	for _, c := range riskDecision.Checks {
		sections[c.Category] = c
		if c.Applied {
			applied = append(applied, string(c.Category))
		}
	}
	sort.Strings(applied)
	clean := func(cat risk.AdvancedCategory) bool {
		c, ok := sections[cat]
		return ok && c.Applied && len(c.Codes) == 0
	}
	rec.step("Risk Kernel V2 evaluates the intent pre-trade with the sections read from the database (account, identity, instrument, oracle, kill switches) and the ones only the marketplace vouches for (strategy, code sandbox, the paper book's balance)",
		"decision_hash", fi.DecisionHash, "policy", riskDecision.PolicyVersion, "sections_applied", strings.Join(applied, ","))
	rec.check("Risk V2 decided ALLOW, its STRATEGY, CODE_SANDBOX and BALANCE sections applied and clean",
		verdict == string(risk.Allow) && riskDecision.Verdict == risk.Allow && len(riskDecision.ReasonCodes) == 0 &&
			len(riskDecision.MissingSections) == 0 && clean(risk.CatStrategy) && clean(risk.CatCodeSandbox) && clean(risk.CatBalance),
		"verdict %s, reasons [%s], missing [%s]; strategy %t, code sandbox %t, balance %t", verdict,
		strings.Join(riskDecision.ReasonCodes, ","), strings.Join(riskDecision.MissingSections, ","), clean(risk.CatStrategy),
		clean(risk.CatCodeSandbox), clean(risk.CatBalance))

	// --- the paper venue's fill, its journal and its fee booking -------------
	fillID := strings.TrimPrefix(fi.ExternalRef, "paper-fill:")
	var fillBase, fillNotional, fillSpread, fillFees, fillTotal, fillDisclosure string
	var journalID ledger.TransactionID
	if err := w.DB.QueryRow(ctx, `SELECT base_quantity::text, notional_at_reference::text, spread_amount::text, platform_fee_amount::text,
		total_quote::text, disclosure_hash, journal_transaction_id FROM paper_fills WHERE id::text = $1 AND intent_id = $2`,
		fillID, fi.IntentID).Scan(&fillBase, &fillNotional, &fillSpread, &fillFees, &fillTotal, &fillDisclosure, &journalID); err != nil {
		return fmt.Errorf("read the paper fill: %w", err)
	}
	q := func(s string) money.Quantity {
		v, err := money.ParseQuantity(s)
		if err != nil {
			return money.Quantity{}
		}
		return v
	}
	rec.step("the paper venue fills it immediately at the 100.00 mark under the published marketplace schedule (10 bps spread, 10 bps platform fee, 5 bps strategy fee), disclosed on the fill",
		"fill", fillID, "journal", journalID.String(), "disclosure_hash", fillDisclosure)
	rec.check("the fill is 1.000000 SBX-NOVA for 100.00, plus a 0.10 spread and 0.15 of fees: 100.25 SBX-USD in all",
		q(fillBase).Equal(sandboxworld.Units(1, nova.Decimals)) && q(fillNotional).Equal(usd(100)) &&
			q(fillSpread).Equal(money.QuantityFromInt64(100_000)) && q(fillFees).Equal(money.QuantityFromInt64(150_000)) &&
			q(fillTotal).Equal(money.QuantityFromInt64(100_250_000)),
		"%s for %s, spread %s, fees %s, total %s", showNova(q(fillBase)), showUSD(q(fillNotional)), showUSD(q(fillSpread)),
		showUSD(q(fillFees)), showUSD(q(fillTotal)))

	journal, err := w.Ledger.Transaction(ctx, w.DB, journalID)
	if err != nil {
		return err
	}
	symbol := map[assets.AssetID]string{w.USD.ID: w.USD.Symbol, nova.ID: nova.Symbol}
	decimals := map[assets.AssetID]uint8{w.USD.ID: w.USD.Decimals, nova.ID: nova.Decimals}
	balanced := map[assets.AssetID]money.Quantity{}
	var lines []string
	var walletUSD, walletNova money.Quantity
	for _, e := range journal.Entries {
		signed := e.Quantity
		if e.Side == ledger.Credit {
			signed = e.Quantity.Neg()
		}
		balanced[e.Account.AssetID] = balanced[e.Account.AssetID].Add(signed)
		if e.Account.OwnerType == ledger.OwnerCustomer && e.Account.OwnerID == bea.Account.String() && e.Account.Code == ledger.CodeWallet {
			switch e.Account.AssetID {
			case w.USD.ID:
				walletUSD = walletUSD.Add(signed)
			case nova.ID:
				walletNova = walletNova.Add(signed)
			}
		}
		lines = append(lines, fmt.Sprintf("%s %s %s %s %s", e.Account.OwnerType, e.Account.Code, symbol[e.Account.AssetID], e.Side,
			e.Quantity.ToDecimalString(decimals[e.Account.AssetID])))
	}
	allBalanced := len(balanced) == 2
	for _, net := range balanced {
		allBalanced = allBalanced && net.IsZero()
	}
	rec.step("the fill posts one SIM_TRADE journal: the exchange at the reference notional and each disclosed charge on its own pair of entries",
		"entries", strings.Join(lines, "; "))
	rec.check("the journal is a SIM_TRADE that balances in each asset and moves Bea's wallets by exactly the fill",
		journal.Kind == ledger.KindSimTrade && allBalanced && walletUSD.Equal(usd(100).Add(money.QuantityFromInt64(250_000)).Neg()) &&
			walletNova.Equal(sandboxworld.Units(1, nova.Decimals)),
		"%s, %d entries, balanced %t; wallet %s and %s", journal.Kind, len(journal.Entries), allBalanced,
		walletUSD.ToDecimalString(w.USD.Decimals)+" SBX-USD", walletNova.ToDecimalString(nova.Decimals)+" SBX-NOVA")

	var bookingMode, bookedSpread, bookedPlatform, bookedStrategy, scheduleKey, scheduleLabel string
	var sameDisclosure bool
	if err := w.DB.QueryRow(ctx, `SELECT b.booking_mode, b.spread_amount::text, b.platform_fee::text, b.strategy_fee::text,
		s.schedule_key, s.label, q.disclosure_hash = f.disclosure_hash
		FROM fee_bookings b
		JOIN paper_fills f ON f.journal_transaction_id = b.journal_transaction_id AND f.account_id = b.account_id
		JOIN fee_quote_disclosures q ON q.id = b.disclosure_id
		JOIN fee_schedules s ON s.id = q.schedule_id
		WHERE f.id::text = $1`, fillID).Scan(&bookingMode, &bookedSpread, &bookedPlatform, &bookedStrategy, &scheduleKey,
		&scheduleLabel, &sameDisclosure); err != nil {
		return fmt.Errorf("read the fee booking: %w", err)
	}
	rec.check("the fill's spread and fees equal their fee-ledger booking under the marketplace schedule published for the instrument",
		bookingMode == "EMBEDDED" && q(bookedSpread).Equal(q(fillSpread)) && q(bookedPlatform).Add(q(bookedStrategy)).Equal(q(fillFees)) &&
			q(bookedStrategy).Equal(money.QuantityFromInt64(50_000)) && sameDisclosure &&
			scheduleKey == "marketplace:paper:"+inst.ID.String() && scheduleLabel == "marketplace-paper-v1",
		"%s booking: spread %s, platform %s + strategy %s = %s (the fill's %s); same disclosure %t; schedule %s under marketplace:paper:<instrument> %t",
		bookingMode, showUSD(q(bookedSpread)), showUSD(q(bookedPlatform)), showUSD(q(bookedStrategy)),
		showUSD(q(bookedPlatform).Add(q(bookedStrategy))), showUSD(q(fillFees)), sameDisclosure, scheduleLabel,
		scheduleKey == "marketplace:paper:"+inst.ID.String())

	cash, err := w.Wallet(ctx, bea, w.USD.ID)
	if err != nil {
		return err
	}
	held, err := w.Wallet(ctx, bea, nova.ID)
	if err != nil {
		return err
	}
	rec.check("Bea's wallets moved by exactly the fill: 1,000.00 -> 899.75 SBX-USD and 0 -> 1.000000 SBX-NOVA",
		cashBefore.Equal(usd(1_000)) && heldBefore.IsZero() && cash.Equal(usd(899).Add(money.QuantityFromInt64(750_000))) &&
			held.Equal(sandboxworld.Units(1, nova.Decimals)),
		"%s -> %s and %s -> %s", showUSD(cashBefore), showUSD(cash), showNova(heldBefore), showNova(held))

	key := strings.TrimPrefix(run1.EvidenceRef, "pg://provider_evidence/")
	var archived []byte
	if err := w.DB.QueryRow(ctx, `SELECT sha256 FROM provider_evidence WHERE key = $1`, key).Scan(&archived); err != nil {
		return fmt.Errorf("read the follow evidence: %w", err)
	}
	rec.step("the follow run's evidence document (input, run digest, marks, what became of the intent) is archived write-once",
		"evidence", run1.EvidenceRef, "sha256", hex.EncodeToString(run1.EvidenceSHA256))
	wantKey := "advanced/marketplace/follow/" + run1.ID.String() + ".json"
	rec.check("the follow run's evidence is in the write-once archive with the digest the run names",
		key == wantKey && bytes.Equal(archived, run1.EvidenceSHA256),
		"archived under advanced/marketplace/follow/<follow run>.json %t, its sha256 is the run's %t", key == wantKey,
		bytes.Equal(archived, run1.EvidenceSHA256))

	// --- a minute later the price has moved: a second run, and performance ----
	w.Clock.Advance(time.Minute)
	if _, err := w.Mark(ctx, nova.ID, "110.00", "demo-reference-feed"); err != nil {
		return err
	}
	run2, err := follow()
	if err != nil {
		return fmt.Errorf("second paper follow: %w", err)
	}
	cash2, _ := nodalsdk.ParseDecimal(run2.PaperCash)
	equity2, _ := nodalsdk.ParseDecimal(run2.Equity)
	equity1, _ := nodalsdk.ParseDecimal(run1.Equity)
	rec.step("a minute later SBX-NOVA marks at 110.00 and Bea follows again: the strategy holds the unit it bought",
		"follow_run", run2.ID.String(), "paper_cash", run2.PaperCash, "equity", run2.Equity)
	rec.check("the second run's equity is the paper book at the new mark: 9,900.00 cash + 1 x 110.00 = 10,010.00, and it proposed nothing",
		equity1.Cmp(nodalsdk.IntDecimal(10_000)) == 0 && cash2.Cmp(nodalsdk.IntDecimal(9_900)) == 0 &&
			equity2.Cmp(nodalsdk.IntDecimal(10_010)) == 0 && run2.ProposalsCount == 0 && run2.IntentsDispatched == 0,
		"first equity %s; second cash %s, equity %s, %d proposed", equity1, cash2, equity2, run2.ProposalsCount)
	var perf marketplace.PerformanceSnapshot
	if err := w.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		perf, err = market.RecordPerformance(ctx, tx, install.ID, follower)
		return err
	}); err != nil {
		return fmt.Errorf("record performance: %w", err)
	}
	var fromBook bool
	var storedReturn int64
	if err := w.DB.QueryRow(ctx, `SELECT from_paper_book, return_bps FROM marketplace_performance_snapshots WHERE install_id = $1`,
		install.ID).Scan(&fromBook, &storedReturn); err != nil {
		return err
	}
	rec.step("the install's paper performance is computed exactly from the two book-valued runs and recorded",
		"history_sha256", hex.EncodeToString(perf.HistoryDigest))
	rec.check("performance from the books: +10 bps return, no drawdown, no volatility, 100 bps turnover",
		perf.Points == 2 && perf.ReturnBPS == 10 && perf.MaxDrawdownBPS == 0 && perf.VolatilityBPS == 0 && perf.TurnoverBPS == 100 &&
			fromBook && storedReturn == perf.ReturnBPS,
		"%d points: return %d bps (10,000.00 -> 10,010.00), drawdown %d, volatility %d, turnover %d bps (100.00 over a mean equity of 10,005.00); recorded from the paper book %t",
		perf.Points, perf.ReturnBPS, perf.MaxDrawdownBPS, perf.VolatilityBPS, perf.TurnoverBPS, fromBook)
	return nil
}
