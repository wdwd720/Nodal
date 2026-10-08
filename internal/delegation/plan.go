package delegation

import (
	"crypto/sha256"
	"encoding/hex"
	"math/big"
	"strconv"
	"time"

	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// Plan versions and hash domains. A plan's hash is the MEMO of the
// transaction that executes it, binding every signature to exactly that plan.
const (
	swapPlanVersion          = "nodal-sandbox/delegated-swap-plan/v1"
	swapPlanHashDomain       = "nodal-sandbox/delegated-swap-plan/v1\x00"
	withdrawalPlanVersion    = "nodal-sandbox/delegated-withdrawal-plan/v1"
	withdrawalPlanHashDomain = "nodal-sandbox/delegated-withdrawal-plan/v1\x00"
)

// PlanPrice is a price a plan was computed with: whole quote units (the
// sandbox dollar) per whole unit of the asset, Mantissa x 10^-Scale. A par
// price is the registry's USD peg, not an observation.
type PlanPrice struct {
	Mantissa   string    `json:"mantissa"`
	Scale      string    `json:"scale"`
	Source     string    `json:"source"`
	ObservedAt time.Time `json:"observed_at"`
	Par        bool      `json:"par"`
}

func planPrice(p money.Price, par bool) PlanPrice {
	return PlanPrice{Mantissa: p.Mantissa.String(), Scale: strconv.FormatInt(int64(p.Scale), 10), Source: p.Source,
		ObservedAt: dbTime(p.At), Par: par}
}

// SwapPlan is the canonical settlement plan of one delegated swap: a
// delivery-versus-payment exchange in one transaction, the wallet paying
// AmountIn of the pay asset to the venue and the venue paying AmountOut of
// the receive asset to the wallet. Every number is a decimal string.
type SwapPlan struct {
	Version         string    `json:"version"`
	SwapID          string    `json:"swap_id"`
	AccountID       string    `json:"account_id"`
	AgentID         string    `json:"agent_id"`
	CapabilityID    string    `json:"capability_id"`
	CapabilityHash  string    `json:"capability_hash"`
	IntentID        string    `json:"intent_id"`
	RequestNonce    string    `json:"request_nonce"`
	ActionClass     string    `json:"action_class"`
	Mode            string    `json:"mode"`
	Chain           string    `json:"chain"`
	Wallet          string    `json:"wallet"`
	VenueID         string    `json:"venue_id"`
	VenueAddress    string    `json:"venue_address"`
	PayAssetID      string    `json:"pay_asset_id"`
	PayMint         string    `json:"pay_mint"`
	PayDecimals     string    `json:"pay_decimals"`
	ReceiveAssetID  string    `json:"receive_asset_id"`
	ReceiveMint     string    `json:"receive_mint"`
	ReceiveDecimals string    `json:"receive_decimals"`
	AmountIn        string    `json:"amount_in"`
	AmountOut       string    `json:"amount_out"`
	MinAmountOut    string    `json:"min_amount_out"`
	PayPrice        PlanPrice `json:"pay_price"`
	ReceivePrice    PlanPrice `json:"receive_price"`
	SpreadBPS       string    `json:"spread_bps"`
	MaxSlippageBPS  string    `json:"max_slippage_bps"`
	NotionalUSD     string    `json:"notional_usd"`
	FeePerTx        string    `json:"fee_per_tx"`
	RequestedAt     time.Time `json:"requested_at"`
	QuoteExpiresAt  time.Time `json:"quote_expires_at"`
}

// Hash returns the plan hash.
func (p SwapPlan) Hash() ([32]byte, error) { return planHash(swapPlanHashDomain, p) }

// swapParts is a plan with its keys and amounts parsed.
type swapParts struct {
	chain                  devnet.ChainID
	wallet, venue          devnet.PublicKey
	payMint, receiveMint   devnet.PublicKey
	amountIn, amountOut    uint64
	minAmountOut           uint64
	fee                    uint64
	slippage               money.BPS
	quoteExpiresAt         time.Time
	payDecimals, recvDecim uint8
}

func (p SwapPlan) parts() (swapParts, error) {
	var out swapParts
	var err error
	bad := func() (swapParts, error) {
		return swapParts{}, errs.New(errs.CodeValidationFailed, "delegation: the swap plan does not parse")
	}
	if p.Version != swapPlanVersion {
		return bad()
	}
	out.chain = devnet.ChainID(p.Chain)
	if out.chain.Validate() != nil {
		return bad()
	}
	if out.wallet, err = devnet.ParsePublicKey(p.Wallet); err != nil {
		return bad()
	}
	if out.venue, err = devnet.ParsePublicKey(p.VenueAddress); err != nil {
		return bad()
	}
	if out.payMint, err = devnet.ParsePublicKey(p.PayMint); err != nil {
		return bad()
	}
	if out.receiveMint, err = devnet.ParsePublicKey(p.ReceiveMint); err != nil {
		return bad()
	}
	if out.amountIn, err = strconv.ParseUint(p.AmountIn, 10, 64); err != nil || out.amountIn == 0 {
		return bad()
	}
	if out.amountOut, err = strconv.ParseUint(p.AmountOut, 10, 64); err != nil || out.amountOut == 0 {
		return bad()
	}
	if out.minAmountOut, err = strconv.ParseUint(p.MinAmountOut, 10, 64); err != nil || out.minAmountOut == 0 || out.minAmountOut > out.amountOut {
		return bad()
	}
	if out.fee, err = strconv.ParseUint(p.FeePerTx, 10, 64); err != nil {
		return bad()
	}
	slip, err := strconv.ParseInt(p.MaxSlippageBPS, 10, 64)
	if err != nil || slip < 0 || slip > 10_000 {
		return bad()
	}
	out.slippage = money.BPS(slip)
	if out.payDecimals, err = parseDecimals(p.PayDecimals); err != nil {
		return bad()
	}
	if out.recvDecim, err = parseDecimals(p.ReceiveDecimals); err != nil {
		return bad()
	}
	if out.wallet == out.venue || out.payMint == out.receiveMint {
		return bad()
	}
	out.quoteExpiresAt = p.QuoteExpiresAt.UTC()
	return out, nil
}

// Message builds the transaction the plan describes, referencing blockhash:
// [TOKEN_TRANSFER wallet -> venue (pay), TOKEN_TRANSFER venue -> wallet
// (receive), MEMO plan hash], paid by the wallet.
func (p SwapPlan) Message(blockhash devnet.Hash) (devnet.Message, error) {
	parts, err := p.parts()
	if err != nil {
		return devnet.Message{}, err
	}
	h, err := p.Hash()
	if err != nil {
		return devnet.Message{}, err
	}
	m := devnet.Message{
		ChainID: parts.chain, RecentBlockhash: blockhash, FeePayer: parts.wallet,
		Instructions: []devnet.Instruction{
			devnet.TokenTransfer{Mint: parts.payMint, FromOwner: parts.wallet, ToOwner: parts.venue, Amount: parts.amountIn},
			devnet.TokenTransfer{Mint: parts.receiveMint, FromOwner: parts.venue, ToOwner: parts.wallet, Amount: parts.amountOut},
			devnet.Memo{Data: memoOf(h)},
		},
	}
	if err := m.Validate(); err != nil {
		return devnet.Message{}, err
	}
	return m, nil
}

// WithdrawalPlan is the canonical plan of one delegated withdrawal: the
// wallet pays Amount of the asset to the approved destination. The approval
// binds its hash, which is why neither the destination nor the amount can
// change after approval.
type WithdrawalPlan struct {
	Version            string    `json:"version"`
	WithdrawalID       string    `json:"withdrawal_id"`
	AccountID          string    `json:"account_id"`
	AgentID            string    `json:"agent_id"`
	CapabilityID       string    `json:"capability_id"`
	CapabilityHash     string    `json:"capability_hash"`
	IntentID           string    `json:"intent_id"`
	ProposalNonce      string    `json:"proposal_nonce"`
	Mode               string    `json:"mode"`
	Chain              string    `json:"chain"`
	Wallet             string    `json:"wallet"`
	DestinationID      string    `json:"destination_id"`
	DestinationAddress string    `json:"destination_address"`
	AssetID            string    `json:"asset_id"`
	Mint               string    `json:"mint"`
	Decimals           string    `json:"decimals"`
	Amount             string    `json:"amount"`
	NotionalUSD        string    `json:"notional_usd"`
	Price              PlanPrice `json:"price"`
	PolicyID           string    `json:"policy_id"`
	PolicyVersion      string    `json:"policy_version"`
	PolicyHash         string    `json:"policy_hash"`
	EvaluationOutcome  string    `json:"evaluation_outcome"`
	RiskScore          string    `json:"risk_score"`
	FeePerTx           string    `json:"fee_per_tx"`
	ProposedAt         time.Time `json:"proposed_at"`
	ExecutableAt       time.Time `json:"executable_at"`
	ApprovalExpiresAt  time.Time `json:"approval_expires_at"`
}

// Hash returns the plan hash.
func (p WithdrawalPlan) Hash() ([32]byte, error) { return planHash(withdrawalPlanHashDomain, p) }

type withdrawalParts struct {
	chain               devnet.ChainID
	wallet, destination devnet.PublicKey
	mint                devnet.PublicKey
	amount              uint64
	fee                 uint64
	executableAt        time.Time
}

func (p WithdrawalPlan) parts() (withdrawalParts, error) {
	var out withdrawalParts
	var err error
	bad := func() (withdrawalParts, error) {
		return withdrawalParts{}, errs.New(errs.CodeValidationFailed, "delegation: the withdrawal plan does not parse")
	}
	if p.Version != withdrawalPlanVersion {
		return bad()
	}
	out.chain = devnet.ChainID(p.Chain)
	if out.chain.Validate() != nil {
		return bad()
	}
	if out.wallet, err = devnet.ParsePublicKey(p.Wallet); err != nil {
		return bad()
	}
	if out.destination, err = devnet.ParsePublicKey(p.DestinationAddress); err != nil {
		return bad()
	}
	if out.mint, err = devnet.ParsePublicKey(p.Mint); err != nil {
		return bad()
	}
	if out.amount, err = strconv.ParseUint(p.Amount, 10, 64); err != nil || out.amount == 0 {
		return bad()
	}
	if out.fee, err = strconv.ParseUint(p.FeePerTx, 10, 64); err != nil {
		return bad()
	}
	if out.wallet == out.destination {
		return bad()
	}
	out.executableAt = p.ExecutableAt.UTC()
	return out, nil
}

// Message builds [TOKEN_TRANSFER wallet -> destination, MEMO plan hash],
// paid by the wallet.
func (p WithdrawalPlan) Message(blockhash devnet.Hash) (devnet.Message, error) {
	parts, err := p.parts()
	if err != nil {
		return devnet.Message{}, err
	}
	h, err := p.Hash()
	if err != nil {
		return devnet.Message{}, err
	}
	m := devnet.Message{
		ChainID: parts.chain, RecentBlockhash: blockhash, FeePayer: parts.wallet,
		Instructions: []devnet.Instruction{
			devnet.TokenTransfer{Mint: parts.mint, FromOwner: parts.wallet, ToOwner: parts.destination, Amount: parts.amount},
			devnet.Memo{Data: memoOf(h)},
		},
	}
	if err := m.Validate(); err != nil {
		return devnet.Message{}, err
	}
	return m, nil
}

func planHash(domain string, plan any) ([32]byte, error) {
	b, err := audit.CanonicalJSON(plan)
	if err != nil {
		return [32]byte{}, errs.Wrap(err, errs.CodeInternal, "delegation: plan is not canonically encodable")
	}
	return sha256.Sum256(append([]byte(domain), b...)), nil
}

func memoOf(h [32]byte) []byte { return []byte(hex.EncodeToString(h[:])) }

func parseDecimals(s string) (uint8, error) {
	n, err := strconv.ParseUint(s, 10, 8)
	if err != nil || n > 18 {
		return 0, errs.New(errs.CodeValidationFailed, "delegation: decimals out of range")
	}
	return uint8(n), nil
}

// QuoteInput is what a venue quote is computed from: the exact amount the
// wallet pays, both assets' decimals and prices in the same quote unit, the
// venue's spread and the agent's slippage bound.
type QuoteInput struct {
	AmountIn        money.Quantity
	PayDecimals     uint8
	ReceiveDecimals uint8
	PayPrice        money.Price
	ReceivePrice    money.Price
	SpreadBPS       money.BPS
	MaxSlippageBPS  money.BPS
}

// Quote is a venue's executable answer.
type Quote struct {
	// MarkOut is the receive amount at the two prices with no spread,
	// rounded down.
	MarkOut money.Quantity
	// AmountOut is what the venue pays: the mark less its spread, rounded
	// down (a venue never pays a fraction it did not quote).
	AmountOut money.Quantity
	// MinAmountOut is the least the agent accepts: the mark less its
	// slippage bound, rounded up (the bound is never loosened by rounding).
	MinAmountOut money.Quantity
	// NotionalUSD is the value of AmountIn in cents, rounded up, so a limit
	// is never passed by rounding.
	NotionalUSD money.USD
}

// Quote refusal reasons.
const (
	ReasonSlippageExceeded = "DELEGATION_SLIPPAGE_EXCEEDED"
	ReasonAmountTooSmall   = "DELEGATION_AMOUNT_TOO_SMALL"
)

// ComputeQuote prices a swap exactly. With num = AmountIn x Mp x
// 10^(dr+Sr) and den = Mr x 10^(dp+Sp) (prices Mp x 10^-Sp and Mr x 10^-Sr
// in one quote unit per whole unit, dp and dr the decimals):
//
//	MarkOut      = floor(num / den)
//	AmountOut    = floor(num x (10000 - spread)   / (den x 10000))
//	MinAmountOut = ceil (num x (10000 - slippage) / (den x 10000))
//
// Each quantity is one exact rational rounded once. It refuses a quote whose
// AmountOut is below MinAmountOut (DELEGATION_SLIPPAGE_EXCEEDED) or rounds to
// nothing (DELEGATION_AMOUNT_TOO_SMALL).
func ComputeQuote(in QuoteInput) (Quote, error) {
	if !in.AmountIn.IsPositive() {
		return Quote{}, errs.New(errs.CodeValidationFailed, "delegation: the amount in must be positive")
	}
	if err := in.PayPrice.Validate(); err != nil || !in.PayPrice.Mantissa.IsPositive() {
		return Quote{}, errs.New(errs.CodeStaleMarketData, "delegation: the pay asset has no usable price")
	}
	if err := in.ReceivePrice.Validate(); err != nil || !in.ReceivePrice.Mantissa.IsPositive() {
		return Quote{}, errs.New(errs.CodeStaleMarketData, "delegation: the receive asset has no usable price")
	}
	if in.PayPrice.QuoteAsset != in.ReceivePrice.QuoteAsset {
		return Quote{}, errs.New(errs.CodeValidationFailed, "delegation: the two prices are not in the same quote unit")
	}
	if in.SpreadBPS < 0 || in.SpreadBPS > money.OneHundredPercent || in.MaxSlippageBPS < 0 || in.MaxSlippageBPS > money.OneHundredPercent {
		return Quote{}, errs.New(errs.CodeValidationFailed, "delegation: spread and slippage are 0..10000 basis points")
	}
	num := new(big.Int).Mul(in.AmountIn.BigInt(), in.PayPrice.Mantissa.BigInt())
	num.Mul(num, pow10(int(in.ReceiveDecimals)+int(in.ReceivePrice.Scale)))
	den := new(big.Int).Mul(in.ReceivePrice.Mantissa.BigInt(), pow10(int(in.PayDecimals)+int(in.PayPrice.Scale)))
	numQ, denQ := money.QuantityFromBigInt(num), money.QuantityFromBigInt(den)
	full := money.QuantityFromInt64(int64(money.OneHundredPercent))
	mark, err := numQ.Div(denQ, money.RoundDown)
	if err != nil {
		return Quote{}, errs.Wrap(err, errs.CodeInternal, "delegation: mark")
	}
	out, err := numQ.MulDiv(money.QuantityFromInt64(int64(money.OneHundredPercent-in.SpreadBPS)), denQ.Mul(full), money.RoundDown)
	if err != nil {
		return Quote{}, errs.Wrap(err, errs.CodeInternal, "delegation: amount out")
	}
	minOut, err := numQ.MulDiv(money.QuantityFromInt64(int64(money.OneHundredPercent-in.MaxSlippageBPS)), denQ.Mul(full), money.RoundUp)
	if err != nil {
		return Quote{}, errs.Wrap(err, errs.CodeInternal, "delegation: minimum amount out")
	}
	cents, err := money.Notional(in.AmountIn, in.PayDecimals, in.PayPrice, 2, money.RoundUp)
	if err != nil {
		return Quote{}, errs.Wrap(err, errs.CodeInternal, "delegation: notional")
	}
	minor, err := cents.Int64()
	if err != nil {
		return Quote{}, errs.New(errs.CodeOverflow, "delegation: the notional exceeds the USD range")
	}
	if !out.IsPositive() || !minOut.IsPositive() || minor <= 0 {
		return Quote{}, errs.New(errs.CodeValidationFailed, "delegation: the swap is too small to quote").WithField("reason", ReasonAmountTooSmall)
	}
	if out.Cmp(minOut) < 0 {
		return Quote{}, errs.New(errs.CodeNoValidPlan, "delegation: the venue's quote is outside the slippage bound").
			WithField("reason", ReasonSlippageExceeded).WithField("amount_out", out.String()).WithField("min_amount_out", minOut.String())
	}
	return Quote{MarkOut: mark, AmountOut: out, MinAmountOut: minOut, NotionalUSD: money.USDFromMinor(minor)}, nil
}

// ValueUSD values q base units of an asset with decimals at p in cents with
// the given rounding.
func ValueUSD(q money.Quantity, decimals uint8, p money.Price, mode money.RoundingMode) (money.USD, error) {
	cents, err := money.Notional(q, decimals, p, 2, mode)
	if err != nil {
		return money.USD{}, errs.Wrap(err, errs.CodeInternal, "delegation: value")
	}
	minor, err := cents.Int64()
	if err != nil {
		return money.USD{}, errs.New(errs.CodeOverflow, "delegation: the value exceeds the USD range")
	}
	return money.USDFromMinor(minor), nil
}

func pow10(n int) *big.Int {
	if n <= 0 {
		return big.NewInt(1)
	}
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
}
