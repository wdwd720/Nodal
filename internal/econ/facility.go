package econ

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/ledger"
)

// Facility is a facilities row: a platform-operated economic facility with
// segregated SIMULATED books (ledger owner type FACILITY).
type Facility struct {
	ID        FacilityID
	Kind      ledger.FacilityKind
	Name      string
	Status    FacilityStatus
	CreatedBy Actor
	CreatedAt time.Time
}

// HouseAccount is a house_accounts row: a principal / treasury book with
// segregated SIMULATED books (ledger owner type HOUSE).
type HouseAccount struct {
	ID        HouseAccountID
	Name      string
	Strategy  HouseStrategy
	Status    HouseStatus
	CreatedBy Actor
	CreatedAt time.Time
}

// Account returns the ledger reference for one of the facility's accounts.
func (f Facility) Account(code ledger.Code, asset assets.AssetID) ledger.AccountRef {
	return ledger.AccountRef{OwnerType: ledger.OwnerFacility, OwnerID: f.ID.String(), Code: code, AssetID: asset}
}

// Account returns the ledger reference for one of the house book's accounts.
func (h HouseAccount) Account(code ledger.Code, asset assets.AssetID) ledger.AccountRef {
	return ledger.AccountRef{OwnerType: ledger.OwnerHouse, OwnerID: h.ID.String(), Code: code, AssetID: asset}
}

// Registry creates and reads facilities and house books.
type Registry struct{}

// NewRegistry returns a Registry.
func NewRegistry() *Registry { return &Registry{} }

// EnsureFacility returns the facility of this kind and name, creating it in
// ACTIVE if it does not exist. Facilities are named so that "the clearing
// house" or "the PSA stable issuer" is the same row on every call.
func (r *Registry) EnsureFacility(ctx context.Context, tx pgx.Tx, kind ledger.FacilityKind, name string, by Actor) (Facility, error) {
	if !kind.Valid() {
		return Facility{}, errs.Newf(errs.CodeValidationFailed, "unknown facility kind %q", kind)
	}
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 200 {
		return Facility{}, errs.New(errs.CodeValidationFailed, "facility name must be 1..200 characters")
	}
	if err := by.validate(); err != nil {
		return Facility{}, err
	}
	_, err := tx.Exec(ctx, `INSERT INTO facilities (id, kind, name, status, created_by_actor_type, created_by_actor_id)
		VALUES ($1,$2,$3,'ACTIVE',$4,$5) ON CONFLICT (kind, name) DO NOTHING`,
		NewFacilityID(), kind, name, by.Type, by.ID)
	if err != nil {
		return Facility{}, dbErr("create facility", err)
	}
	return r.facility(ctx, tx, `kind = $1 AND name = $2`, kind, name)
}

// Facility returns one facility by id.
func (r *Registry) Facility(ctx context.Context, q db.Querier, fid FacilityID) (Facility, error) {
	return r.facility(ctx, q, `id = $1`, fid)
}

func (r *Registry) facility(ctx context.Context, q db.Querier, where string, args ...any) (Facility, error) {
	var f Facility
	err := q.QueryRow(ctx, `SELECT id, kind, name, status, created_by_actor_type, created_by_actor_id, created_at
		FROM facilities WHERE `+where, args...).
		Scan(&f.ID, &f.Kind, &f.Name, &f.Status, &f.CreatedBy.Type, &f.CreatedBy.ID, &f.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Facility{}, errs.New(errs.CodeNotFound, "facility not found")
	}
	if err != nil {
		return Facility{}, dbErr("load facility", err)
	}
	f.CreatedAt = f.CreatedAt.UTC()
	return f, nil
}

// RequireActive fails unless the facility is ACTIVE.
func (f Facility) RequireActive() error {
	if f.Status != FacilityActive {
		return errs.Newf(errs.CodeInvalidStateTransition, "facility %s is %s", f.Name, f.Status).
			WithField("facility_id", f.ID.String())
	}
	return nil
}

// CreateHouseAccount creates a house book in ACTIVE.
func (r *Registry) CreateHouseAccount(ctx context.Context, tx pgx.Tx, name string, strategy HouseStrategy, by Actor) (HouseAccount, error) {
	valid := false
	for _, s := range AllHouseStrategies() {
		if s == strategy {
			valid = true
		}
	}
	if !valid {
		return HouseAccount{}, errs.Newf(errs.CodeValidationFailed, "unknown house strategy %q", strategy)
	}
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 200 {
		return HouseAccount{}, errs.New(errs.CodeValidationFailed, "house book name must be 1..200 characters")
	}
	if err := by.validate(); err != nil {
		return HouseAccount{}, err
	}
	hid := NewHouseAccountID()
	_, err := tx.Exec(ctx, `INSERT INTO house_accounts (id, name, strategy, status, created_by_actor_type, created_by_actor_id)
		VALUES ($1,$2,$3,'ACTIVE',$4,$5)`, hid, name, strategy, by.Type, by.ID)
	if err != nil {
		if db.IsUniqueViolation(err) {
			return HouseAccount{}, errs.Wrap(err, errs.CodeConflict, fmt.Sprintf("a house book named %q already exists", name))
		}
		return HouseAccount{}, dbErr("create house book", err)
	}
	return r.HouseAccount(ctx, tx, hid)
}

// HouseAccount returns one house book by id.
func (r *Registry) HouseAccount(ctx context.Context, q db.Querier, hid HouseAccountID) (HouseAccount, error) {
	var h HouseAccount
	err := q.QueryRow(ctx, `SELECT id, name, strategy, status, created_by_actor_type, created_by_actor_id, created_at
		FROM house_accounts WHERE id = $1`, hid).
		Scan(&h.ID, &h.Name, &h.Strategy, &h.Status, &h.CreatedBy.Type, &h.CreatedBy.ID, &h.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return HouseAccount{}, errs.New(errs.CodeNotFound, "house book not found").WithField("house_account_id", hid.String())
	}
	if err != nil {
		return HouseAccount{}, dbErr("load house book", err)
	}
	h.CreatedAt = h.CreatedAt.UTC()
	return h, nil
}
