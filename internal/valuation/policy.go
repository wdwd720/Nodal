package valuation

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/money"
)

type policyKind struct{}

// PolicyID identifies one row of the asset policy history.
type PolicyID = id.ID[policyKind]

// NewPolicyID returns a fresh policy identifier.
func NewPolicyID() PolicyID { return id.New[policyKind]() }

// StablecoinStatus is the peg-health state of a stablecoin (PART 26). The
// empty value means "not set", which for a stablecoin is treated as
// RESTRICTED (fail closed) by Classify.
type StablecoinStatus string

// Stablecoin statuses.
const (
	StablecoinNormal     StablecoinStatus = "NORMAL"
	StablecoinDegraded   StablecoinStatus = "DEGRADED"
	StablecoinRestricted StablecoinStatus = "RESTRICTED"
	StablecoinHalted     StablecoinStatus = "HALTED"
)

// Valid reports whether s is a declared, non-empty status.
func (s StablecoinStatus) Valid() bool {
	switch s {
	case StablecoinNormal, StablecoinDegraded, StablecoinRestricted, StablecoinHalted:
		return true
	}
	return false
}

// MissingPolicyVersion is the PolicyVersion carried by the fail-closed
// policy returned when an asset has no effective policy row.
const MissingPolicyVersion = "MISSING"

// AssetPolicy is one row of the append-only policy history, or the
// fail-closed default when PolicyMissing is true.
type AssetPolicy struct {
	ID               PolicyID
	AssetID          assets.AssetID
	Status           assets.Status
	CollateralFactor money.BPS
	StablecoinStatus StablecoinStatus // empty for non-stablecoins
	MaxPriceAge      time.Duration
	PolicyVersion    string
	EffectiveAt      time.Time
	ExpiresAt        *time.Time
	ActorType        string
	ActorID          string
	Reason           string
	CreatedAt        time.Time

	// PolicyMissing is true when no policy row was effective at the
	// requested time and the remaining fields are the fail-closed default.
	PolicyMissing bool
}

// FailClosedPolicy is the policy applied when none is recorded: the asset is
// RESTRICTED (portfolio value only), contributes no collateral, and accepts
// no price age, so nothing can be extended against it by accident.
func FailClosedPolicy(assetID assets.AssetID) AssetPolicy {
	return AssetPolicy{
		AssetID:          assetID,
		Status:           assets.StatusRestricted,
		CollateralFactor: 0,
		MaxPriceAge:      0,
		PolicyVersion:    MissingPolicyVersion,
		PolicyMissing:    true,
	}
}

// PolicyReader is the fixed read contract (FINANCIAL_MODEL §7).
type PolicyReader interface {
	Current(ctx context.Context, q db.Querier, assetID assets.AssetID, at time.Time) (AssetPolicy, error)
}

// NewPolicy is the input to RecordPolicy.
type NewPolicy struct {
	AssetID          assets.AssetID
	Status           assets.Status
	CollateralFactor money.BPS
	StablecoinStatus StablecoinStatus // leave empty for non-stablecoins
	MaxPriceAge      time.Duration    // whole milliseconds, > 0
	PolicyVersion    string
	EffectiveAt      time.Time
	ExpiresAt        *time.Time
	ActorType        string // never "AGENT"
	ActorID          string
	Reason           string
}

// Validate checks the structural rules RecordPolicy enforces before touching
// the database.
func (p NewPolicy) Validate() error {
	if p.ActorType == "" || p.ActorType == "AGENT" {
		return errs.New(errs.CodeForbidden, "asset policy can only be recorded by a non-agent actor")
	}
	var problems []string
	if p.AssetID.IsZero() {
		problems = append(problems, "asset_id required")
	}
	if !p.Status.Valid() {
		problems = append(problems, fmt.Sprintf("unknown status %q", p.Status))
	}
	if p.CollateralFactor < 0 || p.CollateralFactor > money.OneHundredPercent {
		problems = append(problems, "collateral_factor_bps must be within [0, 10000]")
	}
	if p.StablecoinStatus != "" && !p.StablecoinStatus.Valid() {
		problems = append(problems, fmt.Sprintf("unknown stablecoin status %q", p.StablecoinStatus))
	}
	switch {
	case p.MaxPriceAge <= 0:
		problems = append(problems, "max_price_age must be positive")
	case p.MaxPriceAge%time.Millisecond != 0:
		problems = append(problems, "max_price_age must be a whole number of milliseconds")
	case p.MaxPriceAge.Milliseconds() > math.MaxInt32:
		problems = append(problems, "max_price_age exceeds the storable range")
	}
	if strings.TrimSpace(p.PolicyVersion) == "" {
		problems = append(problems, "policy_version required")
	}
	if p.EffectiveAt.IsZero() {
		problems = append(problems, "effective_at required")
	}
	if p.ExpiresAt != nil && !p.ExpiresAt.After(p.EffectiveAt) {
		problems = append(problems, "expires_at must be after effective_at")
	}
	if strings.TrimSpace(p.ActorID) == "" {
		problems = append(problems, "actor_id required")
	}
	if strings.TrimSpace(p.Reason) == "" {
		problems = append(problems, "reason required")
	}
	if len(problems) > 0 {
		return errs.New(errs.CodeValidationFailed, "invalid asset policy").WithField("problems", problems)
	}
	return nil
}

