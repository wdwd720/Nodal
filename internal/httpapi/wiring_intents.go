package httpapi

import (
	"context"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/intent"
	"github.com/nodal/controlplane/internal/settlement"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// Domain B and C reach the settlement compiler (STAGE 12, PART XXVI).
//
// wiring_compiler.go put every Domain A command through one place that decides
// value domain, legal rail, capability, verification, confirmation and
// executor. Trade intents did not go through it. `PostIntents` validated its
// body and called `intent.Submit`, and everything that decided whether the
// action was permitted lived further down: the eligibility engine, the risk
// evaluator, the execution planner, each sound on its own.
//
// "Sound on its own, in several places" is precisely what PART XXVI says is
// not enough. The compiler is now in front of all of it, so a Domain B or C
// intent is refused at the same place and in the same shape as a Domain A one,
// with the policy version, the rule index and every reason attached.
//
// It is an addition, not a substitution. The deeper checks all still run; a
// gate that exists only at the edge is one a worker walks around.

// intentActionType maps a trade intent onto the compiler's action vocabulary.
//
// Two questions decide it, and they are independent:
//
//  1. Is this real capital? That is the MODE, and nothing else. BACKTEST,
//     PAPER and SHADOW move simulated capital whatever the instrument is;
//     CANARY, LIMITED and LIVE move real capital. The submitter states the
//     mode and it is never inferred from the UI or the account (PART 159).
//  2. Which real rail? That is the instrument's base asset value domain, read
//     from the registry rather than guessed from a symbol or a chain name.
//
// The direction follows from the action. TARGET_EXPOSURE is the one that
// cannot be decided here: "make my exposure X" is a buy or a sell depending on
// a position this layer does not know. It is routed as a BUY, which is stated
// rather than hidden — and it is immaterial today because the buy and sell
// profiles of every external rail are identical in every field that affects a
// routing decision. settlement's TestProfiles_BuyAndSellAgreeOnEveryExternalRail fails if that
// stops being true, at which point this function must learn the position.
func intentActionType(mode intent.Mode, action intent.Action, base valuedomain.Domain) (settlement.ActionType, error) {
	switch mode {
	case intent.ModeBacktest, intent.ModePaper, intent.ModeShadow:
		return settlement.ActionSimulatedTrade, nil
	case intent.ModeCanary, intent.ModeLimited, intent.ModeLive:
	default:
		return "", errs.Newf(errs.CodeValidationFailed, "unknown mode %q", mode)
	}

	buy, err := intentIsBuy(action)
	if err != nil {
		return "", err
	}
	switch base {
	case valuedomain.SelfCustodialCrypto:
		if buy {
			return settlement.ActionBuyOnchainAsset, nil
		}
		return settlement.ActionSellOnchainAsset, nil
	case valuedomain.HostedCrypto, valuedomain.HostedFiat:
		if buy {
			return settlement.ActionBuyHostedAsset, nil
		}
		return settlement.ActionSellHostedAsset, nil
	case valuedomain.InternalNativeAsset:
		// A Nodal-native asset reached through the intent surface. It has a
		// route, and that route requires a NATIVE_MARKET subject rather than
		// an instrument, so it is refused here instead of being compiled into
		// an intent whose subject would be wrong. The native endpoints are
		// where this belongs.
		return "", errs.New(errs.CodeUnsupported,
			"a Nodal-native asset is traded through the native market endpoints, not the intent surface").
			WithField("value_domain", string(base))
	}
	return "", errs.Newf(errs.CodeUnsupported,
		"no settlement route exists for real capital in the %s value domain", base).
		WithField("value_domain", string(base))
}

// intentIsBuy reports whether an action increases exposure.
func intentIsBuy(action intent.Action) (bool, error) {
	switch action {
	case intent.ActionAcquireNotional, intent.ActionBuyEventOutcome:
		return true, nil
	case intent.ActionReduceNotional, intent.ActionClosePosition:
		return false, nil
	case intent.ActionTargetExposure:
		// See intentActionType: routed as a buy, deliberately and visibly.
		return true, nil
	}
	return false, errs.Newf(errs.CodeValidationFailed, "unknown action %q", action)
}

// intentBase reads the instrument's base asset: its id, which the legal router
// keys on, and its value domain, which decides the rail.
//
// It is resolved for EVERY mode, not only the real-capital ones. The first
// version of this skipped the lookup for simulated modes on the reasoning that
// a simulation is simulated whatever the instrument is -- and the conservative
// policy immediately refused every PAPER intent with SUBJECT_ASSET_NOT_STATED,
// because the policy is keyed by asset even for simulation. A simulation of a
// prohibited asset is still a question the policy is entitled to answer.
//
// A deployment with no instrument registry cannot say what an instrument is,
// and the answer to a question about money that cannot be answered is no.
func (s *Server) intentBase(ctx context.Context, instrumentID instruments.InstrumentID) (string, valuedomain.Domain, error) {
	if s.opts.Ports.Instruments == nil {
		return "", "", errs.New(errs.CodeUnsupported,
			"this deployment cannot determine what an instrument trades, so it cannot route an intent")
	}
	detail, err := s.opts.Ports.Instruments.Detail(ctx, instrumentID)
	if err != nil {
		return "", "", err
	}
	return detail.Base.ID.String(), detail.Base.ValueDomain, nil
}
