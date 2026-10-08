package predmarket

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
)

// The sandbox oracle's observations are durable. An operator records what
// the sandbox oracle observed for a market; the observation is signed with the
// sandbox key and appended to prediction_oracle_observations, with an audit
// event, in the caller's transaction. FetchReport reads the latest stored
// observation back and verifies it against the market's registered key before
// returning it, and Resolve verifies it again before accepting it. The
// operator who recorded an observation for a market can never accept a
// resolution of that market (Go here and in Resolve, SQL in the resolution
// guard): the person who said what the oracle saw is not the person who rules
// that it was so.

// ObservationRequest records one sandbox oracle observation.
type ObservationRequest struct {
	MarketID MarketID
	// Outcome is the symbol of the outcome observed.
	Outcome string
	// ObservedAt is when the oracle observed it; never in the future.
	ObservedAt time.Time
	// Evidence is the source document the oracle observed (1..16384 bytes).
	Evidence []byte
	// Actor is the operator recording the observation.
	Actor econ.Actor
}

// ObservationResult is a recorded observation. Replay is true when the
// identical observation had been recorded before: nothing new was written.
type ObservationResult struct {
	Observation OracleObservation
	Replay      bool
}

// RecordObservation signs what the sandbox oracle observed for a market and
// stores it. The market must have registered the sandbox oracle (its source
// and its key) and must not have settled or been voided; the outcome must be
// one of its outcomes. Recording the identical observation again is a replay.
// A later observation supersedes an earlier one for FetchReport, which is how
// the sandbox models an oracle correcting itself; every one stays on record.
func (s *Service) RecordObservation(ctx context.Context, tx pgx.Tx, req ObservationRequest) (ObservationResult, error) {
	by, err := canonicalOperator(req.Actor)
	if err != nil {
		return ObservationResult{}, err
	}
	if s.sandbox == nil {
		return ObservationResult{}, errs.New(errs.CodeProviderUnavailable, "predmarket: no sandbox oracle is configured in this deployment")
	}
	observed := req.ObservedAt.UTC()
	if observed.After(s.now()) {
		return ObservationResult{}, errs.New(errs.CodeValidationFailed, "predmarket: the sandbox oracle cannot observe the future").
			WithField("observed_at", observed.Format(time.RFC3339Nano))
	}
	var out ObservationResult
	err = atomically(ctx, tx, func(tx pgx.Tx) error {
		// FOR SHARE: a resolution of this market (FOR UPDATE) waits for this
		// observation to commit, and then sees who recorded it.
		m, err := s.lockMarket(ctx, tx, req.MarketID, lockShare)
		if err != nil {
			return err
		}
		if !s.sandbox.signsFor(m) {
			return errs.New(errs.CodeForbidden, "predmarket: this market did not register the sandbox oracle; its reports come from its own oracle").
				WithField("market_id", m.ID.String())
		}
		if m.Status.Terminal() {
			return errs.Newf(errs.CodeInvalidStateTransition, "predmarket: market %s is %s; there is nothing left to observe", m.Symbol, m.Status).
				WithField("market_id", m.ID.String())
		}
		outcome, ok := m.OutcomeBySymbol(req.Outcome)
		if !ok {
			return errs.New(errs.CodeValidationFailed, "predmarket: the observation names an outcome this market does not have").
				WithField("market_id", m.ID.String())
		}
		report, err := s.sandbox.Sign(OracleReport{
			MarketID: m.ID, Outcome: outcome.Symbol, ObservedAt: observed, Evidence: append([]byte(nil), req.Evidence...),
		})
		if err != nil {
			return err
		}
		hash := report.Hash()
		if prior, found, err := observationByHash(ctx, tx, m.ID, hash); err != nil {
			return err
		} else if found {
			out = ObservationResult{Observation: prior, Replay: true}
			return nil
		}
		sum := sha256.Sum256(report.Evidence)
		o := OracleObservation{
			ID: id.New[observationKind](), MarketID: m.ID, OutcomeIndex: outcome.Index, Report: report, ReportHash: hash,
			EvidenceSHA256: sum[:], RecordedBy: by, RecordedAt: s.now(),
		}
		if err := insertObservation(ctx, tx, o); err != nil {
			return err
		}
		if err := s.auditObservation(ctx, tx, m, o); err != nil {
			return err
		}
		stored, found, err := observationByHash(ctx, tx, m.ID, hash)
		if err != nil {
			return err
		}
		if !found {
			return errs.New(errs.CodeInternal, "predmarket: the observation vanished")
		}
		out = ObservationResult{Observation: stored}
		return nil
	})
	return out, err
}

