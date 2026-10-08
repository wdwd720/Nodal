package agent

import (
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/delegation"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/devnethost"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/strategy/ir"
)

// stageOf is the stage that runs mode.
func stageOf(m Mode) Stage {
	switch m {
	case ModeShadow:
		return StageShadow
	case ModeCanary:
		return StageCanary
	case ModeLimited:
		return StageLimited
	case ModeLive:
		return StageLive
	}
	return StageBacktestEligible
}

// propCapability is a hashed capability for agentID on account, granted at
// grantedAt and expiring at expiresAt, granting actions.
func propCapability(t *rapid.T, agentID string, account accounts.AccountID, actions []delegation.ActionClass,
	grantedAt, expiresAt time.Time) delegation.Capability {
	cid, err := delegation.ParseCapabilityID(id.New[id.Any]().String())
	if err != nil {
		t.Fatalf("capability id: %v", err)
	}
	c := delegation.Capability{
		ID: cid, GrantedAt: grantedAt, Nonce: strings.Repeat("cd", 32), IdempotencyKey: "grant-" + cid.String(),
		Status:    delegation.CapabilityActive,
		GrantedBy: delegation.Actor{Type: security.ActorUser, ID: id.New[id.Any]().String()},
		Terms: delegation.CapabilityTerms{
			AccountID: account, AgentID: agentID, StrategyID: id.New[id.Any]().String(), Environment: config.EnvTest,
			Mode: "SANDBOX", InstrumentClasses: []delegation.InstrumentClass{delegation.ClassStableValue},
			ActionClasses: actions, MaxNotional: money.USDFromMinor(1_000), MaxDailyNotional: money.USDFromMinor(1_000),
			MaxSingleTrade:       money.USDFromMinor(1_000),
			DestinationAllowlist: []devnet.PublicKey{devnet.NewKeypairFromSeed([32]byte{9}).PublicKey()},
			ChainAllowlist:       []devnet.ChainID{devnethost.ChainSolana}, ExpiresAt: expiresAt,
		},
	}
	c.Terms = c.Terms.Normalize()
	h, err := c.ComputeContentHash()
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	c.ContentHash = h
	return c
}

// TestProp_DelegatedToolsAreExactlyTheAgentsUsableGrants: for any mode,
// envelope, effect set, clock and mix of grants -- the agent's own and
// others', active and revoked, current, expired and future, intact and
// tampered -- the toolset is exactly one tool per granted kind under each
// usable capability of the agent's own on its account when the run is
// admitted to delegation at all, and nothing otherwise; it does not depend on
// the order the grants were read in; and an empty toolset always says why.
func TestProp_DelegatedToolsAreExactlyTheAgentsUsableGrants(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		mode := rapid.SampledFrom(Modes()).Draw(t, "mode")
		stage := stageOf(mode)
		envelope := ""
		if stage.RealCapital() || rapid.Bool().Draw(t, "envelope") {
			envelope = "env-1"
		}
		doc := testIR()
		declares := rapid.Bool().Draw(t, "declares_create_trade_intent")
		if !declares {
			doc.Effects = []ir.Effect{ir.EffectReadMarketData, ir.EffectCallModel, ir.EffectCommitPrediction}
		}
		a, err := NewAuthority(AuthorityInput{AgentID: NewAgentID(), AgentVersion: 1, AccountID: id.New[id.Any]().String(),
			StrategyVersionID: "sv", Stage: stage, Mode: mode, EnvelopeID: envelope, IR: doc})
		if err != nil {
			t.Fatalf("authority: %v", err)
		}
		admitted := mode == ModePaper && envelope == "" && declares
		own, err := accounts.ParseAccountID(a.AccountID())
		if err != nil {
			t.Fatalf("account: %v", err)
		}

		now := nowForTest()
		var caps []delegation.Capability
		want := map[string]bool{}
		for i, n := 0, rapid.IntRange(0, 6).Draw(t, "grants"); i < n; i++ {
			agentID, account, mine := a.AgentID().String(), own, true
			switch rapid.IntRange(0, 2).Draw(t, "owner") {
			case 1:
				agentID, mine = NewAgentID().String(), false
			case 2:
				other, perr := accounts.ParseAccountID(id.New[id.Any]().String())
				if perr != nil {
					t.Fatalf("account: %v", perr)
				}
				account, mine = other, false
			}
			var actions []delegation.ActionClass
			for _, ac := range []delegation.ActionClass{delegation.ActionAcquireExposure, delegation.ActionReduceExposure, delegation.ActionProposeWithdrawal} {
				if rapid.Bool().Draw(t, "grants_"+string(ac)) {
					actions = append(actions, ac)
				}
			}
			if len(actions) == 0 {
				actions = []delegation.ActionClass{delegation.ActionProposeWithdrawal}
			}
			grantedAt := now.Add(time.Duration(rapid.IntRange(-48, 2).Draw(t, "granted_h")) * time.Hour)
			expiresAt := grantedAt.Add(time.Duration(rapid.IntRange(1, 72).Draw(t, "life_h")) * time.Hour)
			c := propCapability(t, agentID, account, actions, grantedAt, expiresAt)
			revoked := rapid.IntRange(0, 3).Draw(t, "revoked") == 0
			if revoked {
				c.Status = delegation.CapabilityRevoked
			}
			tampered := rapid.IntRange(0, 5).Draw(t, "tampered") == 0
			if tampered {
				c.Terms.MaxSingleTrade = money.USDFromMinor(c.Terms.MaxSingleTrade.Minor() + 1)
			}
			usable := mine && !revoked && !tampered && !now.Before(grantedAt) && now.Before(expiresAt)
			if admitted && usable {
				for _, ac := range actions {
					if ac.IsSwap() {
						want[c.ID.String()+"|"+string(ActionRequestDelegatedSwap)] = true
					} else {
						want[c.ID.String()+"|"+string(ActionProposeDelegatedWithdrawal)] = true
					}
				}
			}
			caps = append(caps, c)
		}

		ts := DelegatedToolsFor(a, caps, now)
		got := map[string]bool{}
		for i, tool := range ts.Tools {
			key := tool.CapabilityID + "|" + string(tool.Kind)
			if got[key] {
				t.Fatalf("tool %s offered twice", key)
			}
			got[key] = true
			if i > 0 {
				prev := ts.Tools[i-1]
				if prev.CapabilityID > tool.CapabilityID || (prev.CapabilityID == tool.CapabilityID && prev.Kind >= tool.Kind) {
					t.Fatalf("tools out of order at %d", i)
				}
			}
		}
		if !reflect.DeepEqual(want, got) {
			t.Fatalf("offered %v, want %v (admitted=%t)", got, want, admitted)
		}
		if len(ts.Tools) == 0 && (ts.Reason == "" || ts.Detail == "") {
			t.Fatalf("an empty toolset does not say why")
		}
		if len(ts.Tools) > 0 && ts.Reason != "" {
			t.Fatalf("a toolset with tools carries a refusal reason %q", ts.Reason)
		}
		reversed := make([]delegation.Capability, len(caps))
		for i := range caps {
			reversed[len(caps)-1-i] = caps[i]
		}
		if !reflect.DeepEqual(ts, DelegatedToolsFor(a, reversed, now)) {
			t.Fatalf("the toolset depends on the order the grants were read in")
		}
	})
}

