package ledger

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// Side is the side of a journal entry.
type Side string

// Sides.
const (
	Debit  Side = "DEBIT"
	Credit Side = "CREDIT"
)

// Valid reports whether s is DEBIT or CREDIT.
func (s Side) Valid() bool { return s == Debit || s == Credit }

// Opposite returns the other side.
func (s Side) Opposite() Side {
	if s == Debit {
		return Credit
	}
	return Debit
}

// OwnerType says whose books a ledger account belongs to.
type OwnerType string

// Owner types.
const (
	OwnerCustomer OwnerType = "CUSTOMER"
	OwnerPlatform OwnerType = "PLATFORM"
)

// Valid reports whether o is a declared owner type.
func (o OwnerType) Valid() bool { return o == OwnerCustomer || o == OwnerPlatform }

// PlatformOwnerID is the fixed owner_id of every PLATFORM ledger account. It
// is a version-7-shaped UUID so it parses wherever account ids do, and it is
// the only owner id a PLATFORM account may carry.
const PlatformOwnerID = "00000000-0000-7000-8000-000000000001"

// Code identifies the role of a ledger account (FINANCIAL_MODEL §2.1).
type Code string

// Account codes.
const (
	// CodeWallet is the customer's entitlement to asset units held in their
	// embedded wallet under platform control. DEBIT-normal, never negative.
	CodeWallet Code = "WALLET"
	// CodeCapital is capital contributed by the customer net of withdrawals.
	// CREDIT-normal, never negative.
	CodeCapital Code = "CAPITAL"
	// CodeTradingOutflow is asset units disposed through trades. DEBIT-normal.
	CodeTradingOutflow Code = "TRADING_OUTFLOW"
	// CodeTradingInflow is asset units acquired through trades. CREDIT-normal.
	CodeTradingInflow Code = "TRADING_INFLOW"
	// CodeFeesNetwork is network fees paid (e.g. SOL). DEBIT-normal.
	CodeFeesNetwork Code = "FEES_NETWORK"
	// CodeFeesVenue is explicit venue fees. DEBIT-normal.
	CodeFeesVenue Code = "FEES_VENUE"
	// CodeFeesPlatform is platform fees paid by the customer. DEBIT-normal.
	CodeFeesPlatform Code = "FEES_PLATFORM"
	// CodeDeficit is what a customer owes the platform after a funding
	// reversal exceeded their WALLET balance (PART 27). It is never negative.
	//
	// It is CREDIT-normal. The CUSTOMER chart is the customer's own books, in
	// which WALLET is an asset, CAPITAL is equity and an obligation to the
	// platform is a liability. FINANCIAL_MODEL §2.1 lists DEFICIT as
	// DEBIT-normal while §2.2 requires the deficit path to net to
	// CAPITAL −amount and DEFICIT +shortfall; with a DEBIT-normal DEFICIT the
	// two cannot both hold, because balanced transactions force, per asset,
	// Σ debit-normal balances == Σ credit-normal balances, i.e.
	// CAPITAL == WALLET + TRADING_OUTFLOW + DEFICIT. The stated net effect is
	// the economically meaningful one (a fully reversed deposit leaves no
	// contributed capital), so this package follows it; see
	// FundingReversalPostings.
	CodeDeficit Code = "DEFICIT"
	// CodeReconciliationAdjustment holds reason-coded corrections when
	// external truth differs (dust, airdrop, rounding). CREDIT-normal,
	// bidirectional.
	CodeReconciliationAdjustment Code = "RECONCILIATION_ADJUSTMENT"
	// CodePlatformFeeReceivable is fees owed to / collected by the platform.
	// PLATFORM-owned, DEBIT-normal.
	CodePlatformFeeReceivable Code = "PLATFORM_FEE_RECEIVABLE"
	// CodePlatformFeeRevenue is platform fee revenue. PLATFORM-owned,
	// CREDIT-normal.
	CodePlatformFeeRevenue Code = "PLATFORM_FEE_REVENUE"
	// CodePlatformAdjustment is the platform-side offset for corrections.
	// PLATFORM-owned, CREDIT-normal, bidirectional.
	CodePlatformAdjustment Code = "PLATFORM_ADJUSTMENT"

	// --- Nodal-native economy, customer side (gola.md PARTS XII-XVII) ---

	// CodeCreditBalance is the customer's spendable Nodal Credits.
	// DEBIT-normal, never negative.
	CodeCreditBalance Code = "CREDIT_BALANCE"
	// CodeCreditIssuance is the customer-side origin of the Credits they
	// hold: what they bought, were granted or earned. CREDIT-normal, the
	// Credit analogue of CAPITAL, never negative.
	CodeCreditIssuance Code = "CREDIT_ISSUANCE"
	// CodeNativeAssetBalance is the customer's holding of a Nodal-native
	// asset. DEBIT-normal, never negative.
	CodeNativeAssetBalance Code = "NATIVE_ASSET_BALANCE"
	// CodePayoutReserved is Credits committed to a payout request and no
	// longer spendable. Its value domain is PAYOUT_PENDING rather than the
	// asset's INTERNAL_CREDIT, which is what makes reserving a payout a
	// gated cross-domain movement. DEBIT-normal, never negative.
	CodePayoutReserved Code = "PAYOUT_RESERVED"

	// --- Nodal-native economy, platform side ---

	// CodeMarketReserve is Credits held in a native market's reserve.
	// DEBIT-normal, never negative: a market may not owe Credits it does not
	// hold.
	CodeMarketReserve Code = "MARKET_RESERVE"
	// CodeMarketInventory is the unsold native-asset units a market's curve
	// still holds. DEBIT-normal, never negative: a market cannot sell supply
	// that does not exist, which is the invariant that stops a creator
	// minting behind the curve.
	CodeMarketInventory Code = "MARKET_INVENTORY"
	// CodePayoutClearing is the platform side of reserved payout value.
	// CREDIT-normal, domain PAYOUT_PENDING.
	CodePayoutClearing Code = "PAYOUT_CLEARING"
	// CodePayoutSettled is value irrevocably paid out through an approved
	// provider. DEBIT-normal, domain EXTERNAL_SETTLED, terminal.
	CodePayoutSettled Code = "PAYOUT_SETTLED"
)

