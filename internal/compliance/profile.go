package compliance

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/security"
)

// IdentityState is the identity-verification state.
//
// It is the financial verification state machine of goal §20, and
// `internal/verification` owns its edges: this package declares the values and
// the attribute half of the profile, and migration 00761 makes a transition row
// the only way the column moves. `verification.AllStates` and the list below
// are the same list, held together by TestIdentityStatesMirrorTheStateMachine.
type IdentityState string

// Identity states. The first five predate migration 00761; the last five are
// the remainder of §20's canonical list.
const (
	IdentityUnverified       IdentityState = "UNVERIFIED"
	IdentityPending          IdentityState = "PENDING"
	IdentityVerified         IdentityState = "VERIFIED"
	IdentityRejected         IdentityState = "REJECTED"
	IdentityExpired          IdentityState = "EXPIRED"
	IdentityRequired         IdentityState = "REQUIRED"
	IdentityStarted          IdentityState = "STARTED"
	IdentityNeedsInformation IdentityState = "NEEDS_INFORMATION"
	IdentityRestricted       IdentityState = "RESTRICTED"
	IdentitySuspended        IdentityState = "SUSPENDED"
)

var allIdentityStates = []IdentityState{
	IdentityUnverified, IdentityRequired, IdentityStarted, IdentityPending, IdentityNeedsInformation,
	IdentityVerified, IdentityRejected, IdentityExpired, IdentityRestricted, IdentitySuspended,
}

// AllIdentityStates returns every declared identity state (a copy). It is
// compared against compliance_profiles_identity_state_check by
// test/integration/enums.
func AllIdentityStates() []IdentityState {
	return append([]IdentityState(nil), allIdentityStates...)
}

// Valid reports whether s is declared.
func (s IdentityState) Valid() bool {
	for _, x := range allIdentityStates {
		if x == s {
			return true
		}
	}
	return false
}

// SanctionsState is the sanctions-screening state.
type SanctionsState string

// Sanctions states.
const (
	SanctionsUnknown SanctionsState = "UNKNOWN"
	SanctionsClear   SanctionsState = "CLEAR"
	SanctionsHit     SanctionsState = "HIT"
	SanctionsReview  SanctionsState = "REVIEW"
)

var allSanctionsStates = []SanctionsState{
	SanctionsUnknown, SanctionsClear, SanctionsHit, SanctionsReview,
}

// AllSanctionsStates returns every declared sanctions state (a copy).
func AllSanctionsStates() []SanctionsState {
	return append([]SanctionsState(nil), allSanctionsStates...)
}

// Valid reports whether s is declared.
func (s SanctionsState) Valid() bool {
	for _, x := range allSanctionsStates {
		if x == s {
			return true
		}
	}
	return false
}

var countryRE = regexp.MustCompile(`^[A-Z]{2}$`)

// Profile mirrors compliance_profiles.
type Profile struct {
	UserID              accounts.UserID
	IdentityState       IdentityState
	AgeVerified         bool
	JurisdictionCountry string
	JurisdictionRegion  string
	ResidencyCountry    string
	SanctionsState      SanctionsState
	Provider            string
	ProviderRef         string
	PolicyVersion       string
	Restrictions        []string
	VerifiedAt          *time.Time
	ExpiresAt           *time.Time
	UpdatedAt           time.Time
}

// Validate checks enum and code formats. Unknown values fail closed here so
// that a malformed profile can never reach the eligibility engine as "valid".
func (p Profile) Validate() error {
	if !p.IdentityState.Valid() {
		return errs.Newf(errs.CodeValidationFailed, "unknown identity state %q", p.IdentityState)
	}
	if err := p.validateAttributes(); err != nil {
		return err
	}
	if p.IdentityState == IdentityVerified && p.VerifiedAt == nil {
		return errs.New(errs.CodeValidationFailed, "verified profiles require verified_at")
	}
	return nil
}

