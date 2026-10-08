package advancedjobs

import (
	"sort"
	"strings"
	"time"

	"github.com/nodal/controlplane/internal/errs"
)

// Job names. They are the jobs' stable identities: metric attributes, log
// fields, CP_ADVANCED_WORKER_INTERVALS / _BATCHES keys and the argument of
// `advanced-worker once`. Renaming one is a breaking change for all of them.
const (
	NameCapitalExpiry          = "capital.reservation_expiry"
	NameMarginInterest         = "margin.interest"
	NameMarginMarkToMarket     = "margin.mark_to_market"
	NameMarginLiquidation      = "margin.liquidation_queue"
	NameDerivativesVariation   = "derivatives.variation_margin"
	NameDerivativesFunding     = "derivatives.funding"
	NameDerivativesExpiry      = "derivatives.expiry_settlement"
	NameDerivativesClearing    = "derivatives.clearing_reconciliation"
	NamePredictionClose        = "predmarket.close_due"
	NamePredictionEscrow       = "predmarket.escrow_reconciliation"
	NameOrdersCompleteTerminal = "orders.complete_terminal"
	NameOrdersBookProof        = "orders.book_proof"
	NameEquitiesSettlement     = "equities.settlement"
	NameEquitiesCorporate      = "equities.corporate_actions"
	NameEquitiesVenue          = "equities.venue"
	NameMarketplaceFollows     = "marketplace.scheduled_follows"
	NameStableMonitor          = "stable.monitor"
	NameStableSupply           = "stable.supply_reconciliation"
	NameStableUnknown          = "stable.unknown_submissions"
	NameStableRedemptions      = "stable.redemptions"
	NameBridgeLegs             = "bridge.legs"
	NameBridgeRoutes           = "bridge.routes"
	NameTransfersReconcile     = "transfers.reconciliation"
	NameDelegationUpkeep       = "delegation.upkeep"
)

// NeedsDevnet is what a deployment configures for the jobs that run on the
// persistent simulated chains (internal/sandboxchain).
const NeedsDevnet = "CP_SANDBOX_DEVNET_NETWORK and CP_SANDBOX_DEVNET_SEED_REF"

// Descriptor is a job's documented default and what it drives.
type Descriptor struct {
	Name    string
	Default Settings
	// MaxBatch is the bound the subsystem's own operation enforces; a larger
	// batch is clamped to it (zero: no bound beyond the job's own).
	MaxBatch   int
	Calls      string
	KillSwitch string
	// Needs names the configuration the job cannot be wired without ("" when
	// it is always wired).
	Needs string
}

// Kill-switch behaviour, stated once per shape.
const (
	ksNone = "consults no kill switch or gate: it only releases, settles or records, which must keep running " +
		"while new risk is stopped (PART 52)"
	ksMargin = "margin authorizes it as risk-reducing: a kill switch or a gate switched off after the account " +
		"opened does not stop it; LIVE and an unknown environment refuse"
	ksPredictionClose = "closing needs no authorization and a kill switch never stops it (it only removes activity)"
	ksMarketplace     = "adds risk (paper intents): the marketplace authorizes every follow and refuses it while " +
		"the MARKETPLACE gate is inactive in STAGING/PROD or a kill switch stops it; the job only reports the refusal"
	ksBridge = "consults no kill switch: completing and reconciling a leg already in flight reduces risk; a new " +
		"leg is authorized by the bridge, never here"
	ksEquitiesVenue = "consults no switch itself: the service withholds the mark from an order a kill switch or its " +
		"instrument's status now blocks, so a blocked order never executes (a resting one keeps resting, a blocked market " +
		"order is refused) while an unblocked sell still can"
	ksDelegation = "expiry follows the package's own rule: a pending swap or withdrawal that a kill switch, a closed gate " +
		"or a lapsed capability now stops is ended and its funds released (that reduces risk); observing and settling a " +
		"signed or submitted one consults no switch -- what the chain finalized is booked; nothing is signed or submitted"
)

// maxBatch is the bound on any job's batch.
const maxBatch = 10_000