type codeInfo struct {
	owner         OwnerType
	normal        Side
	allowNegative bool
}

// codeRegistry is the chart of accounts. It is never mutated after init.
var codeRegistry = map[Code]codeInfo{
	CodeWallet:                   {OwnerCustomer, Debit, false},
	CodeCapital:                  {OwnerCustomer, Credit, false},
	CodeTradingOutflow:           {OwnerCustomer, Debit, false},
	CodeTradingInflow:            {OwnerCustomer, Credit, false},
	CodeFeesNetwork:              {OwnerCustomer, Debit, false},
	CodeFeesVenue:                {OwnerCustomer, Debit, false},
	CodeFeesPlatform:             {OwnerCustomer, Debit, false},
	CodeDeficit:                  {OwnerCustomer, Credit, false},
	CodeReconciliationAdjustment: {OwnerCustomer, Credit, true},
	CodePlatformFeeReceivable:    {OwnerPlatform, Debit, false},
	CodePlatformFeeRevenue:       {OwnerPlatform, Credit, false},
	CodePlatformAdjustment:       {OwnerPlatform, Credit, true},

	CodeCreditBalance:      {OwnerCustomer, Debit, false},
	CodeCreditIssuance:     {OwnerCustomer, Credit, false},
	CodeNativeAssetBalance: {OwnerCustomer, Debit, false},
	CodePayoutReserved:     {OwnerCustomer, Debit, false},

	CodeMarketReserve:   {OwnerPlatform, Debit, false},
	CodeMarketInventory: {OwnerPlatform, Debit, false},
	CodePayoutClearing:  {OwnerPlatform, Credit, false},
	CodePayoutSettled:   {OwnerPlatform, Debit, false},
}

var allCodes = []Code{
	CodeWallet, CodeCapital, CodeTradingOutflow, CodeTradingInflow,
	CodeFeesNetwork, CodeFeesVenue, CodeFeesPlatform, CodeDeficit, CodeReconciliationAdjustment,
	CodePlatformFeeReceivable, CodePlatformFeeRevenue, CodePlatformAdjustment,
	CodeCreditBalance, CodeCreditIssuance, CodeNativeAssetBalance, CodePayoutReserved,
	CodeMarketReserve, CodeMarketInventory, CodePayoutClearing, CodePayoutSettled,
}

// AllCodes returns every account code in chart order (a copy).
func AllCodes() []Code { return append([]Code(nil), allCodes...) }

// Valid reports whether c is in the chart of accounts.
func (c Code) Valid() bool {
	_, ok := codeRegistry[c]
	return ok
}

