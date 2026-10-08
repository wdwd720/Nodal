package marketplace

import (
	"context"
	"encoding/json"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/legalrouter"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/sandboxrt"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/settlement"
	"github.com/nodal/controlplane/internal/valuedomain"
)

func TestLifecycle_LegalityTable(t *testing.T) {
	legal := map[[2]Status]bool{
		{StatusDraft, StatusSubmitted}:       true,
		{StatusSubmitted, StatusValidated}:   true,
		{StatusSubmitted, StatusRejected}:    true,
		{StatusValidated, StatusInReview}:    true,
		{StatusValidated, StatusRejected}:    true,
		{StatusInReview, StatusApproved}:     true,
		{StatusInReview, StatusRejected}:     true,
		{StatusApproved, StatusDeprecated}:   true,
		{StatusDraft, StatusApproved}:        false,
		{StatusSubmitted, StatusApproved}:    false,
		{StatusValidated, StatusApproved}:    false,
		{StatusRejected, StatusDraft}:        false,
		{StatusDeprecated, StatusApproved}:   false,
		{StatusApproved, StatusRejected}:     false,
		{StatusApproved, StatusInReview}:     false,
		{StatusInReview, StatusValidated}:    false,
		{StatusDraft, StatusRejected}:        false,
		{StatusRejected, StatusSubmitted}:    false,
		{StatusDeprecated, StatusDeprecated}: false,
	}
	for _, from := range AllStatuses() {
		for _, to := range AllStatuses() {
			want := legal[[2]Status{from, to}]
			assert.Equal(t, want, CanTransition(from, to), "%s -> %s", from, to)
		}
	}
	for _, terminal := range []Status{StatusRejected, StatusDeprecated} {
		for _, to := range AllStatuses() {
			assert.False(t, CanTransition(terminal, to), "%s is terminal", terminal)
		}
	}
	// Moderation: TAKEN_DOWN is final, CLEARED is reached from review or a flag.
	assert.True(t, CanModerate(ModerationUnreviewed, ModerationCleared))
	assert.True(t, CanModerate(ModerationFlagged, ModerationCleared))
	for _, to := range AllModerationStates() {
		assert.False(t, CanModerate(ModerationTakenDown, to))
		assert.False(t, CanModerate(to, to))
	}
	assert.False(t, CanModerate(ModerationCleared, ModerationUnreviewed))
}

func TestInstall_AnythingButPaperIsRefusedBeforeTheDatabase(t *testing.T) {
	assert.Equal(t, []gates.Mode{gates.ModePaper}, AllInstallModes())
	s := &Service{}
	for _, m := range []gates.Mode{gates.ModeSandbox, gates.ModeDevnet, gates.ModeLive, "REAL"} {
		_, _, err := s.InstallVersion(context.Background(), nil, InstallRequest{
			VersionID: VersionID(econ.NewIntentID()), AccountID: accounts.AccountID(econ.NewIntentID()), Mode: m,
			Actor: econ.Actor{Type: security.ActorUser, ID: "u"},
		})
		require.Error(t, err, m)
		assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err), m)
	}
	_, _, err := s.InstallVersion(context.Background(), nil, InstallRequest{Mode: gates.ModePaper, Actor: econ.Actor{Type: security.ActorAgent, ID: "a"}})
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err), "an agent never installs")
}

func validVersionRequest() VersionRequest {
	return VersionRequest{
		Description: "momentum on SMA(3) vs SMA(5)", RiskClass: RiskModerate,
		SupportedInstrumentIDs: []string{"0193b2e0-0000-7000-8000-00000000000a"},
		Manifest: sandboxrt.Manifest{AllowedIntentKinds: []econ.IntentKind{econ.KindAcquireExposure},
			AllowedInstrumentIDs: []string{"0193b2e0-0000-7000-8000-00000000000a"}, MaxNotionalPerIntent: "1000.00",
			MaxIntentsPerRun: 2, MinIntervalSeconds: 3600}.Canonicalize(),
		Source:       sandboxrt.Source{"main.go": []byte("package main\n\nfunc main() {}\n")},
		SourceCommit: "abc123", DataDependencies: []string{"marks"},
		Fees:   FeeModel{PerformanceFeeBPS: 2000, ManagementFeeBPS: 100, HighWaterMark: true},
		Config: map[string]string{"order_units": "1000000"},
	}
}

