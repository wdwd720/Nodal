package delegation

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

// --- Capabilities -------------------------------------------------------------

const capabilityColumns = `id, account_id, agent_id::text, strategy_id::text, environment, mode, instrument_classes,
	action_classes, max_notional_minor, max_daily_notional_minor, max_single_trade_minor, destination_allowlist,
	chain_allowlist, granted_at, expires_at, nonce, requires_human_approval, content_hash, idempotency_key, status,
	granted_by_actor_type, granted_by_actor_id, coalesce(revoked_by_actor_type, ''), coalesce(revoked_by_actor_id, ''),
	coalesce(revocation_reason, ''), revoked_at, created_at, updated_at`

func scanCapability(row pgx.Row) (Capability, error) {
	var c Capability
	var instr, actions, dests, chains []string
	var maxN, maxD, maxS int64
	var hash []byte
	var revType, revID string
	if err := row.Scan(&c.ID, &c.Terms.AccountID, &c.Terms.AgentID, &c.Terms.StrategyID, &c.Terms.Environment, &c.Terms.Mode,
		&instr, &actions, &maxN, &maxD, &maxS, &dests, &chains, &c.GrantedAt, &c.Terms.ExpiresAt, &c.Nonce,
		&c.Terms.RequiresHumanApproval, &hash, &c.IdempotencyKey, &c.Status, &c.GrantedBy.Type, &c.GrantedBy.ID,
		&revType, &revID, &c.RevocationReason, &c.RevokedAt, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return Capability{}, err
	}
	for _, s := range instr {
		c.Terms.InstrumentClasses = append(c.Terms.InstrumentClasses, InstrumentClass(s))
	}
	for _, s := range actions {
		c.Terms.ActionClasses = append(c.Terms.ActionClasses, ActionClass(s))
	}
	for _, s := range chains {
		c.Terms.ChainAllowlist = append(c.Terms.ChainAllowlist, devnet.ChainID(s))
	}
	for _, s := range dests {
		k, err := devnet.ParsePublicKey(s)
		if err != nil {
			return Capability{}, errs.New(errs.CodeInternal, "delegation: a stored destination is not a devnet address")
		}
		c.Terms.DestinationAllowlist = append(c.Terms.DestinationAllowlist, k)
	}
	c.Terms.MaxNotional, c.Terms.MaxDailyNotional, c.Terms.MaxSingleTrade =
		money.USDFromMinor(maxN), money.USDFromMinor(maxD), money.USDFromMinor(maxS)
	if len(hash) != 32 {
		return Capability{}, errs.New(errs.CodeInternal, "delegation: stored capability hash has the wrong length")
	}
	copy(c.ContentHash[:], hash)
	if revType != "" {
		c.RevokedBy = &Actor{Type: security.ActorType(revType), ID: revID}
	}
	c.GrantedAt, c.Terms.ExpiresAt = c.GrantedAt.UTC(), c.Terms.ExpiresAt.UTC()
	if c.RevokedAt != nil {
		t := c.RevokedAt.UTC()
		c.RevokedAt = &t
	}
	c.CreatedAt, c.UpdatedAt = c.CreatedAt.UTC(), c.UpdatedAt.UTC()
	return c, nil
}

func getCapability(ctx context.Context, q db.Querier, cid CapabilityID, forUpdate bool) (Capability, error) {
	sql := `SELECT ` + capabilityColumns + ` FROM delegated_capabilities WHERE id = $1`
	if forUpdate {
		sql += ` FOR UPDATE`
	}
	c, err := scanCapability(q.QueryRow(ctx, sql, cid))
	if errors.Is(err, pgx.ErrNoRows) {
		return Capability{}, errs.New(errs.CodeNotFound, "delegation: capability not found").WithField("capability_id", cid.String())
	}
	if err != nil {
		return Capability{}, dbErr("load capability", err)
	}
	return c, nil
}

// usageTotals returns what a capability has already used on day and in all.
func usageTotals(ctx context.Context, q db.Querier, cid CapabilityID, day time.Time) (money.USD, money.USD, error) {
	var dayMinor, allMinor int64
	err := q.QueryRow(ctx, `SELECT
		coalesce(sum(notional_minor) FILTER (WHERE usage_day = $2::date), 0)::bigint,
		coalesce(sum(notional_minor), 0)::bigint
		FROM delegated_capability_usage WHERE capability_id = $1`, cid, day.UTC().Format(time.DateOnly)).Scan(&dayMinor, &allMinor)
	if err != nil {
		return money.USD{}, money.USD{}, dbErr("read capability usage", err)
	}
	return money.USDFromMinor(dayMinor), money.USDFromMinor(allMinor), nil
}

