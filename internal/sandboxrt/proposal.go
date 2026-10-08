package sandboxrt

import (
	"strconv"

	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/money"
	nodalsdk "github.com/nodal/controlplane/internal/sandboxrt/sdk"
)

// ProposableKinds are the only econ intent kinds a strategy may propose:
// taking and reducing exposure. TRANSFER_VALUE, WITHDRAW, BRIDGE_VALUE,
// BORROW, REPAY, MINT/BURN_STABLE_ASSET, liquidity provision, option exercise
// and prediction positions are absent on purpose, so no strategy output can
// ever become a request to move value out of an account.
func ProposableKinds() []econ.IntentKind {
	return []econ.IntentKind{econ.KindAcquireExposure, econ.KindClosePosition, econ.KindOpenPosition, econ.KindReduceExposure}
}

// Proposable reports whether a kind may be proposed by a strategy.
func Proposable(k econ.IntentKind) bool {
	for _, x := range ProposableKinds() {
		if x == k {
			return true
		}
	}
	return false
}

// Proposal is a validated intent proposal. It is still only a proposal: the
// manifest decides whether it may become an intent, and the risk kernel
// decides whether that intent may proceed.
type Proposal struct {
	Seq          int             `json:"seq"`
	Kind         econ.IntentKind `json:"kind"`
	InstrumentID string          `json:"instrument_id"`
	Side         Side            `json:"side"`
	Quantity     money.Quantity  `json:"quantity"`
	// LimitPrice is a canonical positive decimal, or empty.
	LimitPrice string `json:"limit_price"`
}

// ValidateProposal turns one structurally valid intent record into a
// proposal, or into the one violation that drops it. The snapshot is the
// input the strategy ran on: a proposal for an instrument the strategy was
// not shown is refused.
func ValidateProposal(rec IntentRecord, snap nodalsdk.Snapshot) (Proposal, *Violation) {
	drop := func(code ViolationCode, detail string) (Proposal, *Violation) {
		return Proposal{}, &Violation{Code: code, Seq: rec.Seq, Detail: detail}
	}
	kind := econ.IntentKind(rec.Kind)
	if !kind.Valid() || !Proposable(kind) {
		return drop(ViolationKindNotProposable, "kind "+strconv.Quote(truncate(rec.Kind, 64))+" cannot be proposed by a strategy")
	}
	if !nodalsdk.ValidID(rec.InstrumentID) {
		return drop(ViolationInstrumentInvalid, "instrument must be a canonical lowercase uuid")
	}
	found := false
	for _, ins := range snap.Instruments {
		if ins.ID == rec.InstrumentID {
			found = true
		}
	}
	if !found {
		return drop(ViolationInstrumentNotInSnapshot, "instrument "+rec.InstrumentID+" was not in the input")
	}
	side := Side(rec.Side)
	switch {
	case side != SideBuy && side != SideSell:
		return drop(ViolationSideInvalid, "side must be BUY or SELL")
	case kind == econ.KindAcquireExposure && side != SideBuy:
		return drop(ViolationSideInvalid, "ACQUIRE_EXPOSURE buys")
	case kind == econ.KindReduceExposure && side != SideSell:
		return drop(ViolationSideInvalid, "REDUCE_EXPOSURE sells")
	}
	units, err := nodalsdk.ParseUnits(rec.Quantity)
	if err != nil || units.Sign() <= 0 {
		return drop(ViolationQuantityInvalid, "quantity must be a positive integer of base units")
	}
	qty, err := money.ParseQuantity(units.String())
	if err != nil || !qty.IsPositive() {
		return drop(ViolationQuantityInvalid, "quantity out of range")
	}
	limit := ""
	if rec.LimitPrice != "" {
		d, err := nodalsdk.ParseDecimal(rec.LimitPrice)
		if err != nil || d.Sign() <= 0 {
			return drop(ViolationLimitPriceInvalid, "limit price must be a positive decimal")
		}
		limit = d.String()
	}
	return Proposal{Seq: rec.Seq, Kind: kind, InstrumentID: rec.InstrumentID, Side: side, Quantity: qty, LimitPrice: limit}, nil
}

// ValidateLog checks a log record's event and fields.
func ValidateLog(rec LogRecord) *Violation {
	if err := nodalsdk.CheckLog(rec.Event, rec.Fields); err != nil {
		return &Violation{Code: ViolationLogInvalid, Seq: rec.Seq, Detail: truncate(err.Error(), 200)}
	}
	return nil
}
