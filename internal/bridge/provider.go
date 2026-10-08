package bridge

import (
	"context"
	"strconv"
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/devnethost"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// BridgeQuote is what a route offers for an amount.
type BridgeQuote struct {
	RouteID            RouteID        `json:"route_id"`
	SourceChain        devnet.ChainID `json:"source_chain"`
	DestinationChain   devnet.ChainID `json:"destination_chain"`
	SourceAssetID      assets.AssetID `json:"source_asset_id"`
	DestinationAssetID assets.AssetID `json:"destination_asset_id"`
	AmountIn           money.Quantity `json:"amount_in"`
	AmountOut          money.Quantity `json:"amount_out"`
	// Fee is zero: the devnet bridge releases 1:1; network fees are paid in
	// native units by the holder (lock) and the attester (release).
	Fee money.Quantity `json:"fee"`
	// SourceFinality and DestinationFinality are the blocks each side needs
	// before the value moves on.
	SourceFinality      uint64    `json:"source_finality"`
	DestinationFinality uint64    `json:"destination_finality"`
	QuotedAt            time.Time `json:"quoted_at"`
	ExpiresAt           time.Time `json:"expires_at"`
}

// PlanKind is what a prepared transaction does.
type PlanKind string

// Plan kinds.
const (
	PlanLock    PlanKind = "LOCK"
	PlanRelease PlanKind = "RELEASE"
	PlanRefund  PlanKind = "REFUND"
)

// LegRef is what the provider needs to know about a leg.
type LegRef struct {
	LegID            LegID
	AccountID        accounts.AccountID
	Quantity         money.Quantity
	SourceOwner      devnet.PublicKey
	DestinationOwner devnet.PublicKey
	Nonce            uint64
	// LockTxID is the finalized lock, for RELEASE and REFUND.
	LockTxID devnet.TxID
}

// PlanRequest asks the provider to prepare one transaction.
type PlanRequest struct {
	Kind  PlanKind
	Route Route
	Leg   LegRef
	// Quote is the accepted quote (LOCK only).
	Quote BridgeQuote
}

// PreparedTx is a transaction ready to be recorded and then submitted once.
type PreparedTx struct {
	Kind            PlanKind
	Chain           devnet.ChainID
	RecentBlockhash devnet.Hash
	BlockhashHeight uint64
	// LOCK: the plan and the unsigned message the holder signs.
	Plan     *BridgePlan
	PlanHash [32]byte
	Lock     devnet.BridgeLock
	Message  devnet.Message
	// RELEASE and REFUND: signed by the provider's attester after
	// inspection against Expectation.
	Expectation *ReleaseExpectation
	TxID        devnet.TxID
	Raw         []byte
}

// Execution outcomes.
const (
	ExecSubmitted = "SUBMITTED"
	ExecUnknown   = "UNKNOWN"
	ExecRefused   = "REFUSED"
)

// BridgeExecution is how one submission ended.
type BridgeExecution struct {
	Chain   devnet.ChainID `json:"chain"`
	Outcome string         `json:"outcome"`
	Err     error          `json:"-"`
}

// BridgeObservation is what a chain says about one transaction, at one
// instant.
type BridgeObservation struct {
	Chain           devnet.ChainID  `json:"chain"`
	TxID            devnet.TxID     `json:"tx_id"`
	State           string          `json:"state"`
	Status          devnet.TxStatus `json:"-"`
	FinalizedHeight uint64          `json:"finalized_height"`
	// Expired: not held by the chain and its blockhash a full window below
	// finality, so it can never land, reorg or not.
	Expired bool `json:"expired"`
}

// Finalized reports success in a finalized block.
func (o BridgeObservation) Finalized() bool { return o.Status.State == devnet.TxFinalized }

// FailedFinal reports failure in a finalized block.
func (o BridgeObservation) FailedFinal() bool {
	return o.Status.State == devnet.TxFailed && o.Status.Final
}

// Landed reports that the transaction's effects are in the chain state.
func (o BridgeObservation) Landed() bool {
	return o.Status.State == devnet.TxConfirmed || o.Status.State == devnet.TxFinalized
}

// Dead reports that the transaction failed for good or can never land.
func (o BridgeObservation) Dead() bool { return o.FailedFinal() || o.Expired }

// Final reports that the outcome can no longer change.
func (o BridgeObservation) Final() bool { return o.Finalized() || o.Dead() }

// BridgeProvider is a bridge: it quotes a route, prepares the lock (for the
// holder to sign) and the release or refund (signed by its attester), submits
// a prepared transaction exactly once, and observes outcomes. Reads that run
// inside a database transaction take it as q (F-27).
type BridgeProvider interface {
	Quote(ctx context.Context, route Route, amount money.Quantity, now time.Time) (BridgeQuote, error)
	Plan(ctx context.Context, q db.Querier, req PlanRequest) (PreparedTx, error)
	Execute(ctx context.Context, chain devnet.ChainID, raw []byte) BridgeExecution
	Observe(ctx context.Context, q db.Querier, chain devnet.ChainID, txID devnet.TxID, blockhash devnet.Hash, bhHeight uint64) (BridgeObservation, error)
}

// DevnetProvider is the deterministic provider over the devnet host: locks
// are BRIDGE_LOCK on the source chain, releases BRIDGE_RELEASE on the
// destination (devnet.BuildRelease from a FINALIZED lock), single-use per
// lock and nonce on the chain itself.
type DevnetProvider struct {
	host     *devnethost.Host
	attester *Attester
	// QuoteTTL bounds how long a quote may be accepted.
	quoteTTL time.Duration
}

// NewDevnetProvider wires the provider. The attester must be the bridge
// authority the host's chains are configured with.
func NewDevnetProvider(host *devnethost.Host, attester *Attester) (*DevnetProvider, error) {
	if host == nil || attester == nil {
		return nil, errs.New(errs.CodeInternal, "bridge: the devnet provider needs a host and an attester")
	}
	for _, p := range host.Config().Chains {
		if p.BridgeAuthority != attester.PublicKey() {
			return nil, errs.New(errs.CodeValidationFailed, "bridge: the attester is not the bridge authority of chain "+string(p.ID))
		}
	}
	return &DevnetProvider{host: host, attester: attester, quoteTTL: time.Minute}, nil
}

// Quote implements BridgeProvider: a 1:1 release with no bridge fee.
func (p *DevnetProvider) Quote(_ context.Context, r Route, amount money.Quantity, now time.Time) (BridgeQuote, error) {
	if !amount.IsPositive() {
		return BridgeQuote{}, errs.New(errs.CodeValidationFailed, "bridge: a quote needs a positive amount")
	}
	src, ok := p.host.Config().Params(r.SourceChain)
	dst, ok2 := p.host.Config().Params(r.DestinationChain)
	if !ok || !ok2 {
		return BridgeQuote{}, errs.New(errs.CodeValidationFailed, "bridge: the host does not run the route's chains")
	}
	return BridgeQuote{
		RouteID: r.ID, SourceChain: r.SourceChain, DestinationChain: r.DestinationChain,
		SourceAssetID: r.SourceAssetID, DestinationAssetID: r.DestinationAssetID,
		AmountIn: amount, AmountOut: amount, Fee: money.Quantity{},
		SourceFinality: src.FinalityDepth, DestinationFinality: dst.FinalityDepth,
		QuotedAt: now.UTC(), ExpiresAt: now.UTC().Add(p.quoteTTL),
	}, nil
}

func (p *DevnetProvider) tip(ctx context.Context, q db.Querier, chain devnet.ChainID) (devnet.BlockInfo, error) {
	var tip devnet.BlockInfo
	err := p.host.ViewIn(ctx, q, func(v devnethost.View) error {
		var err error
		tip, err = v.Tip(chain)
		return err
	})
	return tip, err
}

// Plan implements BridgeProvider.
func (p *DevnetProvider) Plan(ctx context.Context, q db.Querier, req PlanRequest) (PreparedTx, error) {
	switch req.Kind {
	case PlanLock:
		return p.planLock(ctx, q, req)
	case PlanRelease:
		return p.planRelease(ctx, q, req)
	case PlanRefund:
		return p.planRefund(ctx, q, req)
	}
	return PreparedTx{}, errs.New(errs.CodeValidationFailed, "bridge: unknown plan kind")
}

func (p *DevnetProvider) planLock(ctx context.Context, q db.Querier, req PlanRequest) (PreparedTx, error) {
	r, leg := req.Route, req.Leg
	if req.Quote.RouteID != r.ID || !req.Quote.AmountIn.Equal(leg.Quantity) {
		return PreparedTx{}, errs.New(errs.CodeValidationFailed, "bridge: the quote does not describe this leg")
	}
	tip, err := p.tip(ctx, q, r.SourceChain)
	if err != nil {
		return PreparedTx{}, err
	}
	plan := BridgePlan{
		Version: planVersion, LegID: leg.LegID.String(), RouteID: r.ID.String(), AccountID: leg.AccountID.String(),
		SourceChain: string(r.SourceChain), DestinationChain: string(r.DestinationChain),
		SourceMint: r.SourceMint.String(), DestinationMint: r.DestinationMint.String(),
		SourceOwner: leg.SourceOwner.String(), DestinationOwner: leg.DestinationOwner.String(),
		Amount: leg.Quantity.String(), Nonce: strconv.FormatUint(leg.Nonce, 10),
		RecentBlockhash: tip.Hash.String(), BlockhashHeight: strconv.FormatUint(tip.Height, 10),
		QuoteExpiresAt: req.Quote.ExpiresAt.UTC(),
	}
	msg, lock, h, err := plan.LockMessage()
	if err != nil {
		return PreparedTx{}, err
	}
	return PreparedTx{Kind: PlanLock, Chain: r.SourceChain, RecentBlockhash: tip.Hash, BlockhashHeight: tip.Height,
		Plan: &plan, PlanHash: h, Lock: lock, Message: msg}, nil
}

// planRelease builds the destination release from the lock as the source
// chain holds it: devnet.BuildRelease refuses a lock that is not FINALIZED,
// and the lock must be exactly the leg's.
func (p *DevnetProvider) planRelease(ctx context.Context, q db.Querier, req PlanRequest) (PreparedTx, error) {
	r, leg := req.Route, req.Leg
	var lt devnet.LockedTransfer
	var tip devnet.BlockInfo
	err := p.host.ViewIn(ctx, q, func(v devnethost.View) error {
		var err error
		if lt, err = v.LockedTransfer(r.SourceChain, leg.LockTxID); err != nil {
			return err
		}
		tip, err = v.Tip(r.DestinationChain)
		return err
	})
	if err != nil {
		return PreparedTx{}, err
	}
	amount, err := devnet.AmountOf(leg.Quantity)
	if err != nil {
		return PreparedTx{}, err
	}
	want := devnet.BridgeLock{Mint: r.SourceMint, Owner: leg.SourceOwner, Amount: amount, DestChain: r.DestinationChain,
		DestOwner: leg.DestinationOwner, Nonce: leg.Nonce}
	if lt.Lock != want {
		return PreparedTx{}, errs.New(errs.CodeConflict, "bridge: the lock on chain is not the leg's lock").WithField("leg_id", leg.LegID.String())
	}
	msg, err := devnet.BuildRelease(lt, devnet.ReleaseParams{
		DestMint: r.DestinationMint, Authority: p.attester.PublicKey(), FeePayer: p.attester.PublicKey(), RecentBlockhash: tip.Hash,
	})
	if err != nil {
		return PreparedTx{}, err
	}
	exp := ReleaseExpectation{Chain: r.DestinationChain, Mint: r.DestinationMint, ToOwner: leg.DestinationOwner, Amount: amount,
		SourceChain: r.SourceChain, SourceTxID: leg.LockTxID, Nonce: leg.Nonce}
	id, raw, err := p.attester.SignRelease(exp, msg)
	if err != nil {
		return PreparedTx{}, err
	}
	return PreparedTx{Kind: PlanRelease, Chain: r.DestinationChain, RecentBlockhash: tip.Hash, BlockhashHeight: tip.Height,
		Expectation: &exp, TxID: id, Raw: raw, Message: msg}, nil
}

// planRefund builds the refund: a release on the SOURCE chain, keyed by the
// destination chain and the lock's tx id and nonce (so it is single-use too),
// that mints replacement units to the holder. The escrowed originals stay in
// the escrow address, which no key controls: retired, and counted as such by
// the route reconciliation.
func (p *DevnetProvider) planRefund(ctx context.Context, q db.Querier, req PlanRequest) (PreparedTx, error) {
	r, leg := req.Route, req.Leg
	tip, err := p.tip(ctx, q, r.SourceChain)
	if err != nil {
		return PreparedTx{}, err
	}
	amount, err := devnet.AmountOf(leg.Quantity)
	if err != nil {
		return PreparedTx{}, err
	}
	exp := ReleaseExpectation{Chain: r.SourceChain, Mint: r.SourceMint, ToOwner: leg.SourceOwner, Amount: amount,
		SourceChain: r.DestinationChain, SourceTxID: leg.LockTxID, Nonce: leg.Nonce}
	msg := devnet.Message{ChainID: r.SourceChain, RecentBlockhash: tip.Hash, FeePayer: p.attester.PublicKey(),
		Instructions: []devnet.Instruction{exp.release(p.attester.PublicKey())}}
	id, raw, err := p.attester.SignRelease(exp, msg)
	if err != nil {
		return PreparedTx{}, err
	}
	return PreparedTx{Kind: PlanRefund, Chain: r.SourceChain, RecentBlockhash: tip.Hash, BlockhashHeight: tip.Height,
		Expectation: &exp, TxID: id, Raw: raw, Message: msg}, nil
}

// definiteRefusals are rejections that prove the transaction was not
// accepted and never will be.
var definiteRefusals = map[devnet.ErrorCode]bool{
	devnet.CodeMalformedTransaction: true, devnet.CodeInvalidMessage: true, devnet.CodeInvalidChainID: true,
	devnet.CodeInvalidBase58: true, devnet.CodeWrongChain: true, devnet.CodeBlockhashNotFound: true,
	devnet.CodeSignatureMissing: true, devnet.CodeSignatureExtra: true, devnet.CodeSignatureInvalid: true,
}

// Execute implements BridgeProvider: one submission, classified. Anything
// that is not a definite acceptance or refusal is UNKNOWN.
func (p *DevnetProvider) Execute(ctx context.Context, chain devnet.ChainID, raw []byte) BridgeExecution {
	_, err := p.host.Submit(ctx, chain, raw)
	switch {
	case err == nil:
		return BridgeExecution{Chain: chain, Outcome: ExecSubmitted}
	case definiteRefusals[devnet.ReasonOf(err)]:
		return BridgeExecution{Chain: chain, Outcome: ExecRefused, Err: err}
	default:
		return BridgeExecution{Chain: chain, Outcome: ExecUnknown, Err: err}
	}
}

// Observe implements BridgeProvider.
func (p *DevnetProvider) Observe(ctx context.Context, q db.Querier, chain devnet.ChainID, txID devnet.TxID, _ devnet.Hash, bhHeight uint64) (BridgeObservation, error) {
	params, ok := p.host.Config().Params(chain)
	if !ok {
		return BridgeObservation{}, errs.New(errs.CodeValidationFailed, "bridge: the host does not run chain "+string(chain))
	}
	var obs BridgeObservation
	err := p.host.ViewIn(ctx, q, func(v devnethost.View) error {
		st, err := v.Status(chain, txID)
		if err != nil {
			return err
		}
		finalized, err := v.FinalizedHeight(chain)
		if err != nil {
			return err
		}
		obs = BridgeObservation{Chain: chain, TxID: txID, State: st.State.String(), Status: st, FinalizedHeight: finalized,
			Expired: st.State == devnet.TxNotFound && finalized >= bhHeight+params.BlockhashWindow}
		return nil
	})
	return obs, err
}
