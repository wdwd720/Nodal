package signing_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	signingv1 "github.com/nodal/controlplane/internal/gen/proto/controlplane/signing/v1"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/signing"
	"github.com/nodal/controlplane/internal/signing/inspect"
	"github.com/nodal/controlplane/internal/wallet/wallettest"
)

// --- test doubles for the chain sources -------------------------------------

type fakeChain struct {
	height    uint64
	heightErr error
	sim       *inspect.SimulationResult
	simErr    error
	tables    map[string][]string
	tablesErr error
}

func (f *fakeChain) CurrentBlockHeight(context.Context) (uint64, error) { return f.height, f.heightErr }

func (f *fakeChain) LoadSimulation(context.Context, string, string) (*inspect.SimulationResult, error) {
	return f.sim, f.simErr
}

func (f *fakeChain) LookupTables(context.Context, []string) (map[string][]string, error) {
	return f.tables, f.tablesErr
}

func baseDeps(t *testing.T, env config.Environment, fake *wallettest.Fake) signing.Deps {
	t.Helper()
	chain := &fakeChain{height: 1}
	return signing.Deps{
		DB:           &db.DB{},
		Audit:        audit.NewWriterWithBuildVersion("test"),
		Clock:        clock.NewFake(time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)),
		Wallets:      fake,
		Signer:       fake,
		BlockHeights: chain,
		Simulations:  chain,
		LookupTables: chain,
		Env:          env,
		ServiceName:  "execution-worker-test",
		Policy:       signing.DefaultPolicy(),
	}
}

func TestNew_FailsClosedWithoutVerifiedDelegation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, env := range []config.Environment{config.EnvDev, config.EnvStaging, config.EnvProd} {
		t.Run(string(env), func(t *testing.T) {
			t.Parallel()
			// The fake refuses STAGING/PROD; construct it under TEST and only
			// present its (unverified) capability to the service.
			fake, err := wallettest.New(config.EnvTest, wallettest.WithDelegationVerified(false))
			require.NoError(t, err)
			_, err = signing.New(ctx, baseDeps(t, env, fake))
			require.Error(t, err)
			assert.Equal(t, errs.CodeDelegationNotVerified, errs.CodeOf(err))

			fake.SetDelegationVerified(true)
			svc, err := signing.New(ctx, baseDeps(t, env, fake))
			require.NoError(t, err)
			assert.True(t, svc.Capability().DelegationVerified())
		})
	}
	t.Run("LOCAL tolerates an unverified probe", func(t *testing.T) {
		t.Parallel()
		fake, err := wallettest.New(config.EnvLocal, wallettest.WithDelegationVerified(false))
		require.NoError(t, err)
		svc, err := signing.New(ctx, baseDeps(t, config.EnvLocal, fake))
		require.NoError(t, err)
		assert.False(t, svc.Capability().DelegationVerified())
	})
	t.Run("probe failure fails closed everywhere", func(t *testing.T) {
		t.Parallel()
		fake, err := wallettest.New(config.EnvTest)
		require.NoError(t, err)
		fake.FailWith(errs.New(errs.CodeProviderUnavailable, "down"))
		_, err = signing.New(ctx, baseDeps(t, config.EnvTest, fake))
		require.Error(t, err)
		assert.Equal(t, errs.CodeDelegationNotVerified, errs.CodeOf(err))
	})
	t.Run("lookup table source is mandatory outside LOCAL/TEST", func(t *testing.T) {
		t.Parallel()
		fake, err := wallettest.New(config.EnvTest)
		require.NoError(t, err)
		deps := baseDeps(t, config.EnvDev, fake)
		deps.LookupTables = nil
		_, err = signing.New(ctx, deps)
		require.Error(t, err)
		assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	})
	t.Run("simulation cannot be optional outside LOCAL/TEST", func(t *testing.T) {
		t.Parallel()
		fake, err := wallettest.New(config.EnvTest)
		require.NoError(t, err)
		deps := baseDeps(t, config.EnvStaging, fake)
		deps.Policy.RequireSimulation = false
		_, err = signing.New(ctx, deps)
		require.Error(t, err)
	})
	t.Run("missing collaborators", func(t *testing.T) {
		t.Parallel()
		fake, err := wallettest.New(config.EnvTest)
		require.NoError(t, err)
		for name, mutate := range map[string]func(*signing.Deps){
			"db":      func(d *signing.Deps) { d.DB = nil },
			"audit":   func(d *signing.Deps) { d.Audit = nil },
			"wallets": func(d *signing.Deps) { d.Wallets = nil },
			"signer":  func(d *signing.Deps) { d.Signer = nil },
			"heights": func(d *signing.Deps) { d.BlockHeights = nil },
			"sims":    func(d *signing.Deps) { d.Simulations = nil },
			"env":     func(d *signing.Deps) { d.Env = "" },
			"service": func(d *signing.Deps) { d.ServiceName = "" },
			"policy":  func(d *signing.Deps) { d.Policy.AllowedPrograms = nil },
		} {
			deps := baseDeps(t, config.EnvTest, fake)
			mutate(&deps)
			_, err := signing.New(ctx, deps)
			require.Error(t, err, name)
		}
	})
}

