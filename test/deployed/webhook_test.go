//go:build deployed

package deployed

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These run against the deployment's real webhook endpoint over the public
// internet, signed the way Stripe signs, and read the deployment's own
// database to check what the endpoint did with each delivery.
//
// Every delivery here is FOREIGN: a payment_intent carrying no Nodal metadata,
// which the adapter recognises as another product's payment and the pipeline
// records as IGNORED. That is deliberate. It exercises the whole path --
// signature, timestamp, evidence, inbox, dispatch -- while moving no money and
// touching no ledger, so these are safe to run against a deployment at any
// time.

// foreignIntent is a payment that is not ours: a real Stripe shape with no
// nodal_credit_purchase_id in its metadata.
func foreignIntent() map[string]any {
	return paymentIntent("pi_deployed_foreign", "succeeded", 4200, map[string]string{
		"some_other_product": "yes",
	})
}

// TestDeployed_AnUnsignedDeliveryIsRefused.
//
// The webhook endpoint is the one route with no session and no API key in
// front of it, because the signature IS the authentication. If an unsigned
// body were accepted, anyone who found the URL could mint Credits.
func TestDeployed_AnUnsignedDeliveryIsRefused(t *testing.T) {
	body := stripeEvent(uniqueEventID("unsigned"), "payment_intent.succeeded", foreignIntent())

	res := post(t, webhookPath, body, "")
	assert.Equal(t, http.StatusBadRequest, res.Status, "an unsigned delivery must be refused")
}

// TestDeployed_ADeliverySignedWithTheWrongSecretIsRefused proves the secret in
// the deployment is the secret Stripe was given, and not merely that SOME
// signature is checked.
func TestDeployed_ADeliverySignedWithTheWrongSecretIsRefused(t *testing.T) {
	requireSecret(t)
	body := stripeEvent(uniqueEventID("wrongsecret"), "payment_intent.succeeded", foreignIntent())

	// A well-formed header, correct in every respect except the key.
	res := post(t, webhookPath, body, signWith(t, "whsec_this_is_not_the_deployments_secret", body, time.Now()))
	assert.Equal(t, http.StatusBadRequest, res.Status)
}

// TestDeployed_ATamperedBodyIsRefused.
//
// The signature covers the body, so somebody who captures a real delivery and
// changes the amount must not be able to replay it. This signs one body and
// sends another.
func TestDeployed_ATamperedBodyIsRefused(t *testing.T) {
	secret := requireSecret(t)
	id := uniqueEventID("tampered")
	signed := stripeEvent(id, "payment_intent.succeeded", paymentIntent("pi_x", "succeeded", 100, nil))
	sent := stripeEvent(id, "payment_intent.succeeded", paymentIntent("pi_x", "succeeded", 100000000, nil))
	require.NotEqual(t, string(signed), string(sent))

	res := post(t, webhookPath, sent, signWith(t, secret, signed, time.Now()))
	assert.Equal(t, http.StatusBadRequest, res.Status, "a body that is not the body that was signed must be refused")
}

// TestDeployed_ASignatureOutsideToleranceIsRefused.
//
// A valid signature is valid forever unless the timestamp is checked, which is
// what turns a captured delivery into a replayable one. Both directions are
// checked: the past, and a clock claiming to be in the future.
func TestDeployed_ASignatureOutsideToleranceIsRefused(t *testing.T) {
	secret := requireSecret(t)

	for _, tc := range []struct {
		name string
		at   time.Time
	}{
		{"an hour old", time.Now().Add(-time.Hour)},
		{"an hour ahead", time.Now().Add(time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := stripeEvent(uniqueEventID("stale"), "payment_intent.succeeded", foreignIntent())
			res := post(t, webhookPath, body, signWith(t, secret, body, tc.at))
			assert.Equal(t, http.StatusBadRequest, res.Status, "a signature %s must be refused", tc.name)
		})
	}
}

// TestDeployed_AValidlySignedForeignDeliveryIsAccepted is the positive case
// the refusals are only meaningful against.
func TestDeployed_AValidlySignedForeignDeliveryIsAccepted(t *testing.T) {
	requireSecret(t)
	d := requireDeployedDB(t)

	id := uniqueEventID("accepted")
	body := stripeEvent(id, "payment_intent.succeeded", foreignIntent())

	res := deliver(t, body, time.Now())
	require.Equal(t, http.StatusOK, res.Status, "a correctly signed delivery must be accepted: %s", res.Raw)

	got := record(t, d, id)
	require.Equal(t, 1, got.Events, "an accepted delivery must be recorded once")
	assert.True(t, got.SignatureVerified)
	assert.Equal(t, "IGNORED", got.ProcessingStatus,
		"a payment with no Nodal metadata belongs to another product and must be ignored, not applied")
	assert.Equal(t, 1, got.InboxRows)
	assert.Equal(t, "PROCESSED", got.InboxStatus)

	// And the bytes that were signed are the bytes that were kept. This is the
	// evidence a dispute is argued from, so it has to be what Stripe sent and
	// not a re-serialisation of what we understood.
	assert.Equal(t, body, evidenceBody(t, d, got.RawRef),
		"the archived evidence is not the delivery that was signed")
}

