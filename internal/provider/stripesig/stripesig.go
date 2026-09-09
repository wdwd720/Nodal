// Package stripesig verifies Stripe webhook signatures.
//
// It exists because there is now more than one Stripe adapter in this
// repository -- the crypto onramp in internal/provider/stripe and the credit
// purchase adapter in internal/provider/stripecredit -- and signature
// verification is the one piece of a webhook path that must not be written
// twice. A second implementation is a second chance to compare with ==, to
// forget the tolerance check, or to accept a scheme Stripe has deprecated, and
// the failure mode is that forged events are believed.
//
// The scheme is Stripe's documented one and nothing else:
//
//	Stripe-Signature: t=<unix seconds>,v1=<hex HMAC-SHA256 of "<t>.<raw body>">
//
// Only v1 is accepted. Several v1 values may be present while a secret is
// being rolled and any one matching is sufficient. Comparison is
// hmac.Equal. The signed timestamp must be within tolerance of now in both
// directions -- a signature from the future is as suspicious as a stale one.
package stripesig

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/webhook"
)

// Header is the header Stripe signs deliveries with.
const Header = "Stripe-Signature"

// DefaultTolerance is Stripe's documented replay window.
const DefaultTolerance = 300 * time.Second

// maxHeader bounds the header length. A handful of v1 values is a few hundred
// bytes; anything approaching this is not a rolling secret.
const maxHeader = 4096

// Sign computes the v1 signature of raw at signedAt with secret: the hex
// HMAC-SHA256 of "<unix seconds>.<raw>".
//
// It is exported for fakes and for contract fixtures. Nothing in a production
// path signs a Stripe webhook, because Stripe does.
func Sign(secret string, raw []byte, signedAt time.Time) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(signedAt.Unix(), 10)))
	mac.Write([]byte("."))
	mac.Write(raw)
	return hex.EncodeToString(mac.Sum(nil))
}

// HeaderValue renders a Stripe-Signature value for raw.
func HeaderValue(secret string, raw []byte, signedAt time.Time) string {
	return "t=" + strconv.FormatInt(signedAt.Unix(), 10) + ",v1=" + Sign(secret, raw, signedAt)
}

// Failure wraps a webhook sentinel with the stable code and a machine-readable
// reason, so that a rejected delivery can be counted by cause without parsing
// a message.
func Failure(cause error, reason string) error {
	return errs.Wrap(cause, errs.CodeWebhookSignatureInvalid, "stripe: "+reason).WithField("reason", reason)
}

// Verify parses the Stripe-Signature header, verifies at least one v1
// signature against secret in constant time, then enforces tolerance on the
// signed timestamp. It returns the signed time on success.
//
// Schemes other than v1 are ignored rather than rejected: Stripe may add one,
// and a delivery that carries a valid v1 alongside something new is still
// valid.
func Verify(headers http.Header, raw []byte, secret string, now time.Time, tolerance time.Duration) (time.Time, error) {
	header := headers.Get(Header)
	switch {
	case header == "":
		return time.Time{}, Failure(webhook.ErrSignatureInvalid, "missing signature header")
	case len(header) > maxHeader:
		return time.Time{}, Failure(webhook.ErrSignatureInvalid, "signature header too long")
	case secret == "":
		return time.Time{}, Failure(webhook.ErrSignatureInvalid, "webhook secret not configured")
	}
	if tolerance <= 0 {
		tolerance = DefaultTolerance
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
				return time.Time{}, Failure(webhook.ErrSignatureInvalid, "signature timestamp is not an integer")
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
		return time.Time{}, Failure(webhook.ErrSignatureInvalid, "signature timestamp missing")
	}
	if len(v1s) == 0 {
		return time.Time{}, Failure(webhook.ErrSignatureInvalid, "no v1 signature present")
	}
	signedAt := time.Unix(ts, 0).UTC()
	expected, _ := hex.DecodeString(Sign(secret, raw, signedAt))
	matched := false
	for _, sig := range v1s {
		// Every candidate is compared, and the loop does not break early. An
		// early return here would make the number of comparisons depend on
		// which secret matched.
		if hmac.Equal(sig, expected) {
			matched = true
		}
	}
	if !matched {
		return time.Time{}, Failure(webhook.ErrSignatureInvalid, "no v1 signature matches")
	}
	if delta := now.Sub(signedAt); delta > tolerance || delta < -tolerance {
		return time.Time{}, Failure(webhook.ErrTimestampOutOfTolerance, "timestamp outside tolerance")
	}
	return signedAt, nil
}
