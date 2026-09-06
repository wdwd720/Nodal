package signing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/signing/inspect"
	"github.com/nodal/controlplane/internal/wallet"
)

// Request is a bounded signing request: identifiers plus the exact bytes to
// sign. Nothing in it carries authority.
type Request struct {
	AttemptID      string
	PlanID         string
	IntentID       string
	RiskDecisionID string
	WalletID       string
	UnsignedTx     []byte
	// ExpectedTxHash is sha256(UnsignedTx) as computed by the caller.
	ExpectedTxHash []byte
	// LookupTables are caller-observed ALT contents (address → keys). Used
	// only when no LookupTableSource is configured (LOCAL/TEST).
	LookupTables map[string][]string
	// ClaimedPlanHash is the caller's belief of the approved plan hash
	// (optional; compared, never trusted).
	ClaimedPlanHash []byte
	SimulationRef   string
	CorrelationID   string
}

// Validate checks the request shape.
func (r Request) Validate() error {
	for name, v := range map[string]string{"attempt_id": r.AttemptID, "plan_id": r.PlanID, "intent_id": r.IntentID, "risk_decision_id": r.RiskDecisionID, "wallet_id": r.WalletID} {
		if _, err := id.ParseAny(v); err != nil {
			return errs.Newf(errs.CodeValidationFailed, "signing: %s must be a canonical uuid", name).WithField("field", name)
		}
	}
	if len(r.UnsignedTx) == 0 {
		return errs.New(errs.CodeValidationFailed, "signing: unsigned transaction required")
	}
	if len(r.UnsignedTx) > inspect.MaxTransactionSize {
		return errs.Newf(errs.CodeValidationFailed, "signing: transaction exceeds %d bytes", inspect.MaxTransactionSize)
	}
	if len(r.ExpectedTxHash) != sha256.Size {
		return errs.New(errs.CodeValidationFailed, "signing: expected transaction hash must be 32 bytes")
	}
	if len(r.ClaimedPlanHash) != 0 && len(r.ClaimedPlanHash) != sha256.Size {
		return errs.New(errs.CodeValidationFailed, "signing: claimed plan hash must be 32 bytes")
	}
	return nil
}

// Decision is a recorded signing decision.
type Decision struct {
	ID               string
	AttemptID        string
	PlanID           string
	IntentID         string
	RiskDecisionID   string
	WalletID         string
	Approved         bool
	ReasonCodes      []string
	Checks           []inspect.Check
	InspectorVersion string
	ExpectedTxHash   []byte
	InspectedTxHash  []byte
	ProviderSignRef  string
	DecidedAt        time.Time
	// Replayed is true when the decision had already been recorded for the
	// attempt before this call.
	Replayed bool
}

// Service is the bounded signing boundary (EXECUTION.md §3).
type Service interface {
	// Sign inspects and, if every check passes, signs. A rejection is a
	// Decision with Approved=false and a nil signed transaction, not an
	// error. Errors are infrastructure or provider failures.
	Sign(ctx context.Context, req Request) (Decision, []byte, error)
	// GetDecision returns a previously recorded decision.
	GetDecision(ctx context.Context, decisionID string) (Decision, error)
}

// BlockHeightSource reports the current chain block height.
type BlockHeightSource interface {
	CurrentBlockHeight(ctx context.Context) (uint64, error)
}

// SimulationSource loads the persisted simulation result for an attempt
// (nil, nil when none was recorded).
type SimulationSource interface {
	LoadSimulation(ctx context.Context, attemptID, simulationRef string) (*inspect.SimulationResult, error)
}

// LookupTableSource fetches address-lookup-table contents from the chain.
type LookupTableSource interface {
	LookupTables(ctx context.Context, addresses []string) (map[string][]string, error)
}

// Policy is the configured inspection policy.
type Policy struct {
	// AllowedPrograms is the base program allowlist.
	AllowedPrograms []string
	// RoutePrograms are the venue programs decoded as Jupiter v6 routes.
	RoutePrograms []string
	// Token2022EnabledAssets lists asset ids (uuid) explicitly enabled for
	// Token-2022 (still rejected when the asset carries extensions).
	Token2022EnabledAssets map[string]bool
	MaxPriorityFeeLamports uint64
	MaxComputeUnits        uint32
	BlockHeightMargin      uint64
	// RequireSimulation must be true outside LOCAL/TEST.
	RequireSimulation bool
}

