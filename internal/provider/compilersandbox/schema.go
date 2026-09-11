package compilersandbox

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strings"
)

// SchemaVersion is the only StructuredStrategy schema this build understands.
// It is on the wire so a document written against a later grammar is refused by
// name rather than silently read as this one.
const SchemaVersion = 1

// MaxIntervalMinutes is a week, matching ir.MaxIntervalMS. MinIntervalMinutes
// is one: the evaluation cadence of a strategy is stated in whole minutes, and
// a sub-minute cadence is not something a person types into a form.
const (
	MinIntervalMinutes = 1
	MaxIntervalMinutes = 7 * 24 * 60
	MaxIntentsPerHour  = 3600
)

// Comparator is the relation an entry or exit rule states between the
// instrument's price and the threshold the user typed.
type Comparator string

// The four comparators. EQ and NE are deliberately absent: a price exactly
// equal to a threshold is a coincidence of tick size, not a rule anybody means.
const (
	CmpLT  Comparator = "LT"
	CmpLTE Comparator = "LTE"
	CmpGT  Comparator = "GT"
	CmpGTE Comparator = "GTE"
)

var allComparators = []Comparator{CmpLT, CmpLTE, CmpGT, CmpGTE}

// Valid reports whether c is a declared comparator.
func (c Comparator) Valid() bool {
	for _, v := range allComparators {
		if v == c {
			return true
		}
	}
	return false
}

// RuleKind is the shape of an entry or exit rule.
type RuleKind string

// The two rule shapes, and there are deliberately only two.
//
// PRICE_THRESHOLD is "act when the price crosses this number". EVERY_INTERVAL
// is "act on every evaluation", which is how a rebalance-every-N-minutes rule
// is stated: the cadence is already in `frequency`, so the rule itself carries
// no condition at all.
const (
	RulePriceThreshold RuleKind = "PRICE_THRESHOLD"
	RuleEveryInterval  RuleKind = "EVERY_INTERVAL"
)

// Mode is the execution mode the strategy is compiled for.
type Mode string

// ModePaper is the only mode this build compiles. Anything else is refused with
// the reason rather than downgraded, because silently compiling a LIVE request
// as PAPER would be answering a question nobody asked.
const ModePaper Mode = "PAPER"

// Universe is the one instrument the strategy may touch and the venue it is
// traded on. Both must exist in this deployment's registry; the venue must list
// the instrument.
type Universe struct {
	// Instrument is the canonical name, e.g. "SOL/USDC".
	Instrument string `json:"instrument"`
	// Venue is the venue code, e.g. "JUPITER".
	Venue string `json:"venue"`
}

// Rule is one entry or exit rule.
type Rule struct {
	Kind RuleKind `json:"kind"`
	// Comparator and PriceUSD are required by PRICE_THRESHOLD and refused by
	// EVERY_INTERVAL, so a rule cannot half-state a threshold.
	Comparator Comparator `json:"comparator,omitempty"`
	// PriceUSD is an exact USD amount in MINOR units, as a string of digits:
	// "13500" is $135.00. It is never a float and never carries a decimal
	// point, for the reason every money field in this system is a string.
	PriceUSD string `json:"price_usd,omitempty"`
}

// RiskLimits are the three ceilings goal §18 asks a person to state.
type RiskLimits struct {
	MaxSingleTradeUSD string `json:"max_single_trade_usd"`
	MaxPositionUSD    string `json:"max_position_usd"`
	MaxDailyLossUSD   string `json:"max_daily_loss_usd"`
}

// CapitalLimit is the smallest envelope the strategy needs to run at all.
type CapitalLimit struct {
	MinAllocationUSD string `json:"min_allocation_usd"`
}

// Frequency is how often the strategy evaluates and how many trade intents it
// may produce in an hour.
type Frequency struct {
	IntervalMinutes   int `json:"interval_minutes"`
	MaxIntentsPerHour int `json:"max_intents_per_hour"`
}