func TestVersionRequest_RefusesEveryMalformedField(t *testing.T) {
	require.NoError(t, validVersionRequest().validate())
	cases := map[string]func(*VersionRequest){
		"no description":        func(r *VersionRequest) { r.Description = " " },
		"risk class":            func(r *VersionRequest) { r.RiskClass = "YOLO" },
		"withdraw manifest":     func(r *VersionRequest) { r.Manifest.AllowedIntentKinds = []econ.IntentKind{econ.KindWithdraw} },
		"no source":             func(r *VersionRequest) { r.Source = nil },
		"test file":             func(r *VersionRequest) { r.Source = sandboxrt.Source{"x_test.go": []byte("package main")} },
		"path traversal":        func(r *VersionRequest) { r.Source = sandboxrt.Source{"../x.go": []byte("package main")} },
		"no commit":             func(r *VersionRequest) { r.SourceCommit = "" },
		"bad dependency":        func(r *VersionRequest) { r.DataDependencies = []string{"Bad Dep"} },
		"fee too high":          func(r *VersionRequest) { r.Fees.PerformanceFeeBPS = 9000 },
		"negative fee":          func(r *VersionRequest) { r.Fees.ManagementFeeBPS = -1 },
		"bad config key":        func(r *VersionRequest) { r.Config = map[string]string{"KEY": "v"} },
		"bad supported id":      func(r *VersionRequest) { r.SupportedInstrumentIDs = []string{"BTC"} },
		"oversized source":      func(r *VersionRequest) { r.Source = sandboxrt.Source{"main.go": make([]byte, maxSourceFileBytes+1)} },
		"too many sources":      func(r *VersionRequest) { r.Source = manyFiles(maxSourceFiles + 1) },
		"evidence ref too long": func(r *VersionRequest) { r.BacktestEvidenceRef = strings.Repeat("x", 501) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := validVersionRequest()
			mutate(&r)
			err := r.validate()
			require.Error(t, err)
			assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
		})
	}
}

func manyFiles(n int) sandboxrt.Source {
	src := sandboxrt.Source{}
	for i := 0; i < n; i++ {
		src["f"+strconv.Itoa(i)+".go"] = []byte("package main")
	}
	return src
}

// TestRoute_ASandboxProposalRoutesAsASimulation shows the routing step of a
// paper-follow intent: the settlement compiler sees a SERVICE intent on the
// simulated rail, and the conservative legal policy permits simulation.
func TestRoute_ASandboxProposalRoutesAsASimulation(t *testing.T) {
	router, err := legalrouter.New(legalrouter.ConservativePolicy())
	require.NoError(t, err)
	a := sandboxrt.AcceptedProposal{Proposal: sandboxrt.Proposal{Seq: 1, Kind: econ.KindAcquireExposure,
		InstrumentID: "0193b2e0-0000-7000-8000-00000000000a", Side: sandboxrt.SideBuy, Quantity: money.QuantityFromInt64(1_000_000)},
		InstrumentKind: "SPOT_PAIR", Notional: "150.25000000", PriceSource: sandboxrt.PriceMark}
	in, err := a.Intent(sandboxrt.IntentContext{AccountID: accounts.AccountID(econ.NewIntentID()), Mode: gates.ModePaper,
		Capability: gates.StrategyMarketplace, ActorID: "marketplace", StrategyRef: "0193b2e0-0000-7000-8000-000000000001@1",
		RunID: sandboxrt.NewRunID(), RunDigest: make([]byte, 32), ArtifactDigest: make([]byte, 32), KeyPrefix: "mkt-follow:x"})
	require.NoError(t, err)
	in.ID = econ.NewIntentID()
	route, err := econ.Route(econ.RouteRequest{Intent: in, Subject: settlement.Subject{Type: settlement.SubjectInstrument, ID: in.InstrumentID},
		Jurisdiction: "US", Verification: valuedomain.VerificationNone}, router, time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	assert.True(t, route.Permitted, "%v", route.Reasons)
	assert.Equal(t, valuedomain.Simulated, route.ValueDomain)

	var terms map[string]any
	require.NoError(t, json.Unmarshal(in.Terms, &terms))
	assert.Equal(t, sandboxrt.OriginSandbox, terms["origin"])
}

// TestImportBoundary_MarketplaceCannotReachAuthority mirrors the sandbox
// runtime's boundary test for this package alone.
func TestImportBoundary_MarketplaceCannotReachAuthority(t *testing.T) {
	const module = "github.com/nodal/controlplane/"
	bad := []string{"internal/signing", "internal/wallet", "internal/withdrawal", "internal/delegation"}
	checked := 0
	err := filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		checked++
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			rel := strings.TrimPrefix(p, module)
			for _, b := range bad {
				assert.False(t, rel == b || strings.HasPrefix(rel, b+"/"), "%s imports %s", path, p)
			}
		}
		return nil
	})
	require.NoError(t, err)
	require.Greater(t, checked, 5)
}

// TestNoFloatingPointInSource: no non-test source of this package mentions a
// binary floating point type or conversion.
func TestNoFloatingPointInSource(t *testing.T) {
	banned := []string{"float32", "float64", "big.Float", "big.Rat", "ParseFloat", "FormatFloat",
		"math.Round", "math.Floor", "math.Ceil", "math.Trunc"}
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name) // #nosec G304 -- this package's own sources
		require.NoError(t, err)
		checked++
		for _, tok := range banned {
			if idx := strings.Index(string(src), tok); idx >= 0 {
				t.Errorf("%s:%d: forbidden token %q", name, 1+strings.Count(string(src[:idx]), "\n"), tok)
			}
		}
	}
	require.Greater(t, checked, 5)
}
