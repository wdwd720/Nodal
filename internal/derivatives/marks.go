package derivatives

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/money"
)

// RecordMarkRequest is one mark observation.
type RecordMarkRequest struct {
	InstrumentID instruments.InstrumentID
	// Mark is the observed mark (an option's: its premium). It must be
	// expressible exactly at the instrument's price scale and lie on its tick
	// grid; an oracle adapter rounds, this store never does.
	Mark       money.Price
	Source     string
	ObservedAt time.Time
	// Index overrides the index. Without it the index is the underlying's
	// spot price from valuation asset_prices (quoted in the settlement asset)
	// as of ObservedAt, when one is fresh within the instrument's oracle max
	// age; otherwise the mark is recorded without an index.
	Index *money.Price
}

const markColumns = `id, instrument_id, mark_mantissa, mark_scale, index_mantissa::text, index_scale, coalesce(index_source, ''),
	source, observed_at, recorded_at`

func scanMark(row pgx.Row) (Mark, error) {
	var m Mark
	var idx *string
	var idxScale *int32
	if err := row.Scan(&m.ID, &m.InstrumentID, &m.MarkMantissa, &m.MarkScale, &idx, &idxScale, &m.IndexSource,
		&m.Source, &m.ObservedAt, &m.RecordedAt); err != nil {
		return Mark{}, err
	}
	if idx != nil && idxScale != nil {
		q, err := money.ScanQuantity(*idx)
		if err != nil {
			return Mark{}, err
		}
		m.HasIndex, m.IndexMantissa, m.IndexScale = true, q, *idxScale
	}
	m.ObservedAt = m.ObservedAt.UTC()
	m.RecordedAt = m.RecordedAt.UTC()
	return m, nil
}