func TestRequest_Validate(t *testing.T) {
	t.Parallel()
	h := sha256.Sum256([]byte("tx"))
	good := signing.Request{
		AttemptID: id.New[id.Any]().String(), PlanID: id.New[id.Any]().String(), IntentID: id.New[id.Any]().String(),
		RiskDecisionID: id.New[id.Any]().String(), WalletID: id.New[id.Any]().String(),
		UnsignedTx: []byte("tx"), ExpectedTxHash: h[:],
	}
	require.NoError(t, good.Validate())
	cases := map[string]func(r *signing.Request){
		"attempt":  func(r *signing.Request) { r.AttemptID = "nope" },
		"plan":     func(r *signing.Request) { r.PlanID = "" },
		"tx":       func(r *signing.Request) { r.UnsignedTx = nil },
		"tx size":  func(r *signing.Request) { r.UnsignedTx = make([]byte, inspect.MaxTransactionSize+1) },
		"hash":     func(r *signing.Request) { r.ExpectedTxHash = []byte{1} },
		"plan hsh": func(r *signing.Request) { r.ClaimedPlanHash = []byte{1, 2} },
	}
	for name, mutate := range cases {
		r := good
		mutate(&r)
		err := r.Validate()
		require.Error(t, err, name)
		assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), name)
	}
}

// --- gRPC adapter over a stub service ----------------------------------------

type stubService struct {
	last     signing.Request
	decision signing.Decision
	signed   []byte
	err      error
}

func (s *stubService) Sign(_ context.Context, req signing.Request) (signing.Decision, []byte, error) {
	s.last = req
	return s.decision, s.signed, s.err
}

func (s *stubService) GetDecision(context.Context, string) (signing.Decision, error) {
	return s.decision, s.err
}

