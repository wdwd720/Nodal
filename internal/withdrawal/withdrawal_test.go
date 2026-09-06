package withdrawal_test

import (
	"context"
	"crypto/rand"
	"math/big"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/withdrawal"
)

const (
	wrappedSOL = "So11111111111111111111111111111111111111112"
	zeroKey    = "11111111111111111111111111111111"
	alphabet   = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
)

// encodeBase58 is a reference encoder for generating valid test keys.
func encodeBase58(b []byte) string {
	n := new(big.Int).SetBytes(b)
	radix := big.NewInt(58)
	var out []byte
	for n.Sign() > 0 {
		mod := new(big.Int)
		n.DivMod(n, radix, mod)
		out = append(out, alphabet[mod.Int64()])
	}
	for _, c := range b {
		if c != 0 {
			break
		}
		out = append(out, '1')
	}
	slices.Reverse(out)
	return string(out)
}

func randomAddress(t *testing.T) string {
	t.Helper()
	var key [32]byte
	_, err := rand.Read(key[:])
	require.NoError(t, err)
	return encodeBase58(key[:])
}

func TestValidateSolanaAddress(t *testing.T) {
	t.Parallel()
	require.NoError(t, withdrawal.ValidateSolanaAddress(wrappedSOL))
	for i := 0; i < 20; i++ {
		require.NoError(t, withdrawal.ValidateSolanaAddress(randomAddress(t)))
	}
	require.ErrorIs(t, withdrawal.ValidateSolanaAddress(zeroKey), withdrawal.ErrAddressZero)
	require.ErrorIs(t, withdrawal.ValidateSolanaAddress(""), withdrawal.ErrAddressLength)
	require.ErrorIs(t, withdrawal.ValidateSolanaAddress("abc"), withdrawal.ErrAddressLength)
	require.ErrorIs(t, withdrawal.ValidateSolanaAddress(wrappedSOL+"11"), withdrawal.ErrAddressLength)
	require.ErrorIs(t, withdrawal.ValidateSolanaAddress("0OIl"+wrappedSOL[4:]), withdrawal.ErrAddressAlphabet)
	// 33 bytes encodes to ≤ 45 chars; pick one that fits the length window but decodes to 31 bytes.
	short := encodeBase58(append([]byte{0, 0}, make([]byte, 29)...))
	if len(short) >= 32 && len(short) <= 44 {
		require.ErrorIs(t, withdrawal.ValidateSolanaAddress(short), withdrawal.ErrAddressBytes)
	}
	require.ErrorIs(t, withdrawal.ValidateSolanaAddress("zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"), withdrawal.ErrAddressBytes)
	raw, err := withdrawal.DecodeBase58(wrappedSOL)
	require.NoError(t, err)
	require.Len(t, raw, 32)
}

func TestHumanFrom(t *testing.T) {
	t.Parallel()
	acct := accounts.NewAccountID().String()
	_, err := withdrawal.HumanFrom(security.AgentPrincipal("agent-1", acct))
	require.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	_, err = withdrawal.HumanFrom(security.Principal{SubjectID: "svc", ActorType: security.ActorService})
	require.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	_, err = withdrawal.HumanFrom(security.Principal{SubjectID: "sys", ActorType: security.ActorSystem})
	require.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	_, err = withdrawal.HumanFrom(security.Principal{})
	require.Equal(t, errs.CodeUnauthenticated, errs.CodeOf(err))
	h, err := withdrawal.HumanFrom(security.Principal{SubjectID: "u", ActorType: security.ActorUser, Roles: []security.Role{security.RoleCustomer}})
	require.NoError(t, err)
	require.True(t, h.Valid())
	require.False(t, withdrawal.HumanActor{}.Valid())
}

func TestStateMachine(t *testing.T) {
	t.Parallel()
	all := withdrawal.AllStatuses()
	require.Len(t, all, 10)
	for _, from := range all {
		_, ok := withdrawal.Transitions[from]
		require.True(t, ok, from)
		for _, to := range all {
			want := from != to && slices.Contains(withdrawal.Transitions[from], to)
			require.Equal(t, want, withdrawal.CanTransition(from, to), "%s -> %s", from, to)
		}
		if from.Final() {
			require.Empty(t, withdrawal.Transitions[from])
		}
	}
	require.False(t, withdrawal.CanTransition(withdrawal.StatusRequested, withdrawal.StatusSettled), "nothing settles without approval and submission")
	require.False(t, withdrawal.CanTransition(withdrawal.StatusSubmissionUnknown, withdrawal.StatusSubmitted))
	require.True(t, withdrawal.StatusSettled.Active())
	require.False(t, withdrawal.StatusRejected.Active())
}