// DefaultPolicy is the V1 policy: core programs + Jupiter v6, 0.01 SOL max
// priority fee, the runtime compute cap, a 30-block expiry margin, and
// mandatory simulation.
func DefaultPolicy() Policy {
	return Policy{
		AllowedPrograms:        inspect.DefaultAllowedPrograms(),
		RoutePrograms:          []string{inspect.JupiterV6Program},
		MaxPriorityFeeLamports: 10_000_000,
		MaxComputeUnits:        inspect.MaxComputeUnitLimit,
		BlockHeightMargin:      30,
		RequireSimulation:      true,
	}
}

// Validate checks the policy.
func (p Policy) Validate(env config.Environment) error {
	if len(p.AllowedPrograms) == 0 {
		return errs.New(errs.CodeValidationFailed, "signing: policy allowlist is empty")
	}
	if len(p.RoutePrograms) == 0 {
		return errs.New(errs.CodeValidationFailed, "signing: policy has no route program")
	}
	if p.MaxComputeUnits == 0 || p.MaxComputeUnits > inspect.MaxComputeUnitLimit {
		return errs.New(errs.CodeValidationFailed, "signing: policy max compute units out of range")
	}
	if p.BlockHeightMargin == 0 {
		return errs.New(errs.CodeValidationFailed, "signing: policy block height margin must be positive")
	}
	if !p.RequireSimulation && !env.AllowsDefaults() {
		return errs.New(errs.CodeValidationFailed, "signing: simulation is mandatory outside LOCAL/TEST")
	}
	return nil
}

// Deps are the boundary's collaborators.
type Deps struct {
	DB           *db.DB
	Audit        audit.Writer
	Clock        clock.Clock
	Logger       *slog.Logger
	Wallets      wallet.WalletProvider
	Signer       wallet.SigningProvider
	BlockHeights BlockHeightSource
	Simulations  SimulationSource
	// LookupTables may be nil only in LOCAL/TEST (caller tables are used).
	LookupTables LookupTableSource
	Env          config.Environment
	// ServiceName is recorded as requested_by_service and as the audit actor.
	ServiceName string
	Policy      Policy
}

// PGService is the PostgreSQL-backed Service.
type PGService struct {
	deps       Deps
	repo       *repository
	capability wallet.Capability
}

var _ Service = (*PGService)(nil)

// New validates deps, probes the wallet provider's delegated-signing
// capability and fails closed outside LOCAL/TEST unless it is VERIFIED.
func New(ctx context.Context, deps Deps) (*PGService, error) {
	switch {
	case deps.DB == nil:
		return nil, errs.New(errs.CodeValidationFailed, "signing: database required")
	case deps.Audit == nil:
		return nil, errs.New(errs.CodeValidationFailed, "signing: audit writer required")
	case deps.Wallets == nil:
		return nil, errs.New(errs.CodeValidationFailed, "signing: wallet provider required")
	case deps.Signer == nil:
		return nil, errs.New(errs.CodeValidationFailed, "signing: signing provider required")
	case deps.BlockHeights == nil:
		return nil, errs.New(errs.CodeValidationFailed, "signing: block height source required")
	case deps.Simulations == nil:
		return nil, errs.New(errs.CodeValidationFailed, "signing: simulation source required")
	case !deps.Env.IsValid():
		return nil, errs.New(errs.CodeValidationFailed, "signing: environment required")
	case deps.ServiceName == "":
		return nil, errs.New(errs.CodeValidationFailed, "signing: service name required")
	}
	if deps.Clock == nil {
		deps.Clock = clock.System()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if err := deps.Policy.Validate(deps.Env); err != nil {
		return nil, err
	}
	strict := !deps.Env.AllowsDefaults() // DEV, STAGING, PROD
	if strict && deps.LookupTables == nil {
		return nil, errs.New(errs.CodeValidationFailed, "signing: a lookup table source is required outside LOCAL/TEST")
	}
	capability, err := deps.Wallets.Capabilities(ctx)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeDelegationNotVerified, "signing: wallet provider capability probe failed")
	}
	if strict && !capability.DelegationVerified() {
		return nil, errs.Newf(errs.CodeDelegationNotVerified, "signing: provider %s delegated signing is %s (%s); refusing to start in %s",
			capability.Provider, capability.DelegatedSigning, capability.Detail, deps.Env)
	}
	if !capability.DelegationVerified() {
		deps.Logger.Warn("signing: wallet provider delegated signing is not verified; permitted only in LOCAL/TEST",
			slog.String("provider", capability.Provider), slog.String("state", string(capability.DelegatedSigning)))
	}
	return &PGService{deps: deps, repo: newRepository(), capability: capability}, nil
}

