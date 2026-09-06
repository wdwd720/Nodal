package reality

import (
	"bytes"
	"encoding/json"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/nodal/controlplane/internal/chain"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
)

// Event types produced by the chain normalizer.
const (
	EventTypeWalletTransaction = "chain.wallet_transaction"
	// RawEventTypeWalletEvent is the raw_archive_objects.event_type of a
	// stream wallet event body (a chain.WalletEvent encoded as JSON).
	RawEventTypeWalletEvent = "wallet_event"
	// RawEventTypeReconnect is the raw event type of a stream reconnect
	// marker (archived so the discontinuity itself is evidence).
	RawEventTypeReconnect = "stream_reconnect"
)

// MaxWalletEventBytes bounds a wallet event body handed to the decoder.
const MaxWalletEventBytes = 4 << 20

// Limits on decoded identifiers (Solana signatures are 87–88 base58
// characters; pubkeys 32–44).
const (
	maxSignatureLength = 128
	maxWalletLength    = 64
	maxDeltas          = 4096
)

// DecodeWalletEvent parses the JSON body of an archived chain.WalletEvent.
// It never panics on any input and rejects bodies that would not be a
// usable observation.
func DecodeWalletEvent(body []byte) (chain.WalletEvent, error) {
	if len(body) == 0 {
		return chain.WalletEvent{}, errs.New(errs.CodeValidationFailed, "reality: empty wallet event body")
	}
	if len(body) > MaxWalletEventBytes {
		return chain.WalletEvent{}, errs.New(errs.CodeValidationFailed, "reality: wallet event body too large")
	}
	var ev chain.WalletEvent
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&ev); err != nil {
		return chain.WalletEvent{}, errs.Wrap(err, errs.CodeValidationFailed, "reality: malformed wallet event json")
	}
	if dec.More() {
		return chain.WalletEvent{}, errs.New(errs.CodeValidationFailed, "reality: trailing data after wallet event")
	}
	if err := validateWalletEvent(ev); err != nil {
		return chain.WalletEvent{}, err
	}
	return ev, nil
}

func validateWalletEvent(ev chain.WalletEvent) error {
	fields := map[string]any{}
	switch ev.Kind {
	case chain.EventTransaction:
		sig := ev.Observation.Signature
		switch {
		case sig == "":
			fields["signature"] = "required"
		case len(sig) > maxSignatureLength || !cleanIdentifier(sig):
			fields["signature"] = "invalid"
		}
		switch {
		case ev.Wallet == "":
			fields["wallet"] = "required"
		case len(ev.Wallet) > maxWalletLength || !cleanIdentifier(ev.Wallet):
			fields["wallet"] = "invalid"
		}
		if !ev.Observation.Found {
			fields["found"] = "a stream transaction must be an observed transaction"
		}
		if len(ev.Observation.TokenBalanceDeltas) > maxDeltas || len(ev.Observation.LamportDeltas) > maxDeltas {
			fields["deltas"] = "too many"
		}
		if ev.Observation.Commitment != "" && !ev.Observation.Commitment.Valid() {
			fields["commitment"] = "unknown"
		}
		if ev.Origin != "" && ev.Origin != chain.OriginStream && ev.Origin != chain.OriginBackfill {
			fields["origin"] = "unknown"
		}
	case chain.EventReconnect:
	default:
		fields["kind"] = "unknown"
	}
	if ev.ReceivedAt.IsZero() {
		fields["received_at"] = "required"
	}
	if len(fields) > 0 {
		return errs.New(errs.CodeValidationFailed, "reality: invalid wallet event").WithFields(fields)
	}
	return nil
}

func cleanIdentifier(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r < 0x21 || r == 0x7f {
			return false
		}
	}
	return true
}

// ChainNormalizer normalizes archived chain.WalletEvent bodies into
// chain.wallet_transaction events. It is pure given now.
type ChainNormalizer struct {
	source        string
	dedupStrategy string
	policy        AvailabilityPolicy
}

var _ Normalizer = ChainNormalizer{}

// NewChainNormalizer binds the normalizer to a data source code, its dedup
// strategy and the availability policy.
func NewChainNormalizer(source, dedupStrategy string, policy AvailabilityPolicy) (ChainNormalizer, error) {
	if source == "" {
		return ChainNormalizer{}, errs.New(errs.CodeValidationFailed, "reality: normalizer source is required")
	}
	if dedupStrategy != DedupProviderID && dedupStrategy != DedupCompositeHash {
		return ChainNormalizer{}, errs.New(errs.CodeValidationFailed, "reality: unknown dedup strategy").WithField("strategy", dedupStrategy)
	}
	if err := policy.Validate(); err != nil {
		return ChainNormalizer{}, err
	}
	return ChainNormalizer{source: source, dedupStrategy: dedupStrategy, policy: policy}, nil
}