func TestVelocityPolicy(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	q := money.QuantityFromInt64
	p := withdrawal.VelocityPolicy{MaxPerRequest: q(500), MaxPerWindow: q(1000), MaxCountPerWindow: 3, Window: 24 * time.Hour}
	require.NoError(t, p.Validate())
	require.Error(t, withdrawal.VelocityPolicy{MaxPerWindow: q(1)}.Validate(), "rolling bound needs a window")
	require.Error(t, withdrawal.VelocityPolicy{MaxCountPerWindow: -1}.Validate())
	require.NoError(t, withdrawal.VelocityPolicy{}.Validate(), "zero policy is unlimited")
	require.NoError(t, withdrawal.VelocityPolicy{}.Check(q(1<<40), nil, now))

	recent := []withdrawal.Withdrawal{
		{Quantity: q(400), Status: withdrawal.StatusRequested, CreatedAt: now.Add(-time.Hour)},
		{Quantity: q(300), Status: withdrawal.StatusRejected, CreatedAt: now.Add(-time.Hour)},     // freed
		{Quantity: q(900), Status: withdrawal.StatusSettled, CreatedAt: now.Add(-25 * time.Hour)}, // outside window
	}
	require.NoError(t, p.Check(q(500), recent, now))
	err := p.Check(q(501), recent, now)
	require.Equal(t, errs.CodeWithdrawalVelocityLimit, errs.CodeOf(err))
	err = p.Check(q(500), append(recent, withdrawal.Withdrawal{Quantity: q(200), Status: withdrawal.StatusApproved, CreatedAt: now}), now)
	require.Equal(t, errs.CodeWithdrawalVelocityLimit, errs.CodeOf(err), "400+200+500 > 1000")
	many := []withdrawal.Withdrawal{
		{Quantity: q(1), Status: withdrawal.StatusRequested, CreatedAt: now},
		{Quantity: q(1), Status: withdrawal.StatusRequested, CreatedAt: now},
		{Quantity: q(1), Status: withdrawal.StatusRequested, CreatedAt: now},
	}
	err = p.Check(q(1), many, now)
	require.Equal(t, errs.CodeWithdrawalVelocityLimit, errs.CodeOf(err), "count bound")
}

// --- Service fakes -------------------------------------------------------

type fakeTx struct{ calls int }

func (f *fakeTx) InTx(ctx context.Context, _ db.TxOptions, fn func(context.Context, pgx.Tx) error) error {
	f.calls++
	return fn(ctx, nil)
}

type fakeGates struct {
	active bool
	calls  []gates.Capability
}

func (g *fakeGates) RequireActive(_ context.Context, _ db.Querier, c gates.Capability) error {
	g.calls = append(g.calls, c)
	if g.active {
		return nil
	}
	return errs.Newf(errs.CodeCapabilityNotApproved, "capability %s is not active", c)
}

type fakeKill struct{ blocked bool }

func (k *fakeKill) Check(_ context.Context, _ db.Querier, a killswitch.Action) error {
	if err := a.Validate(); err != nil {
		return err
	}
	if k.blocked && a.Class == killswitch.Withdraw {
		return errs.New(errs.CodeKillSwitchActive, "WITHDRAWALS_DISABLE")
	}
	return nil
}

type fakeAccounts struct{ status accounts.Status }

func (a *fakeAccounts) Get(_ context.Context, _ db.Querier, id accounts.AccountID) (accounts.Account, error) {
	return accounts.Account{ID: id, Status: a.status}, nil
}

type memStore struct {
	mu   sync.Mutex
	rows []withdrawal.Withdrawal
}

