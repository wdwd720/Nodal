package delegation

import (
	"context"
	"regexp"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/money"
)

// VenueSpec registers a sandbox venue: a settlement address on one chain,
// backed by a house book that is the swap's counterparty in the books.
type VenueSpec struct {
	Name           string
	Chain          devnet.ChainID
	Address        devnet.PublicKey
	HouseAccountID econ.HouseAccountID
	SpreadBPS      money.BPS
}

var venueName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// ReasonVenueHouseNotDedicated: the house book named for a venue is a
// treasury book, or already backs another venue.
const ReasonVenueHouseNotDedicated = "DELEGATION_VENUE_HOUSE_NOT_DEDICATED"

// venueHouseLockKey serializes the registrations that claim one house book:
// a venue's here and in cp_delegation_venue_house_dedicated, a treasury
// book's in cp_delegation_treasury_book_not_a_venue (migration 00762).
const venueHouseLockKey = "delegation/venue-house/"

// RegisterVenue records a venue. Only an operator, a service or the system
// registers one; a venue is immutable (stop one with a VENUE_DISABLE kill
// switch scoped to its id). A venue's counterparty is a house book of its
// own: never a treasury book (whose actions, limits and crossing disclosures
// a venue's swaps would bypass) and never another venue's (whose inventory it
// would promise twice). The database refuses both too (DG008).
func (s *Service) RegisterVenue(ctx context.Context, tx pgx.Tx, spec VenueSpec, by Actor) (Venue, error) {
	if !contains(VenueCreatorActorTypes(), by.Type) || by.ID == "" {
		return Venue{}, errs.New(errs.CodeForbidden, "delegation: only an operator, a service or the system registers a venue")
	}
	if !venueName.MatchString(spec.Name) || !contains(Chains(), spec.Chain) || spec.Address.IsZero() ||
		spec.SpreadBPS < 0 || spec.SpreadBPS > 1000 || spec.HouseAccountID.IsZero() {
		return Venue{}, errs.New(errs.CodeValidationFailed, "delegation: a venue needs a name, a host chain, an address, a house book and a spread of 0..1000 bps")
	}
	house, err := econ.NewRegistry().HouseAccount(ctx, tx, spec.HouseAccountID)
	if err != nil {
		return Venue{}, err
	}
	if house.Status != econ.HouseActive {
		return Venue{}, errs.New(errs.CodeInvalidStateTransition, "delegation: the venue's house book is "+string(house.Status))
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, venueHouseLockKey+house.ID.String()); err != nil {
		return Venue{}, dbErr("lock house book", err)
	}
	var treasury bool
	var backing string
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM treasury_books WHERE house_account_id = $1),
		coalesce((SELECT name FROM delegation_venues WHERE house_account_id = $1 ORDER BY created_at, id LIMIT 1), '')`,
		house.ID).Scan(&treasury, &backing); err != nil {
		return Venue{}, dbErr("check house book", err)
	}
	switch {
	case treasury:
		return Venue{}, errs.New(errs.CodeConflict, "delegation: house book "+house.Name+" is a treasury book; a venue needs a house book of its own").
			WithField("reason", ReasonVenueHouseNotDedicated)
	case backing != "":
		return Venue{}, errs.New(errs.CodeConflict, "delegation: house book "+house.Name+" already backs venue "+backing).
			WithField("reason", ReasonVenueHouseNotDedicated)
	}
	vid := id.New[venueKind]()
	if _, err := tx.Exec(ctx, `INSERT INTO delegation_venues (id, name, chain_id, address, house_account_id, spread_bps,
		created_by_actor_type, created_by_actor_id) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		vid, spec.Name, spec.Chain, spec.Address.String(), spec.HouseAccountID, int64(spec.SpreadBPS), by.Type, by.ID); err != nil {
		if db.IsUniqueViolation(err) {
			return Venue{}, errs.Wrap(err, errs.CodeConflict, "delegation: a venue with this name or address already exists")
		}
		return Venue{}, dbErr("register venue", err)
	}
	if err := s.auditEvent(ctx, tx, by, "delegation.venue.registered", "delegation_venue", vid.String(), accounts.AccountID{},
		"venue registered", map[string]any{"venue_id": vid.String(), "name": spec.Name, "chain": string(spec.Chain),
			"address": spec.Address.String(), "spread_bps": int64(spec.SpreadBPS)}); err != nil {
		return Venue{}, err
	}
	return getVenue(ctx, tx, vid)
}

// GetVenue returns one venue.
func (s *Service) GetVenue(ctx context.Context, q db.Querier, vid VenueID) (Venue, error) {
	return getVenue(ctx, q, vid)
}
