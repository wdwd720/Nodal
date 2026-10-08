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
	existing, err := scanMark(tx.QueryRow(ctx, `SELECT `+markColumns+` FROM derivative_marks
		WHERE instrument_id = $1 AND source = $2 AND observed_at = $3`, r.InstrumentID, src, observed))
	if err != nil {
		return Mark{}, dbErr("read existing mark", err)
	}
	// A replay must say what was said the first time: the mark, and the
	// index when it was submitted (a derived index is compared by the mark it
	// came with, since asset prices observed earlier may arrive later).
	sameIndex := r.Index == nil ||
		(existing.HasIndex && existing.IndexMantissa.Equal(r.Index.Mantissa) && existing.IndexScale == r.Index.Scale)
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

// markAt returns the latest mark observed in (at − maxAge, at]: the mark
// that was current, and fresh, at a moment.
func markAt(ctx context.Context, q db.Querier, iid instruments.InstrumentID, at time.Time, maxAge time.Duration) (Mark, bool, error) {
	m, err := scanMark(q.QueryRow(ctx, `SELECT `+markColumns+` FROM derivative_marks WHERE instrument_id = $1
		AND observed_at <= $2 AND observed_at > $3 ORDER BY observed_at DESC, recorded_at DESC, id DESC LIMIT 1`,
		iid, at.UTC(), at.Add(-maxAge).UTC()))
	if errors.Is(err, pgx.ErrNoRows) {
		return Mark{}, false, nil
	}
	if err != nil {
		return Mark{}, false, dbErr("mark at time", err)
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

// marksWithIndexIn returns the marks with an index observed in [start, end),
// oldest first: the samples of a funding interval.
func marksWithIndexIn(ctx context.Context, q db.Querier, iid instruments.InstrumentID, start, end time.Time) ([]Mark, error) {
	rows, err := q.Query(ctx, `SELECT `+markColumns+` FROM derivative_marks WHERE instrument_id = $1
		AND observed_at >= $2 AND observed_at < $3 AND index_mantissa IS NOT NULL ORDER BY observed_at, recorded_at, id`,
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
