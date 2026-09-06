package eligibility

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/security"
)

type (
	policyKind   struct{}
	decisionKind struct{}
)

// PolicyID identifies an eligibility_policies row.
type PolicyID = id.ID[policyKind]

// DecisionID identifies an eligibility_decisions row.
type DecisionID = id.ID[decisionKind]

// ErrNoPolicy is returned by CurrentPolicy when no policy row is effective at
// the requested time. Callers evaluate with the zero Policy to persist a
// fail-closed decision carrying ELIGIBILITY_POLICY_MISSING.
var ErrNoPolicy = errors.New("eligibility: no policy is effective at the requested time")

// Store reads and writes eligibility_policies and eligibility_decisions and
// reads accounts and compliance_profiles. It holds no state and no clock.
type Store struct{}

// NewStore returns a Store.
func NewStore() *Store { return &Store{} }

// PolicyRecord is the input to RecordPolicy.
type PolicyRecord struct {
	Version     string
	Rules       json.RawMessage
	EffectiveAt time.Time
	ExpiresAt   *time.Time
	ActorType   security.ActorType
	ActorID     string
	Reason      string
}

// CurrentPolicy returns the policy effective at `at` (latest effective_at
// wins) and its version. The stored rules are re-parsed and their hash
// re-verified so a tampered or unparseable row fails closed.
func (s *Store) CurrentPolicy(ctx context.Context, q db.Querier, at time.Time) (Policy, string, error) {
	var (
		version string
		rules   []byte
		hash    []byte
	)
	err := q.QueryRow(ctx, `SELECT version, rules, rules_hash FROM eligibility_policies
		WHERE effective_at <= $1 AND (expires_at IS NULL OR expires_at > $1)
		ORDER BY effective_at DESC, created_at DESC, id DESC LIMIT 1`, at.UTC()).Scan(&version, &rules, &hash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Policy{}, "", ErrNoPolicy
		}
		return Policy{}, "", fmt.Errorf("eligibility: current policy: %w", err)
	}
	p, err := ParsePolicy(rules)
	if err != nil {
		return Policy{}, "", errs.Wrap(err, errs.CodeInternal, "eligibility: stored policy does not parse").WithField("version", version)
	}
	if hex.EncodeToString(hash) != p.Hash() {
		return Policy{}, "", errs.New(errs.CodeInternal, "eligibility: stored policy hash mismatch").WithField("version", version)
	}
	p.Version = version
	return p, version, nil
}

