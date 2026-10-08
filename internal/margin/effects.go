package margin

import (
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/ledger"
)

// EffectJournalShape is the journal one kind of margin economic effect
// posts: its journal kind and its reference type. Every margin posting is
// made under its effect's key (postOnce), so an APPLIED effect names exactly
// the journal its own key posted.
type EffectJournalShape struct {
	EffectKind    string
	JournalKind   ledger.Kind
	ReferenceType string
}

// Reference types of the margin postings (journal_transactions.reference_type).
const (
	refCollateralMove   = "margin_collateral_move"
	refBorrowLot        = "margin_borrow_lot"
	refRepayment        = "margin_repayment"
	refInterestAccrual  = "margin_interest_accrual"
	refSpotTrade        = "margin_spot_trade"
	refLiquidationStep  = "margin_liquidation_step"
	refLiquidationCase  = "margin_liquidation_case"
	refDeficitRepayment = "margin_deficit_repayment"
	refFacilityFunding  = "margin_facility_funding"
)

var effectJournalShapes = []EffectJournalShape{
	{"COLLATERAL_POST", ledger.KindMarginCollateralMove, refCollateralMove},
	{"COLLATERAL_RELEASE", ledger.KindMarginCollateralMove, refCollateralMove},
	{"BORROW", ledger.KindMarginBorrow, refBorrowLot},
	{"REPAY", ledger.KindMarginRepay, refRepayment},
	{"INTEREST", ledger.KindMarginInterest, refInterestAccrual},
	{"SPOT_BORROW", ledger.KindMarginBorrow, refBorrowLot},
	{"SPOT_TRADE", ledger.KindSimTrade, refSpotTrade},
	{"SPOT_COLLATERAL", ledger.KindMarginCollateralMove, refCollateralMove},
	{"LIQUIDATION", ledger.KindLiquidation, refLiquidationStep},
	{"LIQUIDATION_RELEASE", ledger.KindMarginCollateralMove, refLiquidationStep},
	{"LIQUIDATION_REPAY", ledger.KindMarginRepay, refLiquidationStep},
	{"LIQUIDATION_EXCESS", ledger.KindMarginCollateralMove, refLiquidationStep},
	{"LIQUIDATION_SWEEP", ledger.KindMarginRepay, refLiquidationStep},
	{"DEFICIT_COVERAGE", ledger.KindDeficitCoverage, refLiquidationCase},
	{"DEFICIT_REPAY", ledger.KindMarginRepay, refDeficitRepayment},
	{"FACILITY_FUNDING", ledger.KindFacilityFunding, refFacilityFunding},
}

// EffectJournalShapes returns every margin effect kind with the journal it
// posts. It is the Go twin of cp_margin_effect_journal_shape (migration
// 00754), which refuses at COMMIT an APPLIED margin effect whose journal is
// of another kind or reference type, was posted under another key, or moves
// another customer's books (MG008).
func EffectJournalShapes() []EffectJournalShape {
	return append([]EffectJournalShape(nil), effectJournalShapes...)
}

// checkEffectJournal refuses, before anything is written, a posting whose
// kind or reference type is not the one its effect kind posts: the database
// would refuse it at COMMIT, and a defect is cheaper found here.
func checkEffectJournal(effectKind string, p ledger.Posting) error {
	for _, s := range effectJournalShapes {
		if s.EffectKind != effectKind {
			continue
		}
		if s.JournalKind != p.Kind || s.ReferenceType != p.Reference.Type {
			return errs.Newf(errs.CodeInternal, "margin: effect %s posts %s/%s, not %s/%s",
				effectKind, s.JournalKind, s.ReferenceType, p.Kind, p.Reference.Type)
		}
		return nil
	}
	return errs.Newf(errs.CodeInternal, "margin: effect kind %s has no journal shape", effectKind)
}
