package nativeasset

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/money"
)

func q(n int64) money.Quantity { return money.QuantityFromInt64(n) }

func anAccountID(t *testing.T) accounts.AccountID { t.Helper(); return accounts.NewAccountID() }
func accountZero() accounts.AccountID             { return accounts.AccountID{} }

func qs(t *testing.T, s string) money.Quantity {
	t.Helper()
	v, err := money.ParseQuantity(s)
	require.NoError(t, err)
	return v
}

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

func TestStatus_AllSevenDeclared(t *testing.T) {
	require.Len(t, AllStatuses(), 7)
	for _, s := range AllStatuses() {
		require.True(t, s.Valid(), "%s", s)
	}
	require.False(t, Status("LIVE").Valid())
}

func TestStatus_OnlyActiveOpensNewExposure(t *testing.T) {
	require.True(t, StatusActive.AllowsBuy())
	for _, s := range AllStatuses() {
		if s == StatusActive {
			continue
		}
		require.False(t, s.AllowsBuy(), "%s must not permit new exposure", s)
	}
}

func TestStatus_CloseOnlyLetsHoldersOut(t *testing.T) {
	require.True(t, StatusCloseOnly.AllowsSell(),
		"trapping holders is worse than stopping new positions")
	require.False(t, StatusCloseOnly.AllowsBuy())
	require.False(t, StatusHalted.AllowsSell(), "a halt stops everything; that is what makes it a halt")
	require.False(t, StatusDelisted.AllowsSell())
}

func TestStatus_ALiveAssetCannotBeDelistedWithoutWarning(t *testing.T) {
	require.False(t, CanTransition(StatusActive, StatusDelisted),
		"a live market must pass through CLOSE_ONLY or HALTED, so holders get a chance to exit or the halt is recorded")
	require.True(t, CanTransition(StatusActive, StatusCloseOnly))
	require.True(t, CanTransition(StatusCloseOnly, StatusDelisted))
	require.True(t, CanTransition(StatusHalted, StatusDelisted))
}

func TestStatus_TerminalStatesGoNowhere(t *testing.T) {
	for _, s := range []Status{StatusDelisted, StatusRejected} {
		require.True(t, s.Terminal(), "%s", s)
		for _, to := range AllStatuses() {
			require.False(t, CanTransition(s, to), "%s claims it can become %s", s, to)
		}
	}
}

func TestStatus_EveryTransitionTargetIsDeclared(t *testing.T) {
	for from, tos := range statusTransitions {
		require.True(t, from.Valid(), "unknown source %q", from)
		for _, to := range tos {
			require.True(t, to.Valid(), "%s -> %q names an unknown status", from, to)
			require.NotEqual(t, from, to)
		}
	}
}

func TestModerationState_OnlyApprovedOrFlaggedMayActivate(t *testing.T) {
	require.True(t, ModerationApproved.PermitsActivation())
	require.True(t, ModerationFlagged.PermitsActivation(),
		"a flag asks for review; it is not a refusal")
	require.False(t, ModerationPending.PermitsActivation())
	require.False(t, ModerationRejected.PermitsActivation())
	require.False(t, ModerationState("MAYBE").Valid())
}

// ---------------------------------------------------------------------------
// Supply and policy
// ---------------------------------------------------------------------------

func TestSupplyModel_Validate(t *testing.T) {
	ok := SupplyModel{MaxSupply: q(1_000_000), CreatorAllocation: q(100_000)}
	require.NoError(t, ok.Validate())
	require.Equal(t, "900000", ok.PoolSupply().String())

	require.Error(t, SupplyModel{MaxSupply: q(0)}.Validate())
	require.Error(t, SupplyModel{MaxSupply: q(100), CreatorAllocation: q(-1)}.Validate())
	require.Error(t, SupplyModel{MaxSupply: q(100), CreatorAllocation: q(200)}.Validate())
	require.Error(t, SupplyModel{MaxSupply: q(100), CreatorAllocation: q(20), TreasuryAllocation: q(80)}.Validate(),
		"allocations that leave nothing for the market to sell make it not a market")
}

