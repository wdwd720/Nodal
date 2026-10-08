package feeledger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// scheduleKeyPattern is the shape of a schedule key, e.g.
// "orders:paper:<instrument id>" or "transfers:default"; the database checks
// the same pattern.
var scheduleKeyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.:/-]{0,127}$`)

// PublishedSchedule is one stored version of a fee schedule.
type PublishedSchedule struct {
	ID            ScheduleID
	Key           string
	Version       int
	Label         string
	Schedule      fees.Schedule
	Canonical     []byte
	Hash          string
	QuoteAsset    assets.AssetID
	EffectiveFrom time.Time
	PublishedAt   time.Time
	Actor         econ.Actor
	Reason        string
}

// PublishRequest publishes the next version of a schedule key.
type PublishRequest struct {
	Key      string
	Schedule fees.Schedule
	// EffectiveFrom is when the version takes over; zero means now. It is
	// never in the past and always after the previous version's.
	EffectiveFrom time.Time
	Reason        string
	Actor         econ.Actor
}

// PublishSchedule records the next version of a schedule key: validated,
// immutable, stored in canonical form with its hash, effective from an
// instant that is never in the past. Publishing the same label with the same
// content and effective instant again returns the stored version; the same
// label with anything else is a CONFLICT. A person or a service publishes;
// an agent never does.
func (s *Service) PublishSchedule(ctx context.Context, tx pgx.Tx, r PublishRequest) (PublishedSchedule, error) {
	if err := requireActor(r.Actor); err != nil {
		return PublishedSchedule{}, err
	}
	if !scheduleKeyPattern.MatchString(r.Key) {
		return PublishedSchedule{}, errs.New(errs.CodeValidationFailed, "feeledger: a schedule key is lower-case [a-z0-9_.:/-], at most 128 bytes")
	}
	if strings.TrimSpace(r.Reason) == "" {
		return PublishedSchedule{}, errs.New(errs.CodeValidationFailed, "feeledger: publishing a schedule needs a reason")
	}
	if err := r.Schedule.Validate(); err != nil {
		return PublishedSchedule{}, err
	}
	quote, err := s.assets.Get(ctx, tx, r.Schedule.QuoteAsset)
	if err != nil {
		return PublishedSchedule{}, err
	}
	if quote.Kind != assets.KindSimulated || quote.ValueDomain != valuedomain.Simulated {
		return PublishedSchedule{}, errs.New(errs.CodeForbidden, "feeledger: a fee schedule is quoted in a SIMULATED asset")
	}
	if quote.Decimals != r.Schedule.QuoteDecimals {
		return PublishedSchedule{}, errs.New(errs.CodeValidationFailed, "feeledger: the schedule's quote decimals are not its asset's")
	}
	canonical, err := r.Schedule.Canonical()
	if err != nil {
		return PublishedSchedule{}, err
	}
	hash, err := r.Schedule.Hash()
	if err != nil {
		return PublishedSchedule{}, err
	}
	now := s.clk.Now().UTC()
	effective := r.EffectiveFrom.UTC()
	if r.EffectiveFrom.IsZero() {
		effective = now
	}
	if err := lockKey(ctx, tx, "schedule:"+r.Key); err != nil {
		return PublishedSchedule{}, err
	}
	if prior, found, err := s.scheduleWhere(ctx, tx, `schedule_key = $1 AND label = $2`, r.Key, r.Schedule.Version); err != nil {
		return PublishedSchedule{}, err
	} else if found {
		if prior.Hash == hash && (r.EffectiveFrom.IsZero() || prior.EffectiveFrom.Equal(effective)) {
			return prior, nil
		}
		return PublishedSchedule{}, errs.New(errs.CodeConflict, "feeledger: this schedule label is already published with different content").
			WithField("schedule_id", prior.ID.String())
	}
	if effective.Before(now) {
		return PublishedSchedule{}, rejection(errs.CodeValidationFailed, ReasonBackdated, "a schedule never takes effect in the past")
	}
	latest, found, err := s.scheduleWhere(ctx, tx, `schedule_key = $1 ORDER BY version DESC LIMIT 1`, r.Key)
	if err != nil {
		return PublishedSchedule{}, err
	}
	version := 1
	if found {
		if !effective.After(latest.EffectiveFrom) {
			return PublishedSchedule{}, rejection(errs.CodeValidationFailed, ReasonBackdated,
				"a schedule version takes effect after the version before it")
		}
		version = latest.Version + 1
	}
	p := PublishedSchedule{
		ID: id.New[scheduleKind](), Key: r.Key, Version: version, Label: r.Schedule.Version, Schedule: r.Schedule,
		Canonical: canonical, Hash: hash, QuoteAsset: quote.ID, EffectiveFrom: effective, PublishedAt: now, Actor: r.Actor,
		Reason: r.Reason,
	}
	if _, err := tx.Exec(ctx, `INSERT INTO fee_schedules (id, schedule_key, version, label, quote_asset_id, quote_decimals,
		spread_bps, percentage_fee_bps, max_total_fee_bps, max_total_fee, schedule, schedule_hash, effective_from, published_at,
		created_by_actor_type, created_by_actor_id, reason)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::numeric,$11,$12,$13,$14,$15,$16,$17)`,
		p.ID, p.Key, p.Version, p.Label, quote.ID, int16(quote.Decimals), int(r.Schedule.SpreadBPS),
		int(r.Schedule.PercentageFeeBPS), int(r.Schedule.MaxTotalFeeBPS), r.Schedule.MaxTotalFee.String(), canonical, hash,
		effective, now, r.Actor.Type, r.Actor.ID, r.Reason); err != nil {
		return PublishedSchedule{}, dbErr("publish fee schedule", err)
	}
	payload, err := json.Marshal(map[string]any{
		"schedule_id": p.ID.String(), "schedule_key": p.Key, "version": p.Version, "label": p.Label, "schedule_hash": hash,
		"effective_from": effective, "value_domain": "SIMULATED",
	})
	if err != nil {
		return PublishedSchedule{}, errs.Wrap(err, errs.CodeInternal, "feeledger: encode audit payload")
	}
	if _, err := s.audit.Append(ctx, tx, audit.Event{
		Stream: audit.AdminStream, ActorType: string(r.Actor.Type), ActorID: r.Actor.ID, Action: "fees.schedule.published",
		ResourceType: "fee_schedule", ResourceID: p.ID.String(), Reason: r.Reason, Payload: payload, OccurredAt: now,
	}); err != nil {
		return PublishedSchedule{}, err
	}
	return p, nil
}

const scheduleColumns = `id, schedule_key, version, label, quote_asset_id, schedule, schedule_hash, effective_from, published_at,
	created_by_actor_type, created_by_actor_id, reason`

func (s *Service) scheduleWhere(ctx context.Context, q db.Querier, where string, args ...any) (PublishedSchedule, bool, error) {
	var p PublishedSchedule
	err := q.QueryRow(ctx, `SELECT `+scheduleColumns+` FROM fee_schedules WHERE `+where, args...).Scan(&p.ID, &p.Key, &p.Version,
		&p.Label, &p.QuoteAsset, &p.Canonical, &p.Hash, &p.EffectiveFrom, &p.PublishedAt, &p.Actor.Type, &p.Actor.ID, &p.Reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return PublishedSchedule{}, false, nil
	}
	if err != nil {
		return PublishedSchedule{}, false, dbErr("load fee schedule", err)
	}
	sched, err := DecodeSchedule(p.Canonical)
	if err != nil {
		return PublishedSchedule{}, false, err
	}
	if h, err := sched.Hash(); err != nil || h != p.Hash || sched.Version != p.Label || sched.QuoteAsset != p.QuoteAsset {
		return PublishedSchedule{}, false, rejection(errs.CodeInternal, ReasonRecordDoesNotVerify, "a stored schedule is not the one its row names")
	}
	p.Schedule = sched
	p.EffectiveFrom, p.PublishedAt = p.EffectiveFrom.UTC(), p.PublishedAt.UTC()
	return p, true, nil
}

// EffectiveSchedule returns the version of a schedule key in force at an
// instant: the latest whose effective_from is at or before it.
func (s *Service) EffectiveSchedule(ctx context.Context, q db.Querier, key string, at time.Time) (PublishedSchedule, error) {
	p, found, err := s.scheduleWhere(ctx, q, `schedule_key = $1 AND effective_from <= $2 ORDER BY effective_from DESC, version DESC LIMIT 1`,
		key, at.UTC())
	if err != nil {
		return PublishedSchedule{}, err
	}
	if !found {
		return PublishedSchedule{}, rejection(errs.CodeNotFound, ReasonScheduleNotFound, "no version of this schedule is in force at that instant").
			WithField("schedule_key", key)
	}
	return p, nil
}

// Schedule returns one published version.
func (s *Service) Schedule(ctx context.Context, q db.Querier, sid ScheduleID) (PublishedSchedule, error) {
	p, found, err := s.scheduleWhere(ctx, q, `id = $1`, sid)
	if err != nil {
		return PublishedSchedule{}, err
	}
	if !found {
		return PublishedSchedule{}, errs.New(errs.CodeNotFound, "feeledger: schedule not found").WithField("schedule_id", sid.String())
	}
	return p, nil
}

// ScheduleHistory returns every version of a schedule key, oldest first.
func (s *Service) ScheduleHistory(ctx context.Context, q db.Querier, key string) ([]PublishedSchedule, error) {
	rows, err := q.Query(ctx, `SELECT id FROM fee_schedules WHERE schedule_key = $1 ORDER BY version`, key)
	if err != nil {
		return nil, dbErr("schedule history", err)
	}
	var ids []ScheduleID
	for rows.Next() {
		var sid ScheduleID
		if err := rows.Scan(&sid); err != nil {
			rows.Close()
			return nil, dbErr("schedule history", err)
		}
		ids = append(ids, sid)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, dbErr("schedule history", err)
	}
	out := make([]PublishedSchedule, 0, len(ids))
	for _, sid := range ids {
		p, err := s.Schedule(ctx, q, sid)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// scheduleDoc mirrors the canonical form fees.Schedule.Canonical writes.
type scheduleDoc struct {
	Kind                    string              `json:"kind"`
	Version                 string              `json:"version"`
	QuoteAsset              assets.AssetID      `json:"quote_asset"`
	QuoteDecimals           uint8               `json:"quote_decimals"`
	SpreadBPS               money.BPS           `json:"spread_bps"`
	FixedFee                money.Quantity      `json:"fixed_fee"`
	PercentageFeeBPS        money.BPS           `json:"percentage_fee_bps"`
	VenuePassThroughBPS     money.BPS           `json:"venue_pass_through_bps"`
	NetworkFeeAsset         assets.AssetID      `json:"network_fee_asset"`
	NetworkFeeAmount        money.Quantity      `json:"network_fee_amount"`
	SlippageReserveBPS      money.BPS           `json:"slippage_reserve_bps"`
	SubscriptionDiscountBPS money.BPS           `json:"subscription_discount_bps"`
	Tiers                   []tierDoc           `json:"tiers"`
	StrategyFeeBPS          money.BPS           `json:"strategy_fee_bps"`
	MaxTotalFeeBPS          money.BPS           `json:"max_total_fee_bps"`
	MaxTotalFee             money.Quantity      `json:"max_total_fee"`
	MinFee                  money.Quantity      `json:"min_fee"`
	Rounding                scheduleRoundingDoc `json:"rounding"`
}

type tierDoc struct {
	MinTrailingVolume      money.USD `json:"min_trailing_volume_usd"`
	OverridesPercentageFee bool      `json:"overrides_percentage_fee"`
	PercentageFeeBPS       money.BPS `json:"percentage_fee_bps"`
	OverridesSpread        bool      `json:"overrides_spread"`
	SpreadBPS              money.BPS `json:"spread_bps"`
}

type scheduleRoundingDoc struct {
	BuyNotional             string `json:"buy_notional"`
	SellNotional            string `json:"sell_notional"`
	Spread                  string `json:"spread"`
	PercentageFee           string `json:"percentage_fee"`
	VenuePassThrough        string `json:"venue_pass_through"`
	StrategyFee             string `json:"strategy_fee"`
	FeeCap                  string `json:"fee_cap"`
	SlippageReserve         string `json:"slippage_reserve"`
	AllowPlatformFavourable bool   `json:"allow_platform_favourable"`
}

// DecodeSchedule rebuilds a schedule from its canonical form and proves it:
// the result must validate and re-encode to exactly the stored bytes, so the
// hash recorded with the bytes names the schedule returned. It is what an
// auditor runs on a stored schedule.
func DecodeSchedule(canonical []byte) (fees.Schedule, error) {
	var d scheduleDoc
	dec := json.NewDecoder(bytes.NewReader(canonical))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return fees.Schedule{}, rejection(errs.CodeValidationFailed, ReasonRecordDoesNotVerify, "a stored schedule is not a schedule")
	}
	if d.Kind != fees.ScheduleKind {
		return fees.Schedule{}, rejection(errs.CodeValidationFailed, ReasonRecordDoesNotVerify, "a stored schedule has the wrong kind")
	}
	modes := make([]money.RoundingMode, 8)
	for i, name := range []string{d.Rounding.BuyNotional, d.Rounding.SellNotional, d.Rounding.Spread, d.Rounding.PercentageFee,
		d.Rounding.VenuePassThrough, d.Rounding.StrategyFee, d.Rounding.FeeCap, d.Rounding.SlippageReserve} {
		m, err := money.ParseRoundingMode(name)
		if err != nil {
			return fees.Schedule{}, rejection(errs.CodeValidationFailed, ReasonRecordDoesNotVerify, "a stored schedule has an unknown rounding mode")
		}
		modes[i] = m
	}
	s := fees.Schedule{
		Version: d.Version, QuoteAsset: d.QuoteAsset, QuoteDecimals: d.QuoteDecimals, SpreadBPS: d.SpreadBPS, FixedFee: d.FixedFee,
		PercentageFeeBPS: d.PercentageFeeBPS, VenuePassThroughBPS: d.VenuePassThroughBPS,
		NetworkFee:         fees.NetworkFeeEstimate{Asset: d.NetworkFeeAsset, Amount: d.NetworkFeeAmount},
		SlippageReserveBPS: d.SlippageReserveBPS, SubscriptionDiscountBPS: d.SubscriptionDiscountBPS, StrategyFeeBPS: d.StrategyFeeBPS,
		MaxTotalFeeBPS: d.MaxTotalFeeBPS, MaxTotalFee: d.MaxTotalFee, MinFee: d.MinFee,
		Rounding: fees.RoundingPolicy{
			BuyNotional: modes[0], SellNotional: modes[1], Spread: modes[2], PercentageFee: modes[3], VenuePassThrough: modes[4],
			StrategyFee: modes[5], FeeCap: modes[6], SlippageReserve: modes[7],
			AllowPlatformFavourable: d.Rounding.AllowPlatformFavourable,
		},
	}
	for _, t := range d.Tiers {
		s.Tiers = append(s.Tiers, fees.VolumeTier(t))
	}
	if err := s.Validate(); err != nil {
		return fees.Schedule{}, err
	}
	again, err := s.Canonical()
	if err != nil {
		return fees.Schedule{}, err
	}
	if !bytes.Equal(again, canonical) {
		return fees.Schedule{}, rejection(errs.CodeValidationFailed, ReasonRecordDoesNotVerify, "a stored schedule is not in canonical form")
	}
	return s, nil
}
