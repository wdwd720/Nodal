//go:build integration

package transfers

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/settlement"
	"github.com/nodal/controlplane/internal/valuation"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// requireRefusedWithoutPosting checks a recorded refusal: REJECTED with the
// primary code, the intent REJECTED, evidence written, and nothing posted.
func (f *fixture) requireRefusedWithoutPosting(res Result, primary string) {
	f.t.Helper()
	tr := res.Transfer
	require.Equal(f.t, StatusRejected, tr.Status)
	require.Equal(f.t, primary, tr.RejectionCode)
	require.Contains(f.t, tr.ReasonCodes, primary)
	require.Nil(f.t, tr.JournalTransactionID)
	require.Nil(f.t, tr.EffectID)
	require.Equal(f.t, 0, f.journalsOfTransfer(tr.ID))
	require.Equal(f.t, 0, f.count(`SELECT count(*) FROM economic_effects WHERE effect_key = $1`, "p2p:"+tr.ID.String()))
	in, err := f.deps.Intents.Get(f.ctx, testDB, tr.IntentID)
	require.NoError(f.t, err)
	require.Equal(f.t, econ.StatusRejected, in.Status)
	require.Equal(f.t, 1, f.count(`SELECT count(*) FROM provider_evidence WHERE key = $1`, "advanced/p2p/"+tr.ID.String()+".json"))
}

// TestIntegration_ABlockedRecipientFailsClosed covers the per-sender
// blocklist, the allowlist-only mode, and CLEAR. The risk kernel receives the
// same answer as Destination.Allowlisted.
func TestIntegration_ABlockedRecipientFailsClosed(t *testing.T) {
	f := newFixture(t)
	alice, bob, carol := f.customer(), f.customer(), f.customer()
	f.fund(alice.account, 10_000_000)
	before := f.balance(alice.account, ledger.CodeWallet)

	setRule := func(rule RuleKind) {
		require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			return f.svc.SetRecipientRule(ctx, tx, alice.p, alice.account, bob.account, rule, "itest")
		}))
	}
	setRule(RuleBlock)
	res, err := f.send(alice.p, f.req(alice, bob, 1_000))
	requireCode(t, err, errs.CodeForbidden)
	f.requireRefusedWithoutPosting(res, RejectRecipientBlocked)
	assert.Contains(t, res.Transfer.ReasonCodes, risk.ReasonDestinationNotAllowed, "Risk V2 saw Destination.Allowlisted = false")
	assert.Equal(t, before, f.balance(alice.account, ledger.CodeWallet))

	// CLEAR withdraws the block.
	setRule(RuleClear)
	_, err = f.send(alice.p, f.req(alice, bob, 1_000))
	require.NoError(t, err)

	// Allowlist-only: carol is not allowlisted, bob becomes so.
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		return f.svc.SetSenderMode(ctx, tx, alice.p, alice.account, SenderAllowlistOnly)
	}))
	res, err = f.send(alice.p, f.req(alice, carol, 1_000))
	requireCode(t, err, errs.CodeForbidden)
	f.requireRefusedWithoutPosting(res, RejectRecipientNotAllowed)
	setRule(RuleAllow)
	_, err = f.send(alice.p, f.req(alice, bob, 1_000))
	require.NoError(t, err)

	// Only the sender's owner configures the sender's rules.
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		return f.svc.SetRecipientRule(ctx, tx, carol.p, alice.account, bob.account, RuleBlock, "")
	})
	requireCode(t, err, errs.CodeForbidden)
	f.requireReconciled()
}

