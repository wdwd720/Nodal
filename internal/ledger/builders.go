package ledger

import (
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// Posting builders for the canonical postings of FINANCIAL_MODEL §2.2. Every
// builder returns a Posting that already satisfies Validate; the caller still
// runs it through Poster.Post inside its own transaction.

// SwapInputs describes a swap fill: OutQuantity units of OutAsset were given
// up for InQuantity units of InAsset, paying a network fee and a platform fee
// (either may be zero). USD fields are optional valuation metadata for the
// corresponding legs; PriceRefs name the price observation used.
type SwapInputs struct {
	AccountID accounts.AccountID
	FillID    string

	OutAsset    assets.AssetID
	OutQuantity money.Quantity
	InAsset     assets.AssetID
	InQuantity  money.Quantity

	NetworkFeeAsset     assets.AssetID
	NetworkFeeQuantity  money.Quantity
	PlatformFeeAsset    assets.AssetID
	PlatformFeeQuantity money.Quantity

	OutUSD         *money.USD
	InUSD          *money.USD
	NetworkFeeUSD  *money.USD
	PlatformFeeUSD *money.USD
	OutPriceRef    string
	InPriceRef     string

	EffectiveAt   time.Time
	CorrelationID string
	Description   string
}

// contribution is one leg before same-account, same-side legs are merged.
type contribution struct {
	ref      AccountRef
	side     Side
	qty      money.Quantity
	usd      *money.USD
	priceRef string
}

// SwapPosting builds the TRADE_FILL posting of §2.2:
//
//	Cr WALLET:OUT              out (+ platform fee when paid in OUT)
//	Dr TRADING_OUTFLOW:OUT     out
//	Dr WALLET:IN               in
//	Cr TRADING_INFLOW:IN       in
//	Cr WALLET:PF / Dr FEES_PLATFORM:PF / Dr PLATFORM_FEE_RECEIVABLE:PF / Cr PLATFORM_FEE_REVENUE:PF   (platform fee)
//	Cr WALLET:NF / Dr FEES_NETWORK:NF                                                                 (network fee)
//
// Legs on the same account and side are merged into one entry, so the
// example of §2.2 yields a single "Cr WALLET:USDC 100.10". Every asset
// balances by construction. Reference is {"fill", FillID} and the
// idempotency key is "fill:<FillID>".
func SwapPosting(in SwapInputs) (Posting, error) {
	if in.AccountID.IsZero() {
		return Posting{}, errs.New(errs.CodeValidationFailed, "swap: account id is required")
	}
	if in.FillID == "" {
		return Posting{}, errs.New(errs.CodeValidationFailed, "swap: fill id is required")
	}
	if in.OutAsset.IsZero() || in.InAsset.IsZero() {
		return Posting{}, errs.New(errs.CodeValidationFailed, "swap: out and in assets are required")
	}
	if in.OutAsset == in.InAsset {
		return Posting{}, errs.New(errs.CodeValidationFailed, "swap: out and in assets must differ")
	}
	if in.OutQuantity.Sign() <= 0 || in.InQuantity.Sign() <= 0 {
		return Posting{}, errs.New(errs.CodeValidationFailed, "swap: out and in quantities must be positive")
	}
	if in.NetworkFeeQuantity.IsNegative() || in.PlatformFeeQuantity.IsNegative() {
		return Posting{}, errs.New(errs.CodeValidationFailed, "swap: fees cannot be negative")
	}
	if in.NetworkFeeQuantity.IsPositive() && in.NetworkFeeAsset.IsZero() {
		return Posting{}, errs.New(errs.CodeValidationFailed, "swap: network fee asset is required")
	}
	if in.PlatformFeeQuantity.IsPositive() && in.PlatformFeeAsset.IsZero() {
		return Posting{}, errs.New(errs.CodeValidationFailed, "swap: platform fee asset is required")
	}
	if in.EffectiveAt.IsZero() {
		return Posting{}, errs.New(errs.CodeValidationFailed, "swap: effective_at is required")
	}
	cust := func(code Code, asset assets.AssetID) AccountRef { return CustomerAccount(in.AccountID, code, asset) }

	legs := []contribution{
		{cust(CodeWallet, in.OutAsset), Credit, in.OutQuantity, in.OutUSD, in.OutPriceRef},
		{cust(CodeTradingOutflow, in.OutAsset), Debit, in.OutQuantity, in.OutUSD, in.OutPriceRef},
		{cust(CodeWallet, in.InAsset), Debit, in.InQuantity, in.InUSD, in.InPriceRef},
		{cust(CodeTradingInflow, in.InAsset), Credit, in.InQuantity, in.InUSD, in.InPriceRef},
	}
	if in.PlatformFeeQuantity.IsPositive() {
		pf := in.PlatformFeeQuantity
		legs = append(legs,
			contribution{cust(CodeWallet, in.PlatformFeeAsset), Credit, pf, in.PlatformFeeUSD, ""},
			contribution{cust(CodeFeesPlatform, in.PlatformFeeAsset), Debit, pf, in.PlatformFeeUSD, ""},
			contribution{PlatformAccount(CodePlatformFeeReceivable, in.PlatformFeeAsset), Debit, pf, in.PlatformFeeUSD, ""},
			contribution{PlatformAccount(CodePlatformFeeRevenue, in.PlatformFeeAsset), Credit, pf, in.PlatformFeeUSD, ""},
		)
	}
	if in.NetworkFeeQuantity.IsPositive() {
		nf := in.NetworkFeeQuantity
		legs = append(legs,
			contribution{cust(CodeWallet, in.NetworkFeeAsset), Credit, nf, in.NetworkFeeUSD, ""},
			contribution{cust(CodeFeesNetwork, in.NetworkFeeAsset), Debit, nf, in.NetworkFeeUSD, ""},
		)
	}
	entries, err := mergeLegs(legs)
	if err != nil {
		return Posting{}, err
	}
	description := in.Description
	if description == "" {
		description = "swap fill"
	}
	p := Posting{
		Kind:           KindTradeFill,
		IdempotencyKey: "fill:" + in.FillID,
		Reference:      FinancialEventReference{Type: "fill", ID: in.FillID},
		EffectiveAt:    in.EffectiveAt.UTC(),
		Description:    description,
		CorrelationID:  in.CorrelationID,
		Entries:        entries,
	}
	if err := p.Validate(); err != nil {
		return Posting{}, err
	}
	return p, nil
}

// mergeLegs coalesces legs with the same account and side, preserving
// first-seen order. USD is summed only when every merged leg carries it;
// the first non-empty price reference wins.
func mergeLegs(legs []contribution) ([]Entry, error) {
	type slot struct {
		entry   Entry
		usd     *money.USD
		usdFull bool
	}
	index := make(map[string]int)
	var slots []slot
	for _, l := range legs {
		k := l.ref.key() + "|" + string(l.side)
		i, ok := index[k]
		if !ok {
			index[k] = len(slots)
			slots = append(slots, slot{
				entry:   Entry{Account: l.ref, Side: l.side, Quantity: l.qty},
				usd:     l.usd,
				usdFull: l.usd != nil,
			})
			if l.priceRef != "" {
				pr := l.priceRef
				slots[len(slots)-1].entry.PriceRef = &pr
			}
			continue
		}
		s := &slots[i]
		s.entry.Quantity = s.entry.Quantity.Add(l.qty)
		if s.usdFull && l.usd != nil {
			sum, err := s.usd.Add(*l.usd)
			if err != nil {
				return nil, errs.Wrap(err, errs.CodeOverflow, "swap: usd valuation overflow")
			}
			s.usd = &sum
		} else {
			s.usdFull = false
			s.usd = nil
		}
		if s.entry.PriceRef == nil && l.priceRef != "" {
			pr := l.priceRef
			s.entry.PriceRef = &pr
		}
	}
	out := make([]Entry, 0, len(slots))
	for _, s := range slots {
		if s.usdFull && s.usd != nil {
			minor := s.usd.Minor()
			s.entry.USDValueMinor = &minor
		}
		out = append(out, s.entry)
	}
	return out, nil
}

// FundingInputs describes a settled deposit: Quantity units of AssetID were
// observed on chain and reconciled for AccountID.
type FundingInputs struct {
	AccountID     accounts.AccountID
	DepositID     string
	AssetID       assets.AssetID
	Quantity      money.Quantity
	USD           *money.USD
	EffectiveAt   time.Time
	CorrelationID string
}

func (in FundingInputs) validate() error {
	if in.AccountID.IsZero() {
		return errs.New(errs.CodeValidationFailed, "funding: account id is required")
	}
	if in.DepositID == "" {
		return errs.New(errs.CodeValidationFailed, "funding: deposit id is required")
	}
	if in.AssetID.IsZero() {
		return errs.New(errs.CodeValidationFailed, "funding: asset id is required")
	}
	if in.EffectiveAt.IsZero() {
		return errs.New(errs.CodeValidationFailed, "funding: effective_at is required")
	}
	return nil
}

// FundingSettledPosting builds "Dr WALLET / Cr CAPITAL" (kind
// FUNDING_SETTLED, reference {"deposit", DepositID}, idempotency key
// "deposit:<id>:settled").
func FundingSettledPosting(in FundingInputs) (Posting, error) {
	if err := in.validate(); err != nil {
		return Posting{}, err
	}
	if in.Quantity.Sign() <= 0 {
		return Posting{}, errs.New(errs.CodeValidationFailed, "funding: quantity must be positive")
	}
	var usd *int64
	if in.USD != nil {
		m := in.USD.Minor()
		usd = &m
	}
	p := Posting{
		Kind:           KindFundingSettled,
		IdempotencyKey: "deposit:" + in.DepositID + ":settled",
		Reference:      FinancialEventReference{Type: "deposit", ID: in.DepositID},
		EffectiveAt:    in.EffectiveAt.UTC(),
		Description:    "funding settled",
		CorrelationID:  in.CorrelationID,
		Entries: []Entry{
			{Account: CustomerAccount(in.AccountID, CodeWallet, in.AssetID), Side: Debit, Quantity: in.Quantity, USDValueMinor: usd},
			{Account: CustomerAccount(in.AccountID, CodeCapital, in.AssetID), Side: Credit, Quantity: in.Quantity, USDValueMinor: usd},
		},
	}
	if err := p.Validate(); err != nil {
		return Posting{}, err
	}
	return p, nil
}

// FundingReversalInputs describes a provider reversal (chargeback) of a
// deposit. USD, when set, values the whole reversed amount and is allocated
// exactly across the covered and deficit legs.
type FundingReversalInputs struct {
	AccountID     accounts.AccountID
	DepositID     string
	AssetID       assets.AssetID
	USD           *money.USD
	EffectiveAt   time.Time
	CorrelationID string
	Reason        string
}

// FundingReversalPostings builds the compensating transactions for a
// reversal of amount when the customer's WALLET currently holds
// walletBalance (§2.2). covered = min(walletBalance, amount) and
// shortfall = amount − covered:
//
//	T1 FUNDING_REVERSAL          Dr CAPITAL covered    Cr WALLET covered     (when covered > 0)
//	T2 FUNDING_REVERSAL_DEFICIT  Dr CAPITAL shortfall  Cr DEFICIT shortfall  (when shortfall > 0)
//
// Net effect: WALLET −covered (never below zero), CAPITAL −amount,
// DEFICIT +shortfall: the deficit is an explicit, positive liability of the
// customer, never a silently negative wallet. The funding service posts both
// inside one database transaction, freezes the account when T2 exists, and
// raises the negative_deficit_accounts alert. Idempotency keys are
// "deposit:<id>:reversal" and "deposit:<id>:reversal_deficit".
func FundingReversalPostings(in FundingReversalInputs, walletBalance, amount money.Quantity) ([]Posting, error) {
	base := FundingInputs{AccountID: in.AccountID, DepositID: in.DepositID, AssetID: in.AssetID, EffectiveAt: in.EffectiveAt}
	if err := base.validate(); err != nil {
		return nil, err
	}
	if amount.Sign() <= 0 {
		return nil, errs.New(errs.CodeValidationFailed, "funding reversal: amount must be positive")
	}
	if walletBalance.IsNegative() {
		return nil, errs.New(errs.CodeValidationFailed, "funding reversal: wallet balance cannot be negative")
	}
	covered := walletBalance.Min(amount)
	shortfall := amount.Sub(covered)

	coveredUSD, shortfallUSD, err := allocateUSD(in.USD, covered, amount)
	if err != nil {
		return nil, err
	}
	var metadata map[string]any
	if in.Reason != "" {
		metadata = map[string]any{"reason": in.Reason}
	}
	wallet := CustomerAccount(in.AccountID, CodeWallet, in.AssetID)
	capital := CustomerAccount(in.AccountID, CodeCapital, in.AssetID)
	deficit := CustomerAccount(in.AccountID, CodeDeficit, in.AssetID)
	ref := FinancialEventReference{Type: "deposit", ID: in.DepositID}

	var out []Posting
	if covered.IsPositive() {
		out = append(out, Posting{
			Kind:           KindFundingReversal,
			IdempotencyKey: "deposit:" + in.DepositID + ":reversal",
			Reference:      ref,
			EffectiveAt:    in.EffectiveAt.UTC(),
			Description:    "funding reversed",
			CorrelationID:  in.CorrelationID,
			Metadata:       metadata,
			Entries: []Entry{
				{Account: capital, Side: Debit, Quantity: covered, USDValueMinor: coveredUSD},
				{Account: wallet, Side: Credit, Quantity: covered, USDValueMinor: coveredUSD},
			},
		})
	}
	if shortfall.IsPositive() {
		out = append(out, Posting{
			Kind:           KindFundingReversalDeficit,
			IdempotencyKey: "deposit:" + in.DepositID + ":reversal_deficit",
			Reference:      ref,
			EffectiveAt:    in.EffectiveAt.UTC(),
			Description:    "funding reversal exceeded wallet balance; deficit recorded",
			CorrelationID:  in.CorrelationID,
			Metadata:       metadata,
			Entries: []Entry{
				{Account: capital, Side: Debit, Quantity: shortfall, USDValueMinor: shortfallUSD},
				{Account: deficit, Side: Credit, Quantity: shortfall, USDValueMinor: shortfallUSD},
			},
		})
	}
	for _, p := range out {
		if err := p.Validate(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// allocateUSD splits a USD valuation of amount exactly between the covered
// part (rounded half-even) and the remainder, so the two sum to the total.
func allocateUSD(total *money.USD, covered, amount money.Quantity) (coveredMinor, shortfallMinor *int64, err error) {
	if total == nil {
		return nil, nil, nil
	}
	totalQ := money.QuantityFromInt64(total.Minor())
	coveredQ, err := totalQ.MulDiv(covered, amount, money.RoundHalfEven)
	if err != nil {
		return nil, nil, errs.Wrap(err, errs.CodeInternal, "funding reversal: usd allocation")
	}
	c, err := coveredQ.Int64()
	if err != nil {
		return nil, nil, errs.Wrap(err, errs.CodeOverflow, "funding reversal: usd allocation overflow")
	}
	s := total.Minor() - c
	return &c, &s, nil
}