// TestProp_DelegatedNonceIsDeterministicPositiveAndInt64: for any run and
// action name the nonce is the same on every call, positive, within int64,
// and differs between two actions of one run.
func TestProp_DelegatedNonceIsDeterministicPositiveAndInt64(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		run := NewRunID()
		action := rapid.StringN(1, 64, -1).Draw(t, "action")
		other := rapid.StringN(1, 64, -1).Draw(t, "other")
		n := DelegatedNonce(run, action)
		if n != DelegatedNonce(run, action) {
			t.Fatalf("non-deterministic nonce")
		}
		if n == 0 || n > math.MaxInt64 {
			t.Fatalf("nonce %d out of range", n)
		}
		if other != action && n == DelegatedNonce(run, other) {
			t.Fatalf("two actions of one run share a nonce")
		}
	})
}

// TestProp_DelegatedRecordsSurviveTheRunsEvidence: whatever a run recorded,
// a later attempt reads back exactly the delegated requests, in order, and
// nothing that is not one.
func TestProp_DelegatedRecordsSurviveTheRunsEvidence(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		ev := runEvidence{}
		for i, n := 0, rapid.IntRange(0, 4).Draw(t, "checks"); i < n; i++ {
			ev.checks = append(ev.checks, FreshnessCheck{Dependency: rapid.StringN(1, 16, -1).Draw(t, "dep"), Present: rapid.Bool().Draw(t, "present")})
		}
		if rapid.Bool().Draw(t, "wired") {
			ev.tools = toolsRecord(DelegatedToolset{Reason: DelegatedReasonNoCapability, Detail: "none"}, nowForTest())
		}
		var want []DelegatedRecord
		for i, n := 0, rapid.IntRange(0, 4).Draw(t, "calls"); i < n; i++ {
			rec := DelegatedRecord{Kind: EvidenceKindDelegatedCall, Action: "a" + rapid.StringN(0, 16, -1).Draw(t, "action"),
				Tool: string(rapid.SampledFrom(DelegatedActions()).Draw(t, "tool")), CapabilityID: id.New[id.Any]().String(),
				RecordedAt: nowForTest().Add(time.Duration(i) * time.Second)}
			if rapid.Bool().Draw(t, "refused") {
				rec = rec.refusedBy(errs.New(errs.CodeRiskRejected, rapid.StringN(0, 32, -1).Draw(t, "detail")).WithField("reason", "RISK_REJECTED"))
			} else {
				rec.SubjectKind, rec.SubjectID, rec.Status = "SWAP", id.New[id.Any]().String(), "ADMITTED"
			}
			ev = ev.with(rec)
			want = append(want, rec)
		}
		got := delegatedRecordsOf(ev.render())
		if len(got) != len(want) {
			t.Fatalf("read back %d records, wrote %d", len(got), len(want))
		}
		for i := range want {
			if !reflect.DeepEqual(want[i], got[i]) {
				t.Fatalf("record %d: wrote %+v, read %+v", i, want[i], got[i])
			}
		}
	})
}
