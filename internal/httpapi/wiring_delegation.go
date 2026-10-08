package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/delegation"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/devnethost"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

// DelegationService is the delegation service DelegationDeps carries. It is
// an alias, declared here so the shared wiring file needs no import of its
// own.
type DelegationService = delegation.Service

// DelegationDeps serves /advanced/delegation (DelegationPort). A nil Service
// leaves every delegation route UNSUPPORTED: a deployment that opens no
// sandbox chain has no delegated authority, and says so.
type DelegationDeps struct {
	Service *DelegationService
	// CoSigners are the public halves of the venue co-signers the service
	// was built with. A venue is registered at its co-signer's address; a swap
	// at a venue whose co-signer the deployment does not hold is never signed.
	CoSigners []DelegationCoSigner
}

// DelegationCoSigner is the public half of one venue co-signer the
// deployment holds (delegation.VenueSigner), by venue name.
type DelegationCoSigner struct {
	Name    string
	Chain   devnet.ChainID
	Address devnet.PublicKey
}

// DelegationSandboxVenue is a sandbox venue a deployment that opens the
// sandbox chain holds a co-signer for: its name (from which the co-signer's
// key derives, sandboxchain.Seed.VenueSeed), its chain and the spread it is
// registered with at startup.
type DelegationSandboxVenue struct {
	Name      string
	Chain     devnet.ChainID
	SpreadBPS money.BPS
}

// DelegationSandboxVenues are the venues every deployment that opens the
// sandbox chain co-signs for, one per simulated chain. The composition root
// registers the first at startup (EnsureDelegationVenue); an operator
// registers any other. The names are fixed: a venue's co-signer key derives
// from its name, so renaming one orphans the venue on an existing network.
func DelegationSandboxVenues() []DelegationSandboxVenue {
	return []DelegationSandboxVenue{
		{Name: "sandbox-solana", Chain: devnethost.ChainSolana, SpreadBPS: 50},
		{Name: "sandbox-ethereum", Chain: devnethost.ChainEthereum, SpreadBPS: 50},
	}
}

// delegationBootstrapActor registers the startup venue and its house book.
var delegationBootstrapActor = delegation.Actor{Type: security.ActorService, ID: "delegation-venue-bootstrap"}

// EnsureDelegationVenue registers the sandbox venue v at its co-signer's
// address, with a house book of its own, unless a venue of that name is
// already registered there; it returns the venue either way. A venue of that
// name registered at any other address is CONFLICT: this deployment's
// co-signer could never settle a swap there. It is the composition root's
// startup step; concurrent starts serialize on an advisory lock.
func EnsureDelegationVenue(ctx context.Context, d *db.DB, deps DelegationDeps, v DelegationSandboxVenue) (delegation.Venue, error) {
	if d == nil || deps.Service == nil {
		return delegation.Venue{}, errs.New(errs.CodeInternal, "delegation: the venue bootstrap needs the database and the service")
	}
	cs, ok := delegationCoSignerNamed(deps.CoSigners, v.Name)
	if !ok || cs.Chain != v.Chain {
		return delegation.Venue{}, errs.Newf(errs.CodeInternal, "delegation: no co-signer for venue %q on %s", v.Name, v.Chain)
	}
	var out delegation.Venue
	err := d.InTx(ctx, delegationWriteTx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, delegationVenueLockSQL, "delegation/venue-bootstrap/"+v.Name); err != nil {
			return err
		}
		existing, found, err := delegationVenueByName(ctx, tx, v.Name)
		if err != nil {
			return err
		}
		if found {
			if existing.Chain != cs.Chain || existing.Address != cs.Address {
				return errs.Newf(errs.CodeConflict, "delegation: venue %q is registered at another address; this deployment's co-signer cannot settle for it", v.Name)
			}
			out = existing
			return nil
		}
		house, err := delegationVenueHouse(ctx, tx, v.Name, delegationBootstrapActor)
		if err != nil {
			return err
		}
		out, err = deps.Service.RegisterVenue(ctx, tx, delegation.VenueSpec{Name: v.Name, Chain: cs.Chain, Address: cs.Address,
			HouseAccountID: house, SpreadBPS: v.SpreadBPS}, delegationBootstrapActor)
		return err
	})
	return out, err
}

// wireDelegation wires the delegation port when the service is present.
func wireDelegation(p *Ports, d WireDeps) {
	dd := d.Advanced.Delegation
	if dd.Service == nil || d.DB == nil {
		return
	}
	clk := d.Clock
	if clk == nil {
		clk = clock.System()
	}
	p.Delegation = delegationAdapter{svc: dd.Service, db: d.DB, clk: clk,
		coSigners: append([]DelegationCoSigner(nil), dd.CoSigners...)}
}

// delegationAdapter serves DelegationPort over the delegation service.
//
// Every owner command is one service operation in its own transaction,
// under the caller's principal, which the service checks again (the owner,
// by the database; a fresh strong authentication where it asks for one).
// The executions it runs -- after an owner's approval, or in an operator's
// run -- are the service's own: ExecuteSwap and ExecuteWithdrawal refuse an
// agent principal and act under the service's identity, in transactions of
// their own, submitting each signed transaction exactly once. Every read is
// one REPEATABLE READ snapshot.
type delegationAdapter struct {
	svc       *delegation.Service
	db        *db.DB
	clk       clock.Clock
	coSigners []DelegationCoSigner
}

var _ DelegationPort = delegationAdapter{}

var (
	// The delegation service is written for READ COMMITTED and is safe to
	// re-run on a serialization failure: nothing external happens inside
	// its transactions.
	delegationWriteTx = db.TxOptions{Isolation: pgx.ReadCommitted, MaxRetries: 3}
	delegationReadTx  = db.TxOptions{Isolation: pgx.RepeatableRead, ReadOnly: true}
)

// delegationExecutionBudget bounds an execution that follows an owner's
// approval. It runs detached from the request: once the decision is
// committed, a client that goes away must not leave the execution half done
// (and whatever it leaves is reconciled by transaction id, never redone).
const delegationExecutionBudget = 2 * time.Minute

// delegationExecuteMax bounds one operator execution run.
const delegationExecuteMax = 50

func (a delegationAdapter) snapshot(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return a.db.InTx(ctx, delegationReadTx, fn)
}

func (a delegationAdapter) write(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return a.db.InTx(ctx, delegationWriteTx, fn)
}

func (a delegationAdapter) now() time.Time { return a.clk.Now().UTC() }

// --- reads of one account ------------------------------------------------------

func (a delegationAdapter) Agents(ctx context.Context, account accounts.AccountID) ([]DelegationAgentView, error) {
	rows, err := a.db.Query(ctx, delegationAgentsSQL, account)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DelegationAgentView{}
	for rows.Next() {
		var v DelegationAgentView
		if err := rows.Scan(&v.ID, &v.Name, &v.Stage, &v.State, &v.StrategyID, &v.CreatedAt); err != nil {
			return nil, err
		}
		v.CreatedAt = v.CreatedAt.UTC()
		out = append(out, v)
	}
	return out, rows.Err()
}

