package funding

import (
	"strings"
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

type (
	depositKind    struct{}
	transitionKind struct{}
)

// DepositID identifies a deposits row.
type DepositID = id.ID[depositKind]

// TransitionID identifies a deposit_transitions row.
type TransitionID = id.ID[transitionKind]

// NewDepositID mints a deposit id.
func NewDepositID() DepositID { return id.New[depositKind]() }

// ParseDepositID parses the canonical form.
func ParseDepositID(s string) (DepositID, error) { return id.Parse[depositKind](s) }

// NewTransitionID mints a transition id.
func NewTransitionID() TransitionID { return id.New[transitionKind]() }

// FraudState is the provider/compliance fraud disposition of a deposit
// (PART 27). It is orthogonal to Status.
type FraudState string

// Fraud states; mirrors the CHECK constraint on deposits.fraud_state.
const (
	FraudNone      FraudState = "NONE"
	FraudReview    FraudState = "REVIEW"
	FraudConfirmed FraudState = "CONFIRMED_FRAUD"
	FraudCleared   FraudState = "CLEARED"
)

// Valid reports whether f is declared.
func (f FraudState) Valid() bool {
	switch f {
	case FraudNone, FraudReview, FraudConfirmed, FraudCleared:
		return true
	}
	return false
}

// AllowsWithdrawal reports whether the fraud state permits
// withdrawal_eligible to be set (only NONE and CLEARED).
func (f FraudState) AllowsWithdrawal() bool { return f == FraudNone || f == FraudCleared }

// Deposit mirrors one deposits row (migration 00103).
type Deposit struct {
	ID                DepositID
	AccountID         accounts.AccountID
	FundingSourceID   string // uuid text; "" when none
	Provider          string
	ProviderSessionID string
	ProviderRef       string // provider transaction reference once fulfilled
	Status            Status

	FiatAmountMinor *int64
	FiatCurrency    string

	ExpectedAssetID  assets.AssetID
	ExpectedQuantity *money.Quantity // provider-reported destination amount in base units
	ObservedQuantity *money.Quantity // chain-observed credit in base units

	DestinationWalletID string
	DestinationAddress  string
	TxSignature         string
	ChainSlot           *int64

	FraudState                FraudState
	ReversibleUntil           *time.Time
	BuyingPowerEligible       bool
	WithdrawalEligible        bool
	AvailabilityPolicyVersion string

	JournalTransactionID         *ledger.TransactionID
	ReversalJournalTransactionID *ledger.TransactionID

	IdempotencyKey string
	CorrelationID  string

	CreatedAt            time.Time
	SessionCreatedAt     *time.Time
	CustomerActionAt     *time.Time
	ProviderProcessingAt *time.Time
	ProviderConfirmedAt  *time.Time
	SettlementObservedAt *time.Time
	ReconciledAt         *time.Time
	AvailableAt          *time.Time
	FailedAt             *time.Time
	ExpiredAt            *time.Time
	CancelledAt          *time.Time
	ReversedAt           *time.Time
	ReviewRequiredAt     *time.Time
	UpdatedAt            time.Time
}

// SettledQuantity is the quantity the ledger holds for this deposit: the
// observed chain credit (what FUNDING_SETTLED posted). Zero before
// settlement.
func (d Deposit) SettledQuantity() money.Quantity {
	if d.ObservedQuantity == nil {
		return money.Quantity{}
	}
	return *d.ObservedQuantity
}

// Transition mirrors one deposit_transitions row.
type Transition struct {
	ID            TransitionID
	DepositID     DepositID
	From          Status
	To            Status
	ActorType     security.ActorType
	ActorID       string
	Reason        string
	EvidenceRef   string
	CorrelationID string
	OccurredAt    time.Time
}

// TransitionEvidence is who moved the deposit and why. ActorType, ActorID
// and Reason are required (PART 28: every transition audited).
type TransitionEvidence struct {
	ActorType     security.ActorType
	ActorID       string
	Reason        string
	EvidenceRef   string
	CorrelationID string
	RequestID     string
	// Detail is optional structured context carried into the outbox and
	// audit payloads (e.g. reversal amounts, reconciliation deltas).
	Detail map[string]any
}

// Validate checks the evidence is complete and the actor is one the
// transitions table admits. AGENT actors are refused: an agent never moves
// funding state.
func (e TransitionEvidence) Validate() error {
	if !e.ActorType.Valid() {
		return errs.Newf(errs.CodeValidationFailed, "funding: unknown actor type %q", e.ActorType)
	}
	if e.ActorType == security.ActorAgent {
		return errs.New(errs.CodeForbidden, "funding: agents cannot transition deposits")
	}
	if strings.TrimSpace(e.ActorID) == "" {
		return errs.New(errs.CodeValidationFailed, "funding: actor id is required")
	}
	if strings.TrimSpace(e.Reason) == "" {
		return errs.New(errs.CodeValidationFailed, "funding: transition reason is required")
	}
	return nil
}

// SystemEvidence is the evidence of an automated transition.
func SystemEvidence(reason, evidenceRef, correlationID string) TransitionEvidence {
	return TransitionEvidence{
		ActorType: security.ActorSystem, ActorID: SystemActorID, Reason: reason,
		EvidenceRef: evidenceRef, CorrelationID: correlationID,
	}
}

// SystemActorID is the actor id of automated funding transitions.
const SystemActorID = "funding-service"

// Patch carries the column updates applied together with a transition. A
// nil field is left untouched. It exists so a transition and the evidence
// that justifies it (observed quantity, signature, journal transaction) are
// written by one UPDATE inside one transaction.
type Patch struct {
	ProviderSessionID            *string
	ProviderRef                  *string
	FiatAmountMinor              *int64
	FiatCurrency                 *string
	ExpectedQuantity             *money.Quantity
	ObservedQuantity             *money.Quantity
	TxSignature                  *string
	ChainSlot                    *int64
	FraudState                   *FraudState
	ReversibleUntil              *time.Time
	BuyingPowerEligible          *bool
	WithdrawalEligible           *bool
	AvailabilityPolicyVersion    *string
	JournalTransactionID         *ledger.TransactionID
	ReversalJournalTransactionID *ledger.TransactionID
}

// CreateDeposit is the input of Repository.Create.
type CreateDeposit struct {
	AccountID           accounts.AccountID
	Provider            string
	ExpectedAssetID     assets.AssetID
	FiatAmountMinor     *int64
	FiatCurrency        string
	DestinationWalletID string
	DestinationAddress  string
	IdempotencyKey      string
	CorrelationID       string
}

// Validate checks the required fields.
func (c CreateDeposit) Validate() error {
	problems := map[string]any{}
	if c.AccountID.IsZero() {
		problems["account_id"] = "required"
	}
	if strings.TrimSpace(c.Provider) == "" {
		problems["provider"] = "required"
	}
	if c.ExpectedAssetID.IsZero() {
		problems["expected_asset_id"] = "required"
	}
	if c.FiatAmountMinor != nil && *c.FiatAmountMinor <= 0 {
		problems["fiat_amount_minor"] = "must be positive"
	}
	if strings.TrimSpace(c.DestinationAddress) == "" {
		problems["destination_address"] = "required"
	}
	if strings.TrimSpace(c.IdempotencyKey) == "" {
		problems["idempotency_key"] = "required"
	}
	if c.DestinationWalletID != "" {
		if _, err := id.ParseAny(c.DestinationWalletID); err != nil {
			problems["destination_wallet_id"] = "must be a canonical uuid"
		}
	}
	if len(problems) > 0 {
		return errs.New(errs.CodeValidationFailed, "funding: invalid deposit").WithFields(problems)
	}
	return nil
}

// ParseDecimalAmount converts a provider decimal string ("100.00",
// "0.123400") to exact base units of an asset with the given decimals. The
// grammar is digits ["." digits]: no sign, exponent, whitespace or
// fractional digits beyond the asset's precision (PRECISION_LOSS). A float
// never enters the system: the JSON layer already refuses numbers.
func ParseDecimalAmount(s string, decimals uint8) (money.Quantity, error) {
	if s == "" {
		return money.Quantity{}, errs.New(errs.CodeValidationFailed, "funding: amount is empty")
	}
	if strings.HasPrefix(s, "-") || strings.HasPrefix(s, "+") {
		return money.Quantity{}, errs.New(errs.CodeValidationFailed, "funding: amount must not be signed")
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && c != '.' {
			return money.Quantity{}, errs.Newf(errs.CodeValidationFailed, "funding: amount %q is not a plain decimal", s)
		}
	}
	q, err := money.QuantityFromDecimalString(s, decimals, money.RoundExact)
	if err != nil {
		return money.Quantity{}, errs.Wrap(err, errs.CodePrecisionLoss, "funding: amount is not representable at asset precision")
	}
	return q, nil
}
