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
type IdentityState string

// Identity states.
const (
	IdentityUnverified IdentityState = "UNVERIFIED"
	IdentityPending    IdentityState = "PENDING"
	IdentityVerified   IdentityState = "VERIFIED"
	IdentityRejected   IdentityState = "REJECTED"
	IdentityExpired    IdentityState = "EXPIRED"
)

// SanctionsState is the sanctions-screening state.
type SanctionsState string

// Sanctions states.
const (
	SanctionsUnknown SanctionsState = "UNKNOWN"
	SanctionsClear   SanctionsState = "CLEAR"
	SanctionsHit     SanctionsState = "HIT"
	SanctionsReview  SanctionsState = "REVIEW"
)

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
	switch p.IdentityState {
	case IdentityUnverified, IdentityPending, IdentityVerified, IdentityRejected, IdentityExpired:
	default:
		return errs.Newf(errs.CodeValidationFailed, "unknown identity state %q", p.IdentityState)
	}
	switch p.SanctionsState {
	case SanctionsUnknown, SanctionsClear, SanctionsHit, SanctionsReview:
	default:
		return errs.Newf(errs.CodeValidationFailed, "unknown sanctions state %q", p.SanctionsState)
	}
	for _, c := range []string{p.JurisdictionCountry, p.ResidencyCountry} {
		if c != "" && !countryRE.MatchString(c) {
			return errs.Newf(errs.CodeValidationFailed, "country code %q must be ISO 3166-1 alpha-2 upper case", c)
		}
	}
	if p.IdentityState == IdentityVerified && p.VerifiedAt == nil {
		return errs.New(errs.CodeValidationFailed, "verified profiles require verified_at")
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

// Upsert writes the profile (insert or full update) and appends an audit
// event with before/after hashes. AGENT and USER actors are refused: customers
// cannot self-attest compliance state.
func (r *Repository) Upsert(ctx context.Context, tx pgx.Tx, p Profile, ch Change) (Profile, error) {
	if ch.ActorType != security.ActorSystem && ch.ActorType != security.ActorOperator {
		return Profile{}, errs.New(errs.CodeForbidden, "compliance profiles are written only by SYSTEM or OPERATOR actors")
	}
	if ch.Reason == "" || ch.ActorID == "" {
		return Profile{}, errs.New(errs.CodeValidationFailed, "actor id and reason required")
	}
	if err := p.Validate(); err != nil {
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

	row := tx.QueryRow(ctx, `INSERT INTO compliance_profiles
		(user_id, identity_state, age_verified, jurisdiction_country, jurisdiction_region, residency_country, sanctions_state, provider, provider_ref, policy_version, restrictions, verified_at, expires_at)
		VALUES ($1,$2,$3,NULLIF($4,''),NULLIF($5,''),NULLIF($6,''),$7,NULLIF($8,''),NULLIF($9,''),NULLIF($10,''),$11,$12,$13)
		ON CONFLICT (user_id) DO UPDATE SET
			identity_state = EXCLUDED.identity_state, age_verified = EXCLUDED.age_verified,
			jurisdiction_country = EXCLUDED.jurisdiction_country, jurisdiction_region = EXCLUDED.jurisdiction_region,
			residency_country = EXCLUDED.residency_country, sanctions_state = EXCLUDED.sanctions_state,
			provider = EXCLUDED.provider, provider_ref = EXCLUDED.provider_ref, policy_version = EXCLUDED.policy_version,
			restrictions = EXCLUDED.restrictions, verified_at = EXCLUDED.verified_at, expires_at = EXCLUDED.expires_at
		RETURNING `+columns,
		p.UserID, p.IdentityState, p.AgeVerified, p.JurisdictionCountry, p.JurisdictionRegion, p.ResidencyCountry, p.SanctionsState,
		p.Provider, p.ProviderRef, p.PolicyVersion, rjson, p.VerifiedAt, p.ExpiresAt)
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
