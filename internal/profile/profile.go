// Package profile is the Nodal user's product-level record: the name they chose
// to show, the handle they claimed, how to render things for them, how far
// through onboarding they are, which legal documents they have accepted, and the
// request they may have made to close their account.
//
// ADR-0022 decided where this lives: ZITADEL authenticates, `users` is the Nodal
// user, `identity_pii` holds sealed personal data, and product state gets its own
// table. Goal PART 4 says the same thing from the other direction -- "Do not
// combine these into one giant users table."
//
// This package must never: store an e-mail address, a phone number, a legal name
// or a date of birth (those are internal/pii's, and ADR-0021 decides who may read
// them); write a role, a permission or an account balance; or let one user read
// or write another user's record. The last is structural rather than checked:
// every method takes the subject from the caller's principal, and the only route
// that names somebody else is the operator support view, which reads and never
// writes.
package profile

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/nodal/controlplane/internal/errs"
)

// Onboarding is how far through the first-run experience a user is.
//
// It is four timestamps rather than a state machine, and D-053 records why: the
// steps are independent, may be done in any order, cannot be undone, and have no
// illegal edge worth refusing. What IS worth refusing -- a profile born finished,
// and a step restamped later -- is enforced by triggers in migration 00756.
type Onboarding struct {
	// StartedAt is when the profile row appeared, which is the first GET /v1/me
	// after a first login.
	StartedAt time.Time
	// DisplayNameSetAt is when the user first chose what to be called.
	DisplayNameSetAt *time.Time
	// TermsAcceptedAt is when the last document required at onboarding was
	// accepted.
	TermsAcceptedAt *time.Time
	// CompletedAt is when every required step was done. It is stamped by the
	// action that completes the last one, never by a sweep.
	CompletedAt *time.Time
}

// Complete reports whether onboarding is finished.
func (o Onboarding) Complete() bool { return o.CompletedAt != nil }

// StepKey names one onboarding step in the API.
type StepKey string

// The onboarding steps, in the order a first-run experience should ask for them.
// PART 6 sets the budget -- 60 to 90 seconds from landing page to dashboard --
// so there are two, and neither is identity verification.
const (
	StepProfile StepKey = "PROFILE"
	StepTerms   StepKey = "TERMS"
)

// Steps returns the declared steps in order.
func Steps() []StepKey { return []StepKey{StepProfile, StepTerms} }

// Step is one step and whether it is done.
type Step struct {
	Key         StepKey
	Complete    bool
	CompletedAt *time.Time
}

// Steps renders the checklist.
func (o Onboarding) Steps() []Step {
	return []Step{
		{Key: StepProfile, Complete: o.DisplayNameSetAt != nil, CompletedAt: o.DisplayNameSetAt},
		{Key: StepTerms, Complete: o.TermsAcceptedAt != nil, CompletedAt: o.TermsAcceptedAt},
	}
}

// NextStep returns the first incomplete step, or "" when there is none.
func (o Onboarding) NextStep() StepKey {
	for _, s := range o.Steps() {
		if !s.Complete {
			return s.Key
		}
	}
	return ""
}