func (s *Service) auditObservation(ctx context.Context, tx pgx.Tx, m Market, o OracleObservation) error {
	body, err := json.Marshal(map[string]any{
		"market_id": m.ID.String(), "symbol": m.Symbol, "observation_id": o.ID.String(), "outcome": o.Report.Outcome,
		"outcome_index": o.OutcomeIndex, "source": o.Report.Source, "observed_at": o.Report.ObservedAt,
		"evidence_sha256": hex.EncodeToString(o.EvidenceSHA256), "report_hash": hex.EncodeToString(o.ReportHash),
	})
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "predmarket: encode audit payload")
	}
	_, err = s.audit.Append(ctx, tx, audit.Event{
		Stream: audit.AdminStream, ActorType: string(o.RecordedBy.Type), ActorID: o.RecordedBy.ID,
		Action: "predmarket.oracle.observed", ResourceType: "prediction_market", ResourceID: m.ID.String(),
		Reason: "sandbox oracle observation recorded", Payload: body, OccurredAt: o.RecordedAt,
	})
	return err
}

// FetchReport returns the report a resolution of the market can verify. For a
// market that has stored sandbox observations it is the latest one, verified
// here against the market's key (a stored report that does not verify is
// refused, never returned); for a market that registered the configured
// sandbox oracle and has none yet it is NOT_FOUND; for any other market it is
// the configured external oracle's report (PROVIDER_UNAVAILABLE without one),
// which Resolve verifies. An external provider is asked outside any
// transaction: pass the pool, not a transaction, when one may be asked.
func (s *Service) FetchReport(ctx context.Context, q db.Querier, marketID MarketID) (OracleReport, error) {
	m, err := s.Get(ctx, q, marketID)
	if err != nil {
		return OracleReport{}, err
	}
	obs, found, err := latestObservation(ctx, q, m.ID)
	if err != nil {
		return OracleReport{}, err
	}
	if found {
		if err := VerifyReport(m.OraclePublicKey, obs.Report); err != nil {
			return OracleReport{}, err
		}
		if obs.Report.MarketID != m.ID || obs.Report.Source != m.OracleSource || !bytes.Equal(obs.Report.Hash(), obs.ReportHash) {
			return OracleReport{}, errs.New(errs.CodeReconciliationRequired, "predmarket: a stored oracle observation does not match its own record").
				WithField("market_id", m.ID.String()).WithField("observation_id", obs.ID.String())
		}
		return obs.Report, nil
	}
	if s.sandbox.signsFor(m) {
		return OracleReport{}, errs.New(errs.CodeNotFound, "predmarket: the sandbox oracle has no observation for this market yet").
			WithField("market_id", m.ID.String())
	}
	if s.oracle == nil {
		return OracleReport{}, errs.New(errs.CodeProviderUnavailable, "predmarket: no oracle provider is configured for this market's oracle").
			WithField("market_id", m.ID.String())
	}
	return s.oracle.Report(ctx, marketID)
}

