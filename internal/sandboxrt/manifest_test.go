package sandboxrt

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
	nodalsdk "github.com/nodal/controlplane/internal/sandboxrt/sdk"
	"github.com/nodal/controlplane/internal/security"
)

func testManifest() Manifest {
	return Manifest{
		AllowedIntentKinds:     []econ.IntentKind{econ.KindAcquireExposure, econ.KindReduceExposure},
		AllowedInstrumentIDs:   []string{insA},
		AllowedInstrumentKinds: []string{},
		MaxNotionalPerIntent:   "1000.00",
		MaxIntentsPerRun:       2,
		MinIntervalSeconds:     3600,
	}.Canonicalize()
}

func snapshotOf(t *testing.T, in Input) nodalsdk.Snapshot {
	t.Helper()
	snap, err := in.Snapshot()
	require.NoError(t, err)
	return snap
}

func TestValidateProposal_DropsEveryMalformedField(t *testing.T) {
	snap := snapshotOf(t, testInput())
	ok := IntentRecord{Seq: 1, Kind: "ACQUIRE_EXPOSURE", InstrumentID: insA, Side: "BUY", Quantity: "1000000"}
	p, v := ValidateProposal(ok, snap)
	require.Nil(t, v)
	assert.Equal(t, "1000000", p.Quantity.String())

	cases := map[string]struct {
		mutate func(*IntentRecord)
		code   ViolationCode
	}{
		"withdraw":            {func(r *IntentRecord) { r.Kind = "WITHDRAW" }, ViolationKindNotProposable},
		"transfer":            {func(r *IntentRecord) { r.Kind = "TRANSFER_VALUE" }, ViolationKindNotProposable},
		"bridge":              {func(r *IntentRecord) { r.Kind = "BRIDGE_VALUE" }, ViolationKindNotProposable},
		"borrow":              {func(r *IntentRecord) { r.Kind = "BORROW" }, ViolationKindNotProposable},
		"mint":                {func(r *IntentRecord) { r.Kind = "MINT_STABLE_ASSET" }, ViolationKindNotProposable},
		"made up":             {func(r *IntentRecord) { r.Kind = "SIGN_TRANSACTION" }, ViolationKindNotProposable},
		"instrument not uuid": {func(r *IntentRecord) { r.InstrumentID = "SOL-USDC" }, ViolationInstrumentInvalid},
		"instrument upper":    {func(r *IntentRecord) { r.InstrumentID = strings.ToUpper(insA) }, ViolationInstrumentInvalid},
		"not in snapshot":     {func(r *IntentRecord) { r.InstrumentID = "0193b2e0-0000-7000-8000-0000000000ff" }, ViolationInstrumentNotInSnapshot},
		"side":                {func(r *IntentRecord) { r.Side = "SHORT" }, ViolationSideInvalid},
		"acquire sells":       {func(r *IntentRecord) { r.Side = "SELL" }, ViolationSideInvalid},
		"zero quantity":       {func(r *IntentRecord) { r.Quantity = "0" }, ViolationQuantityInvalid},
		"negative quantity":   {func(r *IntentRecord) { r.Quantity = "-5" }, ViolationQuantityInvalid},
		"fractional quantity": {func(r *IntentRecord) { r.Quantity = "1.5" }, ViolationQuantityInvalid},
		"float quantity":      {func(r *IntentRecord) { r.Quantity = "1e6" }, ViolationQuantityInvalid},
		"limit not decimal":   {func(r *IntentRecord) { r.LimitPrice = "cheap" }, ViolationLimitPriceInvalid},
		"limit zero":          {func(r *IntentRecord) { r.LimitPrice = "0.00" }, ViolationLimitPriceInvalid},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			rec := ok
			c.mutate(&rec)
			_, v := ValidateProposal(rec, snap)
			require.NotNil(t, v)
			assert.Equal(t, c.code, v.Code)
			assert.Equal(t, 1, v.Seq)
			assert.False(t, v.Code.RunLevel())
		})
	}
	reduce := ok
	reduce.Kind, reduce.Side = "REDUCE_EXPOSURE", "BUY"
	_, v = ValidateProposal(reduce, snap)
	require.NotNil(t, v)
	assert.Equal(t, ViolationSideInvalid, v.Code)
}