// Source implements Normalizer.
func (n ChainNormalizer) Source() string { return n.source }

// WalletTransactionPayload is the typed payload of a chain.wallet_transaction
// event. Every number is a decimal string.
type WalletTransactionPayload struct {
	Signature     string             `json:"signature"`
	Wallet        string             `json:"wallet"`
	Slot          string             `json:"slot"`
	BlockTime     string             `json:"block_time,omitempty"` // provider clock, untrusted
	Commitment    string             `json:"commitment,omitempty"`
	Err           string             `json:"err,omitempty"`
	Version       string             `json:"version,omitempty"`
	Origin        string             `json:"origin,omitempty"`
	Fee           string             `json:"fee"`
	TokenDeltas   []TokenDeltaRecord `json:"token_deltas"`
	LamportDeltas []LamportRecord    `json:"lamport_deltas"`
	Observer      string             `json:"observer,omitempty"`
	RawRef        string             `json:"raw_ref,omitempty"`
}

// TokenDeltaRecord is one token account balance change (base units).
type TokenDeltaRecord struct {
	Owner        string `json:"owner"`
	Mint         string `json:"mint"`
	TokenAccount string `json:"token_account"`
	Program      string `json:"program,omitempty"`
	Pre          string `json:"pre"`
	Post         string `json:"post"`
	Delta        string `json:"delta"`
	Decimals     string `json:"decimals"`
}

// LamportRecord is one account's lamport change.
type LamportRecord struct {
	Account string `json:"account"`
	Pre     string `json:"pre"`
	Post    string `json:"post"`
	Delta   string `json:"delta"`
}

// Normalize implements Normalizer. A RECONNECT body yields no events.
func (n ChainNormalizer) Normalize(meta RawObjectMeta, body []byte, now time.Time) ([]NormalizedEvent, error) {
	ev, err := DecodeWalletEvent(body)
	if err != nil {
		return nil, err
	}
	if ev.Kind == chain.EventReconnect {
		return nil, nil
	}
	if meta.Timestamps.PlatformReceivedAt.IsZero() {
		return nil, errs.New(errs.CodeValidationFailed, "reality: raw object meta lacks platform_received_at")
	}
	obs := ev.Observation
	providerID := obs.Signature + "/" + ev.Wallet
	dedup, err := DedupID(n.dedupStrategy, providerID, n.source, EventTypeWalletTransaction, map[string]string{"signature": obs.Signature, "wallet": ev.Wallet})
	if err != nil {
		return nil, err
	}
	ts := Timestamps{
		ProviderPublishedAt: meta.Timestamps.ProviderPublishedAt,
		PlatformReceivedAt:  meta.Timestamps.PlatformReceivedAt,
	}
	if obs.BlockTime != nil && !obs.BlockTime.IsZero() {
		ts.SourceEventAt = obs.BlockTime.UTC()
	} else if !meta.Timestamps.SourceEventAt.IsZero() {
		ts.SourceEventAt = meta.Timestamps.SourceEventAt
	}
	if ts.ProviderPublishedAt.IsZero() && ev.ProviderPublishedAt != nil {
		ts.ProviderPublishedAt = ev.ProviderPublishedAt.UTC()
	}
	ts, err = n.policy.Apply(ts, now)
	if err != nil {
		return nil, err
	}
	payload, err := canonicalJSON(walletPayload(ev))
	if err != nil {
		return nil, err
	}
	seq := ev.Sequence
	if seq == nil && meta.Sequence != nil {
		seq = meta.Sequence
	}
	if seq == nil && obs.Slot > 0 {
		// Index within the slot is unknown: slot × 2^20 orders across slots
		// and ties within a slot, which the detector treats as equal.
		s := chain.SequenceOf(obs.Slot, 0)
		seq = &s
	}
	out := NormalizedEvent{
		EventID: NewEventID().String(), DedupID: dedup, SchemaVersion: NormalizedEventSchemaVersion,
		Source: n.source, EventType: EventTypeWalletTransaction, Sequence: seq,
		SourcePartition: meta.Partition, SourceOffset: meta.Offset, Timestamps: ts, Wallet: ev.Wallet,
		RawObjectID: meta.ObjectID, RawObjectHash: meta.Hash, Payload: payload,
	}
	if err := out.Validate(); err != nil {
		return nil, err
	}
	return []NormalizedEvent{out}, nil
}

