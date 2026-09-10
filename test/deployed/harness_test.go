//go:build deployed

package deployed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/provider/stripesig"
)

var (
	baseURL       = strings.TrimRight(os.Getenv("NODAL_DEPLOYED_URL"), "/")
	hookSecret    = os.Getenv("NODAL_WEBHOOK_SECRET")
	deployedDBURL = os.Getenv("NODAL_DEPLOYED_DB_URL")
)

// webhookPath is the route the provider is configured to deliver to. It is the
// adapter's own name, and getting it wrong is a 404 nobody notices -- see
// test/infra.
const webhookPath = "/v1/webhooks/stripe_credit"

func requireTarget(t *testing.T) string {
	t.Helper()
	if baseURL == "" {
		t.Skip("set NODAL_DEPLOYED_URL to run against a live deployment")
	}
	return baseURL
}

func requireSecret(t *testing.T) string {
	t.Helper()
	requireTarget(t)
	if hookSecret == "" {
		t.Skip("set NODAL_WEBHOOK_SECRET to exercise signature verification")
	}
	return hookSecret
}

func requireDeployedDB(t *testing.T) *db.DB {
	t.Helper()
	requireTarget(t)
	if deployedDBURL == "" {
		t.Skip("set NODAL_DEPLOYED_DB_URL to assert against the deployment's own database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	d, err := db.Open(ctx, db.Config{
		URL: deployedDBURL, AppName: "deployed-verification", MaxConns: 4,
		RequireTLS: strings.Contains(deployedDBURL, "verify-"),
	})
	require.NoError(t, err, "opening the deployment's database")
	t.Cleanup(d.Close)
	return d
}

// client is deliberately not http.DefaultClient: a deployment that hangs
// should fail a test rather than a whole run.
var client = &http.Client{Timeout: 30 * time.Second}

// response is what a delivery came back as.
//
// Only the status code is here, and that is the deployment's contract rather
// than a limitation of this file: the generated OpenAPI server answers a
// webhook with a status and no body. Which is right -- Stripe reads the code
// and nothing else, and a body would be a second account of the outcome that
// could disagree with the recorded one.
//
// So the outcome is read from the database instead, which is the stronger
// evidence anyway: what the deployment durably recorded, not what it said.
type response struct {
	Status int
	Raw    string
}

// post sends one delivery. header is the Stripe-Signature value to send, or
// "" to send none at all.
func post(t *testing.T, path string, body []byte, header string) response {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, requireTarget(t)+path, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	if header != "" {
		req.Header.Set(stripesig.Header, header)
	}

	res, err := client.Do(req)
	require.NoError(t, err, "the deployment must answer")
	defer res.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<16))
	require.NoError(t, err)

	return response{Status: res.StatusCode, Raw: string(raw)}
}

// deliver signs body with the deployment's real webhook secret and posts it.
func deliver(t *testing.T, body []byte, signedAt time.Time) response {
	t.Helper()
	return post(t, webhookPath, body, stripesig.HeaderValue(requireSecret(t), body, signedAt))
}

// stripeEvent builds a Stripe event envelope of the given type around a
// PaymentIntent. It is a real Stripe shape, not a fixture invented here: the
// adapter parses it with the same code that parses a delivery from Stripe.
func stripeEvent(id, eventType string, pi map[string]any) []byte {
	b, err := json.Marshal(map[string]any{
		"id":               id,
		"object":           "event",
		"api_version":      "2025-08-27.basil",
		"created":          time.Now().Unix(),
		"livemode":         false,
		"pending_webhooks": 1,
		"type":             eventType,
		"data":             map[string]any{"object": pi},
	})
	if err != nil {
		panic(err)
	}
	return b
}