// TestSupplyModel_CreatorAllocationIsCapped is the control that keeps a market
// from being a wrapper around one person's position.
func TestSupplyModel_CreatorAllocationIsCapped(t *testing.T) {
	atCap := SupplyModel{MaxSupply: q(1_000_000), CreatorAllocation: q(200_000)} // exactly 20%
	require.NoError(t, atCap.Validate())

	over := SupplyModel{MaxSupply: q(1_000_000), CreatorAllocation: q(200_001)}
	err := over.Validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "not a market")
}

func TestPolicyProfile_ConservativeByDefault(t *testing.T) {
	p := ConservativePolicy()
	require.NoError(t, p.Validate())
	require.True(t, p.InternalOnly)
	require.False(t, p.Transferable, "peer transfer is how an internal asset becomes a payment instrument")
	require.False(t, p.CashoutEligible)
	require.False(t, p.CreatorEarningEligible)
	require.False(t, p.MarketProceedsEligible)
	require.Equal(t, 18, p.MinimumAge)
}

func TestPolicyProfile_ExternalAssetsAreRefused(t *testing.T) {
	p := ConservativePolicy()
	p.InternalOnly = false
	err := p.Validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "legal decision")
}

func TestPolicyProfile_PayoutSwitchesMustBeConsistent(t *testing.T) {
	p := ConservativePolicy()
	p.MarketProceedsEligible = true
	require.Error(t, p.Validate(),
		"proceeds cannot be payable while the asset itself is not cashout-eligible; one would look enabled while the other governs")

	p.CashoutEligible = true
	require.NoError(t, p.Validate())
}

// ---------------------------------------------------------------------------
// Screening
// ---------------------------------------------------------------------------

func good() ScreenInput {
	return ScreenInput{Name: "Doggu Coin", Symbol: "DOGGU", Description: "a fun internal asset"}
}

func TestScreen_AcceptsAReasonableProposal(t *testing.T) {
	v := Screen(good())
	require.Equal(t, ModerationApproved, v.State, v.Explain())
	require.Empty(t, v.Findings)
	require.False(t, v.Blocked())
}

func TestScreen_SymbolShape(t *testing.T) {
	for _, sym := range []string{"D", "TOOLONGSYMBOL", "1ABC", "AB-CD", "AB CD", ""} {
		in := good()
		in.Symbol = sym
		v := Screen(in)
		require.True(t, v.Blocked(), "symbol %q should be refused", sym)
	}
	for _, sym := range []string{"DG", "DOGGU", "A1", "ABCDEFGHIJ"} {
		in := good()
		in.Symbol = sym
		require.False(t, Screen(in).Blocked(), "symbol %q should be accepted", sym)
	}
	// A lowercase symbol is normalised rather than refused: people type
	// lowercase, and the registry stores uppercase.
	lower := good()
	lower.Symbol = "doggu"
	require.False(t, Screen(lower).Blocked())
}

func TestScreen_ReservedSymbolsAreRefused(t *testing.T) {
	for _, sym := range []string{"CREDIT", "NODAL", "USD", "USDC", "SOL", "BTC", "ADMIN"} {
		in := good()
		in.Symbol = sym
		v := Screen(in)
		require.True(t, v.Blocked(), "%s must be reserved", sym)
		require.Contains(t, v.Explain(), string(FindingSymbolReserved))
	}
}

func TestScreen_SymbolCollisionIsRefused(t *testing.T) {
	in := good()
	in.ExistingSymbols = []string{"doggu"}
	v := Screen(in)
	require.True(t, v.Blocked(), "symbol matching is case-insensitive")
	require.Contains(t, v.Explain(), string(FindingSymbolTaken))
}

// TestScreen_ImpersonationOfTheProtectedNames covers the case that matters
// most: an asset claiming to be the platform.
func TestScreen_ImpersonationOfTheProtectedNames(t *testing.T) {
	for _, name := range []string{"Nodal", "N0dal", "NODAL", "n o d a l", "Official", "Verified", "Аdmin"} {
		in := good()
		in.Name = name
		v := Screen(in)
		require.True(t, v.Blocked(), "%q must be refused as impersonation", name)
	}
}