// TestIntegration_AComplianceBlockFailsClosed: a screen BLOCK refuses with a
// stable code and posts nothing; a screen that cannot answer refuses too.
func TestIntegration_AComplianceBlockFailsClosed(t *testing.T) {
	f := newFixture(t)
	alice, bob, mallory := f.customer(), f.customer(), f.customer()
	f.fund(alice.account, 10_000_000)
	screen, err := NewListScreen("itest-sanctions-v1", map[string]string{mallory.account.String(): "SANCTIONS_LIST_MATCH"})
	require.NoError(t, err)
	f.rebuild(func(d *Deps) { d.Screen = screen })

	res, err := f.send(alice.p, f.req(alice, mallory, 1_000))
	requireCode(t, err, errs.CodeForbidden)
	f.requireRefusedWithoutPosting(res, RejectComplianceBlocked)
	assert.Contains(t, res.Transfer.ReasonCodes, "SANCTIONS_LIST_MATCH")
	assert.True(t, f.balance(mallory.account, ledger.CodeWallet).IsZero())
	assert.Equal(t, "10000000", f.balance(alice.account, ledger.CodeWallet).String())

	// The same screen clears everyone else.
	_, err = f.send(alice.p, f.req(alice, bob, 1_000))
	require.NoError(t, err)

	// An unreachable screen is not a pass.
	f.rebuild(func(d *Deps) { d.Screen = failingScreen{} })
	res, err = f.send(alice.p, f.req(alice, bob, 1_000))
	requireCode(t, err, errs.CodeProviderUnavailable)
	f.requireRefusedWithoutPosting(res, RejectComplianceUnavailable)
	f.requireReconciled()
}

type failingScreen struct{}

func (failingScreen) Screen(context.Context, ScreenRequest) (ScreenResult, error) {
	return ScreenResult{}, errors.New("screening provider timed out")
}

// TestIntegration_StepUpIsRequiredAboveTheThreshold: above the policy's
// threshold a transfer needs a strong authentication no older than the
// policy allows; nothing is written when it is missing.
func TestIntegration_StepUpIsRequiredAboveTheThreshold(t *testing.T) {
	f := newFixture(t)
	alice, bob := f.customer(), f.customer()
	f.fund(alice.account, 5_000_000_000) // 5,000 SBX
	threshold := f.svc.policy.StepUpAbove
	require.Equal(t, money.USDFromMinor(1_000_00), threshold)

	stale := alice.p
	stale.AuthTime = f.clk.Now().Add(-time.Hour)
	weak := alice.p
	weak.AMR = []string{"pwd"}
	big := f.req(alice, bob, 1_000_000_001) // $1,000.000001 -> $1,000.01 > threshold
	for name, p := range map[string]security.Principal{"stale": stale, "password only": weak} {
		_, err := f.send(p, big)
		requireCode(t, err, errs.CodeStepUpRequired)
		assert.Equal(t, 0, f.count(`SELECT count(*) FROM internal_transfers WHERE sender_account_id = $1`, alice.account), name)
	}
	// At the threshold no step-up is needed, even on a stale session.
	_, err := f.send(stale, f.req(alice, bob, 1_000_000_000))
	require.NoError(t, err)
	// Above it, a fresh MFA session passes.
	_, err = f.send(alice.p, big)
	require.NoError(t, err)
	f.requireReconciled()
}

// TestIntegration_VelocityAndLimitsFailClosed: the velocity count, the
// velocity notional, the daily limit and the per-transfer limit, each read
// from the transfer register, each refusing with its own code.
func TestIntegration_VelocityAndLimitsFailClosed(t *testing.T) {
	f := newFixture(t)
	p := DefaultPolicy()
	p.Version = "itest-limits"
	p.VelocityMaxCount = 3
	p.VelocityMaxNotional = money.USDFromMinor(500_00)
	p.MaxDaily = money.USDFromMinor(800_00)
	p.MaxPerTransfer = money.USDFromMinor(300_00)
	f.rebuild(func(d *Deps) { d.Policy = p })
	alice, bob := f.customer(), f.customer()
	f.fund(alice.account, 10_000_000_000)

	for range 3 {
		_, err := f.send(alice.p, f.req(alice, bob, 1_000_000)) // $1
		require.NoError(t, err)
	}
	res, err := f.send(alice.p, f.req(alice, bob, 1_000_000))
	requireCode(t, err, errs.CodeRiskRejected)
	f.requireRefusedWithoutPosting(res, RejectVelocityCount)

	// A new velocity window: the count resets, the notional limit binds.
	f.clk.Advance(time.Hour + time.Second)
	_, err = f.send(alice.p, f.req(alice, bob, 250_000_000)) // $250
	require.NoError(t, err)
	res, err = f.send(alice.p, f.req(alice, bob, 260_000_000)) // $510 in the hour
	requireCode(t, err, errs.CodeRiskRejected)
	f.requireRefusedWithoutPosting(res, RejectVelocityNotional)

	// Per transfer.
	res, err = f.send(alice.p, f.req(alice, bob, 300_000_001))
	requireCode(t, err, errs.CodeRiskRejected)
	f.requireRefusedWithoutPosting(res, RejectLimitPerTransfer)

	// Daily: $253 so far today; three more hours of $250 would pass velocity
	// but not the $800 day.
	f.clk.Advance(time.Hour + time.Second)
	_, err = f.send(alice.p, f.req(alice, bob, 250_000_000))
	require.NoError(t, err)
	f.clk.Advance(time.Hour + time.Second)
	res, err = f.send(alice.p, f.req(alice, bob, 298_000_000)) // 3 + 250 + 250 + 298 = 801
	requireCode(t, err, errs.CodeRiskRejected)
	f.requireRefusedWithoutPosting(res, RejectLimitDaily)
	// The rolling day moves on.
	f.clk.Advance(24 * time.Hour)
	_, err = f.send(alice.p, f.req(alice, bob, 298_000_000))
	require.NoError(t, err)
	f.requireReconciled()
}