// Observations returns every observation recorded for a market, latest first.
func (s *Service) Observations(ctx context.Context, q db.Querier, marketID MarketID) ([]OracleObservation, error) {
	rows, err := q.Query(ctx, `SELECT `+observationColumns+` FROM prediction_oracle_observations
		WHERE market_id = $1 ORDER BY recorded_at DESC, id DESC`, marketID)
	if err != nil {
		return nil, dbErr("list oracle observations", err)
	}
	defer rows.Close()
	var out []OracleObservation
	for rows.Next() {
		o, err := scanObservation(rows)
		if err != nil {
			return nil, dbErr("scan oracle observation", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// requireNotObserver is the dual-control rule between recording and
// resolving: the operator who recorded an oracle observation for a market
// cannot accept a resolution of it.
func requireNotObserver(ctx context.Context, q db.Querier, m Market, by econ.Actor) error {
	var recorded bool
	if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM prediction_oracle_observations
		WHERE market_id = $1 AND lower(recorded_by_actor_id) = lower($2))`, m.ID, by.ID).Scan(&recorded); err != nil {
		return dbErr("check observation recorders", err)
	}
	if recorded {
		return errs.New(errs.CodeForbidden,
			"predmarket: dual control: the operator who recorded an oracle observation for a market cannot resolve it").
			WithField("actor_id", by.ID).WithField("market_id", m.ID.String())
	}
	return nil
}

const observationColumns = `id, market_id, outcome_index, outcome_symbol, source, observed_at, evidence, evidence_sha256,
	signature, report_hash, recorded_by_actor_type, recorded_by_actor_id, recorded_at`

func scanObservation(row pgx.Row) (OracleObservation, error) {
	var (
		o   OracleObservation
		idx int16
	)
	if err := row.Scan(&o.ID, &o.MarketID, &idx, &o.Report.Outcome, &o.Report.Source, &o.Report.ObservedAt, &o.Report.Evidence,
		&o.EvidenceSHA256, &o.Report.Signature, &o.ReportHash, &o.RecordedBy.Type, &o.RecordedBy.ID, &o.RecordedAt); err != nil {
		return OracleObservation{}, err
	}
	o.OutcomeIndex = int(idx)
	o.Report.MarketID = o.MarketID
	o.Report.ObservedAt = o.Report.ObservedAt.UTC()
	o.RecordedAt = o.RecordedAt.UTC()
	return o, nil
}

func insertObservation(ctx context.Context, tx pgx.Tx, o OracleObservation) error {
	if _, err := tx.Exec(ctx, `INSERT INTO prediction_oracle_observations
		(id, market_id, outcome_index, outcome_symbol, source, observed_at, evidence, evidence_sha256, signature, report_hash,
		 recorded_by_actor_type, recorded_by_actor_id, recorded_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		o.ID, o.MarketID, int16(o.OutcomeIndex), o.Report.Outcome, o.Report.Source, o.Report.ObservedAt, o.Report.Evidence, // #nosec G115 -- bounded by MaxOutcomes
		o.EvidenceSHA256, o.Report.Signature, o.ReportHash, o.RecordedBy.Type, o.RecordedBy.ID, o.RecordedAt); err != nil {
		return dbErr("record oracle observation", err)
	}
	return nil
}

func observationByHash(ctx context.Context, q db.Querier, marketID MarketID, hash []byte) (OracleObservation, bool, error) {
	o, err := scanObservation(q.QueryRow(ctx, `SELECT `+observationColumns+` FROM prediction_oracle_observations
		WHERE market_id = $1 AND report_hash = $2`, marketID, hash))
	if errors.Is(err, pgx.ErrNoRows) {
		return OracleObservation{}, false, nil
	}
	if err != nil {
		return OracleObservation{}, false, dbErr("find oracle observation", err)
	}
	return o, true, nil
}

func latestObservation(ctx context.Context, q db.Querier, marketID MarketID) (OracleObservation, bool, error) {
	o, err := scanObservation(q.QueryRow(ctx, `SELECT `+observationColumns+` FROM prediction_oracle_observations
		WHERE market_id = $1 ORDER BY recorded_at DESC, id DESC LIMIT 1`, marketID))
	if errors.Is(err, pgx.ErrNoRows) {
		return OracleObservation{}, false, nil
	}
	if err != nil {
		return OracleObservation{}, false, dbErr("read the latest oracle observation", err)
	}
	return o, true, nil
}