// StructuredStrategy is the whole input. Every field is required; there is no
// default for anything, because a default is an inference about what somebody
// meant and this compiler makes none.
type StructuredStrategy struct {
	SchemaVersion int          `json:"schema_version"`
	Universe      Universe     `json:"universe"`
	Entry         Rule         `json:"entry"`
	Exit          Rule         `json:"exit"`
	RiskLimits    RiskLimits   `json:"risk_limits"`
	CapitalLimit  CapitalLimit `json:"capital_limit"`
	Frequency     Frequency    `json:"frequency"`
	Mode          Mode         `json:"mode"`
}

// Missing is one field the compiler needed and did not get, in the words the
// person who has to fix it reads.
type Missing struct {
	// Field is the JSON path into StructuredStrategy.
	Field string
	// Detail says what was expected. It never guesses what was meant.
	Detail string
}

func (m Missing) String() string { return m.Field + ": " + m.Detail }

// Decode parses the declared strategy strictly: unknown fields are refused
// rather than ignored, so a misspelled key is a named refusal instead of a
// field that silently did not apply.
//
// A decode failure is reported as a single Missing on the whole document,
// because a body that will not parse has no fields to report against.
func Decode(raw json.RawMessage) (StructuredStrategy, []Missing) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || bytes.Equal(trimmed, []byte("{}")) {
		return StructuredStrategy{}, []Missing{{
			Field:  "constraints",
			Detail: "no structured strategy was stated; this compiler reads the fields you fill in and never your description",
		}}
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	var s StructuredStrategy
	if err := dec.Decode(&s); err != nil {
		return StructuredStrategy{}, []Missing{{
			Field:  "constraints",
			Detail: "not a StructuredStrategy document: " + err.Error(),
		}}
	}
	return s, nil
}

// Validate reports every field the compiler needs and did not get, sorted by
// field so the answer is the same every time it is asked.
//
// It reports ALL of them rather than the first: a person filling in a form
// should be told everything that is missing once, not led through them one
// refusal at a time.
func (s StructuredStrategy) Validate() []Missing {
	var out []Missing
	add := func(field, detail string) { out = append(out, Missing{Field: field, Detail: detail}) }

	if s.SchemaVersion != SchemaVersion {
		add("schema_version", fmt.Sprintf("must be %d; this build understands no other structured strategy schema", SchemaVersion))
	}
	if strings.TrimSpace(s.Universe.Instrument) == "" {
		add("universe.instrument", "name one instrument by its canonical name, e.g. SOL/USDC")
	}
	if strings.TrimSpace(s.Universe.Venue) == "" {
		add("universe.venue", "name the venue code the instrument trades on, e.g. JUPITER")
	}
	out = append(out, s.Entry.validate("entry")...)
	out = append(out, s.Exit.validate("exit")...)

	for _, m := range []struct{ field, value string }{
		{"risk_limits.max_single_trade_usd", s.RiskLimits.MaxSingleTradeUSD},
		{"risk_limits.max_position_usd", s.RiskLimits.MaxPositionUSD},
		{"risk_limits.max_daily_loss_usd", s.RiskLimits.MaxDailyLossUSD},
		{"capital_limit.min_allocation_usd", s.CapitalLimit.MinAllocationUSD},
	} {
		if _, err := ParseMinorUSD(m.value); err != nil {
			add(m.field, err.Error())
		}
	}

	switch {
	case s.Frequency.IntervalMinutes == 0:
		add("frequency.interval_minutes", "state how often the strategy evaluates, in whole minutes")
	case s.Frequency.IntervalMinutes < MinIntervalMinutes || s.Frequency.IntervalMinutes > MaxIntervalMinutes:
		add("frequency.interval_minutes", fmt.Sprintf("must be between %d and %d minutes", MinIntervalMinutes, MaxIntervalMinutes))
	}
	switch {
	case s.Frequency.MaxIntentsPerHour == 0:
		add("frequency.max_intents_per_hour", "state the most trade intents this strategy may create in an hour")
	case s.Frequency.MaxIntentsPerHour < 1 || s.Frequency.MaxIntentsPerHour > MaxIntentsPerHour:
		add("frequency.max_intents_per_hour", fmt.Sprintf("must be between 1 and %d", MaxIntentsPerHour))
	}

	switch s.Mode {
	case ModePaper:
	case "":
		add("mode", "state the mode; this build compiles PAPER only")
	default:
		add("mode", fmt.Sprintf("%q is not compiled by this build: a sandbox tier compiles PAPER only, and a mode that moves value is not downgraded silently", s.Mode))
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].Field < out[j].Field })
	return out
}