// validateAttributes checks the half of the profile the application still owns.
//
// It exists because migration 00761 took the state half away: `identity_state`,
// `verified_at` and `expires_at` are written by the transition trigger and are
// not in `Upsert`'s reach, so validating them there would refuse a caller for a
// field the statement does not use. Validate keeps checking them, because a
// Profile READ back from the database is a complete one and its internal
// consistency is still worth asserting.
func (p Profile) validateAttributes() error {
	if !p.SanctionsState.Valid() {
		return errs.Newf(errs.CodeValidationFailed, "unknown sanctions state %q", p.SanctionsState)
	}
	for _, c := range []string{p.JurisdictionCountry, p.ResidencyCountry} {
		if c != "" && !countryRE.MatchString(c) {
			return errs.Newf(errs.CodeValidationFailed, "country code %q must be ISO 3166-1 alpha-2 upper case", c)
		}
	}
	if p.UserID.IsZero() {
		return errs.New(errs.CodeValidationFailed, "user required")
	}
	return nil
}

// Repository persists profiles.
type Repository struct {
	audit audit.Writer
}

// NewRepository returns a Repository that appends an audit event per change.
func NewRepository(auditWriter audit.Writer) *Repository { return &Repository{audit: auditWriter} }

const columns = `user_id, identity_state, age_verified, coalesce(jurisdiction_country,''), coalesce(jurisdiction_region,''), coalesce(residency_country,''),
	sanctions_state, coalesce(provider,''), coalesce(provider_ref,''), coalesce(policy_version,''), restrictions, verified_at, expires_at, updated_at`

func scan(row pgx.Row) (Profile, error) {
	var p Profile
	var restrictions []byte
	if err := row.Scan(&p.UserID, &p.IdentityState, &p.AgeVerified, &p.JurisdictionCountry, &p.JurisdictionRegion, &p.ResidencyCountry,
		&p.SanctionsState, &p.Provider, &p.ProviderRef, &p.PolicyVersion, &restrictions, &p.VerifiedAt, &p.ExpiresAt, &p.UpdatedAt); err != nil {
		return Profile{}, err
	}
	if len(restrictions) > 0 {
		if err := json.Unmarshal(restrictions, &p.Restrictions); err != nil {
			return Profile{}, fmt.Errorf("compliance: restrictions: %w", err)
		}
	}
	if p.Restrictions == nil {
		p.Restrictions = []string{}
	}
	return p, nil
}

// Get returns a user's profile; a missing profile is reported as NOT_FOUND
// (the eligibility engine treats it as unverified and fails closed).
func (r *Repository) Get(ctx context.Context, q db.Querier, userID accounts.UserID) (Profile, error) {
	p, err := scan(q.QueryRow(ctx, `SELECT `+columns+` FROM compliance_profiles WHERE user_id = $1`, userID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Profile{}, errs.New(errs.CodeNotFound, "compliance profile not found")
		}
		return Profile{}, fmt.Errorf("compliance: get: %w", err)
	}
	return p, nil
}

// Change describes who changed a profile and why.
type Change struct {
	ActorType     security.ActorType // SYSTEM (provider webhook), OPERATOR
	ActorID       string
	Reason        string
	CorrelationID string
}

