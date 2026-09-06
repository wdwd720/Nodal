package quote

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/idempotency"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/intent"
	"github.com/nodal/controlplane/internal/money"
)

type quoteKind struct{}

// QuoteID identifies a quotes row.
type QuoteID = id.ID[quoteKind]

// NewQuoteID returns a fresh quote id.
func NewQuoteID() QuoteID { return id.New[quoteKind]() }

// ParseQuoteID parses the canonical form.
func ParseQuoteID(s string) (QuoteID, error) { return id.Parse[quoteKind](s) }

// Side is the direction of the swap relative to the instrument's base asset.
type Side string

// Sides. BUY pays the quote asset and receives the base asset; SELL is the
// reverse.
const (
	SideBuy  Side = "BUY"
	SideSell Side = "SELL"
)

// Valid reports whether s is declared.
func (s Side) Valid() bool { return s == SideBuy || s == SideSell }

// Limits enforced by Validate.
const (
	MaxProviderLength = 128
	MaxRefLength      = 512
	HashLength        = sha256.Size
	MaxBPS            = money.OneHundredPercent
	MaxRouteBytes     = 1 << 20
)

// Quote mirrors one quotes row. Quantities are base units of the named
// asset; fee asset ids are nil when the fee is zero and unattributed.
type Quote struct {
	ID                QuoteID
	IntentID          *intent.IntentID
	Provider          string
	ProviderRequestID string
	InstrumentID      instruments.InstrumentID
	VenueListingID    instruments.ListingID
	Side              Side

	InputAssetID   assets.AssetID
	InputQuantity  money.Quantity
	OutputAssetID  assets.AssetID
	ExpectedOutput money.Quantity
	MinimumOutput  money.Quantity

	// EffectivePrice is quote-asset units per one base unit. Only Mantissa
	// and Scale are persisted; QuoteAsset is the pair's quote asset id
	// (QuoteAssetID), Source is Provider and At is ReceivedAt, and Validate
	// requires exactly that so a stored quote reloads losslessly.
	EffectivePrice money.Price
	PriceImpactBPS money.BPS
	SlippageBPS    money.BPS

	EstNetworkCost        money.Quantity
	EstNetworkCostAssetID *assets.AssetID
	EstVenueFee           money.Quantity
	EstVenueFeeAssetID    *assets.AssetID
	PlatformFee           money.Quantity
	PlatformFeeAssetID    *assets.AssetID
	PlatformFeeBPS        money.BPS
	FeePolicyVersion      string

	ReceivedAt time.Time
	ExpiresAt  time.Time

	RouteHash       []byte
	RouteSummary    json.RawMessage // JSON value; empty means []
	RawResponseRef  string
	RawResponseHash []byte
	CreatedAt       time.Time
}

// QuoteAssetID returns the asset the effective price is denominated in: the
// input asset of a BUY, the output asset of a SELL.
func (q Quote) QuoteAssetID() assets.AssetID {
	if q.Side == SideSell {
		return q.OutputAssetID
	}
	return q.InputAssetID
}

// EffectivePrice builds the money.Price a Quote must carry for the given
// mantissa and scale, consistent with Validate.
func (q Quote) effectivePrice(mantissa money.Quantity, scale int32) money.Price {
	return money.Price{Mantissa: mantissa, Scale: scale, QuoteAsset: q.QuoteAssetID().String(), Source: q.Provider, At: q.ReceivedAt.UTC()}
}

// NewEffectivePrice returns the money.Price for a quote of provider on the
// given side with the given input/output assets, received at receivedAt:
// value = mantissa × 10^-scale quote-asset units per base unit.
func NewEffectivePrice(mantissa money.Quantity, scale int32, side Side, inputAsset, outputAsset assets.AssetID, provider string, receivedAt time.Time) money.Price {
	q := Quote{Side: side, InputAssetID: inputAsset, OutputAssetID: outputAsset, Provider: provider, ReceivedAt: receivedAt}
	return q.effectivePrice(mantissa, scale)
}

// Validate checks q against the quotes table constraints and the package
// invariants. It returns nil or a VALIDATION_FAILED *errs.Error whose Fields
// name every offending field. It never panics.
func (q Quote) Validate() error {
	fields := map[string]any{}
	fail := func(k, msg string) {
		if _, dup := fields[k]; !dup {
			fields[k] = msg
		}
	}
	q.validateIdentity(fail)
	q.validateAmounts(fail)
	q.validateFees(fail)
	q.validateEvidence(fail)
	if len(fields) == 0 {
		return nil
	}
	return errs.New(errs.CodeValidationFailed, "quote: invalid").WithFields(fields)
}