func walletPayload(ev chain.WalletEvent) WalletTransactionPayload {
	obs := ev.Observation
	p := WalletTransactionPayload{
		Signature: obs.Signature, Wallet: ev.Wallet, Slot: strconv.FormatUint(obs.Slot, 10),
		Commitment: string(obs.Commitment), Err: obs.Err, Version: obs.Version, Origin: string(ev.Origin),
		Fee: obs.Fee.String(), Observer: obs.Source, RawRef: obs.RawRef,
		TokenDeltas: make([]TokenDeltaRecord, 0, len(obs.TokenBalanceDeltas)), LamportDeltas: make([]LamportRecord, 0, len(obs.LamportDeltas)),
	}
	if obs.BlockTime != nil && !obs.BlockTime.IsZero() {
		p.BlockTime = obs.BlockTime.UTC().Format(time.RFC3339Nano)
	}
	deltas := append([]chain.TokenDelta(nil), obs.TokenBalanceDeltas...)
	chain.SortTokenDeltas(deltas)
	for _, d := range deltas {
		p.TokenDeltas = append(p.TokenDeltas, TokenDeltaRecord{
			Owner: d.Owner, Mint: d.Mint, TokenAccount: d.TokenAccount, Program: d.Program,
			Pre: d.Pre.String(), Post: d.Post.String(), Delta: d.Delta().String(), Decimals: strconv.Itoa(int(d.Decimals)),
		})
	}
	lamports := append([]chain.LamportDelta(nil), obs.LamportDeltas...)
	chain.SortLamportDeltas(lamports)
	for _, d := range lamports {
		p.LamportDeltas = append(p.LamportDeltas, LamportRecord{Account: d.Account, Pre: d.Pre.String(), Post: d.Post.String(), Delta: d.Delta().String()})
	}
	return p
}

// canonicalJSON renders v as JSON with sorted object keys and no HTML
// escaping; numbers are preserved textually.
func canonicalJSON(v any) (json.RawMessage, error) {
	first, err := json.Marshal(v)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "reality: marshal payload")
	}
	var generic any
	dec := json.NewDecoder(bytes.NewReader(first))
	dec.UseNumber()
	if err := dec.Decode(&generic); err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "reality: canonicalise payload")
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(generic); err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "reality: canonicalise payload")
	}
	return json.RawMessage(bytes.TrimRight(buf.Bytes(), "\n")), nil
}

// Validate checks the envelope contract.
func (e NormalizedEvent) Validate() error {
	fields := map[string]any{}
	if _, err := ParseEventID(e.EventID); err != nil {
		fields["event_id"] = "must be a canonical uuidv7"
	}
	if err := validateDedupID(e.DedupID); err != nil {
		fields["dedup_id"] = err.Error()
	}
	if e.SchemaVersion < 1 {
		fields["schema_version"] = "must be >= 1"
	}
	if e.Source == "" {
		fields["source"] = "required"
	}
	if e.EventType == "" {
		fields["event_type"] = "required"
	}
	if err := e.Timestamps.Validate(); err != nil {
		fields["timestamps"] = err.Error()
	}
	if e.RawObjectID == "" {
		fields["raw_object_id"] = "required"
	}
	if len(e.RawObjectHash) != 32 {
		fields["raw_object_hash"] = "must be 32 bytes"
	}
	if len(e.Payload) == 0 || !json.Valid(e.Payload) || e.Payload[0] != '{' {
		fields["payload"] = "must be a json object"
	}
	if len(fields) > 0 {
		return errs.New(errs.CodeValidationFailed, "reality: invalid normalized event").WithFields(fields)
	}
	return nil
}

// ParseEventID parses a normalized event id (canonical UUIDv7 form only).
func ParseEventID(s string) (EventID, error) {
	eid, err := id.Parse[eventKind](s)
	if err != nil || eid.IsZero() {
		return EventID{}, errs.New(errs.CodeValidationFailed, "reality: invalid event id")
	}
	return eid, nil
}

// Encode renders the event as the JSON published on the bus.
func (e NormalizedEvent) Encode() ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(e); err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "reality: encode normalized event")
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// DecodeNormalizedEvent parses and validates a bus message value. It never
// panics on any input.
func DecodeNormalizedEvent(value []byte) (NormalizedEvent, error) {
	if len(value) > MaxWalletEventBytes {
		return NormalizedEvent{}, errs.New(errs.CodeValidationFailed, "reality: normalized event too large")
	}
	var e NormalizedEvent
	dec := json.NewDecoder(bytes.NewReader(value))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&e); err != nil {
		return NormalizedEvent{}, errs.Wrap(err, errs.CodeValidationFailed, "reality: malformed normalized event")
	}
	if dec.More() {
		return NormalizedEvent{}, errs.New(errs.CodeValidationFailed, "reality: trailing data after normalized event")
	}
	e.Timestamps = e.Timestamps.UTC()
	if err := e.Validate(); err != nil {
		return NormalizedEvent{}, err
	}
	return e, nil
}
