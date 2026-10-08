package margin

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// OpenerActorTypes are the actor types that may open a margin account; it is
// the Go twin of margin_accounts_opened_by_actor_type_check. An agent never
// opens one.
func OpenerActorTypes() []security.ActorType {
	return []security.ActorType{security.ActorUser, security.ActorOperator, security.ActorService, security.ActorSystem}
}

// OpenRequest opens a margin account for a customer account.
type OpenRequest struct {
	AccountID        accounts.AccountID
	MarginMode       MarginMode
	Mode             gates.Mode
	NumeraireAssetID assets.AssetID
	// IsolatedAssetID names the one risky asset of an ISOLATED account.
	IsolatedAssetID *assets.AssetID
	Actor           econ.Actor
}

func simulatedAsset(a assets.Asset) bool {
	return a.Kind == assets.KindSimulated && a.ValueDomain == valuedomain.Simulated
}

// OpenAccount opens (or returns, when the same account was already opened
// with the same terms) a margin account. It authorizes MARGIN, and LEVERAGE
// too for a mode that can borrow, registers the policy, wires the account
// to the lending pool and the insurance fund, and births it NORMAL.
func (s *Service) OpenAccount(ctx context.Context, tx pgx.Tx, r OpenRequest) (Account, error) {
	if !r.MarginMode.Valid() {
		return Account{}, errs.Newf(errs.CodeValidationFailed, "margin: unknown margin mode %q", r.MarginMode)
	}
	if r.Mode != gates.ModePaper && r.Mode != gates.ModeSandbox {
		return Account{}, errs.Newf(errs.CodeValidationFailed, "margin: a margin account runs in PAPER or SANDBOX, not %q", r.Mode)
	}
	if (r.MarginMode == ModeIsolated) != (r.IsolatedAssetID != nil) {
		return Account{}, errs.New(errs.CodeValidationFailed, "margin: an ISOLATED account names its one risky asset, and only it does")
	}
	opener := false
	for _, t := range OpenerActorTypes() {
		if t == r.Actor.Type {
			opener = true
		}
	}
	if !opener || r.Actor.ID == "" {
		return Account{}, errs.New(errs.CodeForbidden, "margin: a person or a service opens a margin account; an agent never does")
	}
	caps := []gates.Capability{gates.Margin}
	if r.MarginMode != ModeCash {
		caps = append(caps, gates.Leverage)
	}
	if err := s.authorize(ctx, tx, r.Mode, false, caps...); err != nil {
		return Account{}, err
	}
	cust, err := s.accounts.Get(ctx, tx, r.AccountID)
	if err != nil {
		return Account{}, err
	}
	if !cust.Status.AllowsNewRisk() {
		return Account{}, errs.Newf(errs.CodeAccountFrozen, "margin: account is %s", cust.Status).WithField("account_id", r.AccountID.String())
	}
	num, err := s.assets.Get(ctx, tx, r.NumeraireAssetID)
	if err != nil {
		return Account{}, err
	}
	// The engine renders the numeraire in cents at parity, so only a
	// USD-pegged simulated dollar can be one (on main: econ's sandbox dollar,
	// which the caller passes in; this package never creates one).
	if !simulatedAsset(num) || !num.IsStablecoin || num.PegCurrency != "USD" || num.Decimals < 2 || num.Decimals > 18 {
		return Account{}, errs.New(errs.CodeValidationFailed, "margin: the numeraire must be a USD-pegged SIMULATED asset with 2..18 decimals").
			WithField("asset_id", num.ID.String())
	}
	if r.IsolatedAssetID != nil {
		info, err := s.assetInfo(ctx, tx, *r.IsolatedAssetID)
		if err != nil {
			return Account{}, err
		}
		if !simulatedAsset(info.asset) || !info.eligible || *r.IsolatedAssetID == r.NumeraireAssetID {
			return Account{}, errs.New(errs.CodeValidationFailed, "margin: the isolated asset must be an eligible SIMULATED asset other than the numeraire")
		}
	}
	// A platform stable asset is a USD-pegged SIMULATED asset, and so is a
	// twin of one, so either passes the checks above. As the numeraire, the
	// isolated asset or a policy rule's asset it would put the account's
	// loans, interest and liquidations into journal kinds the database
	// refuses for it (supply.go): refused here, before anything is written.
	if err := s.refuseSupplyControlledTerms(ctx, tx, r); err != nil {
		return Account{}, err
	}
	if err := s.st.ensurePolicy(ctx, tx, s.policy); err != nil {
		return Account{}, err
	}
	pool, fund, err := s.facilities(ctx, tx)
	if err != nil {
		return Account{}, err
	}
	if err := pool.RequireActive(); err != nil {
		return Account{}, err
	}
	if err := fund.RequireActive(); err != nil {
		return Account{}, err
	}
	want := Account{
		ID: newAccountID(), AccountID: r.AccountID, MarginMode: r.MarginMode, Mode: r.Mode, Status: StatusNormal,
		PolicyVersion: s.policy.Version, NumeraireAssetID: r.NumeraireAssetID, IsolatedAssetID: r.IsolatedAssetID,
		LendingPoolID: pool.ID, InsuranceFundID: fund.ID, OpenedBy: r.Actor, OpenedAt: s.now(),
	}
	got, created, err := s.st.insertAccount(ctx, tx, want)
	if err != nil {
		return Account{}, err
	}
	if !created {
		same := got.MarginMode == want.MarginMode && got.Mode == want.Mode && got.NumeraireAssetID == want.NumeraireAssetID &&
			((got.IsolatedAssetID == nil && want.IsolatedAssetID == nil) ||
				(got.IsolatedAssetID != nil && want.IsolatedAssetID != nil && *got.IsolatedAssetID == *want.IsolatedAssetID))
		if !same {
			return Account{}, errs.New(errs.CodeConflict, "margin: this account already has a margin account on other terms").
				WithField("margin_account_id", got.ID.String())
		}
		return got, nil
	}
	if err := s.st.auditTransition(ctx, tx, got.AccountID, r.Actor, "account", got.ID.String(), "", string(StatusNormal),
		"margin account opened", "", got.OpenedAt); err != nil {
		return Account{}, err
	}
	return got, nil
}