// RecordPolicy inserts a new policy version. AGENT actors are refused before
// any query, both by the actor type on the record and by any principal on
// the context; the row's CHECK constraint refuses them again.
func (s *Store) RecordPolicy(ctx context.Context, tx pgx.Tx, rec PolicyRecord) (Policy, error) {
	if err := rejectAgent(ctx, rec.ActorType); err != nil {
		return Policy{}, err
	}
	switch rec.ActorType {
	case security.ActorOperator, security.ActorSystem:
	default:
		return Policy{}, errs.Newf(errs.CodeForbidden, "eligibility: policies may only be recorded by OPERATOR or SYSTEM actors, not %q", rec.ActorType)
	}
	switch {
	case rec.Version == "":
		return Policy{}, errs.New(errs.CodeValidationFailed, "eligibility: policy version required")
	case rec.ActorID == "":
		return Policy{}, errs.New(errs.CodeValidationFailed, "eligibility: actor id required")
	case rec.Reason == "":
		return Policy{}, errs.New(errs.CodeValidationFailed, "eligibility: reason required")
	case rec.EffectiveAt.IsZero():
		return Policy{}, errs.New(errs.CodeValidationFailed, "eligibility: effective_at required")
	case rec.ExpiresAt != nil && !rec.ExpiresAt.After(rec.EffectiveAt):
		return Policy{}, errs.New(errs.CodeValidationFailed, "eligibility: expires_at must be after effective_at")
	}
	p, err := ParsePolicy(rec.Rules)
	if err != nil {
		return Policy{}, err
	}
	canon, err := p.CanonicalJSON()
	if err != nil {
		return Policy{}, fmt.Errorf("eligibility: canonical rules: %w", err)
	}
	sum := sha256.Sum256(canon)
	var expires *time.Time
	if rec.ExpiresAt != nil {
		t := rec.ExpiresAt.UTC()
		expires = &t
	}
	_, err = tx.Exec(ctx, `INSERT INTO eligibility_policies
		(id, version, rules, rules_hash, effective_at, expires_at, created_by_actor_type, created_by_actor_id, reason)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		id.New[policyKind](), rec.Version, canon, sum[:], rec.EffectiveAt.UTC(), expires, string(rec.ActorType), rec.ActorID, rec.Reason)
	if err != nil {
		if db.IsUniqueViolation(err) {
			return Policy{}, errs.Wrap(err, errs.CodeConflict, "eligibility: policy version already recorded").WithField("version", rec.Version)
		}
		return Policy{}, fmt.Errorf("eligibility: record policy: %w", err)
	}
	p.Version = rec.Version
	return p, nil
}

// RecordDecision persists a decision produced by Evaluate together with the
// identifiers from its input. The decision hash is re-verified so only an
// unmodified Evaluate output can be recorded.
func (s *Store) RecordDecision(ctx context.Context, tx pgx.Tx, in Input, d Decision, correlationID string) (DecisionID, error) {
	if d.Hash == "" || d.Hash != d.ComputeHash() {
		return DecisionID{}, errs.New(errs.CodeValidationFailed, "eligibility: decision hash mismatch; decisions must come unmodified from Evaluate")
	}
	in = in.normalized()
	if !in.Context.Valid() {
		return DecisionID{}, errs.Newf(errs.CodeValidationFailed, "eligibility: context kind %q cannot be persisted", in.Context)
	}
	accountID, err := optionalUUID("account_id", in.AccountID)
	if err != nil {
		return DecisionID{}, err
	}
	userID, err := optionalUUID("user_id", in.UserID)
	if err != nil {
		return DecisionID{}, err
	}
	intentID, err := optionalUUID("intent_id", in.IntentID)
	if err != nil {
		return DecisionID{}, err
	}
	instrumentID, err := optionalUUID("instrument_id", in.InstrumentID)
	if err != nil {
		return DecisionID{}, err
	}
	contextHash, err := hex.DecodeString(d.ContextHash)
	if err != nil || len(contextHash) != sha256.Size {
		return DecisionID{}, errs.New(errs.CodeValidationFailed, "eligibility: decision context hash is malformed")
	}
	decisionID := id.New[decisionKind]()
	codes := d.ReasonCodes
	if codes == nil {
		codes = []string{}
	}
	_, err = tx.Exec(ctx, `INSERT INTO eligibility_decisions
		(id, account_id, user_id, intent_id, context_kind, instrument_id, asset_class, venue, provider,
		 eligible, policy_version, reason_codes, context_hash, evaluated_at, correlation_id)
		VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,''),NULLIF($8,''),NULLIF($9,''),$10,$11,$12,$13,$14,NULLIF($15,''))`,
		decisionID, accountID, userID, intentID, string(in.Context), instrumentID, in.AssetClass, in.Venue, in.Provider,
		d.Eligible, d.PolicyVersion, codes, contextHash, d.EvaluatedAt.UTC(), correlationID)
	if err != nil {
		return DecisionID{}, fmt.Errorf("eligibility: record decision: %w", err)
	}
	return decisionID, nil
}

// LoadInputFromProfile fills the identity and account dimensions of an Input
// from accounts and compliance_profiles for accountID as of `at`. A missing
// compliance profile is reported as UNVERIFIED identity and UNKNOWN sanctions
// (both fail closed); a VERIFIED profile whose expires_at has passed is
// reported as EXPIRED. The caller adds the context, product, venue,
// provider and capability dimensions.
func (s *Store) LoadInputFromProfile(ctx context.Context, q db.Querier, accountID accounts.AccountID, at time.Time) (Input, error) {
	var (
		in            Input
		userID        accounts.UserID
		identityState *string
		ageVerified   *bool
		sanctions     *string
		restrictions  []byte
		expiresAt     *time.Time
	)
	err := q.QueryRow(ctx, `SELECT a.owner_user_id, a.status,
			cp.identity_state, cp.age_verified,
			coalesce(cp.jurisdiction_country,''), coalesce(cp.jurisdiction_region,''), coalesce(cp.residency_country,''),
			cp.sanctions_state, cp.restrictions, cp.expires_at
		FROM accounts a LEFT JOIN compliance_profiles cp ON cp.user_id = a.owner_user_id
		WHERE a.id = $1`, accountID).
		Scan(&userID, &in.AccountStatus, &identityState, &ageVerified,
			&in.JurisdictionCountry, &in.JurisdictionRegion, &in.ResidencyCountry,
			&sanctions, &restrictions, &expiresAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Input{}, errs.New(errs.CodeNotFound, "account not found").WithField("account_id", accountID.String())
		}
		return Input{}, fmt.Errorf("eligibility: load profile: %w", err)
	}
	in.AccountID = accountID.String()
	in.UserID = userID.String()
	in.IdentityState = IdentityUnverified
	in.SanctionsState = SanctionsUnknown
	if identityState != nil {
		in.IdentityState = *identityState
	}
	if ageVerified != nil {
		in.AgeVerified = *ageVerified
	}
	if sanctions != nil {
		in.SanctionsState = *sanctions
	}
	if in.IdentityState == IdentityVerified && expiresAt != nil && !at.Before(*expiresAt) {
		in.IdentityState = IdentityExpired
	}
	in.Restrictions = []string{}
	if len(restrictions) > 0 {
		if err := json.Unmarshal(restrictions, &in.Restrictions); err != nil {
			return Input{}, errs.Wrap(err, errs.CodeInternal, "eligibility: compliance restrictions are not a string array").WithField("account_id", accountID.String())
		}
	}
	in.Capabilities = map[string]bool{}
	in.Now = at.UTC()
	return in.normalized(), nil
}

func rejectAgent(ctx context.Context, actor security.ActorType) error {
	if actor == security.ActorAgent {
		return errs.New(errs.CodeForbidden, "eligibility: AGENT actors can never write policy")
	}
	if p, ok := security.PrincipalFrom(ctx); ok && p.IsAgent() {
		return errs.New(errs.CodeForbidden, "eligibility: AGENT principals can never write policy")
	}
	return nil
}

// optionalUUID parses a canonical UUID string, mapping "" to SQL NULL.
func optionalUUID(field, s string) (*id.ID[id.Any], error) {
	if s == "" {
		return nil, nil
	}
	v, err := id.ParseAny(s)
	if err != nil {
		return nil, errs.Newf(errs.CodeValidationFailed, "eligibility: %s is not a canonical uuid", field)
	}
	return &v, nil
}
