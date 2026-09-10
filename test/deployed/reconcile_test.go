//go:build deployed

package deployed

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Reconciliation against the provider's own record, in both directions.
//
// One direction is already covered by internal/credit: given a funding, ask
// Stripe what it thinks and adopt that answer. The direction that needs a live
// account is the other one -- a charge that exists at the provider and has no
// funding row here. Nothing internal can find that, because the whole point is
// that we have no record of it.
//
// It matters most on an account that serves more than one product. A Nodal
// deployment reading another product's PaymentIntent must ignore it, and a
// PaymentIntent carrying Nodal metadata that this deployment has never heard of
// is either a charge somebody made against our account or evidence that a
// delivery was lost. Both are worth stopping for.

const stripeAPI = "https://api.stripe.com/v1"

func requireStripeKey(t *testing.T) string {
	t.Helper()
	key := strings.TrimSpace(os.Getenv("NODAL_STRIPE_API_KEY"))
	if key == "" {
		t.Skip("set NODAL_STRIPE_API_KEY (the sandbox secret key) to reconcile against Stripe")
	}
	require.True(t, strings.HasPrefix(key, "sk_test_") || strings.HasPrefix(key, "rk_test_"),
		"this test reconciles a sandbox; a live key here would read a live account")
	return key
}

// stripePaymentIntent is the part of a PaymentIntent reconciliation reads.
type stripePaymentIntent struct {
	ID       string            `json:"id"`
	Status   string            `json:"status"`
	Amount   int64             `json:"amount"`
	Currency string            `json:"currency"`
	Metadata map[string]string `json:"metadata"`
}

