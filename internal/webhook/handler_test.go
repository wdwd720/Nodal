package webhook

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
)

func TestClassifyVerifyError(t *testing.T) {
	t.Parallel()
	kind, _ := classifyVerifyError(errs.Wrap(ErrSignatureInvalid, errs.CodeWebhookSignatureInvalid, "x"))
	require.Equal(t, SecurityEventSignatureFailed, kind)
	kind, _ = classifyVerifyError(errs.Wrap(ErrTimestampOutOfTolerance, errs.CodeWebhookSignatureInvalid, "x"))
	require.Equal(t, SecurityEventTimestampStale, kind)
	kind, _ = classifyVerifyError(errs.Wrap(ErrMalformed, errs.CodeValidationFailed, "x"))
	require.Equal(t, SecurityEventMalformed, kind)
	kind, _ = classifyVerifyError(errors.New("anything else"))
	require.Equal(t, SecurityEventSignatureFailed, kind, "unknown verifier errors fail closed as signature failures")
}

func TestRemoteIP(t *testing.T) {
	t.Parallel()
	require.Equal(t, "10.0.0.1", remoteIP("10.0.0.1:1234"))
	require.Equal(t, "2001:db8::1", remoteIP("[2001:db8::1]:443"))
	require.Equal(t, "10.0.0.2", remoteIP("10.0.0.2"))
	require.Equal(t, "", remoteIP("not-an-ip"))
	require.Equal(t, "", remoteIP("fe80::1%eth0"))
}

func TestSafeReasonAndHelpers(t *testing.T) {
	t.Parallel()
	require.Equal(t, "", safeReason(nil))
	require.Equal(t, "VALIDATION_FAILED: bad", safeReason(errs.New(errs.CodeValidationFailed, "bad")))
	require.Equal(t, "plain", safeReason(errors.New("plain")))
	require.Len(t, truncate(string(make([]byte, 5000)), 1000), 1000)
	require.Equal(t, "webhooks/stripe/evt_1/0123456789abcdef.json", archiveKey(Identity{Provider: "stripe", EventID: "evt_1"}, "0123456789abcdef0123"))
	require.Nil(t, nullTime(time.Time{}))
	require.NotNil(t, nullTime(time.Now()))
}

type fakeEvent struct{ id Identity }

func (e fakeEvent) WebhookIdentity() Identity { return e.id }

type nopVerifier struct{ name string }

func (v nopVerifier) Name() string { return v.name }
func (v nopVerifier) ParseWebhook(context.Context, []byte, http.Header) (fakeEvent, error) {
	return fakeEvent{}, nil
}

func TestNewHandler_Validation(t *testing.T) {
	t.Parallel()
	_, err := NewHandler[fakeEvent](Config[fakeEvent]{})
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	_, err = NewHandler[fakeEvent](Config[fakeEvent]{Verifier: nopVerifier{}})
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "every collaborator is required")
}