// TestDeployed_ARefusedDeliveryLeavesNoTrace.
//
// A rejected delivery must not create a provider_events row. If it did, the
// row would say signature_verified = true about a delivery whose signature was
// never verified, and reconciliation would count it.
func TestDeployed_ARefusedDeliveryLeavesNoTrace(t *testing.T) {
	secret := requireSecret(t)
	d := requireDeployedDB(t)
	ctx := context.Background()

	id := uniqueEventID("notrace")
	body := stripeEvent(id, "payment_intent.succeeded", foreignIntent())

	res := post(t, webhookPath, body, signWith(t, secret, body, time.Now().Add(-time.Hour)))
	require.Equal(t, http.StatusBadRequest, res.Status)

	var n int
	require.NoError(t, d.QueryRow(ctx,
		`SELECT count(*) FROM provider_events WHERE provider_event_id = $1`, id).Scan(&n))
	assert.Zero(t, n, "a refused delivery recorded a provider event")
}

// TestDeployed_ADuplicateDeliveryIsNotAppliedTwice.
//
// Stripe retries. It retried this very endpoint every twenty seconds for an
// hour while the route was returning 404. So the same event arriving twice has
// to be recognised, and the recognition has to survive the process restarting,
// which means it lives in the database rather than in memory.
func TestDeployed_ADuplicateDeliveryIsNotAppliedTwice(t *testing.T) {
	requireSecret(t)
	d := requireDeployedDB(t)
	ctx := context.Background()

	id := uniqueEventID("duplicate")
	body := stripeEvent(id, "payment_intent.succeeded", foreignIntent())

	first := deliver(t, body, time.Now())
	require.Equal(t, http.StatusOK, first.Status, "first delivery: %s", first.Raw)
	afterFirst := record(t, d, id)
	require.Equal(t, 1, afterFirst.Events)
	require.Equal(t, "PROCESSED", afterFirst.InboxStatus)

	second := deliver(t, body, time.Now())
	require.Equal(t, http.StatusOK, second.Status, "a duplicate must be acknowledged, or Stripe retries forever")

	afterSecond := record(t, d, id)
	assert.Equal(t, 1, afterSecond.Events, "two deliveries of one event produced %d provider_events rows", afterSecond.Events)
	assert.Equal(t, 1, afterSecond.InboxRows, "the second delivery was recorded as a new message")
	assert.Equal(t, afterFirst.RawRef, afterSecond.RawRef, "the evidence reference changed on a replay")
	assert.Equal(t, afterFirst.ProcessingStatus, afterSecond.ProcessingStatus,
		"the replay changed what the first delivery was recorded as having done")

	// The count of evidence objects for this key is the other half: a replay
	// must not archive a second copy of the same bytes under a new id.
	var objects int
	require.NoError(t, d.QueryRow(ctx,
		`SELECT count(*) FROM provider_evidence WHERE key = $1`,
		strings.TrimPrefix(afterSecond.RawRef, "pg://provider_evidence/")).Scan(&objects))
	assert.Equal(t, 1, objects, "a replayed delivery archived a second copy")
}

// TestDeployed_AnEventIDReusedWithADifferentPayloadIsRefused.
//
// The inbox keys on the event id, so somebody who knows one could otherwise
// smuggle a different payload under an id already seen -- or, far more likely,
// a provider bug could reuse an id and quietly overwrite what was recorded.
// The payload hash is part of the key, and a mismatch is a refusal rather than
// a duplicate.
func TestDeployed_AnEventIDReusedWithADifferentPayloadIsRefused(t *testing.T) {
	requireSecret(t)

	id := uniqueEventID("reused")
	first := stripeEvent(id, "payment_intent.succeeded", paymentIntent("pi_reuse_a", "succeeded", 1000, nil))
	second := stripeEvent(id, "payment_intent.succeeded", paymentIntent("pi_reuse_b", "succeeded", 900000, nil))

	res := deliver(t, first, time.Now())
	require.Equal(t, http.StatusOK, res.Status, "first delivery: %s", res.Raw)

	res = deliver(t, second, time.Now())
	assert.Equal(t, http.StatusBadRequest, res.Status,
		"an event id reused with a different payload must be refused, not treated as a duplicate")
}

// TestDeployed_TheWebhookRouteRefusesEverythingButPOST.
//
// A GET that answered would make the endpoint fetchable, and an endpoint that
// answers a GET is an endpoint someone will point a browser at.
func TestDeployed_TheWebhookRouteRefusesEverythingButPOST(t *testing.T) {
	requireTarget(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		t.Run(method, func(t *testing.T) {
			req, err := http.NewRequestWithContext(ctx, method, baseURL+webhookPath, nil)
			require.NoError(t, err)
			res, err := client.Do(req)
			require.NoError(t, err)
			defer res.Body.Close()
			assert.NotEqual(t, http.StatusOK, res.StatusCode, "%s must not be served", method)
		})
	}
}
