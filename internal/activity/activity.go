package activity

import (
	"sort"
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/errs"
)

// Kind is one class of thing that happens to an account.
//
// The set is closed and every member has a Source and a summary template. See
// doc.go for how a domain adds one.
type Kind string

// The kinds the feed carries today.
const (
	// KindCreditPurchase is a Credit purchase: real money in, Credits issued.
	KindCreditPurchase Kind = "CREDIT_PURCHASE"
	// KindCreditReversal is a purchase whose funding was taken back --
	// reversed, refunded or disputed. The status says which.
	KindCreditReversal Kind = "CREDIT_REVERSAL"
	// KindNativeTrade is a buy or a sell on an internal market.
	KindNativeTrade Kind = "NATIVE_TRADE"
	// KindNativeAssetCreated is the creation of an asset by this account.
	KindNativeAssetCreated Kind = "NATIVE_ASSET_CREATED"
	// KindPayoutRequested is the account asking for value to leave.
	KindPayoutRequested Kind = "PAYOUT_REQUESTED"
	// KindPayoutStateChanged is every subsequent move of that request.
	KindPayoutStateChanged Kind = "PAYOUT_STATE_CHANGED"
	// KindAdminAdjustment is Credits an operator issued or withdrew by hand.
	// It is its own kind rather than a purchase because its provenance is
	// ADMIN_ADJUSTMENT, which no payout policy in this build permits to leave.
	KindAdminAdjustment Kind = "ADMIN_ADJUSTMENT"
)

var allKinds = []Kind{
	KindCreditPurchase, KindCreditReversal, KindNativeTrade, KindNativeAssetCreated,
	KindPayoutRequested, KindPayoutStateChanged, KindAdminAdjustment,
}

// AllKinds returns every declared kind in declaration order (a copy).
func AllKinds() []Kind { return append([]Kind(nil), allKinds...) }

// Valid reports whether k is declared.
func (k Kind) Valid() bool {
	for _, x := range allKinds {
		if x == k {
			return true
		}
	}
	return false
}

func (k Kind) String() string { return string(k) }

// Temperature is what kind of value an amount is (§46, and the three
// temperatures the product surfaces render).
type Temperature string

// The temperatures.
const (
	// TemperatureEconomy is Nodal Credits: internal, closed-loop platform
	// value that is not a deposit and not redeemable by default.
	TemperatureEconomy Temperature = "ECONOMY"
	// TemperatureReal is money at a payment provider.
	TemperatureReal Temperature = "REAL"
	// TemperatureSimulated is value on a sandbox tier, or attached to an
	// object a demo seeder created. It moves nothing anywhere.
	TemperatureSimulated Temperature = "SIMULATED"
)

// Unit says how an amount's digits should be read.
type Unit string

// The units.
const (
	// UnitCredits is Credit BASE units. The Credit asset's decimals say where
	// the point goes; this package never moves it.
	UnitCredits Unit = "CREDITS"
	// UnitMoneyMinor is minor units of Currency (cents for USD).
	UnitMoneyMinor Unit = "MONEY_MINOR"
	// UnitAssetUnits is base units of a native asset. Its Symbol names which.
	UnitAssetUnits Unit = "ASSET_UNITS"
)

// Amount is one exact quantity on an activity item.
//
// Value is always a string of digits. Nothing in this package parses it into a
// number, and nothing downstream should either without knowing the unit: a
// Credit base unit and a cent are both integers and are not the same thing.
type Amount struct {
	Unit        Unit
	Value       string
	Currency    string
	Symbol      string
	Origin      string
	Temperature Temperature
}

// Reference points at the domain row this item came from.
type Reference struct {
	Type string
	ID   string
}

// Item is one entry of the timeline.
type Item struct {
	ID         string
	Kind       Kind
	OccurredAt time.Time
	// Summary is a sentence built here from a fixed template.
	Summary string
	// Status is the domain's own state where it has one, verbatim.
	Status    string
	Amounts   []Amount
	Reference Reference
	// Simulated marks an item about an object a demo seeder created, or any
	// item at all on a sandbox tier.
	Simulated bool
}

// Page is one cursor page of the feed.
type Page struct {
	Items      []Item
	NextCursor string
}

// MaxLimit bounds one page.
const MaxLimit = 100

// Request is one page of one account's timeline.
type Request struct {
	AccountID accounts.AccountID
	// Kinds filters the feed. Empty means every kind. An unknown kind is a
	// validation error rather than an empty page: a client that misspells a
	// filter should be told, not shown nothing.
	Kinds  []Kind
	Cursor string
	Limit  int
}

// Validate checks the request without touching the database.
func (r Request) Validate() error {
	if r.AccountID.IsZero() {
		return errs.New(errs.CodeValidationFailed, "an activity feed needs an account")
	}
	var unknown []string
	for _, k := range r.Kinds {
		if !k.Valid() {
			unknown = append(unknown, string(k))
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return errs.New(errs.CodeValidationFailed, "unknown activity kind").
			WithField("unknown_kinds", unknown).
			WithField("known_kinds", kindNames())
	}
	return nil
}

func kindNames() []string {
	out := make([]string, 0, len(allKinds))
	for _, k := range allKinds {
		out = append(out, string(k))
	}
	return out
}