func insertUsage(ctx context.Context, tx pgx.Tx, uid UsageID, c Capability, kind SubjectKind, subject string,
	action ActionClass, notional money.USD, at time.Time) error {
	at = dbTime(at)
	_, err := tx.Exec(ctx, `INSERT INTO delegated_capability_usage
		(id, capability_id, account_id, agent_id, subject_kind, subject_id, action_class, notional_minor, used_at, usage_day)
		VALUES ($1,$2,$3,$4::uuid,$5,$6::uuid,$7,$8,$9,$10::date)`,
		uid, c.ID, c.Terms.AccountID, c.Terms.AgentID, kind, subject, action, notional.Minor(), at, at.Format(time.DateOnly))
	if err != nil {
		return dbErr("record capability usage", err)
	}
	return nil
}

// --- Agents ---------------------------------------------------------------------

type agentRow struct {
	ID         string
	AccountID  accounts.AccountID
	StrategyID string
	State      string
}

// usable reports whether the agent may act at all: not paused, failed,
// revoked or superseded.
func (a agentRow) usable() bool {
	switch a.State {
	case "PAUSED", "FAILED", "REVOKED", "SUPERSEDED":
		return false
	}
	return true
}

func getAgent(ctx context.Context, q db.Querier, agentID string) (agentRow, bool, error) {
	var a agentRow
	err := q.QueryRow(ctx, `SELECT id::text, account_id, strategy_id::text, state FROM agents WHERE id = $1::uuid`, agentID).
		Scan(&a.ID, &a.AccountID, &a.StrategyID, &a.State)
	if errors.Is(err, pgx.ErrNoRows) {
		return agentRow{}, false, nil
	}
	if err != nil {
		return agentRow{}, false, dbErr("load agent", err)
	}
	return a, true, nil
}

func strategyActive(ctx context.Context, q db.Querier, strategyID string) (bool, error) {
	var status string
	err := q.QueryRow(ctx, `SELECT status FROM strategies WHERE id = $1::uuid`, strategyID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, dbErr("load strategy", err)
	}
	return status == "ACTIVE", nil
}

// --- Venues ---------------------------------------------------------------------

// Venue is a delegation_venues row: a sandbox counterparty backed by a house
// book, quoting at the mark less its spread.
type Venue struct {
	ID             VenueID
	Name           string
	Chain          devnet.ChainID
	Address        devnet.PublicKey
	HouseAccountID econ.HouseAccountID
	SpreadBPS      money.BPS
	CreatedBy      Actor
	CreatedAt      time.Time
}

const venueColumns = `id, name, chain_id, address, house_account_id, spread_bps, created_by_actor_type, created_by_actor_id, created_at`

func scanVenue(row pgx.Row) (Venue, error) {
	var v Venue
	var addr string
	var spread int64
	if err := row.Scan(&v.ID, &v.Name, &v.Chain, &addr, &v.HouseAccountID, &spread, &v.CreatedBy.Type, &v.CreatedBy.ID, &v.CreatedAt); err != nil {
		return Venue{}, err
	}
	k, err := devnet.ParsePublicKey(addr)
	if err != nil {
		return Venue{}, errs.New(errs.CodeInternal, "delegation: a stored venue address is not a devnet address")
	}
	v.Address, v.SpreadBPS, v.CreatedAt = k, money.BPS(spread), v.CreatedAt.UTC()
	return v, nil
}

func getVenue(ctx context.Context, q db.Querier, vid VenueID) (Venue, error) {
	v, err := scanVenue(q.QueryRow(ctx, `SELECT `+venueColumns+` FROM delegation_venues WHERE id = $1`, vid))
	if errors.Is(err, pgx.ErrNoRows) {
		return Venue{}, errs.New(errs.CodeNotFound, "delegation: venue not found").WithField("venue_id", vid.String())
	}
	if err != nil {
		return Venue{}, dbErr("load venue", err)
	}
	return v, nil
}

// --- Signer keys ------------------------------------------------------------------

