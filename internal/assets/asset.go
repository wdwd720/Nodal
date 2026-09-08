package assets

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/valuedomain"
)

type assetKind struct{}

// AssetID is the internal identity of an asset. Provider identifiers and
// mint addresses are external references, never primary identity.
type AssetID = id.ID[assetKind]

// NewAssetID returns a fresh UUIDv7 asset identifier.
func NewAssetID() AssetID { return id.New[assetKind]() }

// ParseAssetID parses the canonical string form of an AssetID.
func ParseAssetID(s string) (AssetID, error) { return id.Parse[assetKind](s) }

// Kind is the on-chain representation of an asset.
type Kind string

// Asset kinds.
const (
	KindNative       Kind = "NATIVE"
	KindSPLToken     Kind = "SPL_TOKEN"
	KindSPLToken2022 Kind = "SPL_TOKEN_2022" // #nosec G101 -- asset kind enum value, not a credential
	// KindFiat is a valuation-only quote reference (e.g. USD). Fiat assets are
	// never held, traded, or posted to the ledger (migration 00108 enforces it).
	KindFiat Kind = "FIAT"
	// KindCredit is the Nodal Credit: the internal unit of account. It exists
	// on no chain and has no mint (gola.md PART XII).
	KindCredit Kind = "CREDIT"
	// KindNativeAsset is a Nodal-native asset created by a user inside the
	// platform. It is not a blockchain token (gola.md PART XIII).
	KindNativeAsset Kind = "NATIVE_ASSET"
)

// allKinds is every declared kind, in the order the constants are written.
//
// It exists so a test can be exhaustive rather than sampled: the kind-domain
// rule below is duplicated in SQL as `assets_kind_domain_agree`, and a
// comparison that iterated a hand-written list would stop covering a kind the
// day somebody added one.
var allKinds = []Kind{KindNative, KindSPLToken, KindSPLToken2022, KindFiat, KindCredit, KindNativeAsset}

// AllKinds returns every declared asset kind.
func AllKinds() []Kind { return append([]Kind(nil), allKinds...) }

// InternalChain is the chain value every Nodal-internal asset uses. Internal
// assets satisfy the registry's (chain, mint_address) identity honestly: the
// chain is this sentinel and the mint address is the asset's own id, so
// nothing pretends an internal asset has a mint (migration 00710).
const InternalChain = "nodal-internal"

// IsInternal reports whether the kind is a Nodal-internal asset.
func (k Kind) IsInternal() bool { return k == KindCredit || k == KindNativeAsset }

// FiatChain is the chain value every fiat reference uses.
const FiatChain = "fiat"

// RiskClass groups assets for policy purposes.
type RiskClass string

// Risk classes.
const (
	RiskSettlement  RiskClass = "SETTLEMENT"
	RiskMajor       RiskClass = "MAJOR"
	RiskStandard    RiskClass = "STANDARD"
	RiskSpeculative RiskClass = "SPECULATIVE"
	RiskUnsupported RiskClass = "UNSUPPORTED"
)

// Status is the asset safety state (PART 33).
type Status string

// Asset statuses.
const (
	StatusActive     Status = "ACTIVE"
	StatusCloseOnly  Status = "CLOSE_ONLY"
	StatusRestricted Status = "RESTRICTED"
	StatusHalted     Status = "HALTED"
	StatusDelisting  Status = "DELISTING"
	StatusDelisted   Status = "DELISTED"
)

// NativeMintSentinel is the mint_address used for a chain's native asset.
const NativeMintSentinel = "native"