// NormalSide returns the side on which the account's balance grows.
// Unknown codes return the empty Side.
func (c Code) NormalSide() Side { return codeRegistry[c].normal }

// AllowsNegative reports whether the balance may go below zero.
func (c Code) AllowsNegative() bool { return codeRegistry[c].allowNegative }

// OwnerType returns the owner type the code belongs to. Unknown codes
// return the empty OwnerType.
func (c Code) OwnerType() OwnerType { return codeRegistry[c].owner }

// Kind classifies a journal transaction by the financial event it records.
type Kind string

// Transaction kinds.
const (
	KindFundingSettled           Kind = "FUNDING_SETTLED"
	KindFundingReversal          Kind = "FUNDING_REVERSAL"
	KindFundingReversalDeficit   Kind = "FUNDING_REVERSAL_DEFICIT"
	KindTradeFill                Kind = "TRADE_FILL"
	KindFee                      Kind = "FEE"
	KindWithdrawalSettled        Kind = "WITHDRAWAL_SETTLED"
	KindCompensation             Kind = "COMPENSATION"
	KindCorrection               Kind = "CORRECTION"
	KindReconciliationAdjustment Kind = "RECONCILIATION_ADJUSTMENT"
	// KindSeed is for LOCAL/TEST fixtures only; Service rejects it unless
	// AllowSeedPostings was called by the composition root.
	KindSeed Kind = "SEED"

	// --- Nodal-native economy ---

	// KindCreditIssued records Credits minted to a customer against a funding
	// event, a promotional grant or an earning.
	KindCreditIssued Kind = "CREDIT_ISSUED"
	// KindCreditReversed records Credits destroyed because the funding behind
	// them was reversed.
	KindCreditReversed Kind = "CREDIT_REVERSED"
	// KindCreditSpent records Credits spent on a platform service.
	KindCreditSpent Kind = "CREDIT_SPENT"
	// KindNativeTrade records a buy or sell on a Nodal-native market.
	KindNativeTrade Kind = "NATIVE_TRADE"
	// KindInternalPurchase records a creator-economy purchase.
	KindInternalPurchase Kind = "INTERNAL_PURCHASE"
	// KindCreatorEarning records revenue credited to a creator.
	KindCreatorEarning Kind = "CREATOR_EARNING"
	// KindPayoutReserved records eligible Credits moving into PAYOUT_PENDING.
	KindPayoutReserved Kind = "PAYOUT_RESERVED"
	// KindPayoutSettled records reserved value leaving the system.
	KindPayoutSettled Kind = "PAYOUT_SETTLED"
	// KindPayoutReturned records reserved value coming back to the customer
	// after a failed, rejected or cancelled payout.
	KindPayoutReturned Kind = "PAYOUT_RETURNED"
)

var allKinds = []Kind{
	KindFundingSettled, KindFundingReversal, KindFundingReversalDeficit, KindTradeFill, KindFee,
	KindWithdrawalSettled, KindCompensation, KindCorrection, KindReconciliationAdjustment, KindSeed,
	KindCreditIssued, KindCreditReversed, KindCreditSpent, KindNativeTrade,
	KindInternalPurchase, KindCreatorEarning,
	KindPayoutReserved, KindPayoutSettled, KindPayoutReturned,
}

// AllKinds returns every kind in declaration order (a copy).
func AllKinds() []Kind { return append([]Kind(nil), allKinds...) }

// Valid reports whether k is a declared kind.
func (k Kind) Valid() bool {
	for _, x := range allKinds {
		if x == k {
			return true
		}
	}
	return false
}

// RequiresReversalOf reports whether k is a correction that must reference
// the transaction it compensates (FINANCIAL_MODEL §2.3).
func (k Kind) RequiresReversalOf() bool { return k == KindCompensation || k == KindCorrection }

// RequiresReasonCode reports whether k must carry Metadata[MetadataReasonCode].
func (k Kind) RequiresReasonCode() bool {
	return k == KindCompensation || k == KindCorrection || k == KindReconciliationAdjustment
}

// MetadataReasonCode is the Posting.Metadata key whose string value is stored
// in journal_transactions.reason_code.
const MetadataReasonCode = "reason_code"

// AccountRef identifies a ledger account by its business key.
type AccountRef struct {
	OwnerType OwnerType
	OwnerID   string
	Code      Code
	AssetID   assets.AssetID
}