func (a delegationAdapter) Capabilities(ctx context.Context, account accounts.AccountID, cursor string, limit int) (DelegationCapabilityPage, error) {
	at, after, err := delegationCursor(cursor)
	if err != nil {
		return DelegationCapabilityPage{}, err
	}
	limit = clampLimit(limit)
	var page DelegationCapabilityPage
	err = a.snapshot(ctx, func(ctx context.Context, tx pgx.Tx) error {
		items, err := delegationCapabilities(ctx, tx, delegationAccountCapabilitiesSQL, account, at, after, limit+1)
		if err != nil {
			return err
		}
		page = delegationCapabilityPage(items, limit)
		return delegationWithUsage(ctx, tx, page.Items, a.now())
	})
	return page, err
}

func (a delegationAdapter) Destinations(ctx context.Context, account accounts.AccountID) ([]delegation.Destination, error) {
	rows, err := a.db.Query(ctx, delegationAccountDestinationsSQL, account)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []delegation.Destination{}
	for rows.Next() {
		d, err := scanDelegationDestination(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (a delegationAdapter) WithdrawalPolicy(ctx context.Context, account accounts.AccountID) (DelegationPolicyView, error) {
	rows, err := a.db.Query(ctx, delegationPoliciesSQL, account)
	if err != nil {
		return DelegationPolicyView{}, err
	}
	defer rows.Close()
	out := DelegationPolicyView{AccountID: account, Versions: []delegation.WithdrawalPolicy{}}
	for rows.Next() {
		p, err := scanDelegationPolicy(rows)
		if err != nil {
			return DelegationPolicyView{}, err
		}
		out.Versions = append(out.Versions, p)
	}
	return out, rows.Err()
}

func (a delegationAdapter) Swaps(ctx context.Context, account accounts.AccountID, status delegation.SwapStatus, cursor string,
	limit int) (DelegationSwapPage, error) {
	at, after, err := delegationCursor(cursor)
	if err != nil {
		return DelegationSwapPage{}, err
	}
	limit = clampLimit(limit)
	var page DelegationSwapPage
	err = a.snapshot(ctx, func(ctx context.Context, tx pgx.Tx) error {
		items, err := delegationSwaps(ctx, tx, delegationAccountSwapsSQL, account, string(status), at, after, limit+1)
		if err != nil {
			return err
		}
		if len(items) > limit {
			items = items[:limit]
			last := items[limit-1].Swap
			page.NextCursor = encodeCursor(keysetCursor{At: last.CreatedAt, ID: last.ID.String()})
		}
		page.Items = items
		return delegationSwapTrails(ctx, tx, page.Items)
	})
	return page, err
}

func (a delegationAdapter) Withdrawals(ctx context.Context, account accounts.AccountID, status delegation.WithdrawalStatus,
	cursor string, limit int) (DelegationWithdrawalPage, error) {
	at, after, err := delegationCursor(cursor)
	if err != nil {
		return DelegationWithdrawalPage{}, err
	}
	limit = clampLimit(limit)
	var page DelegationWithdrawalPage
	err = a.snapshot(ctx, func(ctx context.Context, tx pgx.Tx) error {
		items, err := delegationWithdrawals(ctx, tx, delegationAccountWithdrawalsSQL, account, string(status), at, after, limit+1)
		if err != nil {
			return err
		}
		if len(items) > limit {
			items = items[:limit]
			last := items[limit-1].Withdrawal
			page.NextCursor = encodeCursor(keysetCursor{At: last.CreatedAt, ID: last.ID.String()})
		}
		page.Items = items
		return delegationWithdrawalTrails(ctx, tx, page.Items)
	})
	return page, err
}

// --- records by their own id -----------------------------------------------------

func (a delegationAdapter) Capability(ctx context.Context, cid delegation.CapabilityID) (DelegationCapabilityView, error) {
	var out DelegationCapabilityView
	err := a.snapshot(ctx, func(ctx context.Context, tx pgx.Tx) error {
		v, err := a.capabilityView(ctx, tx, cid)
		if err != nil {
			return err
		}
		if v.Uses, err = delegationUses(ctx, tx, cid); err != nil {
			return err
		}
		out = v
		return nil
	})
	return out, err
}

func (a delegationAdapter) Destination(ctx context.Context, did delegation.DestinationID) (delegation.Destination, error) {
	d, err := scanDelegationDestination(a.db.QueryRow(ctx, delegationDestinationByIDSQL, did))
	if errors.Is(err, pgx.ErrNoRows) {
		return delegation.Destination{}, errs.New(errs.CodeNotFound, "delegation: destination not found")
	}
	return d, err
}

func (a delegationAdapter) Swap(ctx context.Context, sid delegation.SwapID) (DelegationSwapView, error) {
	var out DelegationSwapView
	err := a.snapshot(ctx, func(ctx context.Context, tx pgx.Tx) error {
		v, err := delegationSwapByID(ctx, tx, sid)
		if err != nil {
			return err
		}
		out = v
		return nil
	})
	return out, err
}

func (a delegationAdapter) Withdrawal(ctx context.Context, wid delegation.WithdrawalID) (DelegationWithdrawalView, error) {
	var out DelegationWithdrawalView
	err := a.snapshot(ctx, func(ctx context.Context, tx pgx.Tx) error {
		v, err := delegationWithdrawalByID(ctx, tx, wid)
		if err != nil {
			return err
		}
		out = v
		return nil
	})
	return out, err
}

func (a delegationAdapter) Venues(ctx context.Context) (DelegationVenuesView, error) {
	rows, err := a.db.Query(ctx, delegationVenuesSQL)
	if err != nil {
		return DelegationVenuesView{}, err
	}
	defer rows.Close()
	out := DelegationVenuesView{Venues: []DelegationVenueView{}, CoSigners: []DelegationCoSignerView{}}
	registered := map[devnet.PublicKey]bool{}
	for rows.Next() {
		v, err := scanDelegationVenue(rows)
		if err != nil {
			return DelegationVenuesView{}, err
		}
		_, held := delegationCoSignerAt(a.coSigners, v.Chain, v.Address)
		registered[v.Address] = true
		out.Venues = append(out.Venues, DelegationVenueView{Venue: v, CoSigner: held})
	}
	if err := rows.Err(); err != nil {
		return DelegationVenuesView{}, err
	}
	for _, cs := range a.coSigners {
		out.CoSigners = append(out.CoSigners, DelegationCoSignerView{Name: cs.Name, Chain: cs.Chain, Address: cs.Address,
			Registered: registered[cs.Address]})
	}
	return out, nil
}

// --- the owner's commands ----------------------------------------------------------

func (a delegationAdapter) Grant(ctx context.Context, r delegation.GrantRequest) (DelegationCapabilityView, error) {
	var out DelegationCapabilityView
	err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		c, _, err := a.svc.GrantCapability(ctx, tx, r)
		if err != nil {
			return err
		}
		out, err = a.capabilityView(ctx, tx, c.ID)
		return err
	})
	return out, err
}

func (a delegationAdapter) Revoke(ctx context.Context, r delegation.RevokeRequest) (DelegationCapabilityView, error) {
	var out DelegationCapabilityView
	err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		c, err := a.svc.RevokeCapability(ctx, tx, r)
		if err != nil {
			return err
		}
		out, err = a.capabilityView(ctx, tx, c.ID)
		return err
	})
	return out, err
}