func registeredKey(ctx context.Context, q db.Querier, account accounts.AccountID, chain devnet.ChainID) (devnet.PublicKey, bool, error) {
	var s string
	err := q.QueryRow(ctx, `SELECT public_key FROM delegated_signer_keys WHERE account_id = $1 AND chain_id = $2`, account, chain).Scan(&s)
	if errors.Is(err, pgx.ErrNoRows) {
		return devnet.PublicKey{}, false, nil
	}
	if err != nil {
		return devnet.PublicKey{}, false, dbErr("load signer key", err)
	}
	k, err := devnet.ParsePublicKey(s)
	if err != nil {
		return devnet.PublicKey{}, false, errs.New(errs.CodeInternal, "delegation: a stored signer key is not a devnet address")
	}
	return k, true, nil
}

// --- Swaps ------------------------------------------------------------------------

// Swap is a delegated_swaps row.
type Swap struct {
	ID                   SwapID
	CapabilityID         CapabilityID
	AccountID            accounts.AccountID
	AgentID              string
	RequestNonce         uint64
	IntentID             econ.IntentID
	UsageID              UsageID
	ReservationID        capital.ReservationID
	VenueID              VenueID
	Mode                 gates.Mode
	Action               ActionClass
	Chain                devnet.ChainID
	Wallet               devnet.PublicKey
	PayAssetID           assets.AssetID
	ReceiveAssetID       assets.AssetID
	AmountIn             money.Quantity
	AmountOut            money.Quantity
	MinAmountOut         money.Quantity
	NotionalUSD          money.USD
	RequestedAt          time.Time
	QuoteExpiresAt       time.Time
	RequiresApproval     bool
	Plan                 SwapPlan
	PlanHash             [32]byte
	Status               SwapStatus
	Message              []byte
	RecentBlockhash      devnet.Hash
	BlockhashHeight      uint64
	SignedAt             *time.Time
	TxID                 devnet.TxID
	SignedTx             []byte
	EffectID             *econ.EffectID
	JournalTransactionID *ledger.TransactionID
	FailureCode          string
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

const swapColumns = `id, capability_id, account_id, agent_id::text, request_nonce::text, intent_id, usage_id, reservation_id,
	venue_id, mode, action_class, chain_id, wallet, pay_asset_id, receive_asset_id, amount_in::text, amount_out::text,
	min_amount_out::text, notional_usd_minor, requested_at, quote_expires_at, requires_approval, plan, plan_hash, status,
	message, coalesce(recent_blockhash, ''), coalesce(blockhash_height, 0), signed_at, coalesce(tx_id, ''), signed_tx,
	effect_id, journal_transaction_id, coalesce(failure_code, ''), created_at, updated_at`

func scanSwap(row pgx.Row) (Swap, error) {
	var s Swap
	var nonce, in, out, minOut, wallet, bh, txID string
	var notional, height int64
	var plan, hash []byte
	if err := row.Scan(&s.ID, &s.CapabilityID, &s.AccountID, &s.AgentID, &nonce, &s.IntentID, &s.UsageID, &s.ReservationID,
		&s.VenueID, &s.Mode, &s.Action, &s.Chain, &wallet, &s.PayAssetID, &s.ReceiveAssetID, &in, &out, &minOut, &notional,
		&s.RequestedAt, &s.QuoteExpiresAt, &s.RequiresApproval, &plan, &hash, &s.Status, &s.Message, &bh, &height,
		&s.SignedAt, &txID, &s.SignedTx, &s.EffectID, &s.JournalTransactionID, &s.FailureCode, &s.CreatedAt, &s.UpdatedAt); err != nil {
		return Swap{}, err
	}
	var err error
	if s.RequestNonce, err = strconv.ParseUint(nonce, 10, 64); err != nil {
		return Swap{}, errs.New(errs.CodeInternal, "delegation: stored request nonce is not a u64")
	}
	if s.AmountIn, err = money.ScanQuantity(in); err != nil {
		return Swap{}, err
	}
	if s.AmountOut, err = money.ScanQuantity(out); err != nil {
		return Swap{}, err
	}
	if s.MinAmountOut, err = money.ScanQuantity(minOut); err != nil {
		return Swap{}, err
	}
	if s.Wallet, err = devnet.ParsePublicKey(wallet); err != nil {
		return Swap{}, errs.New(errs.CodeInternal, "delegation: stored wallet is not a devnet address")
	}
	if err := json.Unmarshal(plan, &s.Plan); err != nil {
		return Swap{}, errs.Wrap(err, errs.CodeInternal, "delegation: stored swap plan does not decode")
	}
	if len(hash) != 32 {
		return Swap{}, errs.New(errs.CodeInternal, "delegation: stored plan hash has the wrong length")
	}
	copy(s.PlanHash[:], hash)
	if bh != "" {
		if s.RecentBlockhash, err = devnet.ParseHash(bh); err != nil {
			return Swap{}, errs.New(errs.CodeInternal, "delegation: stored blockhash does not parse")
		}
	}
	if height < 0 {
		return Swap{}, errs.New(errs.CodeInternal, "delegation: stored blockhash height is negative")
	}
	s.BlockhashHeight = uint64(height)
	s.TxID = devnet.TxID(txID)
	s.NotionalUSD = money.USDFromMinor(notional)
	s.RequestedAt, s.QuoteExpiresAt = s.RequestedAt.UTC(), s.QuoteExpiresAt.UTC()
	if s.SignedAt != nil {
		t := s.SignedAt.UTC()
		s.SignedAt = &t
	}
	s.CreatedAt, s.UpdatedAt = s.CreatedAt.UTC(), s.UpdatedAt.UTC()
	return s, nil
}

func getSwap(ctx context.Context, q db.Querier, sid SwapID, forUpdate bool) (Swap, error) {
	sql := `SELECT ` + swapColumns + ` FROM delegated_swaps WHERE id = $1`
	if forUpdate {
		sql += ` FOR UPDATE`
	}
	s, err := scanSwap(q.QueryRow(ctx, sql, sid))
	if errors.Is(err, pgx.ErrNoRows) {
		return Swap{}, errs.New(errs.CodeNotFound, "delegation: swap not found").WithField("swap_id", sid.String())
	}
	if err != nil {
		return Swap{}, dbErr("load swap", err)
	}
	return s, nil
}

func swapByIntent(ctx context.Context, q db.Querier, iid econ.IntentID) (Swap, bool, error) {
	s, err := scanSwap(q.QueryRow(ctx, `SELECT `+swapColumns+` FROM delegated_swaps WHERE intent_id = $1`, iid))
	if errors.Is(err, pgx.ErrNoRows) {
		return Swap{}, false, nil
	}
	if err != nil {
		return Swap{}, false, dbErr("load swap by intent", err)
	}
	return s, true, nil
}

func listSwapIDs(ctx context.Context, q db.Querier, where string, args ...any) ([]SwapID, error) {
	rows, err := q.Query(ctx, `SELECT id FROM delegated_swaps WHERE `+where, args...)
	if err != nil {
		return nil, dbErr("list swaps", err)
	}
	defer rows.Close()
	var out []SwapID
	for rows.Next() {
		var sid SwapID
		if err := rows.Scan(&sid); err != nil {
			return nil, dbErr("scan swap id", err)
		}
		out = append(out, sid)
	}
	return out, rows.Err()
}

// subjectSet is what a status change also writes. Every field is set once;
// the database refuses a second value (DG002, DW002).
type subjectSet struct {
	message         []byte
	recentBlockhash *devnet.Hash
	blockhashHeight *uint64
	signedAt        *time.Time
	txID            devnet.TxID
	signedTx        []byte
	effectID        *econ.EffectID
	journal         *ledger.TransactionID
	failureCode     string
}

// subjectSetSQL is the SET list of a subject's status and set-once columns,
// spelled out in full so every statement that uses it is a compile-time
// constant (test/security/sqlsource_test.go): $2 is the status and $3..$11
// are the set-once columns, each kept by coalesce when its argument is NULL.
// Setting a column to the value it already holds is no change, so the
// set-once triggers (DG002, DW002) see only the columns actually written.
const subjectSetSQL = `status = coalesce($2, status), message = coalesce($3, message),
	recent_blockhash = coalesce($4, recent_blockhash), blockhash_height = coalesce($5, blockhash_height),
	signed_at = coalesce($6, signed_at), tx_id = coalesce($7, tx_id), signed_tx = coalesce($8, signed_tx),
	effect_id = coalesce($9, effect_id), journal_transaction_id = coalesce($10, journal_transaction_id),
	failure_code = coalesce($11, failure_code)`

// empty reports whether the set writes nothing.
func (u subjectSet) empty() bool {
	return u.message == nil && u.recentBlockhash == nil && u.blockhashHeight == nil && u.signedAt == nil && u.txID == "" &&
		u.signedTx == nil && u.effectID == nil && u.journal == nil && u.failureCode == ""
}

// setArgs are the arguments $3..$11 of subjectSetSQL: nil keeps the stored
// value. A blockhash height beyond int64 cannot be stored and is refused.
func (u subjectSet) setArgs() ([]any, error) {
	args := make([]any, 9)
	if u.message != nil {
		args[0] = u.message
	}
	if u.recentBlockhash != nil {
		args[1] = u.recentBlockhash.String()
	}
	if u.blockhashHeight != nil {
		if *u.blockhashHeight > uint64(1<<63-1) {
			return nil, errs.New(errs.CodeInternal, "delegation: a blockhash height beyond int64 cannot be stored")
		}
		args[2] = int64(*u.blockhashHeight)
	}
	if u.signedAt != nil {
		args[3] = dbTime(*u.signedAt)
	}
	if u.txID != "" {
		args[4] = string(u.txID)
	}
	if u.signedTx != nil {
		args[5] = u.signedTx
	}
	if u.effectID != nil {
		args[6] = *u.effectID
	}
	if u.journal != nil {
		args[7] = *u.journal
	}
	if u.failureCode != "" {
		args[8] = u.failureCode
	}
	return args, nil
}

// --- Withdrawals ----------------------------------------------------------------

// Withdrawal is a delegated_withdrawals row.
type Withdrawal struct {
	ID                   WithdrawalID
	CapabilityID         CapabilityID
	AccountID            accounts.AccountID
	AgentID              string
	ProposalNonce        uint64
	IntentID             econ.IntentID
	UsageID              UsageID
	ReservationID        capital.ReservationID
	PolicyID             PolicyID
	DestinationID        DestinationID
	Environment          config.Environment
	Mode                 gates.Mode
	Chain                devnet.ChainID
	Wallet               devnet.PublicKey
	DestinationAddress   devnet.PublicKey
	AssetID              assets.AssetID
	Amount               money.Quantity
	NotionalUSD          money.USD
	ProposedAt           time.Time
	ApprovalExpiresAt    time.Time
	ExecutableAt         time.Time
	EvaluationOutcome    EvaluationOutcome
	RiskScore            int
	Evaluation           Evaluation
	Plan                 WithdrawalPlan
	PlanHash             [32]byte
	Status               WithdrawalStatus
	Message              []byte
	RecentBlockhash      devnet.Hash
	BlockhashHeight      uint64
	SignedAt             *time.Time
	TxID                 devnet.TxID
	SignedTx             []byte
	EffectID             *econ.EffectID
	JournalTransactionID *ledger.TransactionID
	FailureCode          string
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

const withdrawalColumns = `id, capability_id, account_id, agent_id::text, proposal_nonce::text, intent_id, usage_id, reservation_id,
	policy_id, destination_id, environment, mode, chain_id, wallet, destination_address, asset_id, amount::text,
	notional_usd_minor, proposed_at, approval_expires_at, executable_at, evaluation_outcome, risk_score, evaluation, plan,
	plan_hash, status, message, coalesce(recent_blockhash, ''), coalesce(blockhash_height, 0), signed_at, coalesce(tx_id, ''),
	signed_tx, effect_id, journal_transaction_id, coalesce(failure_code, ''), created_at, updated_at`

func scanWithdrawal(row pgx.Row) (Withdrawal, error) {
	var w Withdrawal
	var nonce, amount, wallet, dest, bh, txID string
	var notional, height int64
	var evaluation, plan, hash []byte
	if err := row.Scan(&w.ID, &w.CapabilityID, &w.AccountID, &w.AgentID, &nonce, &w.IntentID, &w.UsageID, &w.ReservationID,
		&w.PolicyID, &w.DestinationID, &w.Environment, &w.Mode, &w.Chain, &wallet, &dest, &w.AssetID, &amount, &notional,
		&w.ProposedAt, &w.ApprovalExpiresAt, &w.ExecutableAt, &w.EvaluationOutcome, &w.RiskScore, &evaluation, &plan, &hash,
		&w.Status, &w.Message, &bh, &height, &w.SignedAt, &txID, &w.SignedTx, &w.EffectID, &w.JournalTransactionID,
		&w.FailureCode, &w.CreatedAt, &w.UpdatedAt); err != nil {
		return Withdrawal{}, err
	}
	var err error
	if w.ProposalNonce, err = strconv.ParseUint(nonce, 10, 64); err != nil {
		return Withdrawal{}, errs.New(errs.CodeInternal, "delegation: stored proposal nonce is not a u64")
	}
	if w.Amount, err = money.ScanQuantity(amount); err != nil {
		return Withdrawal{}, err
	}
	if w.Wallet, err = devnet.ParsePublicKey(wallet); err != nil {
		return Withdrawal{}, errs.New(errs.CodeInternal, "delegation: stored wallet is not a devnet address")
	}
	if w.DestinationAddress, err = devnet.ParsePublicKey(dest); err != nil {
		return Withdrawal{}, errs.New(errs.CodeInternal, "delegation: stored destination is not a devnet address")
	}
	if err := json.Unmarshal(evaluation, &w.Evaluation); err != nil {
		return Withdrawal{}, errs.Wrap(err, errs.CodeInternal, "delegation: stored evaluation does not decode")
	}
	if err := json.Unmarshal(plan, &w.Plan); err != nil {
		return Withdrawal{}, errs.Wrap(err, errs.CodeInternal, "delegation: stored withdrawal plan does not decode")
	}
	if len(hash) != 32 {
		return Withdrawal{}, errs.New(errs.CodeInternal, "delegation: stored plan hash has the wrong length")
	}
	copy(w.PlanHash[:], hash)
	if bh != "" {
		if w.RecentBlockhash, err = devnet.ParseHash(bh); err != nil {
			return Withdrawal{}, errs.New(errs.CodeInternal, "delegation: stored blockhash does not parse")
		}
	}
	if height < 0 {
		return Withdrawal{}, errs.New(errs.CodeInternal, "delegation: stored blockhash height is negative")
	}
	w.BlockhashHeight = uint64(height)
	w.TxID = devnet.TxID(txID)
	w.NotionalUSD = money.USDFromMinor(notional)
	w.ProposedAt, w.ApprovalExpiresAt, w.ExecutableAt = w.ProposedAt.UTC(), w.ApprovalExpiresAt.UTC(), w.ExecutableAt.UTC()
	if w.SignedAt != nil {
		t := w.SignedAt.UTC()
		w.SignedAt = &t
	}
	w.CreatedAt, w.UpdatedAt = w.CreatedAt.UTC(), w.UpdatedAt.UTC()
	return w, nil
}

func getWithdrawal(ctx context.Context, q db.Querier, wid WithdrawalID, forUpdate bool) (Withdrawal, error) {
	sql := `SELECT ` + withdrawalColumns + ` FROM delegated_withdrawals WHERE id = $1`
	if forUpdate {
		sql += ` FOR UPDATE`
	}
	w, err := scanWithdrawal(q.QueryRow(ctx, sql, wid))
	if errors.Is(err, pgx.ErrNoRows) {
		return Withdrawal{}, errs.New(errs.CodeNotFound, "delegation: withdrawal not found").WithField("withdrawal_id", wid.String())
	}
	if err != nil {
		return Withdrawal{}, dbErr("load withdrawal", err)
	}
	return w, nil
}

func withdrawalByIntent(ctx context.Context, q db.Querier, iid econ.IntentID) (Withdrawal, bool, error) {
	w, err := scanWithdrawal(q.QueryRow(ctx, `SELECT `+withdrawalColumns+` FROM delegated_withdrawals WHERE intent_id = $1`, iid))
	if errors.Is(err, pgx.ErrNoRows) {
		return Withdrawal{}, false, nil
	}
	if err != nil {
		return Withdrawal{}, false, dbErr("load withdrawal by intent", err)
	}
	return w, true, nil
}

func listWithdrawalIDs(ctx context.Context, q db.Querier, where string, args ...any) ([]WithdrawalID, error) {
	rows, err := q.Query(ctx, `SELECT id FROM delegated_withdrawals WHERE `+where, args...)
	if err != nil {
		return nil, dbErr("list withdrawals", err)
	}
	defer rows.Close()
	var out []WithdrawalID
	for rows.Next() {
		var wid WithdrawalID
		if err := rows.Scan(&wid); err != nil {
			return nil, dbErr("scan withdrawal id", err)
		}
		out = append(out, wid)
	}
	return out, rows.Err()
}

// --- Destinations -----------------------------------------------------------------

// Destination is an allowlisted withdrawal destination.
type Destination struct {
	ID        DestinationID
	AccountID accounts.AccountID
	Chain     devnet.ChainID
	Address   devnet.PublicKey
	Label     string
	Status    DestinationStatus
	AddedBy   Actor
	AddedAt   time.Time
	RemovedBy *Actor
	RemovedAt *time.Time
}

const destinationColumns = `id, account_id, chain_id, address, label, status, added_by_actor_type, added_by_actor_id, added_at,
	coalesce(removed_by_actor_type, ''), coalesce(removed_by_actor_id, ''), removed_at`

func scanDestination(row pgx.Row) (Destination, error) {
	var d Destination
	var addr, remType, remID string
	if err := row.Scan(&d.ID, &d.AccountID, &d.Chain, &addr, &d.Label, &d.Status, &d.AddedBy.Type, &d.AddedBy.ID, &d.AddedAt,
		&remType, &remID, &d.RemovedAt); err != nil {
		return Destination{}, err
	}
	k, err := devnet.ParsePublicKey(addr)
	if err != nil {
		return Destination{}, errs.New(errs.CodeInternal, "delegation: a stored destination is not a devnet address")
	}
	d.Address, d.AddedAt = k, d.AddedAt.UTC()
	if remType != "" {
		d.RemovedBy = &Actor{Type: security.ActorType(remType), ID: remID}
	}
	return d, nil
}

func getDestination(ctx context.Context, q db.Querier, did DestinationID, forUpdate bool) (Destination, error) {
	sql := `SELECT ` + destinationColumns + ` FROM delegated_withdrawal_destinations WHERE id = $1`
	if forUpdate {
		sql += ` FOR UPDATE`
	}
	d, err := scanDestination(q.QueryRow(ctx, sql, did))
	if errors.Is(err, pgx.ErrNoRows) {
		return Destination{}, errs.New(errs.CodeNotFound, "delegation: destination not found").WithField("destination_id", did.String())
	}
	if err != nil {
		return Destination{}, dbErr("load destination", err)
	}
	return d, nil
}

// --- Policies ---------------------------------------------------------------------

const policyColumns = `id, account_id, version, auto_approve_enabled, auto_approve_max_minor, auto_approve_max_risk_score,
	max_risk_score, per_withdrawal_limit_minor, daily_limit_minor, velocity_max_count, min_destination_age_seconds,
	min_account_age_seconds, cooling_period_seconds, strong_auth_required, strong_auth_max_age_seconds, policy_hash,
	created_by_actor_type, created_by_actor_id, effective_at, created_at`

func scanPolicy(row pgx.Row) (WithdrawalPolicy, error) {
	var p WithdrawalPolicy
	var autoMax, perW, daily, minDest, minAcct, cooling, authAge int64
	var hash []byte
	if err := row.Scan(&p.ID, &p.AccountID, &p.Version, &p.AutoApproveEnabled, &autoMax, &p.AutoApproveMaxRiskScore,
		&p.MaxRiskScore, &perW, &daily, &p.VelocityMaxCount, &minDest, &minAcct, &cooling, &p.StrongAuthRequired, &authAge,
		&hash, &p.CreatedBy.Type, &p.CreatedBy.ID, &p.EffectiveAt, &p.CreatedAt); err != nil {
		return WithdrawalPolicy{}, err
	}
	p.AutoApproveMax, p.PerWithdrawalLimit, p.DailyLimit = money.USDFromMinor(autoMax), money.USDFromMinor(perW), money.USDFromMinor(daily)
	p.MinDestinationAge, p.MinAccountAge = time.Duration(minDest)*time.Second, time.Duration(minAcct)*time.Second
	p.CoolingPeriod, p.StrongAuthMaxAge = time.Duration(cooling)*time.Second, time.Duration(authAge)*time.Second
	if len(hash) != 32 {
		return WithdrawalPolicy{}, errs.New(errs.CodeInternal, "delegation: stored policy hash has the wrong length")
	}
	copy(p.PolicyHash[:], hash)
	p.EffectiveAt, p.CreatedAt = p.EffectiveAt.UTC(), p.CreatedAt.UTC()
	return p, nil
}

func latestPolicy(ctx context.Context, q db.Querier, account accounts.AccountID) (WithdrawalPolicy, bool, error) {
	p, err := scanPolicy(q.QueryRow(ctx, `SELECT `+policyColumns+` FROM delegated_withdrawal_policies
		WHERE account_id = $1 ORDER BY version DESC LIMIT 1`, account))
	if errors.Is(err, pgx.ErrNoRows) {
		return WithdrawalPolicy{}, false, nil
	}
	if err != nil {
		return WithdrawalPolicy{}, false, dbErr("load withdrawal policy", err)
	}
	return p, true, nil
}

func getPolicy(ctx context.Context, q db.Querier, pid PolicyID) (WithdrawalPolicy, error) {
	p, err := scanPolicy(q.QueryRow(ctx, `SELECT `+policyColumns+` FROM delegated_withdrawal_policies WHERE id = $1`, pid))
	if errors.Is(err, pgx.ErrNoRows) {
		return WithdrawalPolicy{}, errs.New(errs.CodeNotFound, "delegation: withdrawal policy not found").WithField("policy_id", pid.String())
	}
	if err != nil {
		return WithdrawalPolicy{}, dbErr("load withdrawal policy", err)
	}
	return p, nil
}

// --- Approvals ----------------------------------------------------------------------

type approvalRow struct {
	Decision        Decision
	PlanHash        [32]byte
	ApproverType    security.ActorType
	ApproverID      string
	AuthenticatedAt *time.Time
	DecidedAt       time.Time
}

func approvalOf(ctx context.Context, q db.Querier, table, column string, subject string) (approvalRow, bool, error) {
	var a approvalRow
	var hash []byte
	err := q.QueryRow(ctx, `SELECT decision, plan_hash, approver_actor_type, approver_actor_id, authenticated_at, decided_at
		FROM `+table+` WHERE `+column+` = $1::uuid`, subject).
		Scan(&a.Decision, &hash, &a.ApproverType, &a.ApproverID, &a.AuthenticatedAt, &a.DecidedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return approvalRow{}, false, nil
	}
	if err != nil {
		return approvalRow{}, false, dbErr("load approval", err)
	}
	if len(hash) == 32 {
		copy(a.PlanHash[:], hash)
	}
	return a, true, nil
}

func swapApproval(ctx context.Context, q db.Querier, sid SwapID) (approvalRow, bool, error) {
	return approvalOf(ctx, q, "delegated_swap_approvals", "swap_id", sid.String())
}

func withdrawalApproval(ctx context.Context, q db.Querier, wid WithdrawalID) (approvalRow, bool, error) {
	return approvalOf(ctx, q, "delegated_withdrawal_approvals", "withdrawal_id", wid.String())
}

// --- Signing decisions -----------------------------------------------------------

func decisionExists(ctx context.Context, q db.Querier, kind SubjectKind, subject string) (bool, error) {
	_, found, err := decisionOf(ctx, q, kind, subject)
	return found, err
}

// decisionOf returns the one signing decision recorded for a subject, if any.
func decisionOf(ctx context.Context, q db.Querier, kind SubjectKind, subject string) (Verdict, bool, error) {
	column := "swap_id"
	if kind == SubjectWithdrawal {
		column = "withdrawal_id"
	}
	var v Verdict
	err := q.QueryRow(ctx, `SELECT verdict FROM delegated_signing_decisions WHERE `+column+` = $1::uuid`, subject).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, dbErr("look up signing decision", err)
	}
	return v, true, nil
}

// --- Errors -----------------------------------------------------------------------

func dbErr(op string, err error) error {
	if db.IsRetryable(err) {
		return err
	}
	if e, ok := errs.As(err); ok {
		return e
	}
	if state := db.SQLState(err); strings.HasPrefix(state, "DG") || strings.HasPrefix(state, "DW") {
		return errs.Wrap(err, errs.CodeConflict, "delegation: the database refused: "+op).WithField("sqlstate", state)
	}
	if mapped := ledger.MapError(err); mapped != nil {
		if e, ok := errs.As(mapped); ok && e.Code != errs.CodeInternal {
			return e
		}
	}
	return errs.Wrap(err, errs.CodeInternal, "delegation: "+op)
}

func newUsageID() UsageID { return id.New[usageKind]() }
