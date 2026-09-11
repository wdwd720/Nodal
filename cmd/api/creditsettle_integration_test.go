//go:build integration

package main

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/credit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/ledger"
)

// The sandbox tier's settlement window against the real table (D-086).
//
// The unit test holds creditSettlement to its two numbers. This holds the
// window to the thing it is for: a captured Credit purchase on a sandbox tier
// reaches SETTLED, which is the only finality any payout policy in this build
// will let leave, in minutes rather than in a card chargeback window.

// aReversibleFunding writes a captured purchase whose reversibility window
// started `age` ago. The state and reversible_at are what SettleDue reads.
func aReversibleFunding(t *testing.T, d *db.DB, acct accounts.AccountID, age time.Duration) string {
	t.Helper()
	ctx := context.Background()
	fundingID := id.New[id.Any]().String()
	_, err := d.Exec(ctx, `INSERT INTO credit_fundings
		(id, account_id, provider, state, credit_quantity, paid_amount_minor, idempotency_key)
		VALUES ($1::uuid, $2, 'test_provider', 'CREATED', 1000, 500, $3)`,
		fundingID, acct, "idem-"+fundingID)
	require.NoError(t, err)
	// The age is carried on the transition's own occurred_at, because that is
	// what 00743's trigger stamps reversible_at from -- and cp_app holds no
	// UPDATE on credit_fundings at all, so there is no other way to write it
	// and no way for a test to fake one. A window is only interesting once
	// time has passed, and a test must not wait for a card scheme.
	for _, edge := range [][2]string{{"CREATED", "CAPTURED"}, {"CAPTURED", "REVERSIBLE"}} {
		_, err = d.Exec(ctx, `INSERT INTO credit_funding_transitions
			(id, funding_id, from_state, to_state, actor_type, actor_id, reason, occurred_at)
			VALUES ($1::uuid, $2::uuid, $3, $4, 'SYSTEM', 'itest', 'settlement window fixture',
			        now() - make_interval(secs => $5))`,
			id.New[id.Any]().String(), fundingID, edge[0], edge[1], age.Seconds())
		require.NoError(t, err)
	}
	return fundingID
}

func fundingStateOf(t *testing.T, d *db.DB, fundingID string) string {
	t.Helper()
	var state string
	require.NoError(t, d.QueryRow(context.Background(),
		`SELECT state FROM credit_fundings WHERE id = $1::uuid`, fundingID).Scan(&state))
	return state
}

// TestIntegration_ASandboxTierSettlesInMinutesAndAChargebackWindowDoesNot.
func TestIntegration_ASandboxTierSettlesInMinutesAndAChargebackWindowDoesNot(t *testing.T) {
	d := openNotificationsDB(t)
	ctx := context.Background()
	clk := clock.NewFake(time.Now().UTC().Truncate(time.Microsecond))

	repo := accounts.NewRepository()
	user, err := repo.CreateUser(ctx, d, "settle-itest", "sub-"+id.New[id.Any]().String(), nil)
	require.NoError(t, err)
	acct, err := repo.CreateAccount(ctx, d, user.ID, accounts.KindCustomer)
	require.NoError(t, err)

	svc, err := credit.NewPurchaseService(credit.PurchaseServiceConfig{
		Credits: credit.NewService(ledger.NewService(clk, "settle-itest"), clk),
		// SettleDue touches neither of these -- it is one bounded UPDATE over
		// rows whose window has closed -- but the constructor requires both,
		// correctly: a purchase service that could be built without a gate
		// would be one that could sell Credits without one.
		Provider: settleTestProvider{}, Gates: settleTestGate{},
		Pricing: credit.DefaultPricingPolicy(), Clock: clk, Environment: "TEST",
	})
	require.NoError(t, err)

	// Five minutes old: past a sandbox tier's two-minute window and nowhere
	// near a card chargeback window.
	funding := aReversibleFunding(t, d, acct.ID, 5*time.Minute)
	require.Equal(t, "REVERSIBLE", fundingStateOf(t, d, funding))

	// A deployment on its real window leaves it exactly where it is. This is
	// the state STAGING was stuck in: correct, and unwithdrawable forever.
	staging := &config.Config{Env: config.EnvStaging}
	staging.Credit.SettlementWindow = 720 * time.Hour
	window, _ := creditSettlement(staging)
	settleOnce(ctx, d, svc, window, quietLogger())
	assert.Equal(t, "REVERSIBLE", fundingStateOf(t, d, funding),
		"a captured payment is genuinely reversible for its chargeback window")

	// The same deployment declared a sandbox tier settles it.
	sandbox := &config.Config{Env: config.EnvStaging}
	sandbox.API.LegalPolicy = config.LegalPolicySandbox
	sandbox.Credit.SettlementWindow = 720 * time.Hour
	window, interval := creditSettlement(sandbox)
	require.Equal(t, sandboxSettleWait, window)
	require.Equal(t, sandboxSettleInterval, interval)
	settleOnce(ctx, d, svc, window, quietLogger())
	assert.Equal(t, "SETTLED", fundingStateOf(t, d, funding),
		"only SETTLED value is payout-eligible, so this is what makes a rehearsal withdrawal reachable")

	// And a purchase inside even the short window stays put: the state matters
	// and a rehearsal that skipped REVERSIBLE would rehearse the wrong thing.
	fresh := aReversibleFunding(t, d, acct.ID, 10*time.Second)
	settleOnce(ctx, d, svc, window, quietLogger())
	assert.Equal(t, "REVERSIBLE", fundingStateOf(t, d, fresh))

	// The sweep is idempotent: a second pass over a settled funding moves
	// nothing, which is what lets a worker tier be added later with no
	// coordination.
	settleOnce(ctx, d, svc, window, quietLogger())
	assert.Equal(t, "SETTLED", fundingStateOf(t, d, funding))
	var settledTransitions int
	require.NoError(t, d.QueryRow(ctx,
		`SELECT count(*) FROM credit_funding_transitions WHERE funding_id = $1::uuid AND to_state = 'SETTLED'`,
		funding).Scan(&settledTransitions))
	assert.Equal(t, 1, settledTransitions, "one settlement, one transition row")
}

// settleTestProvider and settleTestGate are the two collaborators
// NewPurchaseService requires and SettleDue never calls. Both panic rather than
// returning a zero value, so a future change that made the settlement sweep
// talk to a provider fails loudly here instead of quietly inventing an answer.
type settleTestProvider struct{}

func (settleTestProvider) Name() string { return "settle-itest" }

func (settleTestProvider) Capabilities() credit.PurchaseCapabilities {
	return credit.PurchaseCapabilities{}
}

func (settleTestProvider) CreatePurchase(context.Context, credit.CreatePurchaseRequest) (credit.PurchaseSession, error) {
	panic("the settlement sweep must not open a payment")
}

func (settleTestProvider) GetPurchase(context.Context, string) (credit.PurchaseSnapshot, error) {
	panic("the settlement sweep must not call the provider")
}

func (settleTestProvider) ParseWebhook(context.Context, []byte, http.Header) (credit.PurchaseEvent, error) {
	panic("the settlement sweep parses no webhook")
}

type settleTestGate struct{}

func (settleTestGate) RequireActive(context.Context, db.Querier, gates.Capability) error {
	panic("the settlement sweep checks no gate; it closes a window that already opened")
}