// TestIntegration_AnAgentNeverTransfersAndTheCompilerAgrees: an agent bound
// to the sender account is refused before anything is written, with the
// settlement compiler's own reason.
func TestIntegration_AnAgentNeverTransfersAndTheCompilerAgrees(t *testing.T) {
	f := newFixture(t)
	alice, bob := f.customer(), f.customer()
	f.fund(alice.account, 10_000_000)
	agent := security.AgentPrincipal(uuid.NewString(), alice.account.String())

	_, err := f.send(agent, f.req(alice, bob, 1_000))
	requireCode(t, err, errs.CodeForbidden)
	assert.Equal(t, false, fieldOf(t, err, "route_permitted"))
	assert.Contains(t, fieldOf(t, err, "route_reasons"), settlement.ReasonAgentActionForbidden)
	assert.Equal(t, 0, f.count(`SELECT count(*) FROM internal_transfers WHERE sender_account_id = $1`, alice.account))
	assert.Equal(t, 0, f.count(`SELECT count(*) FROM economic_intents WHERE account_id = $1`, alice.account))

	// Nor does an agent propose, approve or reject a reversal, or configure
	// anything.
	_, err = f.propose(agent, NewTransferID(), "FRAUD_CONFIRMED", "x")
	requireCode(t, err, errs.CodeForbidden)
	_, err = f.approve(agent, NewReversalProposalID())
	requireCode(t, err, errs.CodeForbidden)
	_, err = f.reject(agent, NewReversalProposalID(), "x")
	requireCode(t, err, errs.CodeForbidden)
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		return f.svc.SetSenderMode(ctx, tx, agent, alice.account, SenderAllowlistOnly)
	})
	requireCode(t, err, errs.CodeForbidden)

	// An operator acting "as" the customer is no better.
	_, err = f.send(operator(), f.req(alice, bob, 1_000))
	requireCode(t, err, errs.CodeForbidden)
	// Nor a customer spending someone else's account.
	_, err = f.send(bob.p, f.req(alice, bob, 1_000))
	requireCode(t, err, errs.CodeForbidden)
}