// paymentIntent is a PaymentIntent as Stripe renders one, carrying the Nodal
// metadata the adapter requires to recognise a delivery as its own.
func paymentIntent(id, status string, amountMinor int64, meta map[string]string) map[string]any {
	m := map[string]any{}
	for k, v := range meta {
		m[k] = v
	}
	return map[string]any{
		"id":       id,
		"object":   "payment_intent",
		"amount":   amountMinor,
		"currency": "usd",
		"status":   status,
		"metadata": m,
		"created":  time.Now().Unix(),
	}
}

// uniqueEventID keeps deliveries from colliding with earlier runs, which
// matters because the inbox is durable: a repeat of a previous run's event id
// is legitimately a duplicate.
func uniqueEventID(prefix string) string {
	return fmt.Sprintf("evt_%s_%d", prefix, time.Now().UnixNano())
}

// signWith renders a Stripe-Signature header with an arbitrary secret and
// time, so a test can be wrong on purpose.
func signWith(t *testing.T, secret string, body []byte, at time.Time) string {
	t.Helper()
	return stripesig.HeaderValue(secret, body, at)
}

// recorded is what the deployment durably wrote about one delivery: the
// provider_events row it keeps for reconciliation, and the inbox row that
// makes a repeat a repeat.
type recorded struct {
	Events            int
	InboxRows         int
	ProcessingStatus  string
	SignatureVerified bool
	RawRef            string
	InboxStatus       string
}

// record reads what the deployment wrote for one provider event id.
func record(t *testing.T, d *db.DB, eventID string) recorded {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var r recorded
	require.NoError(t, d.QueryRow(ctx, `
		SELECT count(*),
		       coalesce(max(processing_status), ''),
		       coalesce(bool_or(signature_verified), false),
		       coalesce(max(raw_ref), '')
		  FROM provider_events
		 WHERE provider = 'stripe_credit' AND provider_event_id = $1`, eventID).
		Scan(&r.Events, &r.ProcessingStatus, &r.SignatureVerified, &r.RawRef))

	require.NoError(t, d.QueryRow(ctx, `
		SELECT count(*), coalesce(max(status), '')
		  FROM inbox_messages
		 WHERE source = 'stripe_credit' AND message_id = $1`, eventID).
		Scan(&r.InboxRows, &r.InboxStatus))
	return r
}

// evidenceBody fetches the archived object a raw_ref points at. The ref is a
// pg://provider_evidence/<key> URI on this tier.
func evidenceBody(t *testing.T, d *db.DB, rawRef string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	key := strings.TrimPrefix(rawRef, "pg://provider_evidence/")
	require.NotEqual(t, rawRef, key, "raw_ref %q is not a pg:// evidence reference", rawRef)

	var body []byte
	require.NoError(t, d.QueryRow(ctx,
		`SELECT body FROM provider_evidence WHERE key = $1`, key).Scan(&body))
	return body
}

// evidenceKey turns a pg:// evidence reference back into the archive key.
func evidenceKey(t *testing.T, rawRef string) string {
	t.Helper()
	key := strings.TrimPrefix(rawRef, "pg://provider_evidence/")
	require.NotEqual(t, rawRef, key, "raw_ref %q is not a pg:// evidence reference", rawRef)
	return key
}

// requireOwnerDB opens a connection as the role that OWNS the tables, which is
// the migration role. Privileges do not constrain an owner, so it is the only
// connection that can show whether the guard triggers exist and fire.
func requireOwnerDB(t *testing.T) *db.DB {
	t.Helper()
	requireTarget(t)
	url := os.Getenv("NODAL_DEPLOYED_OWNER_DB_URL")
	if url == "" {
		t.Skip("set NODAL_DEPLOYED_OWNER_DB_URL to test the guards that only bind the table owner")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	d, err := db.Open(ctx, db.Config{
		URL: url, AppName: "deployed-verification-owner", MaxConns: 2,
		RequireTLS: strings.Contains(url, "verify-"),
	})
	require.NoError(t, err, "opening the deployment's database as its owner")
	t.Cleanup(d.Close)
	return d
}