// CustomerAccount builds the reference to a customer-owned account.
func CustomerAccount(accountID accounts.AccountID, code Code, asset assets.AssetID) AccountRef {
	return AccountRef{OwnerType: OwnerCustomer, OwnerID: accountID.String(), Code: code, AssetID: asset}
}

// PlatformAccount builds the reference to a platform-owned account.
func PlatformAccount(code Code, asset assets.AssetID) AccountRef {
	return AccountRef{OwnerType: OwnerPlatform, OwnerID: PlatformOwnerID, Code: code, AssetID: asset}
}

// Validate checks that the reference is well-formed: known owner type and
// code, code owned by that owner type, a non-zero asset, and an owner id
// that is the platform constant (PLATFORM) or a canonical account id
// (CUSTOMER).
func (r AccountRef) Validate() error {
	if !r.OwnerType.Valid() {
		return errs.Newf(errs.CodeValidationFailed, "unknown ledger owner type %q", r.OwnerType)
	}
	if !r.Code.Valid() {
		return errs.Newf(errs.CodeValidationFailed, "unknown ledger account code %q", r.Code)
	}
	if r.Code.OwnerType() != r.OwnerType {
		return errs.Newf(errs.CodeValidationFailed, "ledger code %s belongs to %s accounts, not %s", r.Code, r.Code.OwnerType(), r.OwnerType)
	}
	if r.AssetID.IsZero() {
		return errs.New(errs.CodeValidationFailed, "ledger account asset id is required")
	}
	switch r.OwnerType {
	case OwnerPlatform:
		if !strings.EqualFold(r.OwnerID, PlatformOwnerID) {
			return errs.New(errs.CodeValidationFailed, "platform ledger accounts must use the platform owner id")
		}
	case OwnerCustomer:
		acct, err := accounts.ParseAccountID(r.OwnerID)
		if err != nil || acct.IsZero() || strings.EqualFold(r.OwnerID, PlatformOwnerID) {
			return errs.New(errs.CodeValidationFailed, "customer ledger accounts need a canonical account id as owner id")
		}
	}
	return nil
}

// normalized returns r with the owner id in canonical lowercase form so
// equal references hash and sort identically.
func (r AccountRef) normalized() AccountRef {
	r.OwnerID = strings.ToLower(strings.TrimSpace(r.OwnerID))
	return r
}

// key is the total-order sort key used for deduplication and for acquiring
// account-creation locks in one global order.
func (r AccountRef) key() string {
	r = r.normalized()
	return string(r.OwnerType) + "|" + r.OwnerID + "|" + string(r.Code) + "|" + r.AssetID.String()
}

// String renders "OWNER_TYPE/owner_id/CODE/asset_id".
func (r AccountRef) String() string {
	return string(r.OwnerType) + "/" + r.OwnerID + "/" + string(r.Code) + "/" + r.AssetID.String()
}

// Entry is one leg of a Posting. Quantity is always positive; the side says
// which way it moves. USDValueMinor and PriceRef are valuation metadata.
type Entry struct {
	Account       AccountRef
	Side          Side
	Quantity      money.Quantity
	USDValueMinor *int64
	PriceRef      *string
}

// FinancialEventReference names the domain event a transaction records, e.g.
// {"fill", "<uuid>"}, {"deposit", "<uuid>"}, {"reconciliation_record", "<uuid>"}.
type FinancialEventReference struct {
	Type string
	ID   string
}

type transactionKind struct{}

// TransactionID identifies a journal transaction.
type TransactionID = id.ID[transactionKind]

// NewTransactionID returns a fresh transaction id.
func NewTransactionID() TransactionID { return id.New[transactionKind]() }

// ParseTransactionID parses the canonical form.
func ParseTransactionID(s string) (TransactionID, error) { return id.Parse[transactionKind](s) }

type ledgerAccountKind struct{}

// LedgerAccountID identifies a ledger account row.
type LedgerAccountID = id.ID[ledgerAccountKind]

// NewLedgerAccountID returns a fresh ledger account id.
func NewLedgerAccountID() LedgerAccountID { return id.New[ledgerAccountKind]() }

type journalEntryKind struct{}

// JournalEntryID identifies a journal entry row.
type JournalEntryID = id.ID[journalEntryKind]

// NewJournalEntryID returns a fresh journal entry id.
func NewJournalEntryID() JournalEntryID { return id.New[journalEntryKind]() }

