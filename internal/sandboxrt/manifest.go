package sandboxrt

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/instruments"
	nodalsdk "github.com/nodal/controlplane/internal/sandboxrt/sdk"
)

// ManifestSchema names the manifest format.
const ManifestSchema = "nodal.strategy.manifest/v1"

// Manifest bounds.
const (
	MaxManifestInstruments = 256
	MaxIntentsPerRunBound  = 100
	MinIntervalSecondsMin  = 60
	MinIntervalSecondsMax  = 365 * 24 * 3600
)

// Manifest is a strategy version's declared permissions. It is immutable per
// version (its canonical bytes are hashed and the database refuses any
// change), and it is enforced at run time: every proposal beyond it is
// dropped and recorded as a violation. A strategy can never gain a
// permission its manifest does not state.
type Manifest struct {
	Schema string `json:"schema"`
	// AllowedIntentKinds is a non-empty subset of ProposableKinds.
	AllowedIntentKinds []econ.IntentKind `json:"allowed_intent_kinds"`
	// An instrument is allowed when its id is listed or its kind is listed.
	AllowedInstrumentIDs   []string `json:"allowed_instrument_ids"`
	AllowedInstrumentKinds []string `json:"allowed_instrument_kinds"`
	// MaxNotionalPerIntent bounds quantity x price of one proposal, in
	// simulated dollars (a positive decimal string).
	MaxNotionalPerIntent string `json:"max_notional_per_intent"`
	// MaxIntentsPerRun bounds how many proposals one run may have accepted.
	MaxIntentsPerRun int `json:"max_intents_per_run"`
	// MinIntervalSeconds is the execution frequency: at least this long
	// between two follow runs of one install.
	MinIntervalSeconds int64 `json:"min_interval_seconds"`
}

// Canonicalize returns the manifest with the schema set and every list
// sorted and de-duplicated.
func (m Manifest) Canonicalize() Manifest {
	out := m
	if out.Schema == "" {
		out.Schema = ManifestSchema
	}
	kinds := make([]string, 0, len(m.AllowedIntentKinds))
	for _, k := range m.AllowedIntentKinds {
		kinds = append(kinds, string(k))
	}
	kinds = sortedUnique(kinds)
	out.AllowedIntentKinds = make([]econ.IntentKind, len(kinds))
	for i, k := range kinds {
		out.AllowedIntentKinds[i] = econ.IntentKind(k)
	}
	out.AllowedInstrumentIDs = sortedUnique(m.AllowedInstrumentIDs)
	out.AllowedInstrumentKinds = sortedUnique(m.AllowedInstrumentKinds)
	return out
}

func sortedUnique(in []string) []string {
	out := append([]string{}, in...)
	sort.Strings(out)
	j := 0
	for i, s := range out {
		if i == 0 || s != out[i-1] {
			out[j] = s
			j++
		}
	}
	return out[:j]
}

// Validate refuses a manifest that is not canonical or grants anything
// outside the bounds.
func (m Manifest) Validate() error {
	var problems []string
	if m.Schema != ManifestSchema {
		problems = append(problems, "schema must be "+ManifestSchema)
	}
	c := m.Canonicalize()
	if !equalKinds(c.AllowedIntentKinds, m.AllowedIntentKinds) || !equalStrings(c.AllowedInstrumentIDs, m.AllowedInstrumentIDs) ||
		!equalStrings(c.AllowedInstrumentKinds, m.AllowedInstrumentKinds) {
		problems = append(problems, "lists must be sorted and free of duplicates (use Canonicalize)")
	}
	if len(m.AllowedIntentKinds) == 0 {
		problems = append(problems, "at least one intent kind must be allowed")
	}
	for _, k := range m.AllowedIntentKinds {
		if !Proposable(k) {
			problems = append(problems, fmt.Sprintf("intent kind %q can never be granted to a strategy", k))
		}
	}
	if len(m.AllowedInstrumentIDs) == 0 && len(m.AllowedInstrumentKinds) == 0 {
		problems = append(problems, "at least one instrument id or instrument kind must be allowed")
	}
	if len(m.AllowedInstrumentIDs) > MaxManifestInstruments {
		problems = append(problems, "too many instruments")
	}
	for _, id := range m.AllowedInstrumentIDs {
		if !nodalsdk.ValidID(id) {
			problems = append(problems, fmt.Sprintf("instrument %q is not a canonical lowercase uuid", id))
		}
	}
	for _, k := range m.AllowedInstrumentKinds {
		if !instruments.InstrumentType(k).Valid() {
			problems = append(problems, fmt.Sprintf("instrument kind %q is not declared", k))
		}
	}
	max, err := nodalsdk.ParseDecimal(m.MaxNotionalPerIntent)
	if err != nil || max.Sign() <= 0 || max.String() != m.MaxNotionalPerIntent {
		problems = append(problems, "max_notional_per_intent must be a canonical positive decimal")
	}
	if m.MaxIntentsPerRun < 1 || m.MaxIntentsPerRun > MaxIntentsPerRunBound {
		problems = append(problems, fmt.Sprintf("max_intents_per_run must be in [1, %d]", MaxIntentsPerRunBound))
	}
	if m.MinIntervalSeconds < MinIntervalSecondsMin || m.MinIntervalSeconds > MinIntervalSecondsMax {
		problems = append(problems, fmt.Sprintf("min_interval_seconds must be in [%d, %d]", MinIntervalSecondsMin, MinIntervalSecondsMax))
	}
	if len(problems) > 0 {
		return errs.New(errs.CodeValidationFailed, "sandboxrt: invalid strategy manifest").WithField("problems", problems)
	}
	return nil
}