func TestProposableKinds_NeverIncludeValueMovement(t *testing.T) {
	never := []econ.IntentKind{econ.KindTransferValue, econ.KindWithdraw, econ.KindBridgeValue, econ.KindBorrow, econ.KindRepay,
		econ.KindMintStableAsset, econ.KindBurnStableAsset}
	for _, k := range never {
		assert.False(t, Proposable(k), "%s must never be proposable", k)
	}
	for _, k := range ProposableKinds() {
		assert.True(t, k.Valid())
	}
	sdkKinds := nodalsdk.ProposableKinds()
	require.Len(t, sdkKinds, len(ProposableKinds()))
	for i, k := range ProposableKinds() {
		assert.Equal(t, string(k), sdkKinds[i], "the SDK and the host agree on what can be proposed")
	}
}

func TestManifest_ValidateRefusesWhatCanNeverBeGranted(t *testing.T) {
	require.NoError(t, testManifest().Validate())
	cases := map[string]func(*Manifest){
		"withdraw kind":         func(m *Manifest) { m.AllowedIntentKinds = []econ.IntentKind{econ.KindWithdraw} },
		"transfer kind":         func(m *Manifest) { m.AllowedIntentKinds = []econ.IntentKind{econ.KindTransferValue} },
		"no kinds":              func(m *Manifest) { m.AllowedIntentKinds = []econ.IntentKind{} },
		"no instruments":        func(m *Manifest) { m.AllowedInstrumentIDs = []string{} },
		"bad instrument":        func(m *Manifest) { m.AllowedInstrumentIDs = []string{"SOL"} },
		"unknown kind":          func(m *Manifest) { m.AllowedInstrumentKinds = []string{"CRYPTO_ANYTHING"} },
		"zero notional":         func(m *Manifest) { m.MaxNotionalPerIntent = "0" },
		"non canonical decimal": func(m *Manifest) { m.MaxNotionalPerIntent = "01000" },
		"too many intents":      func(m *Manifest) { m.MaxIntentsPerRun = MaxIntentsPerRunBound + 1 },
		"too frequent":          func(m *Manifest) { m.MinIntervalSeconds = 1 },
		"unsorted": func(m *Manifest) {
			m.AllowedIntentKinds = []econ.IntentKind{econ.KindReduceExposure, econ.KindAcquireExposure}
		},
		"wrong schema": func(m *Manifest) { m.Schema = "v0" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			m := testManifest()
			mutate(&m)
			assert.Error(t, m.Validate())
		})
	}
}

func TestManifest_CanonicalBytesAreTheHashedBytes(t *testing.T) {
	m := testManifest()
	b, err := m.CanonicalBytes()
	require.NoError(t, err)
	h, err := m.Hash()
	require.NoError(t, err)
	assert.Len(t, h, 32)
	back, err := DecodeManifest(b)
	require.NoError(t, err)
	assert.Equal(t, m, back)

	// A nil list and an empty list are one manifest.
	nilLists := m
	nilLists.AllowedInstrumentKinds = nil
	b2, err := nilLists.CanonicalBytes()
	require.NoError(t, err)
	assert.Equal(t, b, b2)

	// Bytes that decode to the same manifest but are not canonical are refused.
	var generic map[string]any
	require.NoError(t, json.Unmarshal(b, &generic))
	pretty, err := json.MarshalIndent(generic, "", "  ")
	require.NoError(t, err)
	_, err = DecodeManifest(pretty)
	assert.Error(t, err)
	_, err = DecodeManifest([]byte(strings.Replace(string(b), `"schema"`, `"extra":"x","schema"`, 1)))
	assert.Error(t, err, "unknown fields are refused")
}