func TestScreen_ConfusableNamesAreFlaggedNotBlocked(t *testing.T) {
	in := good()
	in.Name = "D0ggu C0in" // zeroes for o
	in.ExistingNames = []string{"Doggu Coin"}
	v := Screen(in)
	require.False(t, v.Blocked(), "a confusable name is a judgement call, not an automatic refusal")
	require.Equal(t, ModerationFlagged, v.State)
	require.Contains(t, v.Explain(), string(FindingNameConfusable))
}

func TestScreen_BidiOverridesAreRefused(t *testing.T) {
	in := good()
	// The override is written as an escape rather than as the character: a
	// source file that CONTAINS a bidi control renders wrongly in every
	// reviewer's editor, which is the attack this test is about
	// (internal/nativeasset/moderation.go carries the same note).
	in.Name = "Doggu\u202eCoin" // right-to-left override
	v := Screen(in)
	require.True(t, v.Blocked(),
		"a bidi override lets a name render as something other than what it compares as")
	require.Contains(t, v.Explain(), string(FindingNameBidiControl))
}

func TestScreen_ControlCharactersAreRefused(t *testing.T) {
	in := good()
	in.Name = "Doggu\x00Coin"
	require.True(t, Screen(in).Blocked())

	in = good()
	in.Description = "nice\x07asset"
	require.True(t, Screen(in).Blocked())
}

// TestScreen_ReturnClaimsAreRefused is PART XIII and PART LIII: an internal
// speculative asset must not be marketed as an investment.
func TestScreen_ReturnClaimsAreRefused(t *testing.T) {
	for _, phrase := range []string{
		"guaranteed profit every week",
		"This is RISK FREE",
		"you cannot lose",
		"principal protected asset",
	} {
		in := good()
		in.Description = phrase
		v := Screen(in)
		require.True(t, v.Blocked(), "%q must be refused", phrase)
		require.Contains(t, v.Explain(), string(FindingReturnClaim))
	}
}

func TestScreen_ImageURLs(t *testing.T) {
	for _, u := range []string{
		"javascript:alert(1)",
		"data:image/png;base64,AAAA",
		"http://example.com/a.png",
		"not a url",
		"ftp://example.com/a.png",
	} {
		in := good()
		in.ImageURL = u
		require.True(t, Screen(in).Blocked(), "%q must be refused", u)
	}
	in := good()
	in.ImageURL = "https://example.com/a.png"
	require.False(t, Screen(in).Blocked())

	in = good()
	in.ImageURL = "https://example.com/" + strings.Repeat("a", MaxImageURLLen)
	require.True(t, Screen(in).Blocked(), "an over-long URL must be refused")
}

func TestScreen_MetadataLimits(t *testing.T) {
	in := good()
	in.Metadata = map[string]any{}
	for i := 0; i < MaxMetadataKeys+1; i++ {
		in.Metadata[string(rune('a'+i%26))+strings.Repeat("x", i)] = 1
	}
	require.True(t, Screen(in).Blocked())

	in = good()
	in.Metadata = map[string]any{"blob": strings.Repeat("x", MaxMetadataBytes+1)}
	require.True(t, Screen(in).Blocked())

	in = good()
	in.Metadata = map[string]any{"theme": "dogs", "links": []any{"https://example.com"}}
	require.False(t, Screen(in).Blocked())
}

// TestScreen_FindingsAreDeterministic matters because the verdict is stored:
// a record whose order changes between runs cannot be diffed.
func TestScreen_FindingsAreDeterministic(t *testing.T) {
	in := good()
	in.Name = "x"
	in.Symbol = "USD"
	in.Description = "guaranteed profit"
	in.ImageURL = "javascript:alert(1)"

	first := Screen(in)
	require.True(t, first.Blocked())
	require.Greater(t, len(first.Findings), 2)
	for i := 0; i < 50; i++ {
		require.Equal(t, first.Findings, Screen(in).Findings)
	}
	for i := 1; i < len(first.Findings); i++ {
		require.LessOrEqual(t, string(first.Findings[i-1].Code), string(first.Findings[i].Code))
	}
}

