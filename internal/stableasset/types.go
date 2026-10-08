package stableasset

import (
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
)

type (
	stableAssetKind struct{}
	mintRequestKind struct{}
	burnRequestKind struct{}
)

// StableAssetID identifies a platform stable asset (a stable_assets row).
type StableAssetID = id.ID[stableAssetKind]

// MintRequestID identifies a stable_mint_requests row.
type MintRequestID = id.ID[mintRequestKind]

// BurnRequestID identifies a stable_burn_requests row.
type BurnRequestID = id.ID[burnRequestKind]

// ParseStableAssetID parses the canonical form.
func ParseStableAssetID(s string) (StableAssetID, error) { return id.Parse[stableAssetKind](s) }

// Status is a platform stable asset's status.
type Status string

// Statuses.
const (
	// StatusActive: minting and burning are open.
	StatusActive Status = "ACTIVE"
	// StatusPaused: the circuit breaker (or an operator) stopped both minting
	// and burning while a peg, reserve or supply problem is investigated.
	StatusPaused Status = "PAUSED"
	// StatusRetired: no new issuance; redemptions continue until nothing is
	// outstanding. Terminal.
	StatusRetired Status = "RETIRED"
)

// AllStatuses returns every status (the Go twin of stable_assets_status_check).
func AllStatuses() []Status { return []Status{StatusActive, StatusPaused, StatusRetired} }

var statusTransitions = map[Status][]Status{
	StatusActive: {StatusPaused, StatusRetired},
	StatusPaused: {StatusActive, StatusRetired},
}