func (r Rule) validate(field string) []Missing {
	var out []Missing
	add := func(f, detail string) { out = append(out, Missing{Field: f, Detail: detail}) }
	switch r.Kind {
	case RulePriceThreshold:
		if !r.Comparator.Valid() {
			add(field+".comparator", "must be one of LT, LTE, GT, GTE")
		}
		if _, err := ParseMinorUSD(r.PriceUSD); err != nil {
			add(field+".price_usd", err.Error())
		}
	case RuleEveryInterval:
		if r.Comparator != "" {
			add(field+".comparator", "EVERY_INTERVAL states no comparator; the cadence is frequency.interval_minutes")
		}
		if r.PriceUSD != "" {
			add(field+".price_usd", "EVERY_INTERVAL states no threshold; the cadence is frequency.interval_minutes")
		}
	case "":
		add(field+".kind", "state the rule: PRICE_THRESHOLD with a comparator and a price, or EVERY_INTERVAL")
	default:
		add(field+".kind", fmt.Sprintf("%q is not a rule this compiler knows; use PRICE_THRESHOLD or EVERY_INTERVAL", r.Kind))
	}
	return out
}

// MaxMinorDigits bounds a stated amount at fifteen digits — ten trillion
// dollars in cents — which is comfortably above any real limit and safely
// inside an int64.
const MaxMinorDigits = 15

// ParseMinorUSD parses an exact USD amount written as a string of MINOR units.
//
// Digits only: no sign, no decimal point, no exponent, no separators, no
// leading zero. It never sees a float, and a value that is not exactly
// representable is refused rather than rounded.
func ParseMinorUSD(s string) (int64, error) {
	if strings.TrimSpace(s) == "" {
		return 0, fmt.Errorf("state an exact amount in USD minor units, as digits: \"5000\" is $50.00")
	}
	if s != strings.TrimSpace(s) {
		return 0, fmt.Errorf("an amount carries no surrounding spaces")
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, fmt.Errorf("an amount is digits only, in USD minor units: %q is not", s)
		}
	}
	if len(s) > 1 && s[0] == '0' {
		return 0, fmt.Errorf("an amount carries no leading zero: %q", s)
	}
	if len(s) > MaxMinorDigits {
		return 0, fmt.Errorf("an amount is at most %d digits", MaxMinorDigits)
	}
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return 0, fmt.Errorf("an amount is digits only, in USD minor units: %q is not", s)
	}
	return v.Int64(), nil
}

// FieldNames returns every field of the grammar, in the order a form asks for
// them. It exists so the API and the documentation cannot drift from the
// decoder: both are generated from this one list.
func FieldNames() []string {
	return []string{
		"schema_version",
		"universe.instrument", "universe.venue",
		"entry.kind", "entry.comparator", "entry.price_usd",
		"exit.kind", "exit.comparator", "exit.price_usd",
		"risk_limits.max_single_trade_usd", "risk_limits.max_position_usd", "risk_limits.max_daily_loss_usd",
		"capital_limit.min_allocation_usd",
		"frequency.interval_minutes", "frequency.max_intents_per_hour",
		"mode",
	}
}