// Profile is one user's product-level record.
type Profile struct {
	UserID      string
	DisplayName string
	Handle      string
	Locale      string
	TimeZone    string
	AvatarSeed  string
	Onboarding  Onboarding
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Patch is a partial profile update. A nil field is "leave it alone", which is
// different from a pointer to the empty string -- that is "clear it" for the
// fields that may be cleared.
type Patch struct {
	DisplayName *string
	Handle      *string
	Locale      *string
	TimeZone    *string
}

// Empty reports whether the patch asks for nothing.
func (p Patch) Empty() bool {
	return p.DisplayName == nil && p.Handle == nil && p.Locale == nil && p.TimeZone == nil
}

// Defaults for a freshly created profile. They are neutral rather than guessed:
// a locale inferred from an Accept-Language header and a timezone inferred from
// an IP address are both guesses that read as knowledge, and the second is the
// kind of guess this system refuses to make about a person (cmd/api makes the
// same refusal about jurisdiction).
const (
	DefaultLocale   = "en"
	DefaultTimeZone = "UTC"
)

var (
	handlePattern   = regexp.MustCompile(`^[a-z][a-z0-9_]{2,29}$`)
	localePattern   = regexp.MustCompile(`^[a-z]{2}(-[A-Z]{2})?$`)
	timeZonePattern = regexp.MustCompile(`^(UTC|[A-Za-z][A-Za-z0-9_+-]*(/[A-Za-z0-9_+-]+){1,2})$`)
)

// reservedHandles cannot be claimed. Each entry is here for a reason, and the
// reasons are worth keeping legible because a reserved-word list nobody can
// explain grows until it blocks real names.
//
//   - Impersonation: a handle that reads as the platform or its staff.
//   - Routing: a handle that would collide with a path segment the web app uses
//     or is likely to use, so /u/<handle> can never become ambiguous.
//   - Anti-abuse: a handle that asserts a status the product grants (verified,
//     official) and that no user may assert for themselves.
var reservedHandles = map[string]struct{}{}

func init() {
	for _, group := range [][]string{
		// Impersonation.
		{
			"nodal", "nodal_team", "nodalteam", "nodalsupport", "support", "help",
			"helpdesk", "admin", "administrator", "root", "sysadmin", "moderator",
			"mod", "staff", "team", "security", "abuse", "billing", "legal",
			"compliance", "operator", "operations", "system", "postmaster",
			"webmaster", "noreply", "no_reply",
		},
		// Routing: reserved so a future /u/<handle> can never shadow a page.
		//
		// A route segment shorter than three characters -- /me, /u -- needs no
		// entry: the handle pattern already refuses it, and listing one here
		// would look like a control while doing nothing
		// (TestReservedHandles_AreAllShapesAHandleCouldOtherwiseTake).
		{
			"about", "account", "accounts", "activity", "agents", "api", "assets",
			"auth", "buy", "credits", "dashboard", "developer", "docs", "events",
			"home", "internal", "login", "logout", "markets", "new",
			"notifications", "onboarding", "orders", "payouts", "portfolio",
			"pricing", "privacy", "profile", "search", "sessions", "settings",
			"signup", "static", "status", "terms", "trade", "user", "users",
			"verify", "verification", "wallet", "withdraw", "withdrawals",
		},
		// Anti-abuse: a status the product grants, never one a user claims.
		{"official", "verified", "trusted", "partner", "founder", "owner"},
	} {
		for _, h := range group {
			reservedHandles[h] = struct{}{}
		}
	}
}

// ReservedHandles returns the reserved list, sorted, so a surface can explain a
// refusal without guessing at it.
func ReservedHandles() []string {
	out := make([]string, 0, len(reservedHandles))
	for h := range reservedHandles {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// NormalizeDisplayName trims the name and collapses internal whitespace runs to
// single spaces. It does not change case: a person may capitalise their own name
// however they like.
func NormalizeDisplayName(in string) string {
	return strings.Join(strings.Fields(in), " ")
}

// ValidateDisplayName returns the normalised name or a VALIDATION_FAILED error.
//
// The rules are about what a name can DO, not about taste. A name is shown
// beside other people's names, so it may not carry control characters, direction
// overrides or zero-width characters -- each of which can make a rendered string
// claim to be a different string than it is -- and it must contain at least one
// letter or digit so that a name is not made entirely of decoration.
func ValidateDisplayName(in string) (string, error) {
	name := NormalizeDisplayName(in)
	if name == "" {
		return "", errs.New(errs.CodeValidationFailed, "a display name cannot be empty").
			WithField("field", "display_name")
	}
	if n := len([]rune(name)); n > 64 {
		return "", errs.Newf(errs.CodeValidationFailed, "a display name may be at most 64 characters; this one is %d", n).
			WithField("field", "display_name")
	}
	hasContent := false
	for _, r := range name {
		switch {
		case unicode.IsControl(r):
			return "", errs.New(errs.CodeValidationFailed, "a display name cannot contain control characters").
				WithField("field", "display_name")
		case isDeceptiveRune(r):
			return "", errs.New(errs.CodeValidationFailed,
				"a display name cannot contain zero-width or direction-override characters: they let a name render as something it is not").
				WithField("field", "display_name")
		case unicode.IsLetter(r), unicode.IsNumber(r):
			hasContent = true
		}
	}
	if !hasContent {
		return "", errs.New(errs.CodeValidationFailed, "a display name must contain at least one letter or digit").
			WithField("field", "display_name")
	}
	return name, nil
}

// isDeceptiveRune reports the characters whose whole purpose is to make rendered
// text differ from its code points: zero-width joiners and spaces, the bidi
// overrides and isolates, and the byte-order mark.
// The runes are written as escapes rather than as themselves: a source file
// that contained the characters this function rejects would be a source file
// nobody could read correctly either.
func isDeceptiveRune(r rune) bool {
	switch {
	case r == '\u00ad', r == '\ufeff': // soft hyphen, byte-order mark
		return true
	case r >= '\u200b' && r <= '\u200f': // zero-width space .. right-to-left mark
		return true
	case r >= '\u202a' && r <= '\u202e': // LRE .. RLO
		return true
	case r >= '\u2060' && r <= '\u2064': // word joiner .. invisible plus
		return true
	case r >= '\u2066' && r <= '\u2069': // LRI .. PDI
		return true
	}
	return false
}

// NormalizeHandle lower-cases and trims a handle. Storage is lower case because
// a UNIQUE index over a case-varying column would let `Nodal` and `nodal` both
// exist, and two handles that render identically to a reader are one handle.
func NormalizeHandle(in string) string { return strings.ToLower(strings.TrimSpace(in)) }

// ValidateHandle returns the normalised handle or a VALIDATION_FAILED error.
func ValidateHandle(in string) (string, error) {
	h := NormalizeHandle(in)
	if h == "" {
		return "", errs.New(errs.CodeValidationFailed, "a handle cannot be empty").WithField("field", "handle")
	}
	if !handlePattern.MatchString(h) {
		return "", errs.New(errs.CodeValidationFailed,
			"a handle is 3 to 30 characters, starts with a letter, and contains only lower-case letters, digits and underscores").
			WithField("field", "handle")
	}
	if _, reserved := reservedHandles[h]; reserved {
		return "", errs.Newf(errs.CodeValidationFailed, "the handle %q is reserved", h).WithField("field", "handle")
	}
	return h, nil
}

// ValidateLocale accepts a two-letter language, optionally with a two-letter
// region: `en`, `en-GB`. It is a presentation hint and is never read as a
// jurisdiction.
func ValidateLocale(in string) (string, error) {
	l := strings.TrimSpace(in)
	if !localePattern.MatchString(l) {
		return "", errs.New(errs.CodeValidationFailed,
			"a locale is a two-letter language, optionally with a region: en, en-GB").
			WithField("field", "locale")
	}
	return l, nil
}

// ValidateTimeZone accepts UTC or an IANA-shaped zone name.
//
// It checks the SHAPE and does not claim to check that the zone exists. Resolving
// a name needs the tz database, which this binary does not embed and which a
// container may not carry -- and a check that silently passes everything on one
// host and rejects real zones on another is worse than an honest bound. The
// value is a rendering hint the client applies; a name no client recognises
// renders in UTC, which is the same thing an absent value does.
func ValidateTimeZone(in string) (string, error) {
	tz := strings.TrimSpace(in)
	if len(tz) > 64 || !timeZonePattern.MatchString(tz) {
		return "", errs.New(errs.CodeValidationFailed,
			"a time zone is UTC or an IANA name such as Europe/London").
			WithField("field", "time_zone")
	}
	return tz, nil
}

// AvatarSeedFor derives the sixteen hex characters a client renders an identicon
// from. It is a hash of the user id with a fixed domain separator, so it is
// stable for a user, reveals nothing about them, and is not the user id itself
// (which would put an internal identifier into every rendered avatar URL).
func AvatarSeedFor(userID string) string {
	sum := sha256.Sum256([]byte("nodal.avatar.v1\x00" + userID))
	return hex.EncodeToString(sum[:8])
}