// CanTransition reports whether from -> to is legal. It mirrors
// cp_stable_asset_transition_legal (migration 00747).
func CanTransition(from, to Status) bool {
	for _, t := range statusTransitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// AllowsMint reports whether new issuance is open.
func (s Status) AllowsMint() bool { return s == StatusActive }

// AllowsBurn reports whether burns and redemptions are open. A PAUSED asset
// stops both: the breaker freezes supply changes while it is investigated.
func (s Status) AllowsBurn() bool { return s == StatusActive || s == StatusRetired }

// ReserveKind classifies an eligible reserve asset.
type ReserveKind string

// Reserve kinds.
const (
	// ReserveCashEquivalent: a simulated dollar or equivalent, valued at the
	// lower of its oracle price and par. Only cash equivalents may pay for a
	// mint or be paid out by a redemption.
	ReserveCashEquivalent ReserveKind = "CASH_EQUIVALENT"
	// ReserveTokenized: a tokenized instrument (a simulated T-bill), valued at
	// its oracle price, held by the issuer as over-collateralisation.
	ReserveTokenized ReserveKind = "TOKENIZED"
)

// AllReserveKinds returns every reserve kind (the Go twin of
// stable_reserve_assets_reserve_kind_check).
func AllReserveKinds() []ReserveKind { return []ReserveKind{ReserveCashEquivalent, ReserveTokenized} }

// RedemptionMode says when a finalized burn pays out.
type RedemptionMode string

// Redemption modes.
const (
	// RedemptionInstant posts the redemption as soon as the burn is final.
	RedemptionInstant RedemptionMode = "INSTANT"
	// RedemptionQueued queues a final burn; ProcessRedemptions pays it out.
	RedemptionQueued RedemptionMode = "QUEUED"
)

// AllRedemptionModes returns every redemption mode (the Go twin of
// stable_assets_redemption_mode_check).
func AllRedemptionModes() []RedemptionMode {
	return []RedemptionMode{RedemptionInstant, RedemptionQueued}
}

// MintAuthorityMode says who holds the devnet mint authority.
type MintAuthorityMode string

// MintAuthorityPlatformSigner: the authority key lives only inside the
// package's IssuerSigner.
const MintAuthorityPlatformSigner MintAuthorityMode = "PLATFORM_SIGNER"

// AllMintAuthorityModes returns every mint authority mode (the Go twin of
// stable_assets_mint_authority_mode_check).
func AllMintAuthorityModes() []MintAuthorityMode {
	return []MintAuthorityMode{MintAuthorityPlatformSigner}
}

// BurnAuthorityMode says who signs a burn.
type BurnAuthorityMode string

// BurnAuthorityHolderSigned: the holder's devnet key signs TOKEN_BURN; in the
// sandbox that key is custodied by the package's CustodialWalletSigner.
const BurnAuthorityHolderSigned BurnAuthorityMode = "HOLDER_SIGNED"

// AllBurnAuthorityModes returns every burn authority mode (the Go twin of
// stable_assets_burn_authority_mode_check).
func AllBurnAuthorityModes() []BurnAuthorityMode {
	return []BurnAuthorityMode{BurnAuthorityHolderSigned}
}

// RequestModes are the modes a mint or burn request runs in (the Go twin of
// stable_mint_requests_mode_check and stable_burn_requests_mode_check).
func RequestModes() []gates.Mode { return []gates.Mode{gates.ModeSandbox, gates.ModeDevnet} }

// MintStatus is a mint request's status.
type MintStatus string

// Mint request statuses.
const (
	// MintSubmitting: the plan and signed transaction are recorded and the
	// submission is about to happen (or happened and was not yet recorded).
	MintSubmitting MintStatus = "SUBMITTING"
	// MintSubmitted: the chain accepted the submission.
	MintSubmitted MintStatus = "SUBMITTED"
	// MintSubmissionUnknown: the submission's outcome is unknown. Only
	// reconciliation by tx id resolves it; it is never resubmitted.
	MintSubmissionUnknown MintStatus = "SUBMISSION_UNKNOWN"
	// MintIssued: the mint is FINALIZED on chain and STABLE_ISSUANCE posted.
	MintIssued MintStatus = "ISSUED"
	// MintFailed: the mint failed on chain, expired, or was refused; the
	// reserve reservation was released.
	MintFailed MintStatus = "FAILED"
)

// AllMintStatuses returns every mint status (the Go twin of
// stable_mint_requests_status_check).
func AllMintStatuses() []MintStatus {
	return []MintStatus{MintSubmitting, MintSubmitted, MintSubmissionUnknown, MintIssued, MintFailed}
}

var mintTransitions = map[MintStatus][]MintStatus{
	MintSubmitting:        {MintSubmitted, MintSubmissionUnknown, MintIssued, MintFailed},
	MintSubmitted:         {MintIssued, MintFailed},
	MintSubmissionUnknown: {MintIssued, MintFailed},
}

// CanTransitionMint mirrors cp_stable_mint_transition_legal.
func CanTransitionMint(from, to MintStatus) bool { return legal(mintTransitions, from, to) }

// Terminal reports whether the request can no longer change.
func (s MintStatus) Terminal() bool { return s == MintIssued || s == MintFailed }

// BurnStatus is a burn request's status.
type BurnStatus string

// Burn request statuses.
const (
	BurnSubmitting        BurnStatus = "SUBMITTING"
	BurnSubmitted         BurnStatus = "SUBMITTED"
	BurnSubmissionUnknown BurnStatus = "SUBMISSION_UNKNOWN"
	// BurnRedemptionQueued: the burn is FINALIZED on chain and the payout is
	// queued (redemption mode QUEUED). The holder's stable units are still
	// reserved on the ledger until the payout posts.
	BurnRedemptionQueued BurnStatus = "REDEMPTION_QUEUED"
	// BurnRedeemed: STABLE_REDEMPTION posted.
	BurnRedeemed BurnStatus = "REDEEMED"
	BurnFailed   BurnStatus = "FAILED"
)

// AllBurnStatuses returns every burn status (the Go twin of
// stable_burn_requests_status_check).
func AllBurnStatuses() []BurnStatus {
	return []BurnStatus{BurnSubmitting, BurnSubmitted, BurnSubmissionUnknown, BurnRedemptionQueued, BurnRedeemed, BurnFailed}
}

var burnTransitions = map[BurnStatus][]BurnStatus{
	BurnSubmitting:        {BurnSubmitted, BurnSubmissionUnknown, BurnRedemptionQueued, BurnRedeemed, BurnFailed},
	BurnSubmitted:         {BurnRedemptionQueued, BurnRedeemed, BurnFailed},
	BurnSubmissionUnknown: {BurnRedemptionQueued, BurnRedeemed, BurnFailed},
	BurnRedemptionQueued:  {BurnRedeemed},
}

// CanTransitionBurn mirrors cp_stable_burn_transition_legal.
func CanTransitionBurn(from, to BurnStatus) bool { return legal(burnTransitions, from, to) }

// Terminal reports whether the request can no longer change.
func (s BurnStatus) Terminal() bool { return s == BurnRedeemed || s == BurnFailed }

func legal[S comparable](table map[S][]S, from, to S) bool {
	for _, t := range table[from] {
		if t == to {
			return true
		}
	}
	return false
}

// AlertKind names a peg-monitor finding.
type AlertKind string

// Alert kinds.
const (
	AlertPriceDeviation    AlertKind = "PRICE_DEVIATION"
	AlertPriceStale        AlertKind = "PRICE_STALE"
	AlertReserveShortfall  AlertKind = "RESERVE_SHORTFALL"
	AlertReservePriceStale AlertKind = "RESERVE_PRICE_STALE"
	AlertSupplyMismatch    AlertKind = "SUPPLY_MISMATCH"
)

// AllAlertKinds returns every alert kind (the Go twin of
// stable_peg_alerts_kind_check).
func AllAlertKinds() []AlertKind {
	return []AlertKind{AlertPriceDeviation, AlertPriceStale, AlertReserveShortfall, AlertReservePriceStale, AlertSupplyMismatch}
}

// Severity is an alert's severity.
type Severity string

// Severities.
const (
	SeverityWarning  Severity = "WARNING"
	SeverityCritical Severity = "CRITICAL"
)

// AllSeverities returns every severity (the Go twin of
// stable_peg_alerts_severity_check).
func AllSeverities() []Severity { return []Severity{SeverityWarning, SeverityCritical} }

// Subsystem effect kinds.
const (
	effectKindMint = "STABLE_MINT"
	effectKindBurn = "STABLE_BURN"
)