func (m *memStore) Create(_ context.Context, _ pgx.Tx, w withdrawal.Withdrawal, ev withdrawal.TransitionEvidence) (withdrawal.Withdrawal, bool, error) {
	if err := ev.Validate(); err != nil {
		return withdrawal.Withdrawal{}, false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.rows {
		if r.IdempotencyKey == w.IdempotencyKey {
			return r, false, nil
		}
	}
	w.ID = withdrawal.NewWithdrawalID()
	w.CreatedAt = time.Now().UTC()
	m.rows = append(m.rows, w)
	return w, true, nil
}

func (m *memStore) Get(_ context.Context, _ db.Querier, id withdrawal.WithdrawalID) (withdrawal.Withdrawal, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.rows {
		if r.ID == id {
			return r, nil
		}
	}
	return withdrawal.Withdrawal{}, errs.New(errs.CodeNotFound, "not found")
}

func (m *memStore) ListRecent(_ context.Context, _ db.Querier, account accounts.AccountID, asset assets.AssetID, since time.Time) ([]withdrawal.Withdrawal, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []withdrawal.Withdrawal
	for _, r := range m.rows {
		if r.AccountID == account && r.AssetID == asset && !r.CreatedAt.Before(since) {
			out = append(out, r)
		}
	}
	return out, nil
}

func (m *memStore) Transition(context.Context, pgx.Tx, withdrawal.WithdrawalID, withdrawal.Status, withdrawal.TransitionEvidence) (withdrawal.Withdrawal, error) {
	return withdrawal.Withdrawal{}, errs.New(errs.CodeUnsupported, "not used")
}

type harness struct {
	svc      *withdrawal.Service
	tx       *fakeTx
	gates    *fakeGates
	kill     *fakeKill
	accounts *fakeAccounts
	store    *memStore
	clk      *clock.Fake
	user     accounts.UserID
	account  accounts.AccountID
	asset    assets.AssetID
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		tx: &fakeTx{}, gates: &fakeGates{}, kill: &fakeKill{}, accounts: &fakeAccounts{status: accounts.StatusActive}, store: &memStore{},
		clk: clock.NewFake(time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)), user: accounts.NewUserID(), account: accounts.NewAccountID(), asset: assets.NewAssetID(),
	}
	svc, err := withdrawal.NewService(withdrawal.Deps{
		DB: h.tx, Clock: h.clk, Store: h.store, Accounts: h.accounts, Gates: h.gates, KillSwitches: h.kill,
		Velocity: withdrawal.VelocityPolicy{MaxPerRequest: money.QuantityFromInt64(1000), Window: time.Hour, MaxCountPerWindow: 2},
	})
	require.NoError(t, err)
	h.svc = svc
	return h
}

func (h *harness) customer(stepUp bool) security.Principal {
	p := security.Principal{SubjectID: h.user.String(), ActorType: security.ActorUser, Roles: []security.Role{security.RoleCustomer}, AccountIDs: []string{h.account.String()}, AuthTime: h.clk.Now()}
	if stepUp {
		p.AMR = []string{"mfa"}
	} else {
		p.AMR = []string{"pwd"}
	}
	return p
}

func (h *harness) human(t *testing.T, p security.Principal) withdrawal.HumanActor {
	t.Helper()
	a, err := withdrawal.HumanFrom(p)
	require.NoError(t, err)
	return a
}

func (h *harness) request(key string) withdrawal.Request {
	return withdrawal.Request{
		AccountID: h.account, AssetID: h.asset, Quantity: money.QuantityFromInt64(100), DestinationType: withdrawal.DestinationExternalAddress,
		DestinationAddress: wrappedSOL, SourceWalletAddress: zeroKey, IdempotencyKey: key,
	}
}

func TestService_NoAgentPath(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	// An agent cannot even be expressed as a HumanActor.
	_, err := withdrawal.HumanFrom(security.AgentPrincipal("agent-1", h.account.String()))
	require.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	// A zero actor (the only other way to reach Request) is refused before any collaborator is touched.
	_, err = h.svc.Request(context.Background(), withdrawal.HumanActor{}, h.request("k"))
	require.Equal(t, errs.CodeUnauthenticated, errs.CodeOf(err))
	require.Zero(t, h.tx.calls)
	require.Empty(t, h.gates.calls)
	require.Empty(t, h.store.rows)
}

// TestWithdrawal_AlwaysRefusedToday: the WITHDRAWALS gate is DISABLED in
// every environment, so a fully authorized, stepped-up human request stops
// at CAPABILITY_NOT_APPROVED and nothing is persisted.
func TestWithdrawal_AlwaysRefusedToday(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	_, err := h.svc.Request(context.Background(), h.human(t, h.customer(true)), h.request("k"))
	require.Equal(t, errs.CodeCapabilityNotApproved, errs.CodeOf(err))
	require.Equal(t, []gates.Capability{gates.Withdrawals}, h.gates.calls)
	require.Equal(t, 1, h.tx.calls)
	require.Empty(t, h.store.rows)
}