// Capability returns the provider capability observed at construction.
func (s *PGService) Capability() wallet.Capability { return s.capability }

// Sign implements Service.
func (s *PGService) Sign(ctx context.Context, req Request) (Decision, []byte, error) {
	if err := req.Validate(); err != nil {
		return Decision{}, nil, err
	}
	log := s.deps.Logger.With(
		slog.String("attempt_id", req.AttemptID), slog.String("plan_id", req.PlanID),
		slog.String("wallet_id", req.WalletID), slog.String("correlation_id", req.CorrelationID))

	txHash := sha256.Sum256(req.UnsignedTx)

	// Fast replay path without chain I/O.
	if existing, err := s.repo.findDecision(ctx, s.deps.DB, req.AttemptID); err != nil {
		return Decision{}, nil, err
	} else if existing != nil {
		return s.replay(ctx, req, existing, log)
	}

	// Chain facts are fetched before the transaction so no external I/O
	// happens under the attempt lock during inspection.
	attempt, err := s.repo.getAttempt(ctx, s.deps.DB, req.AttemptID)
	if err != nil {
		return Decision{}, nil, err
	}
	decoded, decodeErr := inspect.Decode(req.UnsignedTx)
	chain, err := s.fetchChain(ctx, req, attempt, decoded, decodeErr == nil)
	if err != nil {
		return Decision{}, nil, err
	}

	var (
		decision Decision
		l        *loaded
	)
	err = s.deps.DB.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		// Re-check under the lock: a concurrent Sign may have decided.
		if _, err := s.repo.lockAttempt(ctx, tx, req.AttemptID); err != nil {
			return err
		}
		existing, err := s.repo.findDecision(ctx, tx, req.AttemptID)
		if err != nil {
			return err
		}
		if existing != nil {
			decision = toDecision(*existing, "")
			decision.Replayed = true
			return nil
		}
		l, err = s.load(ctx, tx, req)
		if err != nil {
			return err
		}
		now := s.deps.Clock.Now()
		result, reasonCodes := s.evaluate(l, req, txHash[:], chain, decoded, decodeErr, now)
		row := decisionRow{
			ID:               NewDecisionID(),
			AttemptID:        req.AttemptID,
			PlanID:           req.PlanID,
			IntentID:         req.IntentID,
			RiskDecisionID:   req.RiskDecisionID,
			WalletID:         req.WalletID,
			ExpectedTxHash:   append([]byte(nil), req.ExpectedTxHash...),
			InspectedTxHash:  txHash[:],
			Decision:         "REJECTED",
			ReasonCodes:      reasonCodes,
			InspectorVersion: inspect.InspectorVersion,
			RequestedBy:      s.deps.ServiceName,
			DecidedAt:        now,
		}
		if result.Approved {
			row.Decision = "APPROVED"
		}
		checks, err := json.Marshal(result.Checks)
		if err != nil {
			return fmt.Errorf("signing: encode checks: %w", err)
		}
		row.Checks = checks
		if err := s.repo.insertDecision(ctx, tx, row); err != nil {
			return err
		}
		if err := s.auditDecision(ctx, tx, l, req, row); err != nil {
			return err
		}
		if !result.Approved {
			detail := map[string]any{
				"decision_id": row.ID.String(), "attempt_id": req.AttemptID, "plan_id": req.PlanID, "intent_id": req.IntentID,
				"wallet_id": req.WalletID, "reason_codes": reasonCodes, "requested_by_service": s.deps.ServiceName,
				"inspected_tx_hash": hex.EncodeToString(txHash[:]), "inspector_version": inspect.InspectorVersion,
			}
			if err := s.repo.insertSecurityEvent(ctx, tx, l.intent.AccountID, detail, req.CorrelationID, now); err != nil {
				return err
			}
		}
		decision = toDecision(row, "")
		return nil
	})
	if err != nil {
		return Decision{}, nil, err
	}
	if decision.Replayed {
		return s.replayLoaded(ctx, req, decision, log)
	}
	log.Info("signing decision recorded", slog.String("decision_id", decision.ID), slog.Bool("approved", decision.Approved),
		slog.Any("reason_codes", decision.ReasonCodes), slog.String("inspected_tx_hash", hex.EncodeToString(txHash[:])))
	if !decision.Approved {
		return decision, nil, nil
	}
	return s.signApproved(ctx, req, decision, l.wallet, l.intent.AccountID, log)
}