// RecordMark stores a mark. It is idempotent on (instrument, source,
// observed_at): the same observation again returns the stored mark; a
// different mark, or a different explicitly submitted index, under the same
// identity is a CONFLICT.
//
// A new observation is recorded only in order and on time: never observed in
// the future, never older than the policy's MaxMarkLag behind now, and never
// older than the instrument's latest recorded mark (the database refuses an
// out-of-order mark as well, DV005). Whoever records marks therefore cannot
// reach back past a price that is already known and choose another one for a
// moment that has passed; and every run and the final settlement read only
// marks recorded by their own cutoff, so a mark recorded later cannot reach
// back into them either.
func (s *Service) RecordMark(ctx context.Context, tx pgx.Tx, r RecordMarkRequest) (Mark, error) {
	if err := r.Mark.Validate(); err != nil {
		return Mark{}, errs.Wrap(err, errs.CodeValidationFailed, "derivatives: invalid mark")
	}
	src := strings.TrimSpace(r.Source)
	if src == "" || src != r.Source || len(src) > 100 {
		return Mark{}, errs.New(errs.CodeValidationFailed, "derivatives: a mark names its source (1..100 characters)")
	}
	if r.ObservedAt.IsZero() {
		return Mark{}, errs.New(errs.CodeValidationFailed, "derivatives: a mark states when it was observed")
	}
	now := s.clk.Now().UTC()
	observed := r.ObservedAt.UTC()
	if observed.After(now) {
		return Mark{}, errs.New(errs.CodeValidationFailed, "derivatives: a mark cannot be observed in the future").
			WithField("observed_at", observed).WithField("now", now)
	}
	spec, err := loadSpec(ctx, tx, s.instruments, r.InstrumentID)
	if err != nil {
		return Mark{}, err
	}
	mark, err := Rescale(r.Mark.Mantissa, r.Mark.Scale, spec.PriceScale, money.RoundExact)
	if err != nil || !spec.OnTick(mark) {
		return Mark{}, errs.New(errs.CodeValidationFailed, "derivatives: a mark must be exact at the instrument's price scale and on its tick grid").
			WithField("mark", r.Mark.String()).WithField("tick_size", spec.Tick.String()).WithField("price_scale", spec.PriceScale)
	}
	// A replay says again what was said the first time; it is answered from
	// the record, whatever has been recorded since.
	existing, found, err := markByIdentity(ctx, tx, r.InstrumentID, src, observed)
	if err != nil {
		return Mark{}, err
	}
	if found {
		return replayMark(existing, mark, r.Index)
	}
	if lag := now.Sub(observed); lag > s.policy.maxMarkLag() {
		return Mark{}, errs.New(errs.CodeValidationFailed,
			"derivatives: this observation is older than the policy allows a mark to be recorded; marks are recorded as they are observed, not afterwards").
			WithField("observed_at", observed).WithField("now", now).WithField("max_mark_lag_ms", s.policy.MaxMarkLagMS)
	}
	if latest, ok, err := LatestMark(ctx, tx, r.InstrumentID); err != nil {
		return Mark{}, err
	} else if ok && observed.Before(latest.ObservedAt) {
		return Mark{}, errs.New(errs.CodeConflict,
			"derivatives: a later observation of this contract is already recorded; an earlier one cannot be added behind it").
			WithField("observed_at", observed).WithField("latest_observed_at", latest.ObservedAt).WithField("latest_mark_id", latest.ID.String())
	}
	var idxMantissa, idxScale, idxSource any
	switch {
	case r.Index != nil:
		if err := r.Index.Validate(); err != nil || !r.Index.Mantissa.IsPositive() {
			return Mark{}, errs.New(errs.CodeValidationFailed, "derivatives: an index must be a valid positive price")
		}
		idxMantissa, idxScale, idxSource = r.Index.Mantissa, r.Index.Scale, r.Index.Source
	default:
		p, err := s.prices.Latest(ctx, tx, spec.UnderlyingAssetID, spec.SettlementAssetID, spec.OracleMaxAge, observed)
		switch {
		case err == nil && p.Mantissa.IsPositive():
			idxMantissa, idxScale, idxSource = p.Mantissa, p.Scale, "asset_prices:"+p.Source
		case err == nil || errs.CodeOf(err) == errs.CodeStaleMarketData:
			// No fresh spot: the mark is recorded without an index, and
			// everything that needs one fails closed.
		default:
			return Mark{}, err
		}
	}
	m, err := scanMark(tx.QueryRow(ctx, `INSERT INTO derivative_marks
		(id, instrument_id, mark_mantissa, mark_scale, index_mantissa, index_scale, index_source, source, observed_at, recorded_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT (instrument_id, source, observed_at) DO NOTHING
		RETURNING `+markColumns,
		newMarkID(), r.InstrumentID, mark, spec.PriceScale, idxMantissa, idxScale, idxSource, src, observed, now))
	if err == nil {
		return m, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Mark{}, dbErr("record mark", err)
	}
	// A concurrent transaction recorded the same observation first.
	existing, found, err = markByIdentity(ctx, tx, r.InstrumentID, src, observed)
	if err != nil {
		return Mark{}, err
	}
	if !found {
		return Mark{}, errs.New(errs.CodeInternal, "derivatives: a mark conflicted on its identity but cannot be read")
	}
	return replayMark(existing, mark, r.Index)
}

// markByIdentity reads the mark recorded under (instrument, source,
// observed_at), if any.
func markByIdentity(ctx context.Context, q db.Querier, iid instruments.InstrumentID, src string, observed time.Time) (Mark, bool, error) {
	m, err := scanMark(q.QueryRow(ctx, `SELECT `+markColumns+` FROM derivative_marks
		WHERE instrument_id = $1 AND source = $2 AND observed_at = $3`, iid, src, observed.UTC()))
	if errors.Is(err, pgx.ErrNoRows) {
		return Mark{}, false, nil
	}
	if err != nil {
		return Mark{}, false, dbErr("read existing mark", err)
	}
	return m, true, nil
}

// replayMark answers a repeated observation. A replay must say what was said
// the first time: the mark, and the index when it was submitted (a derived
// index is compared by the mark it came with, since asset prices observed
// earlier may arrive later).
func replayMark(existing Mark, mark money.Quantity, index *money.Price) (Mark, error) {
	sameIndex := index == nil ||
		(existing.HasIndex && existing.IndexMantissa.Equal(index.Mantissa) && existing.IndexScale == index.Scale)
	if !existing.MarkMantissa.Equal(mark) || !sameIndex {
		return Mark{}, errs.New(errs.CodeConflict, "derivatives: a different mark or index is already recorded for this observation").
			WithField("mark_id", existing.ID.String())
	}
	return existing, nil
}