func TestScreen_EveryFindingNamesItsField(t *testing.T) {
	in := good()
	in.Name = ""
	in.Symbol = "!"
	in.Description = strings.Repeat("x", MaxDescriptionLen+1)
	v := Screen(in)
	for _, f := range v.Findings {
		require.NotEmpty(t, f.Field, "finding %s does not say which field is at fault", f.Code)
		require.NotEmpty(t, f.Detail, "finding %s does not explain itself", f.Code)
	}
}

// TestCombine_ExternalScreeningCanOnlyTighten is the rule that keeps a provider
// from un-rejecting something the local rules blocked.
func TestCombine_ExternalScreeningCanOnlyTighten(t *testing.T) {
	blocked := Screen(func() ScreenInput { in := good(); in.Symbol = "USD"; return in }())
	require.True(t, blocked.Blocked())

	still := Combine(blocked, ModerationApproved, "provider says fine")
	require.True(t, still.Blocked(),
		"an external approval must not override a local commitment")

	clean := Screen(good())
	require.False(t, clean.Blocked())
	rejected := Combine(clean, ModerationRejected, "provider found something")
	require.True(t, rejected.Blocked())

	flagged := Combine(clean, ModerationFlagged, "provider unsure")
	require.Equal(t, ModerationFlagged, flagged.State)
	require.False(t, flagged.Blocked())

	unknown := Combine(clean, ModerationState("WEIRD"), "nonsense")
	require.Equal(t, clean.State, unknown.State, "an unrecognised external verdict changes nothing")
}

func TestNormalizeConfusables(t *testing.T) {
	require.Equal(t, "nodal", normalizeConfusables("N0dal"))
	require.Equal(t, "nodal", normalizeConfusables("N O D A L"))
	require.Equal(t, "credit", normalizeConfusables("CR3DIT"))
	require.Equal(t, "", normalizeConfusables("!!!"))

	// Cyrillic small o (U+043E) is a different code point that renders
	// identically to Latin o, which is the substitution actually used to
	// impersonate a name.
	require.Equal(t, "nodal", normalizeConfusables("Nоdal"))
	// Greek small nu (U+03BD) renders as a Latin v, so folding it to v is
	// right even though its uppercase form looks like an N. Pinning this
	// stops someone "fixing" the mapping in the wrong direction later.
	require.Equal(t, "vodal", normalizeConfusables("νodal"))
}

func TestCreateRequest_Validate(t *testing.T) {
	base := CreateRequest{
		CreatorAccountID: anAccountID(t),
		Name:             "Doggu",
		Symbol:           "DOGGU",
		Supply:           SupplyModel{MaxSupply: qs(t, "1000000000000")},
	}
	require.NoError(t, base.Validate())

	t.Run("no creator", func(t *testing.T) {
		r := base
		r.CreatorAccountID = accountZero()
		require.Error(t, r.Validate())
	})
	t.Run("too many decimals", func(t *testing.T) {
		r := base
		r.Decimals = MaxDecimals + 1
		require.Error(t, r.Validate())
	})
	t.Run("bad supply", func(t *testing.T) {
		r := base
		r.Supply = SupplyModel{}
		require.Error(t, r.Validate())
	})
	t.Run("external policy", func(t *testing.T) {
		r := base
		p := ConservativePolicy()
		p.InternalOnly = false
		r.Policy = &p
		require.Error(t, r.Validate())
	})
}

func TestRegistryStatusFor_NeverOpensExposureByAccident(t *testing.T) {
	// Anything that is not ACTIVE must map to a registry status that refuses
	// new exposure, so a package that only consults the shared registry is
	// still safe.
	for _, s := range AllStatuses() {
		got := registryStatusFor(s)
		if s == StatusActive {
			require.True(t, got.AllowsIncreasingExposure(), "%s", s)
			continue
		}
		require.False(t, got.AllowsIncreasingExposure(),
			"native status %s maps to registry status %s, which permits new exposure", s, got)
	}
}