func TestManifest_ProposalsBeyondManifestDroppedAndCounted(t *testing.T) {
	snap := snapshotOf(t, testInput())
	q := func(s string) money.Quantity {
		v, err := money.ParseQuantity(s)
		require.NoError(t, err)
		return v
	}
	proposals := []Proposal{
		{Seq: 1, Kind: econ.KindAcquireExposure, InstrumentID: insA, Side: SideBuy, Quantity: q("1000000")},                       // 150.25: ok
		{Seq: 2, Kind: econ.KindOpenPosition, InstrumentID: insA, Side: SideBuy, Quantity: q("1")},                                // kind not allowed
		{Seq: 3, Kind: econ.KindAcquireExposure, InstrumentID: insB, Side: SideBuy, Quantity: q("1")},                             // instrument not allowed
		{Seq: 4, Kind: econ.KindAcquireExposure, InstrumentID: insA, Side: SideBuy, Quantity: q("7000000")},                       // 1051.75 > 1000
		{Seq: 5, Kind: econ.KindAcquireExposure, InstrumentID: insA, Side: SideBuy, Quantity: q("7000000"), LimitPrice: "100.00"}, // 700.00 at limit: ok
		{Seq: 6, Kind: econ.KindReduceExposure, InstrumentID: insA, Side: SideSell, Quantity: q("1")},                             // third accepted: over max
	}
	accepted, violations := testManifest().Filter(proposals, snap)
	require.Len(t, accepted, 2)
	assert.Equal(t, 1, accepted[0].Seq)
	assert.Equal(t, "150.25000000", accepted[0].Notional)
	assert.Equal(t, PriceMark, accepted[0].PriceSource)
	assert.Equal(t, 5, accepted[1].Seq)
	assert.Equal(t, "700.00000000", accepted[1].Notional)
	assert.Equal(t, PriceLimit, accepted[1].PriceSource)
	require.Len(t, violations, 4)
	assert.Equal(t, []ViolationCode{ViolationManifestKind, ViolationManifestInstrument, ViolationManifestNotional, ViolationManifestMaxIntents},
		codesOf(violations))
	assert.Equal(t, []int{2, 3, 4, 6}, []int{violations[0].Seq, violations[1].Seq, violations[2].Seq, violations[3].Seq})

	// An instrument allowed by kind.
	byKind := testManifest()
	byKind.AllowedInstrumentKinds = []string{"EQUITY"}
	accepted, violations = byKind.Filter(proposals[2:3], snap)
	assert.Len(t, accepted, 1)
	assert.Empty(t, violations)
}

// TestProp_ManifestFilterNeverAcceptsBeyondTheManifest: for arbitrary
// manifests and proposals, every accepted proposal is of an allowed kind, on
// an allowed instrument, within the notional cap, at most MaxIntentsPerRun
// are accepted, and every proposal is either accepted or dropped with
// exactly one violation.
func TestProp_ManifestFilterNeverAcceptsBeyondTheManifest(t *testing.T) {
	snap := snapshotOf(t, testInput())
	rapid.Check(t, func(rt *rapid.T) {
		kinds := rapid.SliceOfNDistinct(rapid.SampledFrom(ProposableKinds()), 1, 4, func(k econ.IntentKind) econ.IntentKind { return k }).Draw(rt, "kinds")
		ids := rapid.SliceOfDistinct(rapid.SampledFrom([]string{insA, insB}), func(s string) string { return s }).Draw(rt, "ids")
		insKinds := rapid.SliceOfDistinct(rapid.SampledFrom([]string{"SPOT_PAIR", "EQUITY", "FUTURE"}), func(s string) string { return s }).Draw(rt, "insKinds")
		if len(ids) == 0 && len(insKinds) == 0 {
			ids = []string{insA}
		}
		m := Manifest{
			AllowedIntentKinds: kinds, AllowedInstrumentIDs: ids, AllowedInstrumentKinds: insKinds,
			MaxNotionalPerIntent: strconv.Itoa(rapid.IntRange(1, 5000).Draw(rt, "cap")) + ".00",
			MaxIntentsPerRun:     rapid.IntRange(1, 5).Draw(rt, "max"), MinIntervalSeconds: 60,
		}.Canonicalize()
		require.NoError(rt, m.Validate())
		n := rapid.IntRange(0, 12).Draw(rt, "n")
		var proposals []Proposal
		for i := 0; i < n; i++ {
			k := rapid.SampledFrom(ProposableKinds()).Draw(rt, "kind")
			side := SideBuy
			if k == econ.KindReduceExposure {
				side = SideSell
			}
			p := Proposal{Seq: i + 1, Kind: k, InstrumentID: rapid.SampledFrom([]string{insA, insB}).Draw(rt, "ins"), Side: side,
				Quantity: money.QuantityFromInt64(rapid.Int64Range(1, 20_000_000).Draw(rt, "qty"))}
			if rapid.Bool().Draw(rt, "limit") {
				p.LimitPrice = strconv.Itoa(rapid.IntRange(1, 900).Draw(rt, "lp")) + ".5"
			}
			proposals = append(proposals, p)
		}
		accepted, violations := m.Filter(proposals, snap)
		if len(accepted)+len(violations) != len(proposals) {
			rt.Fatalf("%d proposals became %d accepted + %d dropped", len(proposals), len(accepted), len(violations))
		}
		if len(accepted) > m.MaxIntentsPerRun {
			rt.Fatalf("accepted %d > max %d", len(accepted), m.MaxIntentsPerRun)
		}
		capDec, _ := nodalsdk.ParseDecimal(m.MaxNotionalPerIntent)
		for _, a := range accepted {
			if !m.allowsKind(a.Kind) {
				rt.Fatalf("accepted a kind the manifest does not allow: %s", a.Kind)
			}
			if !m.allowsInstrument(a.InstrumentID, a.InstrumentKind) {
				rt.Fatalf("accepted an instrument the manifest does not allow: %s", a.InstrumentID)
			}
			n, err := nodalsdk.ParseDecimal(a.Notional)
			if err != nil || n.Cmp(capDec) > 0 {
				rt.Fatalf("accepted notional %s above cap %s", a.Notional, m.MaxNotionalPerIntent)
			}
		}
	})
}