// Posting is the request to record one journal transaction.
type Posting struct {
	Kind           Kind
	IdempotencyKey string
	Reference      FinancialEventReference
	EffectiveAt    time.Time
	Description    string
	CorrelationID  string
	ReversalOf     *TransactionID
	Entries        []Entry
	Metadata       map[string]any

	// Conversion names the value-domain movement this transaction performs.
	// It must be nil for a single-domain transaction and non-nil for one that
	// spans two domains: a cross-domain movement is always a stated intent
	// recorded in the journal, never something a reader has to infer from
	// which accounts happened to be involved (gola.md PART IX).
	Conversion *valuedomain.ConversionKey
}

// ReasonCode returns Metadata[MetadataReasonCode] when it is a string.
func (p Posting) ReasonCode() string {
	if p.Metadata == nil {
		return ""
	}
	s, _ := p.Metadata[MetadataReasonCode].(string)
	return s
}

// AccountStatus is the lifecycle state of a ledger account.
type AccountStatus string

// Account statuses.
const (
	AccountOpen   AccountStatus = "OPEN"
	AccountClosed AccountStatus = "CLOSED"
)

// LedgerAccount is a ledger_accounts row.
type LedgerAccount struct {
	ID            LedgerAccountID
	Ref           AccountRef
	NormalSide    Side
	AllowNegative bool
	Status        AccountStatus
	Domain        valuedomain.Domain
	CreatedAt     time.Time
}

// JournalEntry is a posted journal_entries row.
type JournalEntry struct {
	ID              JournalEntryID
	TransactionID   TransactionID
	Seq             int32
	LedgerAccountID LedgerAccountID
	Account         AccountRef
	Side            Side
	Quantity        money.Quantity
	USDValueMinor   *int64
	PriceRef        *string
}

// Transaction is a posted journal transaction with its entries in seq order.
type Transaction struct {
	ID                TransactionID
	Kind              Kind
	IdempotencyKey    string
	Reference         FinancialEventReference
	ReversalOf        *TransactionID
	EffectiveAt       time.Time
	PostedAt          time.Time
	Description       string
	CorrelationID     string
	PostedByActorType security.ActorType
	PostedByActorID   string
	ReasonCode        string
	Metadata          map[string]any
	ContentHash       []byte
	BuildVersion      string
	Entries           []JournalEntry
}

// AccountBalance is the trigger-maintained balance of one ledger account in
// normal-side terms.
type AccountBalance struct {
	LedgerAccountID LedgerAccountID
	Account         AccountRef
	Balance         money.Quantity
	EntryCount      int64
	Version         int64
	UpdatedAt       time.Time
}

// PostResult is the outcome of Post. Existing is true when the idempotency
// key had already been posted with identical content; nothing was written.
type PostResult struct {
	TransactionID TransactionID
	ContentHash   []byte
	Existing      bool
}

// Drift is a ledger account whose stored balance disagrees with the sum of
// its entries. Any Drift is a SEV1 ledger_integrity_violation.
type Drift struct {
	LedgerAccountID    LedgerAccountID
	Account            AccountRef
	StoredBalance      money.Quantity
	ComputedBalance    money.Quantity
	StoredEntryCount   int64
	ComputedEntryCount int64
}

// TransactionFilter narrows ListTransactions. Zero fields are ignored. Since
// (inclusive) and Until (exclusive) bound posted_at, the pagination axis.
type TransactionFilter struct {
	OwnerType OwnerType
	OwnerID   string
	AssetID   assets.AssetID
	Kind      Kind
	Since     time.Time
	Until     time.Time
}

// Poster records journal transactions. It is the only write path into the
// ledger and it is idempotent on Posting.IdempotencyKey.
type Poster interface {
	Post(ctx context.Context, tx pgx.Tx, p Posting) (PostResult, error)
}

// Reader reads balances and transactions. Methods accept any db.Querier so
// they run inside or outside a transaction.
type Reader interface {
	Balance(ctx context.Context, q db.Querier, ref AccountRef) (money.Quantity, error)
	BalancesForOwner(ctx context.Context, q db.Querier, ownerType OwnerType, ownerID string) ([]AccountBalance, error)
	Transaction(ctx context.Context, q db.Querier, txID TransactionID) (Transaction, error)
	ListTransactions(ctx context.Context, q db.Querier, filter TransactionFilter, cursor string, limit int) ([]Transaction, string, error)
}