// Catalog returns every job's descriptor in the order the worker registers
// them.
func Catalog() []Descriptor {
	return []Descriptor{
		{Name: NameCapitalExpiry, Default: Settings{Interval: 30 * time.Second, Batch: 500},
			Calls:      "capital.Service.ExpireDueBatch, one reservation per transaction; one that cannot be expired is backed off and reported",
			KillSwitch: ksNone},
		{Name: NameMarginInterest, Default: Settings{Interval: time.Hour, Batch: 500},
			Calls:      "margin.Service.AccrueInterest per margin account holding an open loan lot",
			KillSwitch: ksMargin},
		{Name: NameMarginMarkToMarket, Default: Settings{Interval: time.Minute, Batch: 500},
			Calls:      "margin.Service.Evaluate per NORMAL, WARNING or MARGIN_CALL account (MarkToMarket's unit), paged by account",
			KillSwitch: ksMargin},
		{Name: NameMarginLiquidation, Default: Settings{Interval: 30 * time.Second, Batch: 50}, MaxBatch: 100,
			Calls:      "margin.Service.ProcessLiquidationQueue, each account's step in its own transaction",
			KillSwitch: ksMargin},
		{Name: NameDerivativesVariation, Default: Settings{Interval: 5 * time.Minute, Batch: 200}, MaxBatch: 1000,
			Calls:      "derivatives.Service.RunVariationMargin for the latest completed interval of each unexpired future or perpetual with open positions",
			KillSwitch: ksNone},
		{Name: NameDerivativesFunding, Default: Settings{Interval: 5 * time.Minute, Batch: 200},
			Calls:      "derivatives.Service.RunFunding per completed funding interval of each perpetual, oldest first",
			KillSwitch: ksNone},
		{Name: NameDerivativesExpiry, Default: Settings{Interval: time.Minute, Batch: 50}, MaxBatch: 1000,
			Calls:      "derivatives.Service.SettleExpiry at the mark-derived price (never an override) per expired future or option with open positions",
			KillSwitch: ksNone},
		{Name: NameDerivativesClearing, Default: Settings{Interval: 5 * time.Minute, Batch: 50},
			Calls:      "derivatives.Service.ReconcileClearing per settlement asset of the clearing house",
			KillSwitch: ksNone},
		{Name: NamePredictionClose, Default: Settings{Interval: 30 * time.Second, Batch: 200},
			Calls:      "predmarket.Service.CloseDueBatch, one market per transaction; one that cannot be closed is backed off and reported",
			KillSwitch: ksPredictionClose},
		{Name: NamePredictionEscrow, Default: Settings{Interval: time.Hour, Batch: 200},
			Calls:      "predmarket.Service.ReconcileEscrow per market between OPEN and RESOLVED or DISPUTED",
			KillSwitch: ksNone},
		{Name: NameOrdersCompleteTerminal, Default: Settings{Interval: time.Minute, Batch: 500},
			Calls:      "orders.Service.CompleteTerminal, one intent per transaction; one that cannot be completed is backed off and reported",
			KillSwitch: ksNone},
		{Name: NameOrdersBookProof, Default: Settings{Interval: 15 * time.Minute, Batch: 50},
			Calls:      "orders.Service.VerifyJournal per OPEN or HALTED book; a journal that does not replay opens a reconciliation record",
			KillSwitch: ksNone},
		{Name: NameEquitiesSettlement, Default: Settings{Interval: 5 * time.Minute, Batch: 100},
			Calls:      "equities.Service.SettleDueBatch, one account per transaction, until nothing is due; a settlement that cannot settle is backed off and reported",
			KillSwitch: ksNone},
		{Name: NameEquitiesCorporate, Default: Settings{Interval: 15 * time.Minute, Batch: 100},
			Calls:      "equities.Service.ApplyCorporateAction per ANNOUNCED or APPLYING action whose date has come",
			KillSwitch: ksNone},
		{Name: NameEquitiesVenue, Default: Settings{Interval: 30 * time.Second, Batch: 200}, MaxBatch: 1000,
			Calls: "equities.Service.PlaceAtVenue per ACCEPTED order past the placement grace (PollVenue when its submission " +
				"is unknown; MarkSubmissionUnknown when a placement answers SUBMISSION_STATE_UNKNOWN), PollVenue per OPEN order; " +
				"each answer applied by ApplyOrderReport in its own transaction",
			KillSwitch: ksEquitiesVenue},
		{Name: NameMarketplaceFollows, Default: Settings{Interval: time.Minute, Batch: 100}, MaxBatch: 500,
			Calls:      "marketplace.Service.FollowScheduled per install marketplace.Service.DueInstalls returns",
			KillSwitch: ksMarketplace},
		{Name: NameStableMonitor, Default: Settings{Interval: 5 * time.Minute, Batch: 50},
			Calls:      "stableasset.Service.RunMonitor per stable asset on the configured network",
			KillSwitch: ksNone, Needs: NeedsDevnet},
		{Name: NameStableSupply, Default: Settings{Interval: time.Hour, Batch: 50},
			Calls:      "stableasset.Service.ReconcileSupply (PERIODIC) per stable asset on the configured network",
			KillSwitch: ksNone, Needs: NeedsDevnet},
		{Name: NameStableUnknown, Default: Settings{Interval: time.Minute, Batch: 100}, MaxBatch: 500,
			Calls:      "stableasset.Service.ReconcileUnknown (the service's network only), each request in its own transaction; one that cannot be settled is backed off and reported",
			KillSwitch: ksNone, Needs: NeedsDevnet},
		{Name: NameStableRedemptions, Default: Settings{Interval: time.Minute, Batch: 100}, MaxBatch: 500,
			Calls:      "stableasset.Service.ProcessRedemptions per stable asset with queued redemptions; a payout that cannot be made is backed off and reported",
			KillSwitch: ksNone, Needs: NeedsDevnet},
		{Name: NameBridgeLegs, Default: Settings{Interval: time.Minute, Batch: 100}, MaxBatch: 500,
			Calls: "bridge.Service.ObservePending over the configured network's in-flight legs, each driven on its own " +
				"(ReconcileLeg for a DESTINATION_FINALIZED leg, Observe's drive for an earlier one); one that cannot be advanced is backed off and reported",
			KillSwitch: ksBridge, Needs: NeedsDevnet},
		{Name: NameBridgeRoutes, Default: Settings{Interval: time.Hour, Batch: 50},
			Calls:      "bridge.Service.ReconcileRoute (PERIODIC) per route on the configured network",
			KillSwitch: ksNone, Needs: NeedsDevnet},
		{Name: NameTransfersReconcile, Default: Settings{Interval: 15 * time.Minute, Batch: 1}, MaxBatch: 1,
			Calls:      "transfers.Service.Reconcile in a snapshot; a discrepancy opens a reconciliation record",
			KillSwitch: ksNone},
		{Name: NameDelegationUpkeep, Default: Settings{Interval: 30 * time.Second, Batch: 100}, MaxBatch: 500,
			Calls: "delegation.Service.ExpireStale over the network's pending subjects past their deadline or no longer standing, " +
				"one per transaction; then delegation.Service.ObserveInFlight (observe by transaction id, produce SANDBOX blocks, " +
				"never resubmit); a subject that cannot be handled is backed off and reported",
			KillSwitch: ksDelegation, Needs: NeedsDevnet},
	}
}

