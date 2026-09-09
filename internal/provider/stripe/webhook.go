package stripe

import (
	"net/http"
	"time"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/funding"
	"github.com/nodal/controlplane/internal/provider/stripesig"
	"github.com/nodal/controlplane/internal/webhook"
)

// SignatureHeader is the header Stripe signs deliveries with.
const SignatureHeader = stripesig.Header

// Sign computes the v1 signature of raw at signedAt with secret: the hex
// HMAC-SHA256 of "<unix seconds>.<raw>". Exported for the fake mode and
// the contract fixtures.
//
// The implementation moved to internal/provider/stripesig when a second
// Stripe adapter appeared. Two copies of a signature verifier is two chances
// to get one wrong, and the wrong one believes forged events.
func Sign(secret string, raw []byte, signedAt time.Time) string {
	return stripesig.Sign(secret, raw, signedAt)
}

// SignatureHeaderValue renders a Stripe-Signature value for raw.
func SignatureHeaderValue(secret string, raw []byte, signedAt time.Time) string {
	return stripesig.HeaderValue(secret, raw, signedAt)
}

// verifySignature parses Stripe-Signature, verifies at least one v1
// signature against the secret in constant time, then enforces tolerance
// on the signed timestamp. Schemes other than v1 are ignored.
func verifySignature(headers http.Header, raw []byte, secret string, now time.Time, tolerance time.Duration) (time.Time, error) {
	return stripesig.Verify(headers, raw, secret, now, tolerance)
}

// parseWebhook is the mode-independent webhook path: verify, then decode,
// then check livemode against the adapter's mode.
func parseWebhook(raw []byte, headers http.Header, secret string, now time.Time, tolerance time.Duration, livemode bool) (funding.WebhookEvent, error) {
	signedAt, err := verifySignature(headers, raw, secret, now, tolerance)
	if err != nil {
		return funding.WebhookEvent{}, err
	}
	ev, err := decodeEvent(raw, signedAt)
	if err != nil {
		return funding.WebhookEvent{}, err
	}
	if ev.Identity.Livemode != livemode {
		return funding.WebhookEvent{}, errs.Wrap(webhook.ErrMalformed, errs.CodeValidationFailed, "stripe: event livemode does not match the adapter mode").
			WithField("event_livemode", ev.Identity.Livemode).WithField("adapter_livemode", livemode)
	}
	return ev, nil
}