// Upsert writes the ATTRIBUTE half of the profile — age, jurisdiction,
// residency, sanctions, provider, policy version, restrictions — and appends an
// audit event with before/after hashes. AGENT and USER actors are refused:
// customers cannot self-attest compliance state.
//
// It does NOT write identity_state, verified_at or expires_at, and passing them
// in has no effect. Migration 00761 made a transition row the only way the
// verification state moves and revoked the application's UPDATE on all three
// columns; a profile is born UNVERIFIED and reaches every other state through
// `internal/verification`. The returned Profile is the row as it actually
// stands, so a caller reading the state back gets the truth rather than what it
// asked for.
func (r *Repository) Upsert(ctx context.Context, tx pgx.Tx, p Profile, ch Change) (Profile, error) {
	if ch.ActorType != security.ActorSystem && ch.ActorType != security.ActorOperator {
		return Profile{}, errs.New(errs.CodeForbidden, "compliance profiles are written only by SYSTEM or OPERATOR actors")
	}
	if ch.Reason == "" || ch.ActorID == "" {
		return Profile{}, errs.New(errs.CodeValidationFailed, "actor id and reason required")
	}
	if err := p.validateAttributes(); err != nil {
		return Profile{}, err
	}
	restrictions := append([]string(nil), p.Restrictions...)
	sort.Strings(restrictions)
	if restrictions == nil {
		restrictions = []string{}
	}
	rjson, err := json.Marshal(restrictions)
	if err != nil {
		return Profile{}, fmt.Errorf("compliance: encode restrictions: %w", err)
	}

	var before *Profile
	if prev, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM compliance_profiles WHERE user_id = $1 FOR UPDATE`, p.UserID)); err == nil {
		before = &prev
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return Profile{}, fmt.Errorf("compliance: lock: %w", err)
	}

	// The birth state is a literal, not p.IdentityState: a BEFORE INSERT
	// trigger (00761) refuses any other, and the ON CONFLICT branch cannot
	// name the column at all because cp_app has no UPDATE privilege on it.
	row := tx.QueryRow(ctx, `INSERT INTO compliance_profiles
		(user_id, identity_state, age_verified, jurisdiction_country, jurisdiction_region, residency_country, sanctions_state, provider, provider_ref, policy_version, restrictions)
		VALUES ($1,'UNVERIFIED',$2,NULLIF($3,''),NULLIF($4,''),NULLIF($5,''),$6,NULLIF($7,''),NULLIF($8,''),NULLIF($9,''),$10)
		ON CONFLICT (user_id) DO UPDATE SET
			age_verified = EXCLUDED.age_verified,
			jurisdiction_country = EXCLUDED.jurisdiction_country, jurisdiction_region = EXCLUDED.jurisdiction_region,
			residency_country = EXCLUDED.residency_country, sanctions_state = EXCLUDED.sanctions_state,
			provider = EXCLUDED.provider, provider_ref = EXCLUDED.provider_ref, policy_version = EXCLUDED.policy_version,
			restrictions = EXCLUDED.restrictions
		RETURNING `+columns,
		p.UserID, p.AgeVerified, p.JurisdictionCountry, p.JurisdictionRegion, p.ResidencyCountry, p.SanctionsState,
		p.Provider, p.ProviderRef, p.PolicyVersion, rjson)
	after, err := scan(row)
	if err != nil {
		return Profile{}, fmt.Errorf("compliance: upsert: %w", err)
	}

	beforeHash, afterHash, err := hashes(before, after)
	if err != nil {
		return Profile{}, err
	}
	payload, _ := json.Marshal(map[string]any{
		"identity_state": after.IdentityState, "sanctions_state": after.SanctionsState,
		"jurisdiction_country": after.JurisdictionCountry, "restrictions": after.Restrictions, "provider": after.Provider,
	})
	if _, err := r.audit.Append(ctx, tx, audit.Event{
		Stream: audit.SystemStream, ActorType: string(ch.ActorType), ActorID: ch.ActorID,
		Action: "compliance.profile.upserted", ResourceType: "compliance_profile", ResourceID: p.UserID.String(),
		BeforeHash: beforeHash, AfterHash: afterHash, CorrelationID: ch.CorrelationID, Reason: ch.Reason,
		PolicyVersion: after.PolicyVersion, Payload: payload, OccurredAt: after.UpdatedAt,
	}); err != nil {
		return Profile{}, fmt.Errorf("compliance: audit: %w", err)
	}
	return after, nil
}

// hashProfile is sha256 over the canonical JSON of the state-bearing fields
// (UpdatedAt excluded so identical content hashes identically).
func hashProfile(p Profile) ([]byte, error) {
	b, err := audit.CanonicalJSON(map[string]any{
		"user_id": p.UserID.String(), "identity_state": string(p.IdentityState), "age_verified": p.AgeVerified,
		"jurisdiction_country": p.JurisdictionCountry, "jurisdiction_region": p.JurisdictionRegion, "residency_country": p.ResidencyCountry,
		"sanctions_state": string(p.SanctionsState), "provider": p.Provider, "provider_ref": p.ProviderRef, "policy_version": p.PolicyVersion,
		"restrictions": p.Restrictions, "verified_at": p.VerifiedAt, "expires_at": p.ExpiresAt,
	})
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(b)
	return sum[:], nil
}

func hashes(before *Profile, after Profile) (beforeHash, afterHash []byte, err error) {
	if before != nil {
		if beforeHash, err = hashProfile(*before); err != nil {
			return nil, nil, fmt.Errorf("compliance: hash before: %w", err)
		}
	}
	if afterHash, err = hashProfile(after); err != nil {
		return nil, nil, fmt.Errorf("compliance: hash after: %w", err)
	}
	return beforeHash, afterHash, nil
}
