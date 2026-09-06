package httpmw

import (
	"net/http"
	"strconv"
	"time"
)

// Defaults for SecureHeadersOptions.
const (
	DefaultHSTSMaxAge        = 365 * 24 * time.Hour
	DefaultCSP               = "frame-ancestors 'none'"
	DefaultPermissionsPolicy = "camera=(), microphone=(), geolocation=(), payment=(), usb=(), interest-cohort=()"
	DefaultReferrerPolicy    = "strict-origin-when-cross-origin"
	DefaultCacheControl      = "no-store"
)

// SecureHeadersOptions configures SecureHeaders. Zero values take the
// defaults above.
type SecureHeadersOptions struct {
	// Secure emits Strict-Transport-Security; only set when every path to
	// this server is TLS (otherwise HSTS locks users out).
	Secure bool
	// HSTSMaxAge defaults to one year.
	HSTSMaxAge time.Duration
	// HSTSExcludeSubdomains drops includeSubDomains (included by default).
	HSTSExcludeSubdomains bool
	// HSTSPreload adds the preload directive.
	HSTSPreload bool
	// ContentSecurityPolicy defaults to frame-ancestors 'none' (the API
	// serves no HTML; a web front end sets its own full policy).
	ContentSecurityPolicy string
	// PermissionsPolicy defaults to denying powerful features.
	PermissionsPolicy string
	// ReferrerPolicy defaults to strict-origin-when-cross-origin, which
	// keeps same-origin Referers intact for the legacy CSRF fallback.
	ReferrerPolicy string
	// CacheControl is applied as a default before the handler runs, so
	// handlers may override it (static assets). Defaults to no-store.
	CacheControl string
	// NoCacheControl disables the Cache-Control default entirely.
	NoCacheControl bool
}

// SecureHeaders sets the response security headers. Headers are set before
// the handler runs so a handler can override a default deliberately.
func SecureHeaders(opts SecureHeadersOptions) func(http.Handler) http.Handler {
	if opts.HSTSMaxAge == 0 {
		opts.HSTSMaxAge = DefaultHSTSMaxAge
	}
	if opts.ContentSecurityPolicy == "" {
		opts.ContentSecurityPolicy = DefaultCSP
	}
	if opts.PermissionsPolicy == "" {
		opts.PermissionsPolicy = DefaultPermissionsPolicy
	}
	if opts.ReferrerPolicy == "" {
		opts.ReferrerPolicy = DefaultReferrerPolicy
	}
	if opts.CacheControl == "" {
		opts.CacheControl = DefaultCacheControl
	}
	hsts := "max-age=" + strconv.FormatInt(int64(opts.HSTSMaxAge/time.Second), 10)
	if !opts.HSTSExcludeSubdomains {
		hsts += "; includeSubDomains"
	}
	if opts.HSTSPreload {
		hsts += "; preload"
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("Referrer-Policy", opts.ReferrerPolicy)
			h.Set("Permissions-Policy", opts.PermissionsPolicy)
			h.Set("Content-Security-Policy", opts.ContentSecurityPolicy)
			h.Set("X-Frame-Options", "DENY")
			h.Set("Cross-Origin-Opener-Policy", "same-origin")
			if !opts.NoCacheControl {
				h.Set("Cache-Control", opts.CacheControl)
			}
			if opts.Secure {
				h.Set("Strict-Transport-Security", hsts)
			}
			next.ServeHTTP(w, r)
		})
	}
}