func equalKinds(a, b []econ.IntentKind) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// CanonicalBytes validates the manifest and returns its canonical JSON: the
// exact bytes whose sha256 is the manifest hash.
func (m Manifest) CanonicalBytes() ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	// Encoded from the canonical form so a nil list and an empty list are the
	// same manifest with the same hash.
	b, err := audit.CanonicalJSON(m.Canonicalize())
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeValidationFailed, "sandboxrt: manifest is not canonically encodable")
	}
	return b, nil
}

// Hash returns the sha256 of the canonical bytes.
func (m Manifest) Hash() ([]byte, error) {
	b, err := m.CanonicalBytes()
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(b)
	return sum[:], nil
}

// DecodeManifest parses stored canonical bytes. Anything that is not exactly
// the canonical encoding of a valid manifest is refused, so a manifest read
// back is always the manifest that was hashed.
func DecodeManifest(b []byte) (Manifest, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, errs.Wrap(err, errs.CodeValidationFailed, "sandboxrt: manifest is not valid JSON")
	}
	canon, err := m.CanonicalBytes()
	if err != nil {
		return Manifest{}, err
	}
	if !bytes.Equal(canon, b) {
		return Manifest{}, errs.New(errs.CodeValidationFailed, "sandboxrt: manifest bytes are not canonical")
	}
	return m, nil
}

// PriceSource names the price a notional was computed at.
type PriceSource string

// Price sources.
const (
	PriceLimit PriceSource = "LIMIT"
	PriceMark  PriceSource = "MARK"
)

// AcceptedProposal is a proposal the manifest permits.
type AcceptedProposal struct {
	Proposal
	// InstrumentKind is the instrument's kind in the snapshot.
	InstrumentKind string `json:"instrument_kind"`
	// Notional is quantity x price in simulated dollars, exact.
	Notional    string      `json:"notional"`
	PriceSource PriceSource `json:"price_source"`
}

// Allows reports whether the manifest allows an instrument.
func (m Manifest) allowsInstrument(id, kind string) bool {
	return containsString(m.AllowedInstrumentIDs, id) || containsString(m.AllowedInstrumentKinds, kind)
}

func (m Manifest) allowsKind(k econ.IntentKind) bool {
	for _, x := range m.AllowedIntentKinds {
		if x == k {
			return true
		}
	}
	return false
}

// Filter applies the manifest to validated proposals, in record order. A
// proposal beyond the manifest -- a kind or an instrument it does not allow,
// a notional above its cap, or one more than it permits per run -- is dropped
// and recorded as a violation. The notional is computed exactly at the limit
// price when there is one and at the snapshot mark otherwise; a notional that
// cannot be computed is refused, never assumed small.
func (m Manifest) Filter(proposals []Proposal, snap nodalsdk.Snapshot) ([]AcceptedProposal, []Violation) {
	var accepted []AcceptedProposal
	var violations []Violation
	max, maxErr := nodalsdk.ParseDecimal(m.MaxNotionalPerIntent)
	byID := map[string]nodalsdk.Instrument{}
	for _, ins := range snap.Instruments {
		byID[ins.ID] = ins
	}
	for _, p := range proposals {
		drop := func(code ViolationCode, detail string) {
			violations = append(violations, Violation{Code: code, Seq: p.Seq, Detail: detail})
		}
		if !m.allowsKind(p.Kind) {
			drop(ViolationManifestKind, "the manifest does not allow "+string(p.Kind))
			continue
		}
		ins, ok := byID[p.InstrumentID]
		if !ok {
			drop(ViolationInstrumentNotInSnapshot, "instrument "+p.InstrumentID+" was not in the input")
			continue
		}
		if !m.allowsInstrument(ins.ID, ins.Kind) {
			drop(ViolationManifestInstrument, "the manifest does not allow instrument "+ins.ID+" ("+ins.Kind+")")
			continue
		}
		price, source := ins.Mark, PriceMark
		if p.LimitPrice != "" {
			lp, err := nodalsdk.ParseDecimal(p.LimitPrice)
			if err != nil {
				drop(ViolationLimitPriceInvalid, "limit price unreadable")
				continue
			}
			price, source = lp, PriceLimit
		}
		notional, err := Notional(p.Quantity.String(), ins.BaseDecimals, price)
		if err != nil || maxErr != nil || notional.Cmp(max) > 0 {
			detail := "notional above the manifest's cap of " + m.MaxNotionalPerIntent
			if err != nil {
				detail = "notional cannot be computed exactly: " + truncate(err.Error(), 120)
			}
			drop(ViolationManifestNotional, detail)
			continue
		}
		if len(accepted) >= m.MaxIntentsPerRun {
			drop(ViolationManifestMaxIntents, fmt.Sprintf("the manifest allows %d intents per run", m.MaxIntentsPerRun))
			continue
		}
		accepted = append(accepted, AcceptedProposal{Proposal: p, InstrumentKind: ins.Kind, Notional: notional.String(), PriceSource: source})
	}
	return accepted, violations
}

// Notional is quantity (base units) x 10^-baseDecimals x price, exactly.
func Notional(quantity string, baseDecimals uint8, price nodalsdk.Decimal) (nodalsdk.Decimal, error) {
	q, err := nodalsdk.ParseUnits(quantity)
	if err != nil {
		return nodalsdk.Decimal{}, err
	}
	return positionValue(q, baseDecimals, price)
}
