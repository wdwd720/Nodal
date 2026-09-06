package solanarpc

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestIsIntegerToken(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"0", "1", "-1", "123456789012345678901234567890"} {
		require.True(t, isIntegerToken(ok), ok)
	}
	for _, bad := range []string{"", "-", "1.0", "1e5", "+1", " 1", "0x10", "١"} {
		require.False(t, isIntegerToken(bad), bad)
	}
}

func TestBackoffDelay(t *testing.T) {
	t.Parallel()
	for attempt := 1; attempt <= 6; attempt++ {
		for i := 0; i < 50; i++ {
			d := backoffDelay(attempt, 100*time.Millisecond, time.Second)
			require.GreaterOrEqual(t, d, time.Duration(0))
			require.Less(t, d, time.Second+time.Nanosecond)
			capped := 100 * time.Millisecond << (attempt - 1)
			if capped > time.Second {
				capped = time.Second
			}
			require.LessOrEqual(t, d, capped)
		}
	}
	require.Equal(t, time.Duration(0), backoffDelay(1, 0, 0))
	require.Equal(t, int64(0), cryptoJitter(0))
	require.Less(t, cryptoJitter(10), int64(10))
}

func TestRetryAfterOf(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	h := http.Header{}
	require.Equal(t, time.Duration(0), retryAfterOf(h, now))
	h.Set("Retry-After", "3")
	require.Equal(t, 3*time.Second, retryAfterOf(h, now))
	h.Set("Retry-After", now.Add(90*time.Second).UTC().Format(http.TimeFormat))
	require.Equal(t, 90*time.Second, retryAfterOf(h, now))
	h.Set("Retry-After", now.Add(-time.Minute).UTC().Format(http.TimeFormat))
	require.Equal(t, time.Duration(0), retryAfterOf(h, now))
	h.Set("Retry-After", "soon")
	require.Equal(t, time.Duration(0), retryAfterOf(h, now))
	h.Set("Retry-After", "-5")
	require.Equal(t, time.Duration(0), retryAfterOf(h, now))
}

func TestRedactURL(t *testing.T) {
	t.Parallel()
	require.Equal(t, "https://mainnet.helius-rpc.com/", RedactURL("https://mainnet.helius-rpc.com/?api-key=SECRET"))
	require.Equal(t, "https://rpc.example.com/v1/abc", RedactURL("https://user:pw@rpc.example.com/v1/abc?token=x"))
	require.Equal(t, "<redacted>", RedactURL("::not a url"))
	require.Equal(t, "<redacted>", RedactURL("relative/path"))
}

func TestSanitizedCause(t *testing.T) {
	t.Parallel()
	require.Nil(t, sanitizedCause(nil))
	err := sanitizedCause(&testErr{"Post \"https://h/?api-key=SECRET\": dial failed"})
	require.NotContains(t, err.Error(), "SECRET")
	require.Equal(t, "boom", sanitizedCause(&testErr{"boom"}).Error())
}

type testErr struct{ s string }

func (e *testErr) Error() string { return e.s }