func (a delegationAdapter) AddDestination(ctx context.Context, r delegation.DestinationRequest) (delegation.Destination, error) {
	var out delegation.Destination
	err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = a.svc.AddDestination(ctx, tx, r)
		return err
	})
	return out, err
}

func (a delegationAdapter) RemoveDestination(ctx context.Context, did delegation.DestinationID, reason string) (delegation.Destination, error) {
	var out delegation.Destination
	err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = a.svc.RemoveDestination(ctx, tx, did, reason)
		return err
	})
	return out, err
}

func (a delegationAdapter) SetWithdrawalPolicy(ctx context.Context, p delegation.WithdrawalPolicy) (delegation.WithdrawalPolicy, error) {
	var out delegation.WithdrawalPolicy
	err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = a.svc.SetWithdrawalPolicy(ctx, tx, p)
		return err
	})
	return out, err
}

func (a delegationAdapter) DecideSwap(ctx context.Context, d delegation.SwapDecision) (DelegationSwapOutcome, error) {
	var decided delegation.Swap
	if err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		decided, err = a.svc.DecideSwap(ctx, tx, d)
		return err
	}); err != nil {
		return DelegationSwapOutcome{}, err
	}
	var out DelegationSwapOutcome
	if decided.Status == delegation.SwapApproved {
		run, cancel := a.detached(ctx)
		_, xerr := a.svc.ExecuteSwap(run, decided.ID)
		cancel()
		out.Execution = DelegationExecution{Attempted: true, Err: xerr}
	}
	view, err := a.Swap(ctx, decided.ID)
	if err != nil {
		return DelegationSwapOutcome{}, err
	}
	out.Swap = view
	return out, nil
}

func (a delegationAdapter) DecideWithdrawal(ctx context.Context, d delegation.WithdrawalDecision) (DelegationWithdrawalOutcome, error) {
	var decided delegation.Withdrawal
	if err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		decided, err = a.svc.DecideWithdrawal(ctx, tx, d)
		return err
	}); err != nil {
		return DelegationWithdrawalOutcome{}, err
	}
	var out DelegationWithdrawalOutcome
	if decided.Status == delegation.WithdrawalApproved {
		run, cancel := a.detached(ctx)
		_, xerr := a.svc.ExecuteWithdrawal(run, decided.ID)
		cancel()
		out.Execution = DelegationExecution{Attempted: true, Err: xerr}
	}
	view, err := a.Withdrawal(ctx, decided.ID)
	if err != nil {
		return DelegationWithdrawalOutcome{}, err
	}
	out.Withdrawal = view
	return out, nil
}

func (a delegationAdapter) CancelWithdrawal(ctx context.Context, wid delegation.WithdrawalID, reason string) (DelegationWithdrawalView, error) {
	var out DelegationWithdrawalView
	err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		w, err := a.svc.CancelWithdrawal(ctx, tx, wid, reason)
		if err != nil {
			return err
		}
		out, err = delegationWithdrawalByID(ctx, tx, w.ID)
		return err
	})
	return out, err
}

// detached is the context an execution runs in after a committed decision:
// the caller's values (its principal among them, which the service checks
// is not an agent before it acts as itself) without its cancellation, and a
// bounded budget of its own.
func (a delegationAdapter) detached(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), delegationExecutionBudget)
}

// --- operators -----------------------------------------------------------------------

func (a delegationAdapter) AllCapabilities(ctx context.Context, status delegation.CapabilityStatus, cursor string, limit int) (DelegationCapabilityPage, error) {
	at, after, err := delegationCursor(cursor)
	if err != nil {
		return DelegationCapabilityPage{}, err
	}
	limit = clampLimit(limit)
	var page DelegationCapabilityPage
	err = a.snapshot(ctx, func(ctx context.Context, tx pgx.Tx) error {
		items, err := delegationCapabilities(ctx, tx, delegationAllCapabilitiesSQL, string(status), at, after, limit+1)
		if err != nil {
			return err
		}
		page = delegationCapabilityPage(items, limit)
		return delegationWithUsage(ctx, tx, page.Items, a.now())
	})
	return page, err
}

func (a delegationAdapter) Pending(ctx context.Context, limit int) (DelegationPendingView, error) {
	limit = clampLimit(limit)
	var out DelegationPendingView
	err := a.snapshot(ctx, func(ctx context.Context, tx pgx.Tx) error {
		swaps, err := delegationSwaps(ctx, tx, delegationPendingSwapsSQL, limit)
		if err != nil {
			return err
		}
		if err := delegationSwapTrails(ctx, tx, swaps); err != nil {
			return err
		}
		withdrawals, err := delegationWithdrawals(ctx, tx, delegationPendingWithdrawalsSQL, limit)
		if err != nil {
			return err
		}
		if err := delegationWithdrawalTrails(ctx, tx, withdrawals); err != nil {
			return err
		}
		out = DelegationPendingView{Swaps: swaps, Withdrawals: withdrawals, AsOf: a.now()}
		return nil
	})
	return out, err
}

func (a delegationAdapter) RegisterVenue(ctx context.Context, r DelegationVenueRegistration) (DelegationVenueView, error) {
	// The settlement address is the co-signer's, never the caller's: a venue
	// at any other address could never have a swap co-signed.
	cs, ok := delegationCoSignerNamed(a.coSigners, r.Name)
	if !ok {
		return DelegationVenueView{}, validationError("name", "this deployment holds no co-signer for a venue of that name")
	}
	var out DelegationVenueView
	err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var house econ.HouseAccountID
		if r.HouseAccountID != nil {
			house = *r.HouseAccountID
		} else {
			var err error
			if house, err = delegationVenueHouse(ctx, tx, r.Name, r.Actor); err != nil {
				return err
			}
		}
		v, err := a.svc.RegisterVenue(ctx, tx, delegation.VenueSpec{Name: r.Name, Chain: cs.Chain, Address: cs.Address,
			HouseAccountID: house, SpreadBPS: r.SpreadBPS}, r.Actor)
		if err != nil {
			return err
		}
		out = DelegationVenueView{Venue: v, CoSigner: true}
		return nil
	})
	return out, err
}