// replay handles a Sign for an attempt that already has a decision.
func (s *PGService) replay(ctx context.Context, req Request, existing *decisionRow, log *slog.Logger) (Decision, []byte, error) {
	d := toDecision(*existing, "")
	d.Replayed = true
	return s.replayLoaded(ctx, req, d, log)
}

func (s *PGService) replayLoaded(ctx context.Context, req Request, d Decision, log *slog.Logger) (Decision, []byte, error) {
	decisionID, err := ParseDecisionID(d.ID)
	if err != nil {
		return Decision{}, nil, fmt.Errorf("signing: decision id: %w", err)
	}
	if !d.Approved {
		log.Info("signing decision replayed", slog.String("decision_id", d.ID), slog.Bool("approved", false))
		return d, nil, nil
	}
	res, err := s.repo.findResult(ctx, s.deps.DB, decisionID)
	if err != nil {
		return Decision{}, nil, err
	}
	if res != nil {
		d.ProviderSignRef = res.ProviderSignRef
		log.Info("signing decision replayed", slog.String("decision_id", d.ID), slog.Bool("approved", true))
		return d, res.SignedTx, nil
	}
	// Approved but never signed (provider failure or crash): finish signing.
	l, err := s.loadWalletAndAccount(ctx, req)
	if err != nil {
		return Decision{}, nil, err
	}
	return s.signApproved(ctx, req, d, l.wallet, l.intent.AccountID, log)
}