func TestService_Request_CheckOrder(t *testing.T) {
	t.Parallel()
	t.Run("permission before step-up", func(t *testing.T) {
		h := newHarness(t)
		p := h.customer(false)
		p.Roles = []security.Role{security.RoleSupportReadOnly}
		p.ActorType = security.ActorOperator
		_, err := h.svc.Request(context.Background(), h.human(t, p), h.request("k"))
		require.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
		require.Zero(t, h.tx.calls)
	})
	t.Run("cross tenant", func(t *testing.T) {
		h := newHarness(t)
		p := h.customer(true)
		p.AccountIDs = []string{accounts.NewAccountID().String()}
		_, err := h.svc.Request(context.Background(), h.human(t, p), h.request("k"))
		require.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	})
	t.Run("step-up required", func(t *testing.T) {
		h := newHarness(t)
		_, err := h.svc.Request(context.Background(), h.human(t, h.customer(false)), h.request("k"))
		require.Equal(t, errs.CodeStepUpRequired, errs.CodeOf(err))
		require.Zero(t, h.tx.calls)
		stale := h.customer(true)
		stale.AuthTime = h.clk.Now().Add(-16 * time.Minute)
		_, err = h.svc.Request(context.Background(), h.human(t, stale), h.request("k"))
		require.Equal(t, errs.CodeStepUpRequired, errs.CodeOf(err))
	})
	t.Run("destination validation before the gate", func(t *testing.T) {
		h := newHarness(t)
		req := h.request("k")
		req.DestinationAddress = zeroKey
		_, err := h.svc.Request(context.Background(), h.human(t, h.customer(true)), req)
		require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
		req = h.request("k")
		req.DestinationAddress, req.SourceWalletAddress = wrappedSOL, wrappedSOL
		_, err = h.svc.Request(context.Background(), h.human(t, h.customer(true)), req)
		require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "destination must not be the source wallet")
		req = h.request("k")
		req.DestinationType = withdrawal.DestinationInternalAccount
		_, err = h.svc.Request(context.Background(), h.human(t, h.customer(true)), req)
		require.Equal(t, errs.CodeUnsupported, errs.CodeOf(err))
		req = h.request("k")
		req.Quantity = money.QuantityFromInt64(0)
		_, err = h.svc.Request(context.Background(), h.human(t, h.customer(true)), req)
		require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
		require.Zero(t, h.tx.calls)
		require.Empty(t, h.gates.calls)
	})
	t.Run("hypothetical active gate: kill switch, account status, velocity, persistence", func(t *testing.T) {
		h := newHarness(t)
		h.gates.active = true
		h.kill.blocked = true
		_, err := h.svc.Request(context.Background(), h.human(t, h.customer(true)), h.request("k1"))
		require.Equal(t, errs.CodeKillSwitchActive, errs.CodeOf(err))
		h.kill.blocked = false
		h.accounts.status = accounts.StatusFrozen
		_, err = h.svc.Request(context.Background(), h.human(t, h.customer(true)), h.request("k1"))
		require.Equal(t, errs.CodeAccountFrozen, errs.CodeOf(err))
		h.accounts.status = accounts.StatusActive
		big := h.request("k1")
		big.Quantity = money.QuantityFromInt64(1001)
		_, err = h.svc.Request(context.Background(), h.human(t, h.customer(true)), big)
		require.Equal(t, errs.CodeWithdrawalVelocityLimit, errs.CodeOf(err))
		require.Empty(t, h.store.rows)

		w, err := h.svc.Request(context.Background(), h.human(t, h.customer(true)), h.request("k1"))
		require.NoError(t, err)
		require.Equal(t, withdrawal.StatusRequested, w.Status)
		require.True(t, w.DestinationValidated)
		require.Equal(t, h.user, w.RequestedByUserID)
		require.Equal(t, "gate:WITHDRAWALS", w.CapabilityCheckRef)
		again, err := h.svc.Request(context.Background(), h.human(t, h.customer(true)), h.request("k1"))
		require.NoError(t, err)
		require.Equal(t, w.ID, again.ID, "idempotent")
		_, err = h.svc.Request(context.Background(), h.human(t, h.customer(true)), h.request("k2"))
		require.NoError(t, err)
		_, err = h.svc.Request(context.Background(), h.human(t, h.customer(true)), h.request("k3"))
		require.Equal(t, errs.CodeWithdrawalVelocityLimit, errs.CodeOf(err), "count per window")
	})
}

func TestNewService_Validation(t *testing.T) {
	t.Parallel()
	_, err := withdrawal.NewService(withdrawal.Deps{})
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	h := newHarness(t)
	_, err = withdrawal.NewService(withdrawal.Deps{DB: h.tx, Clock: h.clk, Store: h.store, Accounts: h.accounts, Gates: h.gates, KillSwitches: h.kill, Velocity: withdrawal.VelocityPolicy{MaxPerWindow: money.QuantityFromInt64(1)}})
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
}