// ExecutePending carries out what the delegation service selects as
// executable now (delegation.Service.ExecutableSubjects): only subjects that
// can be signed now, the accounts taking turns with a cap on each, the swap
// whose quote expires soonest first. The selection is the service's; nothing
// here decides which subjects run.
func (a delegationAdapter) ExecutePending(ctx context.Context, limit int) ([]DelegationExecutionResult, error) {
	if limit < 1 || limit > delegationExecuteMax {
		limit = delegationExecuteMax
	}
	candidates, err := a.svc.ExecutableSubjects(ctx, a.db, limit)
	if err != nil {
		return nil, err
	}
	out := make([]DelegationExecutionResult, 0, len(candidates))
	for _, c := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		r := DelegationExecutionResult{Kind: c.Kind, ID: c.ID(), AccountID: c.AccountID, StatusBefore: c.Status}
		switch c.Kind {
		case delegation.SubjectSwap:
			_, r.Err = a.svc.ExecuteSwap(ctx, c.SwapID)
			err = a.db.QueryRow(ctx, delegationSwapStatusSQL, c.SwapID).Scan(&r.Status)
		default:
			_, r.Err = a.svc.ExecuteWithdrawal(ctx, c.WithdrawalID)
			err = a.db.QueryRow(ctx, delegationWithdrawalStatusSQL, c.WithdrawalID).Scan(&r.Status)
		}
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// --- views -----------------------------------------------------------------------------

func (a delegationAdapter) capabilityView(ctx context.Context, q db.Querier, cid delegation.CapabilityID) (DelegationCapabilityView, error) {
	v, err := scanDelegationCapability(q.QueryRow(ctx, delegationCapabilityByIDSQL, cid))
	if errors.Is(err, pgx.ErrNoRows) {
		return DelegationCapabilityView{}, errs.New(errs.CodeNotFound, "delegation: capability not found")
	}
	if err != nil {
		return DelegationCapabilityView{}, err
	}
	items := []DelegationCapabilityView{v}
	if err := delegationWithUsage(ctx, q, items, a.now()); err != nil {
		return DelegationCapabilityView{}, err
	}
	return items[0], nil
}

// --- the read projection ---------------------------------------------------------------
//
// Strictly read-only SELECTs over the delegation tables, for the records the
// delegation package writes but does not list. Nothing here decides
// anything: values are returned exactly as the service stored them.

const delegationVenueLockSQL = `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`

const delegationAgentsSQL = `SELECT id::text, name, stage, state, strategy_id::text, created_at FROM agents
	WHERE account_id = $1 ORDER BY created_at DESC, id DESC LIMIT 200`

const delegationCapabilityColumns = `c.id, c.account_id, c.agent_id::text, c.strategy_id::text, c.environment, c.mode,
	c.instrument_classes, c.action_classes, c.max_notional_minor, c.max_daily_notional_minor, c.max_single_trade_minor,
	c.destination_allowlist, c.chain_allowlist, c.granted_at, c.expires_at, c.nonce, c.requires_human_approval, c.content_hash,
	c.status, c.granted_by_actor_type, c.granted_by_actor_id, coalesce(c.revoked_by_actor_type, ''),
	coalesce(c.revoked_by_actor_id, ''), coalesce(c.revocation_reason, ''), c.revoked_at, c.created_at, c.updated_at, ag.name`

const delegationCapabilityFrom = ` FROM delegated_capabilities c JOIN agents ag ON ag.id = c.agent_id`

const delegationAccountCapabilitiesSQL = `SELECT ` + delegationCapabilityColumns + delegationCapabilityFrom + `
	WHERE c.account_id = $1 AND ($2::timestamptz IS NULL OR (c.created_at, c.id) < ($2::timestamptz, $3::uuid))
	ORDER BY c.created_at DESC, c.id DESC LIMIT $4`

const delegationAllCapabilitiesSQL = `SELECT ` + delegationCapabilityColumns + delegationCapabilityFrom + `
	WHERE ($1::text = '' OR c.status = $1::text) AND ($2::timestamptz IS NULL OR (c.created_at, c.id) < ($2::timestamptz, $3::uuid))
	ORDER BY c.created_at DESC, c.id DESC LIMIT $4`

const delegationCapabilityByIDSQL = `SELECT ` + delegationCapabilityColumns + delegationCapabilityFrom + ` WHERE c.id = $1`

const delegationUsageTotalsSQL = `SELECT capability_id, count(*)::int, coalesce(sum(notional_minor), 0)::bigint,
	coalesce(sum(notional_minor) FILTER (WHERE usage_day = $2::date), 0)::bigint
	FROM delegated_capability_usage WHERE capability_id = ANY($1::text[]::uuid[]) GROUP BY capability_id`

const delegationUsesSQL = `SELECT id::text, subject_kind, subject_id::text, action_class, notional_minor, used_at
	FROM delegated_capability_usage WHERE capability_id = $1 ORDER BY used_at DESC, id DESC LIMIT 50`

const delegationDestinationColumns = `id, account_id, chain_id, address, label, status, added_by_actor_type, added_by_actor_id,
	added_at, coalesce(removed_by_actor_type, ''), coalesce(removed_by_actor_id, ''), removed_at`

const delegationAccountDestinationsSQL = `SELECT ` + delegationDestinationColumns + ` FROM delegated_withdrawal_destinations
	WHERE account_id = $1 ORDER BY added_at DESC, id DESC LIMIT 200`

const delegationDestinationByIDSQL = `SELECT ` + delegationDestinationColumns + ` FROM delegated_withdrawal_destinations WHERE id = $1`

const delegationPoliciesSQL = `SELECT id, account_id, version, auto_approve_enabled, auto_approve_max_minor, auto_approve_max_risk_score,
	max_risk_score, per_withdrawal_limit_minor, daily_limit_minor, velocity_max_count, min_destination_age_seconds,
	min_account_age_seconds, cooling_period_seconds, strong_auth_required, strong_auth_max_age_seconds, policy_hash,
	created_by_actor_type, created_by_actor_id, effective_at, created_at
	FROM delegated_withdrawal_policies WHERE account_id = $1 ORDER BY version DESC LIMIT 50`

const delegationSwapColumns = `s.id, s.capability_id, s.account_id, s.agent_id::text, s.request_nonce::text, s.intent_id, s.usage_id,
	s.reservation_id, s.venue_id, s.mode, s.action_class, s.chain_id, s.wallet, s.pay_asset_id, s.receive_asset_id,
	s.amount_in::text, s.amount_out::text, s.min_amount_out::text, s.notional_usd_minor, s.requested_at, s.quote_expires_at,
	s.requires_approval, s.plan, s.plan_hash, s.status, s.signed_at, coalesce(s.tx_id, ''), s.effect_id, s.journal_transaction_id,
	coalesce(s.failure_code, ''), s.created_at, s.updated_at, v.name, v.address, pa.symbol, pa.decimals, ra.symbol, ra.decimals`

const delegationSwapFrom = ` FROM delegated_swaps s JOIN delegation_venues v ON v.id = s.venue_id
	JOIN assets pa ON pa.id = s.pay_asset_id JOIN assets ra ON ra.id = s.receive_asset_id`

const delegationAccountSwapsSQL = `SELECT ` + delegationSwapColumns + delegationSwapFrom + `
	WHERE s.account_id = $1 AND ($2::text = '' OR s.status = $2::text)
	AND ($3::timestamptz IS NULL OR (s.created_at, s.id) < ($3::timestamptz, $4::uuid))
	ORDER BY s.created_at DESC, s.id DESC LIMIT $5`

const delegationSwapByIDSQL = `SELECT ` + delegationSwapColumns + delegationSwapFrom + ` WHERE s.id = $1`

const delegationPendingSwapsSQL = `SELECT ` + delegationSwapColumns + delegationSwapFrom + `
	WHERE s.status IN ('ADMITTED','AWAITING_APPROVAL','APPROVED','SIGNED','SUBMITTED','UNKNOWN')
	ORDER BY s.created_at, s.id LIMIT $1`

const delegationSwapStatusSQL = `SELECT status FROM delegated_swaps WHERE id = $1`

const delegationSwapApprovalsSQL = `SELECT swap_id::text, decision, plan_hash, approver_actor_type, approver_actor_id,
	authenticated_at, decided_at, reason FROM delegated_swap_approvals WHERE swap_id = ANY($1::text[]::uuid[])`

const delegationSwapDecisionsSQL = `SELECT swap_id::text, id::text, verdict, reason_codes, signer_public_key, capability_hash,
	plan_hash, message_sha256, policy_version, policy_hash, inspector_version, decided_at
	FROM delegated_signing_decisions WHERE swap_id = ANY($1::text[]::uuid[])`

const delegationSwapTransitionsSQL = `SELECT swap_id::text, from_status, to_status, actor_type, actor_id, reason, occurred_at
	FROM delegated_swap_transitions WHERE swap_id = ANY($1::text[]::uuid[]) ORDER BY occurred_at, id`

const delegationWithdrawalColumns = `w.id, w.capability_id, w.account_id, w.agent_id::text, w.proposal_nonce::text, w.intent_id,
	w.usage_id, w.reservation_id, w.policy_id, w.destination_id, w.environment, w.mode, w.chain_id, w.wallet,
	w.destination_address, w.asset_id, w.amount::text, w.notional_usd_minor, w.proposed_at, w.approval_expires_at,
	w.executable_at, w.evaluation_outcome, w.risk_score, w.evaluation, w.plan, w.plan_hash, w.status, w.signed_at,
	coalesce(w.tx_id, ''), w.effect_id, w.journal_transaction_id, coalesce(w.failure_code, ''), w.created_at, w.updated_at,
	a.symbol, a.decimals, d.label, d.status, p.version`

const delegationWithdrawalFrom = ` FROM delegated_withdrawals w JOIN assets a ON a.id = w.asset_id
	JOIN delegated_withdrawal_destinations d ON d.id = w.destination_id
	JOIN delegated_withdrawal_policies p ON p.id = w.policy_id`

const delegationAccountWithdrawalsSQL = `SELECT ` + delegationWithdrawalColumns + delegationWithdrawalFrom + `
	WHERE w.account_id = $1 AND ($2::text = '' OR w.status = $2::text)
	AND ($3::timestamptz IS NULL OR (w.created_at, w.id) < ($3::timestamptz, $4::uuid))
	ORDER BY w.created_at DESC, w.id DESC LIMIT $5`

const delegationWithdrawalByIDSQL = `SELECT ` + delegationWithdrawalColumns + delegationWithdrawalFrom + ` WHERE w.id = $1`

const delegationPendingWithdrawalsSQL = `SELECT ` + delegationWithdrawalColumns + delegationWithdrawalFrom + `
	WHERE w.status IN ('PROPOSED','AWAITING_APPROVAL','APPROVED','SIGNED','SUBMITTED','UNKNOWN')
	ORDER BY w.created_at, w.id LIMIT $1`

const delegationWithdrawalStatusSQL = `SELECT status FROM delegated_withdrawals WHERE id = $1`

const delegationWithdrawalApprovalsSQL = `SELECT withdrawal_id::text, decision, plan_hash, approver_actor_type, approver_actor_id,
	authenticated_at, decided_at, reason FROM delegated_withdrawal_approvals WHERE withdrawal_id = ANY($1::text[]::uuid[])`

const delegationWithdrawalDecisionsSQL = `SELECT withdrawal_id::text, id::text, verdict, reason_codes, signer_public_key,
	capability_hash, plan_hash, message_sha256, policy_version, policy_hash, inspector_version, decided_at
	FROM delegated_signing_decisions WHERE withdrawal_id = ANY($1::text[]::uuid[])`

const delegationWithdrawalTransitionsSQL = `SELECT withdrawal_id::text, from_status, to_status, actor_type, actor_id, reason,
	occurred_at FROM delegated_withdrawal_transitions WHERE withdrawal_id = ANY($1::text[]::uuid[]) ORDER BY occurred_at, id`

const delegationVenueColumns = `id, name, chain_id, address, house_account_id, spread_bps, created_by_actor_type,
	created_by_actor_id, created_at`

const delegationVenuesSQL = `SELECT ` + delegationVenueColumns + ` FROM delegation_venues ORDER BY created_at, id LIMIT 200`

const delegationVenueByNameSQL = `SELECT ` + delegationVenueColumns + ` FROM delegation_venues WHERE name = $1`

const delegationHouseByNameSQL = `SELECT id FROM house_accounts WHERE name = $1`

// delegationCursor decodes a page cursor into the bound parameters of a
// keyset query: (nil, nil) is the first page.
func delegationCursor(cursor string) (*time.Time, *string, error) {
	c, ok, err := decodeCursor(cursor)
	if err != nil || !ok {
		return nil, nil, err
	}
	u, err := uuid.Parse(c.ID)
	if err != nil {
		return nil, nil, validationError("cursor", "cursor is not a valid pagination cursor")
	}
	at, after := c.At, u.String()
	return &at, &after, nil
}

func delegationActor(actorType, actorID string) delegation.Actor {
	return delegation.Actor{Type: security.ActorType(actorType), ID: actorID}
}

func delegationHash(b []byte) ([32]byte, error) {
	var h [32]byte
	if len(b) != len(h) {
		return h, errs.New(errs.CodeInternal, "delegation: a stored hash has the wrong length")
	}
	copy(h[:], b)
	return h, nil
}

func delegationDecimals(d int16) (uint8, error) {
	if d < 0 || d > 38 {
		return 0, errs.New(errs.CodeInternal, "delegation: stored asset decimals are out of range")
	}
	return uint8(d), nil
}

func delegationQuantity(s string) (money.Quantity, error) {
	q, err := money.ScanQuantity(s)
	if err != nil {
		return money.Quantity{}, errs.Wrap(err, errs.CodeInternal, "delegation: a stored amount is not an integer")
	}
	return q, nil
}

func delegationKey(s string) (devnet.PublicKey, error) {
	k, err := devnet.ParsePublicKey(s)
	if err != nil {
		return devnet.PublicKey{}, errs.New(errs.CodeInternal, "delegation: a stored address is not a devnet address")
	}
	return k, nil
}

func delegationCoSignerNamed(list []DelegationCoSigner, name string) (DelegationCoSigner, bool) {
	for _, cs := range list {
		if cs.Name == name {
			return cs, true
		}
	}
	return DelegationCoSigner{}, false
}

func delegationCoSignerAt(list []DelegationCoSigner, chain devnet.ChainID, address devnet.PublicKey) (DelegationCoSigner, bool) {
	for _, cs := range list {
		if cs.Chain == chain && cs.Address == address {
			return cs, true
		}
	}
	return DelegationCoSigner{}, false
}

// delegationVenueHouse returns the house book named for the venue, creating
// it in ACTIVE if it does not exist: a venue's counterparty is a house book of
// its own, so no venue spends another's inventory.
func delegationVenueHouse(ctx context.Context, tx pgx.Tx, venue string, by delegation.Actor) (econ.HouseAccountID, error) {
	name := "delegation venue " + venue
	var hid econ.HouseAccountID
	err := tx.QueryRow(ctx, delegationHouseByNameSQL, name).Scan(&hid)
	if err == nil {
		return hid, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return econ.HouseAccountID{}, err
	}
	h, err := econ.NewRegistry().CreateHouseAccount(ctx, tx, name, econ.HouseLiquidityProvision, by)
	if err != nil {
		return econ.HouseAccountID{}, err
	}
	return h.ID, nil
}

func delegationVenueByName(ctx context.Context, q db.Querier, name string) (delegation.Venue, bool, error) {
	v, err := scanDelegationVenue(q.QueryRow(ctx, delegationVenueByNameSQL, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return delegation.Venue{}, false, nil
	}
	if err != nil {
		return delegation.Venue{}, false, err
	}
	return v, true, nil
}

func scanDelegationVenue(row pgx.Row) (delegation.Venue, error) {
	var v delegation.Venue
	var address, actorType string
	var spread int64
	if err := row.Scan(&v.ID, &v.Name, &v.Chain, &address, &v.HouseAccountID, &spread, &actorType, &v.CreatedBy.ID,
		&v.CreatedAt); err != nil {
		return delegation.Venue{}, err
	}
	var err error
	if v.Address, err = delegationKey(address); err != nil {
		return delegation.Venue{}, err
	}
	v.CreatedBy.Type, v.SpreadBPS, v.CreatedAt = security.ActorType(actorType), money.BPS(spread), v.CreatedAt.UTC()
	return v, nil
}

func scanDelegationCapability(row pgx.Row) (DelegationCapabilityView, error) {
	var v DelegationCapabilityView
	c := &v.Capability
	var instruments, actions, destinations, chains []string
	var maxN, maxD, maxS int64
	var hash []byte
	var grantedType, revokedType, revokedID string
	if err := row.Scan(&c.ID, &c.Terms.AccountID, &c.Terms.AgentID, &c.Terms.StrategyID, &c.Terms.Environment, &c.Terms.Mode,
		&instruments, &actions, &maxN, &maxD, &maxS, &destinations, &chains, &c.GrantedAt, &c.Terms.ExpiresAt, &c.Nonce,
		&c.Terms.RequiresHumanApproval, &hash, &c.Status, &grantedType, &c.GrantedBy.ID, &revokedType, &revokedID,
		&c.RevocationReason, &c.RevokedAt, &c.CreatedAt, &c.UpdatedAt, &v.AgentName); err != nil {
		return DelegationCapabilityView{}, err
	}
	for _, s := range instruments {
		c.Terms.InstrumentClasses = append(c.Terms.InstrumentClasses, delegation.InstrumentClass(s))
	}
	for _, s := range actions {
		c.Terms.ActionClasses = append(c.Terms.ActionClasses, delegation.ActionClass(s))
	}
	for _, s := range chains {
		c.Terms.ChainAllowlist = append(c.Terms.ChainAllowlist, devnet.ChainID(s))
	}
	for _, s := range destinations {
		k, err := delegationKey(s)
		if err != nil {
			return DelegationCapabilityView{}, err
		}
		c.Terms.DestinationAllowlist = append(c.Terms.DestinationAllowlist, k)
	}
	c.Terms.MaxNotional, c.Terms.MaxDailyNotional, c.Terms.MaxSingleTrade =
		money.USDFromMinor(maxN), money.USDFromMinor(maxD), money.USDFromMinor(maxS)
	var err error
	if c.ContentHash, err = delegationHash(hash); err != nil {
		return DelegationCapabilityView{}, err
	}
	c.GrantedBy.Type = security.ActorType(grantedType)
	if revokedType != "" {
		r := delegationActor(revokedType, revokedID)
		c.RevokedBy = &r
	}
	c.GrantedAt, c.Terms.ExpiresAt, c.CreatedAt, c.UpdatedAt = c.GrantedAt.UTC(), c.Terms.ExpiresAt.UTC(), c.CreatedAt.UTC(), c.UpdatedAt.UTC()
	if c.RevokedAt != nil {
		t := c.RevokedAt.UTC()
		c.RevokedAt = &t
	}
	return v, nil
}

func delegationCapabilities(ctx context.Context, q db.Querier, sql string, args ...any) ([]DelegationCapabilityView, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DelegationCapabilityView{}
	for rows.Next() {
		v, err := scanDelegationCapability(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func delegationCapabilityPage(items []DelegationCapabilityView, limit int) DelegationCapabilityPage {
	page := DelegationCapabilityPage{Items: items}
	if len(items) > limit {
		page.Items = items[:limit]
		last := page.Items[limit-1].Capability
		page.NextCursor = encodeCursor(keysetCursor{At: last.CreatedAt, ID: last.ID.String()})
	}
	return page
}

// delegationWithUsage fills each capability's use as of now: on the UTC day
// of now and in all, as the usage rows record it.
func delegationWithUsage(ctx context.Context, q db.Querier, items []DelegationCapabilityView, now time.Time) error {
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	ids := make([]string, 0, len(items))
	for i := range items {
		items[i].AsOf = now
		items[i].Usage = DelegationUsage{Day: day, UsedToday: money.USDFromMinor(0), UsedTotal: money.USDFromMinor(0)}
		ids = append(ids, items[i].Capability.ID.String())
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := q.Query(ctx, delegationUsageTotalsSQL, ids, day.Format(time.DateOnly))
	if err != nil {
		return err
	}
	defer rows.Close()
	totals := map[string]DelegationUsage{}
	for rows.Next() {
		var cid delegation.CapabilityID
		var uses int
		var total, today int64
		if err := rows.Scan(&cid, &uses, &total, &today); err != nil {
			return err
		}
		totals[cid.String()] = DelegationUsage{Day: day, Uses: uses, UsedToday: money.USDFromMinor(today), UsedTotal: money.USDFromMinor(total)}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range items {
		if u, ok := totals[items[i].Capability.ID.String()]; ok {
			items[i].Usage = u
		}
	}
	return nil
}

func delegationUses(ctx context.Context, q db.Querier, cid delegation.CapabilityID) ([]DelegationUse, error) {
	rows, err := q.Query(ctx, delegationUsesSQL, cid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DelegationUse{}
	for rows.Next() {
		var u DelegationUse
		var notional int64
		if err := rows.Scan(&u.ID, &u.SubjectKind, &u.SubjectID, &u.Action, &notional, &u.UsedAt); err != nil {
			return nil, err
		}
		u.Notional, u.UsedAt = money.USDFromMinor(notional), u.UsedAt.UTC()
		out = append(out, u)
	}
	return out, rows.Err()
}

func scanDelegationDestination(row pgx.Row) (delegation.Destination, error) {
	var d delegation.Destination
	var address, addedType, removedType, removedID string
	if err := row.Scan(&d.ID, &d.AccountID, &d.Chain, &address, &d.Label, &d.Status, &addedType, &d.AddedBy.ID, &d.AddedAt,
		&removedType, &removedID, &d.RemovedAt); err != nil {
		return delegation.Destination{}, err
	}
	var err error
	if d.Address, err = delegationKey(address); err != nil {
		return delegation.Destination{}, err
	}
	d.AddedBy.Type, d.AddedAt = security.ActorType(addedType), d.AddedAt.UTC()
	if removedType != "" {
		r := delegationActor(removedType, removedID)
		d.RemovedBy = &r
	}
	if d.RemovedAt != nil {
		t := d.RemovedAt.UTC()
		d.RemovedAt = &t
	}
	return d, nil
}

func scanDelegationPolicy(row pgx.Row) (delegation.WithdrawalPolicy, error) {
	var p delegation.WithdrawalPolicy
	var autoMax, perWithdrawal, daily, minDestination, minAccount, cooling, authAge int64
	var hash []byte
	var createdType string
	if err := row.Scan(&p.ID, &p.AccountID, &p.Version, &p.AutoApproveEnabled, &autoMax, &p.AutoApproveMaxRiskScore,
		&p.MaxRiskScore, &perWithdrawal, &daily, &p.VelocityMaxCount, &minDestination, &minAccount, &cooling,
		&p.StrongAuthRequired, &authAge, &hash, &createdType, &p.CreatedBy.ID, &p.EffectiveAt, &p.CreatedAt); err != nil {
		return delegation.WithdrawalPolicy{}, err
	}
	p.AutoApproveMax, p.PerWithdrawalLimit, p.DailyLimit = money.USDFromMinor(autoMax), money.USDFromMinor(perWithdrawal), money.USDFromMinor(daily)
	p.MinDestinationAge, p.MinAccountAge = time.Duration(minDestination)*time.Second, time.Duration(minAccount)*time.Second
	p.CoolingPeriod, p.StrongAuthMaxAge = time.Duration(cooling)*time.Second, time.Duration(authAge)*time.Second
	var err error
	if p.PolicyHash, err = delegationHash(hash); err != nil {
		return delegation.WithdrawalPolicy{}, err
	}
	p.CreatedBy.Type, p.EffectiveAt, p.CreatedAt = security.ActorType(createdType), p.EffectiveAt.UTC(), p.CreatedAt.UTC()
	return p, nil
}

func scanDelegationSwap(row pgx.Row) (DelegationSwapView, error) {
	var v DelegationSwapView
	s := &v.Swap
	var nonce, wallet, in, out, minOut, txID, venueAddress string
	var notional int64
	var plan, hash []byte
	var payDecimals, receiveDecimals int16
	if err := row.Scan(&s.ID, &s.CapabilityID, &s.AccountID, &s.AgentID, &nonce, &s.IntentID, &s.UsageID, &s.ReservationID,
		&s.VenueID, &s.Mode, &s.Action, &s.Chain, &wallet, &s.PayAssetID, &s.ReceiveAssetID, &in, &out, &minOut, &notional,
		&s.RequestedAt, &s.QuoteExpiresAt, &s.RequiresApproval, &plan, &hash, &s.Status, &s.SignedAt, &txID, &s.EffectID,
		&s.JournalTransactionID, &s.FailureCode, &s.CreatedAt, &s.UpdatedAt, &v.Venue.Name, &venueAddress,
		&v.PayAsset.Symbol, &payDecimals, &v.ReceiveAsset.Symbol, &receiveDecimals); err != nil {
		return DelegationSwapView{}, err
	}
	var err error
	if s.RequestNonce, err = strconv.ParseUint(nonce, 10, 64); err != nil {
		return DelegationSwapView{}, errs.New(errs.CodeInternal, "delegation: a stored request nonce is not a u64")
	}
	if s.Wallet, err = delegationKey(wallet); err != nil {
		return DelegationSwapView{}, err
	}
	if s.AmountIn, err = delegationQuantity(in); err != nil {
		return DelegationSwapView{}, err
	}
	if s.AmountOut, err = delegationQuantity(out); err != nil {
		return DelegationSwapView{}, err
	}
	if s.MinAmountOut, err = delegationQuantity(minOut); err != nil {
		return DelegationSwapView{}, err
	}
	if err := json.Unmarshal(plan, &s.Plan); err != nil {
		return DelegationSwapView{}, errs.Wrap(err, errs.CodeInternal, "delegation: a stored swap plan does not decode")
	}
	if s.PlanHash, err = delegationHash(hash); err != nil {
		return DelegationSwapView{}, err
	}
	if v.PayAsset.Decimals, err = delegationDecimals(payDecimals); err != nil {
		return DelegationSwapView{}, err
	}
	if v.ReceiveAsset.Decimals, err = delegationDecimals(receiveDecimals); err != nil {
		return DelegationSwapView{}, err
	}
	s.TxID, s.NotionalUSD = devnet.TxID(txID), money.USDFromMinor(notional)
	s.RequestedAt, s.QuoteExpiresAt, s.CreatedAt, s.UpdatedAt = s.RequestedAt.UTC(), s.QuoteExpiresAt.UTC(), s.CreatedAt.UTC(), s.UpdatedAt.UTC()
	if s.SignedAt != nil {
		t := s.SignedAt.UTC()
		s.SignedAt = &t
	}
	v.Venue.ID, v.Venue.Address = s.VenueID, venueAddress
	v.PayAsset.ID, v.ReceiveAsset.ID = s.PayAssetID, s.ReceiveAssetID
	v.Transitions = []DelegationTransitionView{}
	return v, nil
}

func delegationSwaps(ctx context.Context, q db.Querier, sql string, args ...any) ([]DelegationSwapView, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DelegationSwapView{}
	for rows.Next() {
		v, err := scanDelegationSwap(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func delegationSwapByID(ctx context.Context, q db.Querier, sid delegation.SwapID) (DelegationSwapView, error) {
	v, err := scanDelegationSwap(q.QueryRow(ctx, delegationSwapByIDSQL, sid))
	if errors.Is(err, pgx.ErrNoRows) {
		return DelegationSwapView{}, errs.New(errs.CodeNotFound, "delegation: swap not found")
	}
	if err != nil {
		return DelegationSwapView{}, err
	}
	items := []DelegationSwapView{v}
	if err := delegationSwapTrails(ctx, q, items); err != nil {
		return DelegationSwapView{}, err
	}
	return items[0], nil
}

// delegationTrail is the decision trail of a set of subjects, by subject id.
type delegationTrail struct {
	approvals   map[string]*DelegationApprovalView
	inspections map[string]*DelegationInspectionView
	transitions map[string][]DelegationTransitionView
}

// delegationTrails reads the approvals, signing decisions and transitions of
// the subjects named by ids, with one statement each.
func delegationTrails(ctx context.Context, q db.Querier, ids []string, approvalsSQL, decisionsSQL, transitionsSQL string) (delegationTrail, error) {
	t := delegationTrail{approvals: map[string]*DelegationApprovalView{}, inspections: map[string]*DelegationInspectionView{},
		transitions: map[string][]DelegationTransitionView{}}
	if len(ids) == 0 {
		return t, nil
	}
	if err := delegationEach(ctx, q, approvalsSQL, ids, func(rows pgx.Rows) error {
		var subject, approverType string
		a := &DelegationApprovalView{}
		if err := rows.Scan(&subject, &a.Decision, &a.PlanHash, &approverType, &a.Approver.ID, &a.AuthenticatedAt, &a.DecidedAt,
			&a.Reason); err != nil {
			return err
		}
		a.Approver.Type, a.DecidedAt = security.ActorType(approverType), a.DecidedAt.UTC()
		if a.AuthenticatedAt != nil {
			at := a.AuthenticatedAt.UTC()
			a.AuthenticatedAt = &at
		}
		t.approvals[subject] = a
		return nil
	}); err != nil {
		return delegationTrail{}, err
	}
	if err := delegationEach(ctx, q, decisionsSQL, ids, func(rows pgx.Rows) error {
		var subject string
		d := &DelegationInspectionView{}
		if err := rows.Scan(&subject, &d.ID, &d.Verdict, &d.ReasonCodes, &d.SignerPublicKey, &d.CapabilityHash, &d.PlanHash,
			&d.MessageSHA256, &d.PolicyVersion, &d.PolicyHash, &d.InspectorVersion, &d.DecidedAt); err != nil {
			return err
		}
		if d.ReasonCodes == nil {
			d.ReasonCodes = []string{}
		}
		d.DecidedAt = d.DecidedAt.UTC()
		t.inspections[subject] = d
		return nil
	}); err != nil {
		return delegationTrail{}, err
	}
	if err := delegationEach(ctx, q, transitionsSQL, ids, func(rows pgx.Rows) error {
		var subject, actorType string
		var tr DelegationTransitionView
		if err := rows.Scan(&subject, &tr.From, &tr.To, &actorType, &tr.Actor.ID, &tr.Reason, &tr.At); err != nil {
			return err
		}
		tr.Actor.Type, tr.At = security.ActorType(actorType), tr.At.UTC()
		t.transitions[subject] = append(t.transitions[subject], tr)
		return nil
	}); err != nil {
		return delegationTrail{}, err
	}
	return t, nil
}

func delegationEach(ctx context.Context, q db.Querier, sql string, ids []string, fn func(rows pgx.Rows) error) error {
	rows, err := q.Query(ctx, sql, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := fn(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

func delegationSwapTrails(ctx context.Context, q db.Querier, items []DelegationSwapView) error {
	ids := make([]string, 0, len(items))
	for _, v := range items {
		ids = append(ids, v.Swap.ID.String())
	}
	t, err := delegationTrails(ctx, q, ids, delegationSwapApprovalsSQL, delegationSwapDecisionsSQL, delegationSwapTransitionsSQL)
	if err != nil {
		return err
	}
	for i := range items {
		id := items[i].Swap.ID.String()
		items[i].Approval, items[i].Inspection = t.approvals[id], t.inspections[id]
		if tr, ok := t.transitions[id]; ok {
			items[i].Transitions = tr
		}
	}
	return nil
}

func scanDelegationWithdrawal(row pgx.Row) (DelegationWithdrawalView, error) {
	var v DelegationWithdrawalView
	w := &v.Withdrawal
	var nonce, wallet, destination, amount, txID string
	var notional int64
	var evaluation, plan, hash []byte
	var decimals int16
	if err := row.Scan(&w.ID, &w.CapabilityID, &w.AccountID, &w.AgentID, &nonce, &w.IntentID, &w.UsageID, &w.ReservationID,
		&w.PolicyID, &w.DestinationID, &w.Environment, &w.Mode, &w.Chain, &wallet, &destination, &w.AssetID, &amount, &notional,
		&w.ProposedAt, &w.ApprovalExpiresAt, &w.ExecutableAt, &w.EvaluationOutcome, &w.RiskScore, &evaluation, &plan, &hash,
		&w.Status, &w.SignedAt, &txID, &w.EffectID, &w.JournalTransactionID, &w.FailureCode, &w.CreatedAt, &w.UpdatedAt,
		&v.Asset.Symbol, &decimals, &v.Destination.Label, &v.Destination.Status, &v.PolicyVersion); err != nil {
		return DelegationWithdrawalView{}, err
	}
	var err error
	if w.ProposalNonce, err = strconv.ParseUint(nonce, 10, 64); err != nil {
		return DelegationWithdrawalView{}, errs.New(errs.CodeInternal, "delegation: a stored proposal nonce is not a u64")
	}
	if w.Wallet, err = delegationKey(wallet); err != nil {
		return DelegationWithdrawalView{}, err
	}
	if w.DestinationAddress, err = delegationKey(destination); err != nil {
		return DelegationWithdrawalView{}, err
	}
	if w.Amount, err = delegationQuantity(amount); err != nil {
		return DelegationWithdrawalView{}, err
	}
	if err := json.Unmarshal(evaluation, &w.Evaluation); err != nil {
		return DelegationWithdrawalView{}, errs.Wrap(err, errs.CodeInternal, "delegation: a stored evaluation does not decode")
	}
	if err := json.Unmarshal(plan, &w.Plan); err != nil {
		return DelegationWithdrawalView{}, errs.Wrap(err, errs.CodeInternal, "delegation: a stored withdrawal plan does not decode")
	}
	if w.PlanHash, err = delegationHash(hash); err != nil {
		return DelegationWithdrawalView{}, err
	}
	if v.Asset.Decimals, err = delegationDecimals(decimals); err != nil {
		return DelegationWithdrawalView{}, err
	}
	w.TxID, w.NotionalUSD = devnet.TxID(txID), money.USDFromMinor(notional)
	w.ProposedAt, w.ApprovalExpiresAt, w.ExecutableAt = w.ProposedAt.UTC(), w.ApprovalExpiresAt.UTC(), w.ExecutableAt.UTC()
	w.CreatedAt, w.UpdatedAt = w.CreatedAt.UTC(), w.UpdatedAt.UTC()
	if w.SignedAt != nil {
		t := w.SignedAt.UTC()
		w.SignedAt = &t
	}
	v.Asset.ID, v.Destination.ID = w.AssetID, w.DestinationID
	v.Transitions = []DelegationTransitionView{}
	return v, nil
}

func delegationWithdrawals(ctx context.Context, q db.Querier, sql string, args ...any) ([]DelegationWithdrawalView, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DelegationWithdrawalView{}
	for rows.Next() {
		v, err := scanDelegationWithdrawal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func delegationWithdrawalByID(ctx context.Context, q db.Querier, wid delegation.WithdrawalID) (DelegationWithdrawalView, error) {
	v, err := scanDelegationWithdrawal(q.QueryRow(ctx, delegationWithdrawalByIDSQL, wid))
	if errors.Is(err, pgx.ErrNoRows) {
		return DelegationWithdrawalView{}, errs.New(errs.CodeNotFound, "delegation: withdrawal not found")
	}
	if err != nil {
		return DelegationWithdrawalView{}, err
	}
	items := []DelegationWithdrawalView{v}
	if err := delegationWithdrawalTrails(ctx, q, items); err != nil {
		return DelegationWithdrawalView{}, err
	}
	return items[0], nil
}

func delegationWithdrawalTrails(ctx context.Context, q db.Querier, items []DelegationWithdrawalView) error {
	ids := make([]string, 0, len(items))
	for _, v := range items {
		ids = append(ids, v.Withdrawal.ID.String())
	}
	t, err := delegationTrails(ctx, q, ids, delegationWithdrawalApprovalsSQL, delegationWithdrawalDecisionsSQL,
		delegationWithdrawalTransitionsSQL)
	if err != nil {
		return err
	}
	for i := range items {
		id := items[i].Withdrawal.ID.String()
		items[i].Approval, items[i].Inspection = t.approvals[id], t.inspections[id]
		if tr, ok := t.transitions[id]; ok {
			items[i].Transitions = tr
		}
	}
	return nil
}