func (q Quote) validateIdentity(fail func(k, msg string)) {
	if q.ID.IsZero() {
		fail("id", "required")
	}
	if q.IntentID != nil && q.IntentID.IsZero() {
		fail("intent_id", "must be set when present")
	}
	checkText(fail, "provider", q.Provider, MaxProviderLength, true)
	checkText(fail, "provider_request_id", q.ProviderRequestID, MaxRefLength, false)
	if q.InstrumentID.IsZero() {
		fail("instrument_id", "required")
	}
	if q.VenueListingID.IsZero() {
		fail("venue_listing_id", "required")
	}
	if !q.Side.Valid() {
		fail("side", "must be BUY or SELL")
	}
	switch {
	case q.InputAssetID.IsZero():
		fail("input_asset_id", "required")
	case q.OutputAssetID.IsZero():
		fail("output_asset_id", "required")
	case q.InputAssetID == q.OutputAssetID:
		fail("output_asset_id", "must differ from input_asset_id")
	}
	switch {
	case q.ReceivedAt.IsZero():
		fail("received_at", "required")
	case q.ExpiresAt.IsZero():
		fail("expires_at", "required")
	case !q.ExpiresAt.After(q.ReceivedAt):
		fail("expires_at", "must be after received_at")
	}
}

func (q Quote) validateAmounts(fail func(k, msg string)) {
	if !q.InputQuantity.IsPositive() {
		fail("input_quantity", "must be positive")
	}
	if q.ExpectedOutput.IsNegative() {
		fail("expected_output", "must not be negative")
	}
	switch {
	case q.MinimumOutput.IsNegative():
		fail("minimum_output", "must not be negative")
	case q.MinimumOutput.Cmp(q.ExpectedOutput) > 0:
		fail("minimum_output", "must not exceed expected_output")
	}
	switch {
	case q.EffectivePrice.Validate() != nil:
		fail("effective_price", "invalid price")
	case q.EffectivePrice.QuoteAsset != q.QuoteAssetID().String():
		fail("effective_price", "quote_asset must be the pair's quote asset id")
	case q.EffectivePrice.Source != q.Provider:
		fail("effective_price", "source must be the provider")
	case !q.EffectivePrice.At.Equal(q.ReceivedAt):
		fail("effective_price", "at must equal received_at")
	}
	if q.PriceImpactBPS < 0 {
		fail("price_impact_bps", "must not be negative")
	}
	if q.SlippageBPS < 0 || q.SlippageBPS > MaxBPS {
		fail("slippage_bps", "must be within 0..10000")
	}
}

func (q Quote) validateFees(fail func(k, msg string)) {
	for _, f := range []struct {
		name   string
		amount money.Quantity
		asset  *assets.AssetID
	}{
		{"est_network_cost", q.EstNetworkCost, q.EstNetworkCostAssetID},
		{"est_venue_fee", q.EstVenueFee, q.EstVenueFeeAssetID},
		{"platform_fee", q.PlatformFee, q.PlatformFeeAssetID},
	} {
		switch {
		case f.amount.IsNegative():
			fail(f.name, "must not be negative")
		case f.amount.IsPositive() && f.asset == nil:
			fail(f.name+"_asset_id", "required when the fee is positive")
		case f.asset != nil && f.asset.IsZero():
			fail(f.name+"_asset_id", "must be set when present")
		}
	}
	if q.PlatformFeeBPS < 0 || q.PlatformFeeBPS > MaxBPS {
		fail("platform_fee_bps", "must be within 0..10000")
	}
	if q.PlatformFeeBPS > 0 && q.FeePolicyVersion == "" {
		fail("fee_policy_version", "required when a platform fee applies")
	}
	checkText(fail, "fee_policy_version", q.FeePolicyVersion, MaxProviderLength, false)
}

func (q Quote) validateEvidence(fail func(k, msg string)) {
	if len(q.RawResponseHash) != HashLength {
		fail("raw_response_hash", "must be a sha256 digest")
	}
	checkText(fail, "raw_response_ref", q.RawResponseRef, MaxRefLength, false)
	summary, ok := normalizeRoute(q.RouteSummary)
	if !ok {
		fail("route_summary", "must be a JSON value")
		return
	}
	switch {
	case len(q.RouteHash) != HashLength:
		fail("route_hash", "must be a sha256 digest")
	case !bytes.Equal(q.RouteHash, RouteHash(summary)):
		fail("route_hash", "does not match route_summary")
	}
}

// IsExpired reports whether q is no longer valid at now: true at and after
// ExpiresAt, so a quote observed exactly at its expiry is already expired.
func (q Quote) IsExpired(now time.Time) bool { return !now.Before(q.ExpiresAt) }

// Age returns now − ReceivedAt (negative when now precedes receipt).
func (q Quote) Age(now time.Time) time.Duration { return now.Sub(q.ReceivedAt) }

// IsFresh reports whether q may still be relied on at now under a maximum
// age: maxAge must be positive, now must not precede ReceivedAt (a quote from
// the future is never fresh), the age must be at most maxAge (an age exactly
// equal to maxAge is still fresh), and the quote must not be expired.
func (q Quote) IsFresh(now time.Time, maxAge time.Duration) bool {
	if maxAge <= 0 || now.Before(q.ReceivedAt) {
		return false
	}
	return q.Age(now) <= maxAge && !q.IsExpired(now)
}

// FeeLine is one disclosed cost: an amount in base units of AssetID. AssetID
// is nil only when the amount is zero and unattributed.
type FeeLine struct {
	Amount  money.Quantity
	AssetID *assets.AssetID
}

