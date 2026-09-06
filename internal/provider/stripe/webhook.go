package stripe

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/funding"
	"github.com/nodal/controlplane/internal/webhook"
)

// SignatureHeader is the header Stripe signs deliveries with.
const SignatureHeader = "Stripe-Signature"

// maxSignatureHeader bounds the header length (a handful of v1 values).
const maxSignatureHeader = 4096

// Sign computes the v1 signature of raw at signedAt with secret: the hex
// HMAC-SHA256 of "<unix seconds>.<raw>". Exported for the fake mode and
// the contract fixtures.
func Sign(secret string, raw []byte, signedAt time.Time) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(signedAt.Unix(), 10)))
	mac.Write([]byte("."))
	mac.Write(raw)
	return hex.EncodeToString(mac.Sum(nil))
}

// SignatureHeaderValue renders a Stripe-Signature value for raw.
func SignatureHeaderValue(secret string, raw []byte, signedAt time.Time) string {
	return "t=" + strconv.FormatInt(signedAt.Unix(), 10) + ",v1=" + Sign(secret, raw, signedAt)
}

// signatureFailure wraps a pipeline sentinel with the stable code.
func signatureFailure(cause error, reason string) error {
	return errs.Wrap(cause, errs.CodeWebhookSignatureInvalid, "stripe: "+reason).WithField("reason", reason)
}

// verifySignature parses Stripe-Signature, verifies at least one v1
// signature against the secret in constant time, then enforces tolerance
// on the signed timestamp. Schemes other than v1 are ignored.
func verifySignature(headers http.Header, raw []byte, secret string, now time.Time, tolerance time.Duration) (time.Time, error) {
	header := headers.Get(SignatureHeader)
	switch {
	case header == "":
		return time.Time{}, signatureFailure(webhook.ErrSignatureInvalid, "missing signature header")
	case len(header) > maxSignatureHeader:
		return time.Time{}, signatureFailure(webhook.ErrSignatureInvalid, "signature header too long")
	case secret == "":
		return time.Time{}, signatureFailure(webhook.ErrSignatureInvalid, "webhook secret not configured")
	}
	var (
		ts   int64
		seen bool
		v1s  [][]byte
	)
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n <= 0 {
				return time.Time{}, signatureFailure(webhook.ErrSignatureInvalid, "signature timestamp is not an integer")
			}
			ts, seen = n, true
		case "v1":
			sig, err := hex.DecodeString(v)
			if err == nil && len(sig) == sha256.Size {
				v1s = append(v1s, sig)
			}
		}
	}
	if !seen {
		return time.Time{}, signatureFailure(webhook.ErrSignatureInvalid, "signature timestamp missing")
	}
	if len(v1s) == 0 {
		return time.Time{}, signatureFailure(webhook.ErrSignatureInvalid, "no v1 signature present")
	}
	signedAt := time.Unix(ts, 0).UTC()
	expected, _ := hex.DecodeString(Sign(secret, raw, signedAt))
	matched := false
	for _, sig := range v1s {
		if hmac.Equal(sig, expected) {
			matched = true
		}
	}
	if !matched {
		return time.Time{}, signatureFailure(webhook.ErrSignatureInvalid, "no v1 signature matches")
	}
	if delta := now.Sub(signedAt); delta > tolerance || delta < -tolerance {
		return time.Time{}, signatureFailure(webhook.ErrTimestampOutOfTolerance, "timestamp outside tolerance")
	}
	return signedAt, nil
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