// Asset is a registry row.
type Asset struct {
	ID           AssetID
	Chain        string
	MintAddress  string
	Kind         Kind
	Symbol       string
	Name         string
	Decimals     uint8
	IsStablecoin bool
	PegCurrency  string
	// ValueDomain classifies what kind of value this asset represents
	// (gola.md PART IX). It is required for every asset that can be held, and
	// is empty only for FIAT quote references, which are never held at all.
	ValueDomain     valuedomain.Domain
	RiskClass       RiskClass
	Status          Status
	MetadataVersion int32
	PolicyRef       string
	TokenExtensions []string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// AllowsIncreasingExposure reports whether new exposure may be opened.
func (s Status) AllowsIncreasingExposure() bool { return s == StatusActive }

// AllowsReducingExposure reports whether positions may be reduced/closed.
// HALTED and DELISTED block every transaction; the others permit risk reduction.
func (s Status) AllowsReducingExposure() bool {
	switch s {
	case StatusActive, StatusCloseOnly, StatusRestricted, StatusDelisting:
		return true
	default:
		return false
	}
}

// Valid reports whether s is a declared status.
func (s Status) Valid() bool {
	switch s {
	case StatusActive, StatusCloseOnly, StatusRestricted, StatusHalted, StatusDelisting, StatusDelisted:
		return true
	}
	return false
}

// statusTransitions is the explicit legal transition table.
var statusTransitions = map[Status][]Status{
	StatusActive:     {StatusCloseOnly, StatusRestricted, StatusHalted, StatusDelisting},
	StatusCloseOnly:  {StatusActive, StatusRestricted, StatusHalted, StatusDelisting},
	StatusRestricted: {StatusActive, StatusCloseOnly, StatusHalted, StatusDelisting},
	StatusHalted:     {StatusActive, StatusCloseOnly, StatusRestricted, StatusDelisting, StatusDelisted},
	StatusDelisting:  {StatusDelisted, StatusHalted},
	StatusDelisted:   {},
}

// CanTransition reports whether from → to is a legal status transition.
func CanTransition(from, to Status) bool {
	for _, t := range statusTransitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// Validate checks structural invariants of an asset definition.
func (a Asset) Validate() error {
	var problems []string
	if a.Chain == "" {
		problems = append(problems, "chain required")
	}
	if a.MintAddress == "" {
		problems = append(problems, "mint_address required")
	}
	switch a.Kind {
	case KindNative, KindSPLToken, KindSPLToken2022, KindFiat, KindCredit, KindNativeAsset:
	default:
		problems = append(problems, fmt.Sprintf("unknown kind %q", a.Kind))
	}
	problems = append(problems, a.valueDomainProblems()...)
	if a.Kind.IsInternal() {
		if a.Chain != InternalChain {
			problems = append(problems, "internal asset must use chain "+InternalChain)
		}
		if a.ID.IsZero() || a.MintAddress != a.ID.String() {
			problems = append(problems, "internal asset mint_address must be its own id")
		}
	} else if a.Chain == InternalChain {
		problems = append(problems, "only CREDIT and NATIVE_ASSET may use chain "+InternalChain)
	}
	if a.Kind == KindNative && a.MintAddress != NativeMintSentinel {
		problems = append(problems, "native asset must use the native mint sentinel")
	}
	if a.Kind != KindNative && a.MintAddress == NativeMintSentinel {
		problems = append(problems, "non-native asset cannot use the native mint sentinel")
	}
	if a.Kind == KindFiat {
		if a.Chain != FiatChain || a.MintAddress != a.Symbol || a.RiskClass != RiskUnsupported || a.IsStablecoin {
			problems = append(problems, "fiat reference must use chain 'fiat', mint_address == symbol, risk class UNSUPPORTED, and not be a stablecoin")
		}
		if a.Status != StatusRestricted && a.Status != StatusHalted {
			problems = append(problems, "fiat reference status must be RESTRICTED or HALTED (never tradable)")
		}
	}
	if a.Decimals > 18 {
		problems = append(problems, "decimals must be <= 18")
	}
	if a.Symbol == "" || a.Name == "" {
		problems = append(problems, "symbol and name required (display only)")
	}
	switch a.RiskClass {
	case RiskSettlement, RiskMajor, RiskStandard, RiskSpeculative, RiskUnsupported:
	default:
		problems = append(problems, fmt.Sprintf("unknown risk class %q", a.RiskClass))
	}
	if !a.Status.Valid() {
		problems = append(problems, fmt.Sprintf("unknown status %q", a.Status))
	}
	if a.IsStablecoin && a.PegCurrency == "" {
		problems = append(problems, "stablecoin requires peg_currency")
	}
	if len(problems) > 0 {
		e := errs.New(errs.CodeValidationFailed, "invalid asset definition")
		return e.WithField("problems", problems)
	}
	return nil
}

// Repository persists assets. All methods take a Querier so callers control
// transaction boundaries.
type Repository struct{}

// NewRepository returns a Repository.
func NewRepository() *Repository { return &Repository{} }

const assetColumns = `id, chain, mint_address, kind, symbol, name, decimals, is_stablecoin, coalesce(peg_currency,''), coalesce(value_domain,''), risk_class, status, metadata_version, coalesce(policy_ref,''), token_extensions, created_at, updated_at`

func scanAsset(row pgx.Row) (Asset, error) {
	var a Asset
	var decimals int16
	var ext []byte
	if err := row.Scan(&a.ID, &a.Chain, &a.MintAddress, &a.Kind, &a.Symbol, &a.Name, &decimals, &a.IsStablecoin, &a.PegCurrency, &a.ValueDomain, &a.RiskClass, &a.Status, &a.MetadataVersion, &a.PolicyRef, &ext, &a.CreatedAt, &a.UpdatedAt); err != nil {
		return Asset{}, err
	}
	if decimals < 0 || decimals > 18 {
		return Asset{}, fmt.Errorf("assets: decimals %d out of range", decimals)
	}
	a.Decimals = uint8(decimals)
	exts, err := decodeExtensions(ext)
	if err != nil {
		return Asset{}, err
	}
	a.TokenExtensions = exts
	return a, nil
}

// Create inserts a new asset. Duplicate (chain, mint_address) → CONFLICT.
func (r *Repository) Create(ctx context.Context, q db.Querier, a Asset) (Asset, error) {
	// The id is assigned before validation because an internal asset's
	// mint_address is its own id, and Validate checks that relationship.
	if a.ID.IsZero() {
		a.ID = NewAssetID()
		if a.Kind.IsInternal() && a.MintAddress == "" {
			a.MintAddress = a.ID.String()
		}
	}
	if err := a.Validate(); err != nil {
		return Asset{}, err
	}
	ext, err := encodeExtensions(a.TokenExtensions)
	if err != nil {
		return Asset{}, err
	}
	row := q.QueryRow(ctx, `INSERT INTO assets (id, chain, mint_address, kind, symbol, name, decimals, is_stablecoin, peg_currency, value_domain, risk_class, status, metadata_version, policy_ref, token_extensions)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,''),NULLIF($10,''),$11,$12,GREATEST($13,1),NULLIF($14,''),$15)
		RETURNING `+assetColumns,
		a.ID, a.Chain, a.MintAddress, a.Kind, a.Symbol, a.Name, int16(a.Decimals), a.IsStablecoin, a.PegCurrency, string(a.ValueDomain), a.RiskClass, a.Status, a.MetadataVersion, a.PolicyRef, ext)
	created, err := scanAsset(row)
	if err != nil {
		if db.IsUniqueViolation(err) {
			return Asset{}, errs.Wrap(err, errs.CodeConflict, "asset already registered for this chain and mint")
		}
		return Asset{}, fmt.Errorf("assets: create: %w", err)
	}
	return created, nil
}

// Get returns an asset by id.
func (r *Repository) Get(ctx context.Context, q db.Querier, assetID AssetID) (Asset, error) {
	a, err := scanAsset(q.QueryRow(ctx, `SELECT `+assetColumns+` FROM assets WHERE id = $1`, assetID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Asset{}, errs.New(errs.CodeNotFound, "asset not found").WithField("asset_id", assetID.String())
		}
		return Asset{}, fmt.Errorf("assets: get: %w", err)
	}
	return a, nil
}

// GetByMint returns an asset by (chain, mint_address).
func (r *Repository) GetByMint(ctx context.Context, q db.Querier, chain, mint string) (Asset, error) {
	a, err := scanAsset(q.QueryRow(ctx, `SELECT `+assetColumns+` FROM assets WHERE chain = $1 AND mint_address = $2`, chain, mint))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Asset{}, errs.New(errs.CodeNotFound, "asset not found").WithField("chain", chain).WithField("mint", mint)
		}
		return Asset{}, fmt.Errorf("assets: get by mint: %w", err)
	}
	return a, nil
}