// listPaymentIntents pages the sandbox's PaymentIntents.
func listPaymentIntents(t *testing.T, key string) []stripePaymentIntent {
	t.Helper()
	var all []stripePaymentIntent
	after := ""

	for page := 0; page < 20; page++ { // 2000 objects is far past this tier's ceilings
		q := url.Values{"limit": {"100"}}
		if after != "" {
			q.Set("starting_after", after)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, stripeAPI+"/payment_intents?"+q.Encode(), nil)
		require.NoError(t, err)
		req.SetBasicAuth(key, "")

		res, err := client.Do(req)
		require.NoError(t, err)

		var body struct {
			Data    []stripePaymentIntent `json:"data"`
			HasMore bool                  `json:"has_more"`
			Error   *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		err = json.NewDecoder(res.Body).Decode(&body)
		res.Body.Close()
		cancel()
		require.NoError(t, err)
		require.Nil(t, body.Error, "Stripe refused the request")
		require.Equal(t, http.StatusOK, res.StatusCode)

		all = append(all, body.Data...)
		if !body.HasMore || len(body.Data) == 0 {
			break
		}
		after = body.Data[len(body.Data)-1].ID
	}
	return all
}

// TestDeployed_NoChargeAtTheProviderIsUnknownHere.
//
// Every PaymentIntent in the account that claims to be ours -- it carries
// nodal_credit_purchase_id and this deployment's environment -- must have a
// funding row. One that does not means either a charge was made against the
// account outside this system, or a delivery was lost and Credits were never
// issued for money that was taken. Both are stop-everything findings.
//
// PaymentIntents belonging to another product are expected and are not
// findings: the adapter ignores them, and this test does too.
func TestDeployed_NoChargeAtTheProviderIsUnknownHere(t *testing.T) {
	key := requireStripeKey(t)
	d := requireDeployedDB(t)
	ctx := context.Background()

	intents := listPaymentIntents(t, key)

	var ours, foreign int
	for _, pi := range intents {
		fundingID := pi.Metadata["nodal_credit_purchase_id"]
		if fundingID == "" {
			foreign++
			continue
		}
		ours++

		var (
			state  string
			minor  int64
			exists bool
		)
		err := d.QueryRow(ctx, `
			SELECT true, state, paid_amount_minor
			  FROM credit_fundings
			 WHERE provider_reference = $1`, pi.ID).Scan(&exists, &state, &minor)
		require.NoError(t, err,
			"Stripe has payment intent %s carrying Nodal metadata (funding %s, %d %s, status %s) "+
				"and this deployment has no funding row for it",
			pi.ID, fundingID, pi.Amount, pi.Currency, pi.Status)

		assert.Equal(t, pi.Amount, minor,
			"%s: Stripe charged %d and the funding records %d", pi.ID, pi.Amount, minor)
	}

	t.Logf("reconciled %d payment intents: %d ours, %d belonging to another product", len(intents), ours, foreign)
}

// TestDeployed_NoFundingHereIsUnknownAtTheProvider is the mirror.
//
// A funding row that has a provider reference Stripe has never heard of means
// this deployment believes it started a payment that does not exist. Under the
// money-at-risk ceiling it also consumes headroom that nothing will ever
// release.
//
// Fundings with no provider reference are excluded: they are purchases that
// were never handed to the provider, which is the state a lost response leaves
// and is exactly what internal/credit's Reconcile resolves.
func TestDeployed_NoFundingHereIsUnknownAtTheProvider(t *testing.T) {
	key := requireStripeKey(t)
	d := requireDeployedDB(t)
	ctx := context.Background()

	known := map[string]stripePaymentIntent{}
	for _, pi := range listPaymentIntents(t, key) {
		known[pi.ID] = pi
	}

	rows, err := d.Query(ctx, `
		SELECT provider_reference, state, paid_amount_minor
		  FROM credit_fundings
		 WHERE provider_reference <> ''
		 ORDER BY created_at`)
	require.NoError(t, err)
	defer rows.Close()

	var checked int
	for rows.Next() {
		var ref, state string
		var minor int64
		require.NoError(t, rows.Scan(&ref, &state, &minor))
		checked++

		pi, ok := known[ref]
		require.True(t, ok,
			"funding references payment intent %s in state %s for %d minor units, and Stripe has no such object",
			ref, state, minor)
		assert.Equal(t, minor, pi.Amount,
			"%s: the funding records %d and Stripe charged %d", ref, minor, pi.Amount)
	}
	require.NoError(t, rows.Err())

	t.Logf("reconciled %d fundings against the provider", checked)
}

// TestDeployed_TheMoneyAtRiskCeilingSeesWhatStripeSees.
//
// The ceiling that bounds what a failure can cost is measured from funding
// states in this database. If Stripe holds money the database does not know
// about, that ceiling is measuring the wrong number -- and it is the ceiling
// the whole free tier depends on.
func TestDeployed_TheMoneyAtRiskCeilingSeesWhatStripeSees(t *testing.T) {
	key := requireStripeKey(t)
	d := requireDeployedDB(t)
	ctx := context.Background()

	// What Stripe is holding for us: anything ours that has taken money and
	// has not been refunded or cancelled.
	var atStripe int64
	for _, pi := range listPaymentIntents(t, key) {
		if pi.Metadata["nodal_credit_purchase_id"] == "" {
			continue
		}
		switch pi.Status {
		case "canceled", "requires_payment_method":
			continue
		}
		atStripe += pi.Amount
	}

	var atRisk int64
	require.NoError(t, d.QueryRow(ctx, `
		SELECT coalesce(sum(paid_amount_minor), 0)::bigint
		  FROM credit_fundings
		 WHERE state <> ALL($1)`, []string{"SETTLED", "REVERSED", "REFUNDED", "FAILED", "CANCELED"}).Scan(&atRisk))

	// Settled money is deliberately not at risk here but is still money Stripe
	// holds, so the ceiling's number may be lower. It must never be higher
	// than what the provider says exists.
	assert.LessOrEqual(t, atRisk, atStripe,
		"the money-at-risk ceiling counts %d minor units and Stripe only knows about %d", atRisk, atStripe)
}