// SeedRequest funds the lending pool and the insurance fund.
type SeedRequest struct {
	Asset      assets.AssetID
	PoolAmount money.Quantity
	FundAmount money.Quantity
	// Key makes the funding exactly once: the same key never funds twice.
	Key   string
	Actor econ.Actor
}

// Facilities are the two facilities margin accounts are wired to.
type Facilities struct {
	Pool econ.Facility
	Fund econ.Facility
}

// SeedFacilities ensures the lending pool and the insurance fund exist and
// funds them with FACILITY_FUNDING postings (POOL_CASH / POOL_CAPITAL and
// INSURANCE_FUND_CASH / INSURANCE_FUND_CAPITAL), exactly once per key. It is
// for demonstrations and tests: in this build the facilities hold simulated
// value only, which the database enforces.
func (s *Service) SeedFacilities(ctx context.Context, tx pgx.Tx, r SeedRequest) (Facilities, error) {
	if err := validKey(r.Key); err != nil {
		return Facilities{}, err
	}
	if r.Actor.Type == security.ActorAgent || r.Actor.ID == "" {
		return Facilities{}, errs.New(errs.CodeForbidden, "margin: facilities are funded by a person or a service")
	}
	a, err := s.assets.Get(ctx, tx, r.Asset)
	if err != nil {
		return Facilities{}, err
	}
	if !simulatedAsset(a) {
		return Facilities{}, errs.New(errs.CodeValidationFailed, "margin: facilities hold SIMULATED value only")
	}
	// FACILITY_FUNDING is refused for a supply-controlled asset (SU001);
	// refused here before the facilities are even ensured.
	if err := refuseSupplyControlledFunding(ctx, tx, r.Asset, r.PoolAmount, r.FundAmount); err != nil {
		return Facilities{}, err
	}
	pool, fund, err := s.facilities(ctx, tx)
	if err != nil {
		return Facilities{}, err
	}
	if err := lockFacilityRows(ctx, tx, pool.ID, fund.ID, poolBooks|fundBooks); err != nil {
		return Facilities{}, err
	}
	for _, f := range []struct {
		fac econ.Facility
		amt money.Quantity
	}{{pool, r.PoolAmount}, {fund, r.FundAmount}} {
		if !f.amt.IsPositive() {
			continue
		}
		key := "margin:facility-funding:" + f.fac.ID.String() + ":" + keyHash(r.Key)
		m := s.meta(refFacilityFunding, f.fac.ID.String(), "simulated facility funding")
		p, err := FacilityFundingPosting(m, f.fac, r.Asset, f.amt)
		if err != nil {
			return Facilities{}, err
		}
		req := map[string]string{"facility_id": f.fac.ID.String(), "asset_id": r.Asset.String(), "amount": f.amt.String(), "key": r.Key}
		if _, err := s.postOnce(ctx, tx, postSpec{key: key, kind: "FACILITY_FUNDING", request: req, posting: p, evidence: req}); err != nil {
			return Facilities{}, err
		}
	}
	return Facilities{Pool: pool, Fund: fund}, nil
}