// Describe returns the descriptor of a job.
func Describe(name string) (Descriptor, bool) {
	for _, d := range Catalog() {
		if d.Name == name {
			return d, true
		}
	}
	return Descriptor{}, false
}

// Resolve applies overrides to the defaults: a zero field keeps the default,
// and the batch is clamped to the subsystem's bound.
func (d Descriptor) Resolve(o Settings) Settings {
	s := d.Default
	if o.Interval > 0 {
		s.Interval = o.Interval
	}
	if o.Batch > 0 {
		s.Batch = o.Batch
	}
	limit := maxBatch
	if d.MaxBatch > 0 {
		limit = d.MaxBatch
	}
	s.Batch = clampBatch(s.Batch, limit)
	return s
}

// spec builds a job's spec from its catalog entry and the overrides. Every
// constructor names a catalog constant; a name that is not in the catalog
// gets no interval, which NewScheduler refuses, so the mistake is a failed
// start rather than a job running on a guessed schedule.
func spec(name string, o Settings) Spec {
	d, ok := Describe(name)
	if !ok {
		return Spec{Name: name}
	}
	return Spec{Name: d.Name, Settings: d.Resolve(o), Calls: d.Calls, KillSwitch: d.KillSwitch}
}

// ValidateOverrides refuses an override naming a job the catalog does not
// have: a typo in a schedule is a failed start, never an ignored setting.
func ValidateOverrides(intervals map[string]time.Duration, batches map[string]int) error {
	var unknown []string
	for name := range intervals {
		if _, ok := Describe(name); !ok {
			unknown = append(unknown, name)
		}
	}
	for name := range batches {
		if _, ok := Describe(name); !ok {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return errs.Newf(errs.CodeValidationFailed, "advancedjobs: an override names a job the worker does not run: %s",
			strings.Join(unknown, ", ")).
			WithField("jobs", unknown)
	}
	return nil
}
