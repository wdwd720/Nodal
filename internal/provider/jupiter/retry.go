package jupiter

import (
	"crypto/rand"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// parseRateLimit reads the documented x-ratelimit-* headers (present on
// success and 429 only). Non-integer values are ignored.
func parseRateLimit(h http.Header) RateLimitInfo {
	var rl RateLimitInfo
	if v := strings.TrimSpace(h.Get(HeaderRateLimitRemaining)); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			rl.Remaining, rl.Present = n, true
		}
	}
	if v := strings.TrimSpace(h.Get(HeaderRateLimitCurrent)); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			rl.Current, rl.Present = n, true
		}
	}
	if v := strings.TrimSpace(h.Get(HeaderRateLimitReset)); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			rl.Reset, rl.Present = time.Unix(n, 0).UTC(), true
		}
	}
	return rl
}

// retryAfterFrom derives how long to wait before retrying from Retry-After
// (delay-seconds or HTTP-date; standard HTTP, honored if present) or, when
// absent, from the documented x-ratelimit-reset unix timestamp. Zero means
// no hint.
func retryAfterFrom(h http.Header, now time.Time) time.Duration {
	if v := strings.TrimSpace(h.Get(HeaderRetryAfter)); v != "" {
		if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
			if secs <= 0 {
				return 0
			}
			return time.Duration(secs) * time.Second
		}
		if t, err := http.ParseTime(v); err == nil {
			if d := t.Sub(now); d > 0 {
				return d
			}
			return 0
		}
	}
	rl := parseRateLimit(h)
	if !rl.Reset.IsZero() {
		if d := rl.Reset.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

// backoffDelay returns the jittered delay before retry number attempt
// (1-based: the delay after the first failed attempt is attempt=1). The
// schedule is exponential from base, capped at maxDelay, with "equal
// jitter": half the delay is fixed and half is uniformly random, so retries
// from many workers never synchronize. jitter must return [0, n).
func backoffDelay(attempt int, base, maxDelay time.Duration, jitter func(n int64) int64) time.Duration {
	if base <= 0 {
		return 0
	}
	if attempt < 1 {
		attempt = 1
	}
	d := base
	for i := 1; i < attempt && d < maxDelay; i++ {
		d *= 2
	}
	if maxDelay > 0 && d > maxDelay {
		d = maxDelay
	}
	half := d / 2
	if half <= 0 {
		return d
	}
	return half + time.Duration(jitter(int64(half)))
}

// cryptoJitter is the default jitter source. It is not a security
// decision; crypto/rand is simply the one source that is never flagged as
// predictable and needs no seeding.
func cryptoJitter(n int64) int64 {
	if n <= 0 {
		return 0
	}
	v, err := rand.Int(rand.Reader, big.NewInt(n))
	if err != nil {
		return 0
	}
	return v.Int64()
}

// retryableStatus reports whether a SAFE_RETRY call may be retried after
// this HTTP status: rate limits and server errors only.
func retryableStatus(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}