// CostLine is a per-asset total of the disclosed fees.
type CostLine struct {
	AssetID assets.AssetID
	Amount  money.Quantity
}

// Disclosure is the customer-facing breakdown of a quote (PART 42, PART 126).
// ExpectedReceive and MinimumReceive are the venue's numbers verbatim;
// PlatformFee is a separate line and is never subtracted from them.
// TotalEstimatedCost sums venue fee, network estimate and platform fee per
// asset, sorted by asset id.
type Disclosure struct {
	QuoteID  QuoteID
	Provider string

	PayAssetID     assets.AssetID
	PayQuantity    money.Quantity
	ReceiveAssetID assets.AssetID

	ExpectedReceive money.Quantity
	MinimumReceive  money.Quantity
	EffectivePrice  money.Price
	PriceImpactBPS  money.BPS
	SlippageBPS     money.BPS

	VenueFee         FeeLine
	NetworkEstimate  FeeLine
	PlatformFee      FeeLine
	PlatformFeeBPS   money.BPS
	FeePolicyVersion string

	TotalEstimatedCost []CostLine
	Route              json.RawMessage
	ReceivedAt         time.Time
	ExpiresAt          time.Time
}

// Disclosure returns the breakdown for q. It performs no arithmetic on the
// venue's numbers beyond summing the fee lines per asset.
func (q Quote) Disclosure() Disclosure {
	d := Disclosure{
		QuoteID:          q.ID,
		Provider:         q.Provider,
		PayAssetID:       q.InputAssetID,
		PayQuantity:      q.InputQuantity,
		ReceiveAssetID:   q.OutputAssetID,
		ExpectedReceive:  q.ExpectedOutput,
		MinimumReceive:   q.MinimumOutput,
		EffectivePrice:   q.EffectivePrice,
		PriceImpactBPS:   q.PriceImpactBPS,
		SlippageBPS:      q.SlippageBPS,
		VenueFee:         FeeLine{Amount: q.EstVenueFee, AssetID: q.EstVenueFeeAssetID},
		NetworkEstimate:  FeeLine{Amount: q.EstNetworkCost, AssetID: q.EstNetworkCostAssetID},
		PlatformFee:      FeeLine{Amount: q.PlatformFee, AssetID: q.PlatformFeeAssetID},
		PlatformFeeBPS:   q.PlatformFeeBPS,
		FeePolicyVersion: q.FeePolicyVersion,
		ReceivedAt:       q.ReceivedAt,
		ExpiresAt:        q.ExpiresAt,
	}
	if summary, ok := normalizeRoute(q.RouteSummary); ok {
		d.Route = summary
	}
	totals := map[assets.AssetID]money.Quantity{}
	for _, line := range []FeeLine{d.VenueFee, d.NetworkEstimate, d.PlatformFee} {
		if line.AssetID == nil || !line.Amount.IsPositive() {
			continue
		}
		totals[*line.AssetID] = totals[*line.AssetID].Add(line.Amount)
	}
	for asset, amount := range totals {
		d.TotalEstimatedCost = append(d.TotalEstimatedCost, CostLine{AssetID: asset, Amount: amount})
	}
	sort.Slice(d.TotalEstimatedCost, func(i, j int) bool {
		return d.TotalEstimatedCost[i].AssetID.String() < d.TotalEstimatedCost[j].AssetID.String()
	})
	return d
}

// RouteHash returns sha256 of the canonical JSON form of routeSummary (object
// keys sorted recursively, no insignificant whitespace, numbers verbatim). An
// empty summary hashes as the empty array, matching the table default. The
// result depends only on the JSON value, never on the producer's key order or
// formatting.
func RouteHash(routeSummary json.RawMessage) []byte {
	summary, ok := normalizeRoute(routeSummary)
	if !ok {
		summary = bytes.TrimSpace(routeSummary)
	}
	sum := sha256.Sum256(idempotency.CanonicalJSON(summary))
	return sum[:]
}

// HashRaw returns sha256 of raw provider response bytes, for
// Quote.RawResponseHash.
func HashRaw(raw []byte) []byte {
	sum := sha256.Sum256(raw)
	return sum[:]
}

// normalizeRoute returns the summary to hash and store: "[]" when empty, the
// trimmed input when it is one valid JSON value, and ok = false otherwise.
func normalizeRoute(summary json.RawMessage) (json.RawMessage, bool) {
	trimmed := bytes.TrimSpace(summary)
	if len(trimmed) == 0 {
		return json.RawMessage("[]"), true
	}
	if len(trimmed) > MaxRouteBytes || !json.Valid(trimmed) {
		return nil, false
	}
	return trimmed, true
}

func checkText(fail func(k, msg string), name, v string, maxLen int, required bool) {
	switch {
	case v == "":
		if required {
			fail(name, "required")
		}
	case strings.TrimSpace(v) == "":
		fail(name, "must not be blank")
	case len(v) > maxLen:
		fail(name, "too long")
	case !isClean(v):
		fail(name, "must be valid utf-8 without control characters")
	}
}

func isClean(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}