// signApproved calls the provider under the attempt lock and records the
// result. It never signs when a result already exists.
func (s *PGService) signApproved(ctx context.Context, req Request, d Decision, w wallet.Wallet, accountID string, log *slog.Logger) (Decision, []byte, error) {
	decisionID, err := ParseDecisionID(d.ID)
	if err != nil {
		return Decision{}, nil, fmt.Errorf("signing: decision id: %w", err)
	}
	if !d.Approved {
		return Decision{}, nil, errs.New(errs.CodeInternal, "signing: refusing to sign a rejected decision")
	}
	var (
		signed      []byte
		providerErr error
	)
	err = s.deps.DB.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := s.repo.lockAttempt(ctx, tx, req.AttemptID); err != nil {
			return err
		}
		if res, err := s.repo.findResult(ctx, tx, decisionID); err != nil {
			return err
		} else if res != nil {
			signed = res.SignedTx
			d.ProviderSignRef = res.ProviderSignRef
			d.Replayed = true
			return nil
		}
		now := s.deps.Clock.Now()
		res, err := s.deps.Signer.SignTransaction(ctx, wallet.SignRequest{
			ProviderWalletID: w.ProviderWalletID,
			Chain:            w.Chain,
			UnsignedTx:       req.UnsignedTx,
			IdempotencyKey:   req.AttemptID,
			Purpose:          wallet.PurposeSwap,
		})
		if err != nil {
			providerErr = err
			// Record the failure as evidence; the decision stands and a
			// later Sign for the same attempt completes signing.
			return s.appendAudit(ctx, tx, accountID, "signing.provider_failed", d.ID, req, map[string]any{
				"decision_id": d.ID, "attempt_id": req.AttemptID, "wallet_id": req.WalletID,
				"provider": s.deps.Signer.Name(), "error_code": string(errs.CodeOf(err)),
			}, now)
		}
		if len(res.SignedTx) == 0 || len(res.Signature) != 64 {
			providerErr = errs.New(errs.CodeSigningRejected, "signing: provider returned no usable signature").WithField("provider", s.deps.Signer.Name())
			return s.appendAudit(ctx, tx, accountID, "signing.provider_failed", d.ID, req, map[string]any{
				"decision_id": d.ID, "attempt_id": req.AttemptID, "wallet_id": req.WalletID,
				"provider": s.deps.Signer.Name(), "error_code": string(errs.CodeSigningRejected),
			}, now)
		}
		signedHash := sha256.Sum256(res.SignedTx)
		signedAt := res.SignedAt
		if signedAt.IsZero() {
			signedAt = now
		}
		retry := string(res.RetryClass)
		if retry == "" {
			retry = "UNKNOWN_EFFECT_WRITE"
		}
		row := resultRow{
			DecisionID:      decisionID,
			AttemptID:       req.AttemptID,
			WalletID:        req.WalletID,
			Provider:        s.deps.Signer.Name(),
			ProviderSignRef: res.ProviderRef,
			IdempotencyKey:  req.AttemptID,
			SignedTx:        res.SignedTx,
			SignedTxHash:    signedHash[:],
			Signature:       res.Signature,
			RetryClass:      retry,
			SignedAt:        signedAt,
		}
		if err := s.repo.insertResult(ctx, tx, row); err != nil {
			return err
		}
		if err := s.appendAudit(ctx, tx, accountID, "signing.signed", d.ID, req, map[string]any{
			"decision_id": d.ID, "attempt_id": req.AttemptID, "wallet_id": req.WalletID, "provider": row.Provider,
			"provider_sign_ref": row.ProviderSignRef, "signed_tx_hash": hex.EncodeToString(signedHash[:]),
			"signature": hex.EncodeToString(res.Signature), "retry_class": retry,
		}, now); err != nil {
			return err
		}
		signed = res.SignedTx
		d.ProviderSignRef = row.ProviderSignRef
		return nil
	})
	if err != nil {
		return Decision{}, nil, err
	}
	if providerErr != nil {
		log.Error("signing provider failed", slog.String("decision_id", d.ID), slog.String("error_code", string(errs.CodeOf(providerErr))))
		return d, nil, providerErr
	}
	log.Info("transaction signed", slog.String("decision_id", d.ID), slog.String("provider_sign_ref", d.ProviderSignRef))
	return d, signed, nil
}

// GetDecision implements Service.
func (s *PGService) GetDecision(ctx context.Context, decisionID string) (Decision, error) {
	did, err := ParseDecisionID(decisionID)
	if err != nil {
		return Decision{}, errs.New(errs.CodeValidationFailed, "signing: decision id must be a canonical uuid")
	}
	row, err := s.repo.getDecision(ctx, s.deps.DB, did)
	if err != nil {
		return Decision{}, err
	}
	ref := ""
	if row.Decision == "APPROVED" {
		res, err := s.repo.findResult(ctx, s.deps.DB, did)
		if err != nil {
			return Decision{}, err
		}
		if res != nil {
			ref = res.ProviderSignRef
		}
	}
	return toDecision(row, ref), nil
}