func testIntentContext() IntentContext {
	return IntentContext{
		AccountID: accounts.AccountID(econ.NewIntentID()), Mode: gates.ModePaper, Capability: gates.StrategyMarketplace,
		ActorID: "marketplace.follow", StrategyRef: "0193b2e0-0000-7000-8000-000000000123@3", RunID: NewRunID(),
		RunDigest: make([]byte, 32), ArtifactDigest: make([]byte, 32), KeyPrefix: "mkt-follow:test",
	}
}

func TestIntent_AProposalBecomesAServiceIntentThatRecordsItsOrigin(t *testing.T) {
	snap := snapshotOf(t, testInput())
	accepted, _ := testManifest().Filter([]Proposal{{Seq: 7, Kind: econ.KindAcquireExposure, InstrumentID: insA, Side: SideBuy,
		Quantity: money.QuantityFromInt64(1_000_000), LimitPrice: "150.30"}}, snap)
	require.Len(t, accepted, 1)
	c := testIntentContext()
	in, err := accepted[0].Intent(c)
	require.NoError(t, err)
	assert.Equal(t, security.ActorService, in.ActorType, "a strategy is never the actor")
	assert.Empty(t, in.AgentID)
	assert.Equal(t, c.StrategyRef, in.StrategyRef)
	assert.Equal(t, gates.ModePaper, in.Mode)
	assert.Equal(t, "mkt-follow:test:7", in.IdempotencyKey)
	assert.Nil(t, in.NotionalUSD, "the quantity is the only amount")
	var terms IntentTerms
	require.NoError(t, json.Unmarshal(in.Terms, &terms))
	assert.Equal(t, OriginSandbox, terms.Origin)
	assert.Equal(t, c.RunID.String(), terms.SandboxRunID)
	assert.Equal(t, 7, terms.ProposalSeq)
	assert.Equal(t, "150.30000000", terms.Notional)

	ri, err := RiskIntent(in, accepted[0])
	require.NoError(t, err)
	assert.True(t, ri.SandboxOriginated)
	assert.Equal(t, money.USDFromMinor(15030), ri.NotionalUSD)
	required := risk.RequiredSections(risk.AdvancedInput{Intent: ri})
	assert.Contains(t, required, risk.SecSandbox)
	assert.Contains(t, required, risk.SecStrategy)

	// Without the sandbox section the kernel rejects; with violations it rejects.
	base := risk.AdvancedInput{Stage: risk.StagePreTrade, Intent: ri, Now: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC),
		Account: &risk.AdvancedAccountSnapshot{Status: "ACTIVE"}, Identity: &risk.IdentitySnapshot{Verified: true},
		Instrument: &risk.InstrumentSnapshot{Status: "ACTIVE", SessionOpen: true},
		Oracle:     &risk.OracleSnapshot{Available: true, ObservedAt: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)},
		Strategy:   &risk.StrategySnapshot{Approved: true, ManifestPermits: true}}
	d := risk.EvaluateAdvanced(risk.DefaultAdvancedPolicy(), base)
	assert.Equal(t, risk.Reject, d.Verdict)
	assert.Contains(t, d.MissingSections, risk.SecSandbox)
	base.Sandbox = SandboxSection(true, 1)
	d = risk.EvaluateAdvanced(risk.DefaultAdvancedPolicy(), base)
	assert.Contains(t, d.ReasonCodes, risk.ReasonSandboxViolation)
	base.Sandbox = SandboxSection(true, 0)
	d = risk.EvaluateAdvanced(risk.DefaultAdvancedPolicy(), base)
	assert.Equal(t, risk.Allow, d.Verdict, "%v", d.ReasonCodes)

	_, err = accepted[0].Intent(IntentContext{})
	assert.Error(t, err, "the host must supply the whole context")
}