// TestIntegration_RecipientResolution: by handle (case-insensitively), and
// the existence, ACTIVE and distinct-party checks, none of which writes.
func TestIntegration_RecipientResolution(t *testing.T) {
	f := newFixture(t)
	alice, bob, frozen := f.customer(), f.customer(), f.customer()
	f.fund(alice.account, 10_000_000)
	handle := "bob-" + uuid.NewString()[:8]
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.RegisterHandle(ctx, tx, bob.p, bob.account, handle)
		return err
	}))
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.RegisterHandle(ctx, tx, alice.p, alice.account, handle)
		return err
	})
	requireCode(t, err, errs.CodeConflict)

	req := f.req(alice, bob, 2_000)
	req.RecipientAccountID = accounts.AccountID{}
	req.RecipientHandle = "@" + strings.ToUpper(handle) // handles are case-insensitive
	res, err := f.send(alice.p, req)
	require.NoError(t, err)
	assert.Equal(t, bob.account, res.Transfer.RecipientAccountID)
	assert.Equal(t, handle, res.Transfer.RecipientHandle)

	unknown := f.req(alice, bob, 1)
	unknown.RecipientAccountID = accounts.AccountID{}
	unknown.RecipientHandle = "nobody-" + uuid.NewString()[:8]
	_, err = f.send(alice.p, unknown)
	requireCode(t, err, errs.CodeNotFound)

	self := f.req(alice, alice, 1)
	_, err = f.send(alice.p, self)
	requireCode(t, err, errs.CodeValidationFailed)

	ghost := f.req(alice, bob, 1)
	ghost.RecipientAccountID = accounts.NewAccountID()
	_, err = f.send(alice.p, ghost)
	requireCode(t, err, errs.CodeNotFound)

	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := accounts.NewRepository().Transition(ctx, tx, frozen.account, accounts.StatusChange{
			To: accounts.StatusFrozen, ActorType: "OPERATOR", ActorID: "itest", Reason: "itest freeze"}, f.clk.Now())
		return err
	}))
	_, err = f.send(alice.p, f.req(alice, frozen, 1))
	requireCode(t, err, errs.CodeAccountFrozen)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM internal_transfers WHERE sender_account_id = $1`, alice.account),
		"only the transfer that resolved was written")
}

// TestIntegration_OnlySimulatedValueMovesAndNeverLive: a non-simulated asset
// and the LIVE mode are refused before anything is written.
func TestIntegration_OnlySimulatedValueMovesAndNeverLive(t *testing.T) {
	f := newFixture(t)
	alice, bob := f.customer(), f.customer()
	f.fund(alice.account, 10_000_000)

	live := f.req(alice, bob, 1)
	live.Mode = gates.ModeLive
	_, err := f.send(alice.p, live)
	requireCode(t, err, errs.CodeCapabilityNotApproved)
	assert.Contains(t, fieldOf(t, err, "reasons"), gates.ReasonLiveNotImplemented)

	devnet := f.req(alice, bob, 1)
	devnet.Mode = gates.ModeDevnet
	_, err = f.send(alice.p, devnet)
	requireCode(t, err, errs.CodeCapabilityNotApproved)

	real, err := assets.NewRepository().Create(f.ctx, testDB, assets.Asset{
		Chain: "solana-itest-" + uuid.NewString(), MintAddress: uuid.NewString(), Kind: assets.KindSPLToken,
		ValueDomain: valuedomain.SelfCustodialCrypto, Symbol: "USDC", Name: "USD Coin", Decimals: 6,
		RiskClass: assets.RiskSettlement, Status: assets.StatusActive,
	})
	require.NoError(t, err)
	notSim := f.req(alice, bob, 1)
	notSim.AssetID = real.ID
	_, err = f.send(alice.p, notSim)
	requireCode(t, err, errs.CodeForbidden)
	assert.Equal(t, 0, f.count(`SELECT count(*) FROM internal_transfers WHERE sender_account_id = $1`, alice.account))

	// Paper mode works like sandbox mode.
	paper := f.req(alice, bob, 1)
	paper.Mode = gates.ModePaper
	res, err := f.send(alice.p, paper)
	require.NoError(t, err)
	assert.Equal(t, gates.ModePaper, res.Transfer.Mode)
}

// TestIntegration_KillSwitchesStopTransfers: CAPABILITY_DISABLE on
// P2P_TRANSFERS stops them at the authorizer; an ACCOUNT_FREEZE on the sender
// stops them in Risk V2, which records the decision.
func TestIntegration_KillSwitchesStopTransfers(t *testing.T) {
	f := newFixture(t)
	alice, bob := f.customer(), f.customer()
	f.fund(alice.account, 10_000_000)
	checker, err := gates.NewChecker("LOCAL", func(gates.Capability) bool { return true }, f.clk)
	require.NoError(t, err)

	disabled := fakeKills{killswitch.Switch{Kind: killswitch.CapabilityDisable, ScopeID: string(gates.P2PTransfers), Active: true}}
	f.rebuild(func(d *Deps) { d.Authorizer = econ.NewAuthorizer(checker, disabled) })
	_, err = f.send(alice.p, f.req(alice, bob, 1_000))
	requireCode(t, err, errs.CodeKillSwitchActive)

	frozen := fakeKills{killswitch.Switch{Kind: killswitch.AccountFreeze, ScopeID: alice.account.String(), Active: true}}
	f.rebuild(func(d *Deps) {
		d.Authorizer = econ.NewAuthorizer(checker, killswitch.NewChecker(killswitch.Policy{}))
		d.KillSwitches = frozen
	})
	res, err := f.send(alice.p, f.req(alice, bob, 1_000))
	requireCode(t, err, errs.CodeRiskRejected)
	f.requireRefusedWithoutPosting(res, risk.ReasonKillSwitch)
	require.NotNil(t, res.Transfer.RiskDecisionID)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM advanced_risk_decisions WHERE id = $1 AND verdict = 'REJECT'`, *res.Transfer.RiskDecisionID))
}