// fetchChain gathers block height, lookup tables and the simulation result.
func (s *PGService) fetchChain(ctx context.Context, req Request, attempt attemptRow, decoded inspect.DecodedTransaction, decodedOK bool) (chainData, error) {
	height, err := s.deps.BlockHeights.CurrentBlockHeight(ctx)
	if err != nil {
		return chainData{}, errs.Wrap(err, errs.CodeProviderUnavailable, "signing: block height unavailable")
	}
	cd := chainData{currentHeight: height, lookupTables: req.LookupTables}
	if s.deps.LookupTables != nil {
		cd.lookupTables = nil
		if decodedOK && len(decoded.LookupTables) > 0 {
			addrs := make([]string, 0, len(decoded.LookupTables))
			for _, l := range decoded.LookupTables {
				addrs = append(addrs, l.Table.String())
			}
			tables, err := s.deps.LookupTables.LookupTables(ctx, addrs)
			if err != nil {
				return chainData{}, errs.Wrap(err, errs.CodeProviderUnavailable, "signing: lookup tables unavailable")
			}
			cd.lookupTables = tables
		}
	}
	if attempt.SimulationOK != nil && *attempt.SimulationOK {
		ref := attempt.SimulationRef
		if ref == "" {
			ref = req.SimulationRef
		}
		sim, err := s.deps.Simulations.LoadSimulation(ctx, req.AttemptID, ref)
		if err != nil {
			return chainData{}, errs.Wrap(err, errs.CodeProviderUnavailable, "signing: simulation result unavailable")
		}
		cd.simulation = sim
	} else if attempt.SimulationOK != nil {
		cd.simulation = &inspect.SimulationResult{Succeeded: false, Error: "attempt recorded simulation_ok = false"}
	}
	return cd, nil
}

// load re-reads every row the decision depends on.
func (s *PGService) load(ctx context.Context, q db.Querier, req Request) (*loaded, error) {
	l := &loaded{}
	var err error
	if l.attempt, err = s.repo.getAttempt(ctx, q, req.AttemptID); err != nil {
		return nil, err
	}
	if l.plan, err = s.repo.getPlan(ctx, q, req.PlanID); err != nil {
		return nil, err
	}
	if l.intent, err = s.repo.getIntent(ctx, q, req.IntentID); err != nil {
		return nil, err
	}
	if l.risk, err = s.repo.getRiskDecision(ctx, q, req.RiskDecisionID); err != nil {
		return nil, err
	}
	quoteID := l.plan.QuoteID
	if quoteID == "" {
		quoteID = l.attempt.QuoteID
	}
	if l.quote, err = s.repo.getQuote(ctx, q, quoteID); err != nil {
		return nil, err
	}
	if l.intent.ReservationID != "" {
		if l.reservation, err = s.repo.getReservation(ctx, q, l.intent.ReservationID); err != nil {
			return nil, err
		}
	}
	wid, err := wallet.ParseWalletID(req.WalletID)
	if err != nil {
		return nil, errs.New(errs.CodeValidationFailed, "signing: wallet id must be a canonical uuid")
	}
	if l.wallet, err = s.repo.wallets.Get(ctx, q, wid); err != nil {
		return nil, err
	}
	if l.inputAsset, err = s.repo.assets.Get(ctx, q, l.quote.InputAssetID); err != nil {
		return nil, err
	}
	if l.outputAsset, err = s.repo.assets.Get(ctx, q, l.quote.OutputAssetID); err != nil {
		return nil, err
	}
	return l, nil
}

// loadWalletAndAccount is the minimal load for finishing an approved
// decision's signing.
func (s *PGService) loadWalletAndAccount(ctx context.Context, req Request) (*loaded, error) {
	l := &loaded{}
	wid, err := wallet.ParseWalletID(req.WalletID)
	if err != nil {
		return nil, errs.New(errs.CodeValidationFailed, "signing: wallet id must be a canonical uuid")
	}
	if l.wallet, err = s.repo.wallets.Get(ctx, s.deps.DB, wid); err != nil {
		return nil, err
	}
	if l.intent, err = s.repo.getIntent(ctx, s.deps.DB, req.IntentID); err != nil {
		return nil, err
	}
	return l, nil
}

