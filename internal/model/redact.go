package model

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/observability"
)

// Guard refuses to let credential material into a model prompt.
//
// It fails the request rather than redacting it. A redacted prompt is a
// prompt that still had a secret in it a moment earlier, and the caller
// that assembled it has a bug worth surfacing; silently scrubbing would
// hide that. The ToolBroker resolves credentials server-side and returns
// only outputs, so a secret reaching this point is always a defect.
type Guard struct {
	// extra lets a deployment add site-specific credential markers.
	extra []*regexp.Regexp
}

// NewGuard returns the default guard.
func NewGuard(extra ...*regexp.Regexp) *Guard { return &Guard{extra: extra} }

// credentialMarkers are unambiguous credential shapes. They are matched
// literally rather than by entropy, because the IR legitimately carries
// 64-character hex hashes and an entropy heuristic would reject valid
// compile retries whose tool results quote an ir_hash.
var credentialMarkers = []*regexp.Regexp{
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`-----BEGIN OPENSSH PRIVATE KEY-----`),
	regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{8,}`),                    // Anthropic API key
	regexp.MustCompile(`\bsk-[A-Za-z0-9]{20,}`),                         // generic provider key
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),                          // AWS access key id
	regexp.MustCompile(`\bASIA[0-9A-Z]{16}\b`),                          // AWS temporary key id
	regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}`),                // Slack token
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}`),                  // GitHub token
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.`), // JWT
	regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._-]{20,}`),
}

// labeledSecret matches "key: value" / "key=value" where the key is on the
// observability redaction denylist and the value is non-trivial. Reusing
// that denylist keeps one vocabulary of what counts as a secret. RE2 has no
// backreferences, so an opening quote is consumed but not matched to its
// closing pair; only the label drives the decision, so that is sufficient.
var labeledSecret = regexp.MustCompile(`(?i)([A-Za-z][A-Za-z0-9_.\- ]{1,40})\s*[:=]\s*"?([^\s"',;]{6,})`)

// Check reports a *errs.Error with CodeSecretInModelContext when any part
// of the request that will be rendered carries credential material. The
// system policy is checked too: a template is code, and a key pasted into
// one is still a leak.
func (g *Guard) Check(r Request) error {
	parts := []struct{ field, text string }{{"system_policy", r.SystemPolicy}}
	for i, s := range r.ToolResults {
		parts = append(parts,
			struct{ field, text string }{fmt.Sprintf("tool_results[%d].label", i), s.Label},
			struct{ field, text string }{fmt.Sprintf("tool_results[%d].content", i), s.Content})
	}
	for i, s := range r.Untrusted {
		parts = append(parts,
			struct{ field, text string }{fmt.Sprintf("untrusted[%d].label", i), s.Label},
			struct{ field, text string }{fmt.Sprintf("untrusted[%d].content", i), s.Content})
	}
	for _, p := range parts {
		if reason, found := g.scan(p.text); found {
			// The detail names the reason and the field, never the value.
			return errs.Newf(errs.CodeSecretInModelContext,
				"model: refusing to send %s: it matches the credential denylist (%s)", p.field, reason).
				WithField("field", p.field).WithField("reason", reason)
		}
	}
	return nil
}

// scan returns a reason when text carries credential material.
func (g *Guard) scan(text string) (string, bool) {
	if text == "" {
		return "", false
	}
	for _, re := range credentialMarkers {
		if re.MatchString(text) {
			return "credential_pattern", true
		}
	}
	for _, re := range g.extra {
		if re.MatchString(text) {
			return "site_credential_pattern", true
		}
	}
	for _, m := range labeledSecret.FindAllStringSubmatch(text, -1) {
		label := strings.TrimSpace(m[1])
		// Take the last whitespace-separated word of the label, so
		// "the wallet private_key" and "export API_KEY" both resolve to the
		// denylisted token.
		if fields := strings.Fields(label); len(fields) > 0 {
			label = fields[len(fields)-1]
		}
		if observability.IsDeniedKey(label) {
			return "labeled_secret:" + strings.ToLower(label), true
		}
	}
	return "", false
}

// Guarded wraps a Provider so no call can bypass the guard. Compose it at
// construction; there is no path to Complete that skips it.
type Guarded struct {
	inner Provider
	guard *Guard
}

// NewGuarded returns p wrapped in the credential guard.
func NewGuarded(p Provider, g *Guard) *Guarded {
	if g == nil {
		g = NewGuard()
	}
	return &Guarded{inner: p, guard: g}
}

// Name reports the wrapped provider's name.
func (g *Guarded) Name() string { return g.inner.Name() }

// Complete checks the request, then delegates. A refused request never
// reaches the provider, so it is never billed and never logged upstream.
func (g *Guarded) Complete(ctx context.Context, req Request) (Response, error) {
	if err := g.guard.Check(req); err != nil {
		return Response{}, err
	}
	return g.inner.Complete(ctx, req)
}