// PolicyStore reads and appends asset_policies.
type PolicyStore struct{}

// NewPolicyStore returns a PolicyStore.
func NewPolicyStore() *PolicyStore { return &PolicyStore{} }

var _ PolicyReader = (*PolicyStore)(nil)

const policyColumns = `id, asset_id, status, collateral_factor_bps, coalesce(stablecoin_status,''), max_price_age_ms,
	policy_version, effective_at, expires_at, created_by_actor_type, created_by_actor_id, reason, created_at`

func scanPolicy(row pgx.Row) (AssetPolicy, error) {
	var p AssetPolicy
	var factor int32
	var maxAgeMS int32
	var stable string
	if err := row.Scan(&p.ID, &p.AssetID, &p.Status, &factor, &stable, &maxAgeMS, &p.PolicyVersion, &p.EffectiveAt, &p.ExpiresAt,
		&p.ActorType, &p.ActorID, &p.Reason, &p.CreatedAt); err != nil {
		return AssetPolicy{}, err
	}
	p.CollateralFactor = money.BPS(factor)
	p.StablecoinStatus = StablecoinStatus(stable)
	p.MaxPriceAge = time.Duration(maxAgeMS) * time.Millisecond
	p.EffectiveAt = p.EffectiveAt.UTC()
	p.CreatedAt = p.CreatedAt.UTC()
	if p.ExpiresAt != nil {
		t := p.ExpiresAt.UTC()
		p.ExpiresAt = &t
	}
	return p, nil
}

// Current returns the policy in force for assetID at the instant at: the row
// with the latest effective_at that is not after at and whose expires_at (if
// any) is still in the future. Ties are broken by created_at then id, so the
// most recently recorded row wins. When no row qualifies the fail-closed
// policy is returned with PolicyMissing set and a nil error.
func (s *PolicyStore) Current(ctx context.Context, q db.Querier, assetID assets.AssetID, at time.Time) (AssetPolicy, error) {
	if assetID.IsZero() {
		return AssetPolicy{}, errs.New(errs.CodeValidationFailed, "asset_id required")
	}
	row := q.QueryRow(ctx, `SELECT `+policyColumns+` FROM asset_policies
		WHERE asset_id = $1 AND effective_at <= $2 AND (expires_at IS NULL OR expires_at > $2)
		ORDER BY effective_at DESC, created_at DESC, id DESC LIMIT 1`, assetID, at.UTC())
	p, err := scanPolicy(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return FailClosedPolicy(assetID), nil
		}
		return AssetPolicy{}, fmt.Errorf("valuation: current policy: %w", err)
	}
	return p, nil
}

// RecordPolicy appends a policy row. It validates the input, refuses AGENT
// actors (FORBIDDEN) and returns the stored row.
func (s *PolicyStore) RecordPolicy(ctx context.Context, q db.Querier, np NewPolicy) (AssetPolicy, error) {
	if err := np.Validate(); err != nil {
		return AssetPolicy{}, err
	}
	var stable *string
	if np.StablecoinStatus != "" {
		v := string(np.StablecoinStatus)
		stable = &v
	}
	var expires *time.Time
	if np.ExpiresAt != nil {
		t := np.ExpiresAt.UTC()
		expires = &t
	}
	maxAgeMS := np.MaxPriceAge.Milliseconds()
	if np.CollateralFactor < 0 || np.CollateralFactor > 10_000 || maxAgeMS <= 0 || maxAgeMS > 1<<31-1 {
		return AssetPolicy{}, errs.New(errs.CodeValidationFailed, "collateral factor must be 0..10000 bps and max price age within int32 milliseconds")
	}
	row := q.QueryRow(ctx, `INSERT INTO asset_policies
		(id, asset_id, status, collateral_factor_bps, stablecoin_status, max_price_age_ms, policy_version, effective_at, expires_at,
		 created_by_actor_type, created_by_actor_id, reason)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		RETURNING `+policyColumns,
		NewPolicyID(), np.AssetID, np.Status, int32(np.CollateralFactor), stable, int32(maxAgeMS),
		np.PolicyVersion, np.EffectiveAt.UTC(), expires, np.ActorType, np.ActorID, np.Reason)
	p, err := scanPolicy(row)
	if err != nil {
		if db.IsForeignKeyViolation(err) {
			return AssetPolicy{}, errs.Wrap(err, errs.CodeNotFound, "asset not found").WithField("asset_id", np.AssetID.String())
		}
		if db.IsCheckViolation(err) {
			return AssetPolicy{}, errs.Wrap(err, errs.CodeValidationFailed, "asset policy rejected by schema constraint")
		}
		return AssetPolicy{}, fmt.Errorf("valuation: record policy: %w", err)
	}
	return p, nil
}