// TestProp_NoStrategyOutputBecomesATransferOrWithdrawal: whatever bytes a
// strategy writes -- valid records, records of other types, intents of every
// kind econ declares, smuggled fields -- the only things that come out the
// other side are SERVICE intents of a proposable kind, inside the manifest.
func TestProp_NoStrategyOutputBecomesATransferOrWithdrawal(t *testing.T) {
	in := testInput()
	canon, err := in.Canonical()
	require.NoError(t, err)
	permissive := Manifest{AllowedIntentKinds: ProposableKinds(), AllowedInstrumentKinds: []string{"EQUITY", "SPOT_PAIR"},
		MaxNotionalPerIntent: "100000000.00", MaxIntentsPerRun: 100, MinIntervalSeconds: 60}.Canonicalize()
	require.NoError(t, permissive.Validate())
	snap := snapshotOf(t, in)
	allKinds := append(econ.AllIntentKinds(), "SIGN_TRANSACTION", "EXPORT_KEY")
	rapid.Check(t, func(rt *rapid.T) {
		var b strings.Builder
		n := rapid.IntRange(0, 8).Draw(rt, "lines")
		for i := 1; i <= n; i++ {
			seq := strconv.Itoa(i)
			switch rapid.IntRange(0, 5).Draw(rt, "shape") {
			case 0:
				b.WriteString(`{"type":"intent","seq":` + seq + `,"kind":"` + string(rapid.SampledFrom(allKinds).Draw(rt, "kind")) +
					`","instrument_id":"` + rapid.SampledFrom([]string{insA, insB, "x"}).Draw(rt, "ins") +
					`","side":"` + rapid.SampledFrom([]string{"BUY", "SELL", "ALL"}).Draw(rt, "side") +
					`","quantity":"` + rapid.SampledFrom([]string{"1", "1000000", "0", "-1", "1.5"}).Draw(rt, "qty") + `"}`)
			case 1:
				b.WriteString(`{"type":"` + rapid.SampledFrom([]string{"sign", "transfer", "withdraw", "approve"}).Draw(rt, "type") + `","seq":` + seq + `}`)
			case 2:
				b.WriteString(`{"type":"intent","seq":` + seq + `,"kind":"ACQUIRE_EXPOSURE","instrument_id":"` + insA +
					`","side":"BUY","quantity":"1","` + rapid.SampledFrom([]string{"destination", "to_account", "signature", "withdraw"}).Draw(rt, "field") + `":"x"}`)
			case 3:
				b.WriteString(`{"type":"log","seq":` + seq + `,"event":"e"}`)
			case 4:
				b.WriteString(rapid.StringN(0, 40, -1).Draw(rt, "garbage"))
			default:
				b.WriteString(`{"type":"intent","seq":` + seq + `,"kind":"REDUCE_EXPOSURE","instrument_id":"` + insB + `","side":"SELL","quantity":"3"}`)
			}
			b.WriteString("\n")
		}
		raw := RawRun{Termination: TerminationOK, Stdout: []byte(b.String()), Input: canon, ArtifactDigest: make([]byte, 32), InputDigest: make([]byte, 32)}
		res, err := Interpret(raw, testLimits())
		if err != nil {
			rt.Fatal(err)
		}
		accepted, _ := permissive.Filter(res.Proposals, snap)
		for _, a := range accepted {
			intent, err := a.Intent(testIntentContext())
			if err != nil {
				rt.Fatalf("an accepted proposal failed to convert: %v", err)
			}
			if !Proposable(intent.Kind) || intent.ActorType != security.ActorService || intent.Mode != gates.ModePaper {
				rt.Fatalf("output produced %s by %s in %s", intent.Kind, intent.ActorType, intent.Mode)
			}
			var terms map[string]any
			if err := json.Unmarshal(intent.Terms, &terms); err != nil {
				rt.Fatal(err)
			}
			for k := range terms {
				switch k {
				case "origin", "side", "limit_price", "notional", "price_source", "sandbox_run_id", "run_digest", "artifact_digest", "proposal_seq":
				default:
					rt.Fatalf("intent terms carry %q, which no strategy can set", k)
				}
			}
		}
		if res.Termination == TerminationInvalidOutput && len(res.Proposals) > 0 {
			rt.Fatal("an invalid output yielded proposals")
		}
	})
}