// CloseAccount closes a margin account that holds nothing and owes the pool
// nothing: from NORMAL, or from BANKRUPT once the insurance fund has covered
// the deficit (the customer's DEFICIT stays on the books). Closing an
// account that is already CLOSED returns it unchanged.
func (s *Service) CloseAccount(ctx context.Context, tx pgx.Tx, acct accounts.AccountID, by econ.Actor) (Account, error) {
	if by.Type == security.ActorAgent || !by.Type.Valid() || by.ID == "" {
		return Account{}, errs.New(errs.CodeForbidden, "margin: a person or a service closes a margin account")
	}
	a, err := s.lockAccount(ctx, tx, acct)
	if err != nil {
		return Account{}, err
	}
	if a.Status == StatusClosed {
		return a, nil
	}
	if err := s.authorize(ctx, tx, a.Mode, true, gates.Margin); err != nil {
		return Account{}, err
	}
	c, found, err := s.st.latestCase(ctx, tx, a.ID, false)
	if err != nil {
		return Account{}, err
	}
	switch a.Status {
	case StatusNormal:
		if found && !c.Status.Terminal() {
			return Account{}, errs.New(errs.CodeInvalidStateTransition, "margin: a liquidation case is open")
		}
	case StatusBankrupt:
		if !found || c.CoverageJournalID == nil {
			return Account{}, errs.New(errs.CodeInvalidStateTransition, "margin: the deficit has not been covered")
		}
	default:
		return Account{}, errs.Newf(errs.CodeInvalidStateTransition, "margin: a %s account cannot close", a.Status)
	}
	balances, err := s.d.Ledger.BalancesForOwner(ctx, tx, ledger.OwnerCustomer, a.AccountID.String())
	if err != nil {
		return Account{}, err
	}
	for _, b := range balances {
		if (b.Account.Code == ledger.CodeMarginCollateral || b.Account.Code == ledger.CodeMarginLoan) && b.Balance.Sign() != 0 {
			return Account{}, errs.Newf(errs.CodeInvalidStateTransition, "margin: the account still holds %s %s", b.Account.Code, b.Balance).
				WithField("asset_id", b.Account.AssetID.String())
		}
	}
	lots, err := s.st.openLots(ctx, tx, a.ID, false)
	if err != nil {
		return Account{}, err
	}
	if len(lots) > 0 {
		return Account{}, errs.New(errs.CodeInvalidStateTransition, "margin: a borrow lot is still open")
	}
	if err := s.st.setAccountStatus(ctx, tx, &a, StatusClosed, s.actor, "closed by "+string(by.Type), "", s.now()); err != nil {
		return Account{}, err
	}
	return a, nil
}