type fakeKills []killswitch.Switch

func (k fakeKills) Snapshot(context.Context, db.Querier) (killswitch.Snapshot, error) {
	return killswitch.Snapshot{Switches: append([]killswitch.Switch(nil), k...)}, nil
}

// TestIntegration_AnUnverifiedSenderIsRefusedByTheRiskKernel: identity is a
// required Risk V2 section and the default policy requires it verified.
func TestIntegration_AnUnverifiedSenderIsRefusedByTheRiskKernel(t *testing.T) {
	f := newFixture(t)
	alice, bob := f.customerWith(false), f.customer()
	f.fund(alice.account, 10_000_000)
	res, err := f.send(alice.p, f.req(alice, bob, 1_000))
	requireCode(t, err, errs.CodeRiskRejected)
	f.requireRefusedWithoutPosting(res, risk.ReasonIdentityUnverified)
}

// TestIntegration_AMarkValuedAssetFailsClosedWithoutAFreshMark: an asset that
// is not a par-valued simulated dollar needs a fresh mark; without one the
// transfer is refused before anything is written, never valued at zero.
func TestIntegration_AMarkValuedAssetFailsClosedWithoutAFreshMark(t *testing.T) {
	f := newFixture(t)
	gold := f.simAsset("G"+uuid.NewString()[:8], 9)
	f.rebuild(func(d *Deps) {
		d.Valuer = RegistryValuer{Prices: f.prices, Assets: f.svc.assets, QuoteAssetID: f.asset.ID, MaxMarkAge: time.Minute}
	})
	alice, bob := f.customer(), f.customer()
	f.fundAsset(alice.account, gold.ID, 5_000_000_000) // 5 GOLD
	req := f.req(alice, bob, 1_000_000_000)            // 1 GOLD
	req.AssetID = gold.ID

	_, err := f.send(alice.p, req)
	requireCode(t, err, errs.CodeStaleMarketData)
	assert.Equal(t, 0, f.count(`SELECT count(*) FROM internal_transfers WHERE sender_account_id = $1`, alice.account))

	// A mark two minutes old is still stale.
	_, err = f.prices.RecordPrice(f.ctx, testDB, valuation.PriceObservation{AssetID: gold.ID, QuoteAssetID: f.asset.ID,
		Mantissa: money.QuantityFromInt64(2_000_000_000), Scale: 6, Source: "itest", ObservedAt: f.clk.Now().Add(-2 * time.Minute)})
	require.NoError(t, err)
	_, err = f.send(alice.p, req)
	requireCode(t, err, errs.CodeStaleMarketData)

	// A fresh mark of 2000 SBX per GOLD values 1 GOLD at $2,000 (above the
	// step-up threshold, and the fixture's session is fresh MFA).
	_, err = f.prices.RecordPrice(f.ctx, testDB, valuation.PriceObservation{AssetID: gold.ID, QuoteAssetID: f.asset.ID,
		Mantissa: money.QuantityFromInt64(2_000_000_000), Scale: 6, Source: "itest", ObservedAt: f.clk.Now().Add(-time.Second)})
	require.NoError(t, err)
	res, err := f.send(alice.p, req)
	require.NoError(t, err)
	assert.Equal(t, money.USDFromMinor(2_000_00), res.Transfer.NotionalUSD)
	assert.Equal(t, "1000000000", f.balanceOf(bob.account, ledger.CodeWallet, gold.ID).String())
	rep, err := f.svc.Reconcile(f.ctx, testDB, gold.ID)
	require.NoError(t, err)
	assert.True(t, rep.OK(), "%+v", rep.Discrepancies)
}