// evaluate produces the inspection result and the full reason code list
// (linkage problems + inspector codes), all from persisted data.
func (s *PGService) evaluate(l *loaded, req Request, txHash []byte, chain chainData, decoded inspect.DecodedTransaction, decodeErr error, now time.Time) (inspect.Result, []string) {
	problems := validateLinkage(l, req, txHash, now)
	if len(problems) > 0 {
		codes := make([]string, 0, len(problems))
		details := make([]string, 0, len(problems))
		for _, p := range problems {
			codes = append(codes, p.code)
			details = append(details, p.code+": "+p.detail)
		}
		codes = dedupe(codes)
		res := inspect.RejectAll(codes[0], "persisted rows are not a signable chain: "+joinMax(details, 8))
		res.ReasonCodes = codes
		return res, codes
	}
	if decodeErr != nil {
		res := inspect.RejectAll(inspect.ReasonDecodeError, decodeErr.Error())
		return res, res.ReasonCodes
	}
	exp, err := buildExpectations(l, chain, s.deps.Policy, req)
	if err != nil {
		res := inspect.RejectAll(inspect.ReasonExpectationsInvalid, err.Error())
		return res, res.ReasonCodes
	}
	res := inspect.Inspect(decoded, exp)
	return res, res.ReasonCodes
}

func joinMax(items []string, max int) string {
	if len(items) > max {
		items = append(items[:max], fmt.Sprintf("(+%d more)", len(items)-max))
	}
	out := ""
	for i, it := range items {
		if i > 0 {
			out += "; "
		}
		out += it
	}
	return out
}

func (s *PGService) auditDecision(ctx context.Context, tx pgx.Tx, l *loaded, req Request, row decisionRow) error {
	action := "signing.decision.rejected"
	if row.Decision == "APPROVED" {
		action = "signing.decision.approved"
	}
	return s.appendAudit(ctx, tx, l.intent.AccountID, action, row.ID.String(), req, map[string]any{
		"decision_id": row.ID.String(), "attempt_id": req.AttemptID, "plan_id": req.PlanID, "intent_id": req.IntentID,
		"risk_decision_id": req.RiskDecisionID, "wallet_id": req.WalletID, "decision": row.Decision,
		"reason_codes": row.ReasonCodes, "inspector_version": row.InspectorVersion,
		"expected_tx_hash": hex.EncodeToString(row.ExpectedTxHash), "inspected_tx_hash": hex.EncodeToString(row.InspectedTxHash),
		"plan_hash": hex.EncodeToString(l.plan.PlanHash), "requested_by_service": row.RequestedBy,
	}, row.DecidedAt)
}

func (s *PGService) appendAudit(ctx context.Context, tx pgx.Tx, accountID, action, resourceID string, req Request, payload map[string]any, at time.Time) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("signing: encode audit payload: %w", err)
	}
	stream := audit.SystemStream
	if accountID != "" {
		stream = audit.AccountStream(accountID)
	}
	_, err = s.deps.Audit.Append(ctx, tx, audit.Event{
		Stream:        stream,
		ActorType:     string(security.ActorService),
		ActorID:       s.deps.ServiceName,
		Action:        action,
		ResourceType:  "signing_decision",
		ResourceID:    resourceID,
		CorrelationID: req.CorrelationID,
		PolicyVersion: inspect.InspectorVersion,
		EvidenceRef:   req.SimulationRef,
		Payload:       body,
		OccurredAt:    at.UTC(),
	})
	if err != nil {
		return fmt.Errorf("signing: audit: %w", err)
	}
	return nil
}

func toDecision(row decisionRow, providerRef string) Decision {
	var checks []inspect.Check
	if len(row.Checks) > 0 {
		_ = json.Unmarshal(row.Checks, &checks) // stored by this package; a decode failure yields no checks, never a panic
	}
	codes := append([]string(nil), row.ReasonCodes...)
	sort.Strings(codes)
	return Decision{
		ID:               row.ID.String(),
		AttemptID:        row.AttemptID,
		PlanID:           row.PlanID,
		IntentID:         row.IntentID,
		RiskDecisionID:   row.RiskDecisionID,
		WalletID:         row.WalletID,
		Approved:         row.Decision == "APPROVED",
		ReasonCodes:      codes,
		Checks:           checks,
		InspectorVersion: row.InspectorVersion,
		ExpectedTxHash:   append([]byte(nil), row.ExpectedTxHash...),
		InspectedTxHash:  append([]byte(nil), row.InspectedTxHash...),
		ProviderSignRef:  providerRef,
		DecidedAt:        row.DecidedAt,
	}
}