// List returns assets ordered by symbol then id (bounded).
func (r *Repository) List(ctx context.Context, q db.Querier, limit int) ([]Asset, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	rows, err := q.Query(ctx, `SELECT `+assetColumns+` FROM assets ORDER BY symbol, id LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("assets: list: %w", err)
	}
	defer rows.Close()
	var out []Asset
	for rows.Next() {
		a, err := scanAsset(rows)
		if err != nil {
			return nil, fmt.Errorf("assets: list scan: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// StatusChange describes an audited status transition request.
type StatusChange struct {
	To            Status
	ActorType     string
	ActorID       string
	Reason        string
	PolicyVersion string
	CorrelationID string
}

// Transition applies a status change, recording the transition row. Illegal
// transitions fail with INVALID_STATE_TRANSITION; AGENT actors are rejected.
func (r *Repository) Transition(ctx context.Context, tx pgx.Tx, assetID AssetID, ch StatusChange) (Asset, error) {
	if ch.ActorType == "AGENT" || ch.ActorType == "" {
		return Asset{}, errs.New(errs.CodeForbidden, "asset status can only be changed by a non-agent actor")
	}
	if ch.Reason == "" {
		return Asset{}, errs.New(errs.CodeValidationFailed, "reason required")
	}
	if !ch.To.Valid() {
		return Asset{}, errs.Newf(errs.CodeValidationFailed, "unknown status %q", ch.To)
	}
	cur, err := scanAsset(tx.QueryRow(ctx, `SELECT `+assetColumns+` FROM assets WHERE id = $1 FOR UPDATE`, assetID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Asset{}, errs.New(errs.CodeNotFound, "asset not found")
		}
		return Asset{}, fmt.Errorf("assets: lock: %w", err)
	}
	if !CanTransition(cur.Status, ch.To) {
		return Asset{}, errs.Newf(errs.CodeInvalidStateTransition, "asset status %s -> %s is not allowed", cur.Status, ch.To).
			WithField("from", string(cur.Status)).WithField("to", string(ch.To))
	}
	if _, err := tx.Exec(ctx, `INSERT INTO asset_status_transitions (id, asset_id, from_status, to_status, actor_type, actor_id, reason, policy_version, correlation_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''),NULLIF($9,''))`,
		id.New[id.Any](), assetID, cur.Status, ch.To, ch.ActorType, ch.ActorID, ch.Reason, ch.PolicyVersion, ch.CorrelationID); err != nil {
		return Asset{}, fmt.Errorf("assets: record transition: %w", err)
	}
	updated, err := scanAsset(tx.QueryRow(ctx, `UPDATE assets SET status = $2 WHERE id = $1 RETURNING `+assetColumns, assetID, ch.To))
	if err != nil {
		return Asset{}, fmt.Errorf("assets: update status: %w", err)
	}
	return updated, nil
}

// valueDomainProblems states the relationship between an asset's kind and its
// value domain. It mirrors the assets_kind_domain_agree constraint added by
// migration 00710; a test asserts the two agree for every kind.
//
// A FIAT row is a quote reference, not a balance: migrations 00602/00108
// already forbid it from being held, traded or posted. Giving it a value
// domain would be inventing a fact, so it carries none, and carrying one is an
// error rather than a harmless extra.
func (a Asset) valueDomainProblems() []string {
	var problems []string
	switch a.Kind {
	case KindFiat:
		if a.ValueDomain != "" {
			problems = append(problems, "fiat quote reference must not carry a value domain; it is never held")
		}
	case KindCredit:
		if a.ValueDomain != valuedomain.InternalCredit {
			problems = append(problems, "CREDIT assets are always domain INTERNAL_CREDIT")
		}
	case KindNativeAsset:
		if a.ValueDomain != valuedomain.InternalNativeAsset {
			problems = append(problems, "NATIVE_ASSET assets are always domain INTERNAL_NATIVE_ASSET")
		}
	case KindNative, KindSPLToken, KindSPLToken2022:
		switch a.ValueDomain {
		case valuedomain.SelfCustodialCrypto, valuedomain.HostedCrypto, valuedomain.Simulated:
		case "":
			problems = append(problems,
				"chain assets must declare a value domain; custody is not inferable from the token")
		default:
			problems = append(problems, fmt.Sprintf(
				"chain asset cannot be domain %s", a.ValueDomain,
			))
		}
	}
	return problems
}