func TestGRPC_InProcessClientRoundTrip(t *testing.T) {
	t.Parallel()
	decidedAt := time.Date(2026, 9, 5, 12, 0, 0, 123, time.UTC)
	stub := &stubService{
		decision: signing.Decision{
			ID: "0192f7a0-1b2c-7d3e-8f4a-5b6c7d8e9f01", Approved: true, InspectorVersion: inspect.InspectorVersion,
			Checks: []inspect.Check{{Name: inspect.CheckFeePayer, Passed: true, Detail: "ok"}}, DecidedAt: decidedAt, ProviderSignRef: "fake:1", Replayed: true,
		},
		signed: []byte{9, 9, 9},
	}
	client := signing.NewInProcessClient(signing.NewServer(stub))
	resp, err := client.Sign(context.Background(), &signingv1.SignRequest{
		AttemptId: "a", PlanId: "p", IntentId: "i", RiskDecisionId: "r", WalletId: "w",
		UnsignedTransaction: []byte{1, 2, 3}, ExpectedTransactionHash: []byte{4},
		LookupTables:  []*signingv1.LookupTable{{Address: "T", AccountKeys: []string{"k1", "k2"}}},
		SimulationRef: "sim://1", CorrelationId: "corr",
	})
	require.NoError(t, err)
	assert.Equal(t, "a", stub.last.AttemptID)
	assert.Equal(t, map[string][]string{"T": {"k1", "k2"}}, stub.last.LookupTables)
	assert.Equal(t, "corr", stub.last.CorrelationID)
	assert.True(t, resp.GetApproved())
	assert.True(t, resp.GetReplayed())
	assert.Equal(t, []byte{9, 9, 9}, resp.GetSignedTransaction())
	assert.Equal(t, "fake:1", resp.GetProviderSignRef())
	assert.Equal(t, decidedAt.Format(time.RFC3339Nano), resp.GetDecidedAt())
	require.Len(t, resp.GetChecks(), 1)
	back := signing.DecisionFromResponse(resp)
	assert.Equal(t, stub.decision.ID, back.ID)
	assert.Equal(t, decidedAt, back.DecidedAt)

	got, err := client.GetDecision(context.Background(), &signingv1.GetDecisionRequest{DecisionId: stub.decision.ID})
	require.NoError(t, err)
	assert.Equal(t, stub.decision.ID, got.GetDecision().GetDecisionId())

	t.Run("rejection is a successful RPC", func(t *testing.T) {
		stub2 := &stubService{decision: signing.Decision{ID: "d", Approved: false, ReasonCodes: []string{"FEE_PAYER_MISMATCH"}}}
		resp, err := signing.NewInProcessClient(signing.NewServer(stub2)).Sign(context.Background(), &signingv1.SignRequest{})
		require.NoError(t, err)
		assert.False(t, resp.GetApproved())
		assert.Equal(t, []string{"FEE_PAYER_MISMATCH"}, resp.GetReasonCodes())
		assert.Empty(t, resp.GetSignedTransaction())
	})
}

func TestGRPC_ErrorMapping(t *testing.T) {
	t.Parallel()
	cases := map[error]codes.Code{
		errs.New(errs.CodeValidationFailed, "bad"):         codes.InvalidArgument,
		errs.New(errs.CodeNotFound, "missing"):             codes.NotFound,
		errs.New(errs.CodeDelegationNotVerified, "no"):     codes.PermissionDenied,
		errs.New(errs.CodeProviderUnavailable, "down"):     codes.Unavailable,
		errs.New(errs.CodeRateLimited, "slow"):             codes.ResourceExhausted,
		errs.New(errs.CodeSigningRejected, "provider"):     codes.FailedPrecondition,
		errs.New(errs.CodeInternal, "secret detail"):       codes.Internal,
		errors.New("plain error with internal detail xyz"): codes.Internal,
	}
	for err, want := range cases {
		stub := &stubService{err: err}
		_, got := signing.NewInProcessClient(signing.NewServer(stub)).Sign(context.Background(), &signingv1.SignRequest{})
		st, ok := status.FromError(got)
		require.True(t, ok)
		assert.Equal(t, want, st.Code(), err.Error())
		if want == codes.Internal {
			assert.Equal(t, "INTERNAL", st.Message(), "internal detail must not leak")
		}
	}
}

func TestPolicy_Validate(t *testing.T) {
	t.Parallel()
	p := signing.DefaultPolicy()
	require.NoError(t, p.Validate(config.EnvProd))
	p.RequireSimulation = false
	require.NoError(t, p.Validate(config.EnvTest))
	require.Error(t, p.Validate(config.EnvProd))
	p = signing.DefaultPolicy()
	p.MaxComputeUnits = 0
	require.Error(t, p.Validate(config.EnvTest))
	p = signing.DefaultPolicy()
	p.BlockHeightMargin = 0
	require.Error(t, p.Validate(config.EnvTest))
}