// LatestMark returns the instrument's most recently observed mark.
func LatestMark(ctx context.Context, q db.Querier, iid instruments.InstrumentID) (Mark, bool, error) {
	m, err := scanMark(q.QueryRow(ctx, `SELECT `+markColumns+` FROM derivative_marks WHERE instrument_id = $1
		ORDER BY observed_at DESC, recorded_at DESC, id DESC LIMIT 1`, iid))
	if errors.Is(err, pgx.ErrNoRows) {
		return Mark{}, false, nil
	}
	if err != nil {
		return Mark{}, false, dbErr("latest mark", err)
	}
	return m, true, nil
}

// markFresh reports whether a mark is fresh at a moment: observed no later
// than it and no more than the instrument's oracle max age before it. Every
// freshness decision of this package -- the risk kernel's oracle section, a
// close's price, a run's mark, the final settlement -- is this one test.
func markFresh(s Spec, m Mark, at time.Time) bool {
	age := at.Sub(m.ObservedAt)
	return age >= 0 && age <= s.OracleMaxAge
}

// markPriceable reports whether a mark is expressed on the instrument's
// current terms: at its price scale and on its tick grid. Terms cannot change
// while positions are open (DV007), but a mark recorded under terms an
// instrument had before it was amended must not price anything after it.
func markPriceable(s Spec, m Mark) bool {
	return m.MarkScale == s.PriceScale && s.OnTick(m.MarkMantissa)
}

// markAsOf returns the latest mark of the instrument that was observed in
// [at − maxAge, at] -- current, and fresh, at that moment -- and recorded no
// later than cutoff: a run or a settlement reads only what had been recorded
// by its own cutoff, never a mark recorded afterwards and stamped earlier.
func markAsOf(ctx context.Context, q db.Querier, iid instruments.InstrumentID, at time.Time, maxAge time.Duration, cutoff time.Time) (Mark, bool, error) {
	m, err := scanMark(q.QueryRow(ctx, `SELECT `+markColumns+` FROM derivative_marks WHERE instrument_id = $1
		AND observed_at <= $2 AND observed_at >= $3 AND recorded_at <= $4
		ORDER BY observed_at DESC, recorded_at DESC, id DESC LIMIT 1`,
		iid, at.UTC(), at.Add(-maxAge).UTC(), cutoff.UTC()))
	if errors.Is(err, pgx.ErrNoRows) {
		return Mark{}, false, nil
	}
	if err != nil {
		return Mark{}, false, dbErr("mark as of a time", err)
	}
	return m, true, nil
}

// markByID returns one mark of an instrument.
func markByID(ctx context.Context, q db.Querier, iid instruments.InstrumentID, mid MarkID) (Mark, error) {
	m, err := scanMark(q.QueryRow(ctx, `SELECT `+markColumns+` FROM derivative_marks WHERE id = $1 AND instrument_id = $2`, mid, iid))
	if errors.Is(err, pgx.ErrNoRows) {
		return Mark{}, errs.New(errs.CodeNotFound, "derivatives: mark not found for this instrument").WithField("mark_id", mid.String())
	}
	if err != nil {
		return Mark{}, dbErr("mark by id", err)
	}
	return m, nil
}

// marksWithIndexIn returns the marks with an index observed in [start, end)
// and recorded no later than end, oldest first: the samples of a funding
// interval. A mark recorded after the interval ended is not one of them,
// whatever moment it claims to have been observed at.
func marksWithIndexIn(ctx context.Context, q db.Querier, iid instruments.InstrumentID, start, end time.Time) ([]Mark, error) {
	rows, err := q.Query(ctx, `SELECT `+markColumns+` FROM derivative_marks WHERE instrument_id = $1
		AND observed_at >= $2 AND observed_at < $3 AND recorded_at <= $3 AND index_mantissa IS NOT NULL
		ORDER BY observed_at, recorded_at, id`,
		iid, start.UTC(), end.UTC())
	if err != nil {
		return nil, dbErr("marks in interval", err)
	}
	defer rows.Close()
	var out []Mark
	for rows.Next() {
		m, err := scanMark(rows)
		if err != nil {
			return nil, dbErr("scan mark", err)
		}
		out = append(out, m)
	}
	return out, dbErr("marks in interval", rows.Err())
}

// sampleOf turns a mark into a funding / divergence sample.
func sampleOf(m Mark) IndexSample {
	return IndexSample{Mark: m.MarkMantissa, MarkScale: m.MarkScale, Index: m.IndexMantissa, IndexScale: m.IndexScale}
}
