package predmarket

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nodal/controlplane/internal/errs"
)

// An oracle report is what a prediction oracle asserts about one market: the
// outcome it observed, where and when it observed it, the evidence it saw, and
// an Ed25519 signature over all of that. A report is accepted only after it
// verifies against the public key the market registered at creation, so an
// oracle can resolve only the markets that named it, and a report signed for
// one market can never resolve another: the market id is inside the signed
// bytes.

// PredictionOracleProvider reports the outcome of a market. Report is an
// external call: callers fetch a report outside any database transaction and
// hand it to Service.Resolve, which verifies it.
type PredictionOracleProvider interface {
	Report(ctx context.Context, marketID MarketID) (OracleReport, error)
}

// Report limits.
const (
	// MaxEvidenceBytes bounds the evidence blob a report carries.
	MaxEvidenceBytes = 16 << 10
	// maxEncodedReportBytes bounds an encoded report before it is parsed.
	maxEncodedReportBytes = 64 << 10
	maxSourceLen          = 200
	reportDomain          = "nodal.predmarket.oracle-report.v1"
)

var (
	marketSymbolPattern  = regexp.MustCompile(`^[A-Z0-9][A-Z0-9_-]{0,31}$`)
	outcomeSymbolPattern = regexp.MustCompile(`^[A-Z0-9][A-Z0-9_-]{0,23}$`)
)

// OracleReport is one signed oracle observation.
type OracleReport struct {
	MarketID   MarketID
	Outcome    string
	Source     string
	ObservedAt time.Time
	// Evidence is the source document the oracle observed, opaque here. Its
	// sha256 is inside the signed bytes, so it cannot be swapped.
	Evidence  []byte
	Signature []byte
}

// validateShape checks everything about a report except its signature.
func (r OracleReport) validateShape() error {
	var problems []string
	if r.MarketID.IsZero() {
		problems = append(problems, "market_id required")
	}
	if !outcomeSymbolPattern.MatchString(r.Outcome) {
		problems = append(problems, "outcome must be an outcome symbol")
	}
	if !validText(r.Source, maxSourceLen) {
		problems = append(problems, "source must be 1..200 printable characters without surrounding whitespace")
	}
	if r.ObservedAt.IsZero() {
		problems = append(problems, "observed_at required")
	} else if !r.ObservedAt.Equal(r.ObservedAt.Truncate(time.Microsecond)) {
		problems = append(problems, "observed_at carries more than microsecond precision, which the archive cannot keep")
	}
	if len(r.Evidence) == 0 || len(r.Evidence) > MaxEvidenceBytes {
		problems = append(problems, "evidence must be 1..16384 bytes")
	}
	if len(r.Signature) != ed25519.SignatureSize {
		problems = append(problems, "signature must be 64 bytes")
	}
	if len(problems) > 0 {
		return errs.New(errs.CodeValidationFailed, "predmarket: malformed oracle report").WithField("problems", problems)
	}
	return nil
}

// validText reports whether s is non-empty, at most max bytes, valid UTF-8,
// trimmed, and free of control characters.
func validText(s string, max int) bool {
	if s == "" || len(s) > max || !utf8.ValidString(s) || strings.TrimSpace(s) != s {
		return false
	}
	for _, c := range s {
		if c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}

// SigningBytes is the exact byte string the oracle signs. Every field is
// unambiguous: none may contain a newline, the time is RFC 3339 in UTC, and
// the evidence enters as its sha256.
func (r OracleReport) SigningBytes() []byte {
	sum := sha256.Sum256(r.Evidence)
	var b bytes.Buffer
	b.WriteString(reportDomain)
	b.WriteByte('\n')
	b.WriteString(r.MarketID.String())
	b.WriteByte('\n')
	b.WriteString(r.Outcome)
	b.WriteByte('\n')
	b.WriteString(r.Source)
	b.WriteByte('\n')
	b.WriteString(r.ObservedAt.UTC().Format(time.RFC3339Nano))
	b.WriteByte('\n')
	b.WriteString(hex.EncodeToString(sum[:]))
	return b.Bytes()
}

// Hash identifies a report: sha256 over its signed bytes and its signature.
// Ed25519 signatures are deterministic, so the same observation signed by the
// same key always has the same hash; that is what makes replay a no-op.
func (r OracleReport) Hash() []byte {
	h := sha256.New()
	h.Write(r.SigningBytes())
	h.Write(r.Signature)
	return h.Sum(nil)
}

// VerifyReport checks a report's shape and its signature against pub.
func VerifyReport(pub ed25519.PublicKey, r OracleReport) error {
	if err := r.validateShape(); err != nil {
		return err
	}
	if len(pub) != ed25519.PublicKeySize {
		return errs.New(errs.CodeInternal, "predmarket: the market's oracle key is not an Ed25519 public key")
	}
	if !ed25519.Verify(pub, r.SigningBytes(), r.Signature) {
		return errs.New(errs.CodeForbidden, "predmarket: the oracle report's signature does not verify against the market's oracle key").
			WithField("market_id", r.MarketID.String()).
			WithField("oracle_refusal", "BAD_SIGNATURE")
	}
	return nil
}

// reportWire is the JSON form of a report. Bytes are standard base64; the
// time is RFC 3339 with up to microsecond precision.
type reportWire struct {
	MarketID   string `json:"market_id"`
	Outcome    string `json:"outcome"`
	Source     string `json:"source"`
	ObservedAt string `json:"observed_at"`
	Evidence   string `json:"evidence"`
	Signature  string `json:"signature"`
}

// EncodeOracleReport renders a report as JSON for transport.
func EncodeOracleReport(r OracleReport) ([]byte, error) {
	if err := r.validateShape(); err != nil {
		return nil, err
	}
	return json.Marshal(reportWire{
		MarketID: r.MarketID.String(), Outcome: r.Outcome, Source: r.Source,
		ObservedAt: r.ObservedAt.UTC().Format(time.RFC3339Nano),
		Evidence:   base64.StdEncoding.EncodeToString(r.Evidence),
		Signature:  base64.StdEncoding.EncodeToString(r.Signature),
	})
}

// DecodeOracleReport parses a report delivered as JSON (for example pushed by
// an oracle webhook). It is strict: one JSON object, no unknown or missing
// fields, canonical base64, and a well-formed report. It never verifies the
// signature -- only VerifyReport, with the market's key, can do that.
func DecodeOracleReport(b []byte) (OracleReport, error) {
	bad := func(detail string) (OracleReport, error) {
		return OracleReport{}, errs.New(errs.CodeValidationFailed, "predmarket: oracle report: "+detail)
	}
	if len(b) == 0 || len(b) > maxEncodedReportBytes {
		return bad("empty or larger than 65536 bytes")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var w reportWire
	if err := dec.Decode(&w); err != nil {
		return bad("not a report object")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return bad("trailing data after the report")
	}
	if w.MarketID == "" || w.Outcome == "" || w.Source == "" || w.ObservedAt == "" || w.Evidence == "" || w.Signature == "" {
		return bad("every field is required")
	}
	mid, err := ParseMarketID(w.MarketID)
	if err != nil {
		return bad("market_id is not a canonical uuid")
	}
	at, err := time.Parse(time.RFC3339Nano, w.ObservedAt)
	if err != nil {
		return bad("observed_at is not RFC 3339")
	}
	ev, err := base64.StdEncoding.Strict().DecodeString(w.Evidence)
	if err != nil {
		return bad("evidence is not canonical base64")
	}
	sig, err := base64.StdEncoding.Strict().DecodeString(w.Signature)
	if err != nil {
		return bad("signature is not canonical base64")
	}
	r := OracleReport{MarketID: mid, Outcome: w.Outcome, Source: w.Source, ObservedAt: at.UTC(), Evidence: ev, Signature: sig}
	if err := r.validateShape(); err != nil {
		return OracleReport{}, err
	}
	return r, nil
}

// SandboxOracle is the deterministic sandbox oracle. Its Ed25519 key is
// derived from a seed, so a test or a sandbox deployment can rebuild the same
// oracle, and its reports -- the outcome it was told it observed, signed --
// are byte-for-byte reproducible. It holds no package-level state.
type SandboxOracle struct {
	source string
	key    ed25519.PrivateKey

	mu           sync.Mutex
	observations map[MarketID]sandboxObservation
}

type sandboxObservation struct {
	outcome    string
	observedAt time.Time
	evidence   []byte
}

var _ PredictionOracleProvider = (*SandboxOracle)(nil)

// NewSandboxOracle builds a sandbox oracle named source whose key derives
// from a 32-byte seed.
func NewSandboxOracle(source string, seed []byte) (*SandboxOracle, error) {
	if !validText(source, maxSourceLen) {
		return nil, errs.New(errs.CodeValidationFailed, "predmarket: an oracle source must be 1..200 printable characters")
	}
	if len(seed) != ed25519.SeedSize {
		return nil, errs.New(errs.CodeValidationFailed, "predmarket: a sandbox oracle seed is 32 bytes")
	}
	return &SandboxOracle{
		source:       source,
		key:          ed25519.NewKeyFromSeed(seed),
		observations: make(map[MarketID]sandboxObservation),
	}, nil
}

// Source returns the oracle's source name.
func (o *SandboxOracle) Source() string { return o.source }

// PublicKey returns the key a market registers to accept this oracle.
func (o *SandboxOracle) PublicKey() ed25519.PublicKey {
	pub, _ := o.key.Public().(ed25519.PublicKey)
	return append(ed25519.PublicKey(nil), pub...)
}

// Observe records what the sandbox oracle saw for a market. A later
// observation replaces an earlier one, which is how a sandbox models an
// oracle correcting itself.
func (o *SandboxOracle) Observe(marketID MarketID, outcome string, observedAt time.Time, evidence []byte) error {
	probe := OracleReport{
		MarketID: marketID, Outcome: outcome, Source: o.source, ObservedAt: observedAt.UTC(),
		Evidence: evidence, Signature: make([]byte, ed25519.SignatureSize),
	}
	if err := probe.validateShape(); err != nil {
		return err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.observations[marketID] = sandboxObservation{
		outcome: outcome, observedAt: observedAt.UTC(), evidence: append([]byte(nil), evidence...),
	}
	return nil
}

// Report implements PredictionOracleProvider: the signed report of the
// market's latest observation, or NOT_FOUND while the oracle has observed
// nothing for it.
func (o *SandboxOracle) Report(ctx context.Context, marketID MarketID) (OracleReport, error) {
	if err := ctx.Err(); err != nil {
		return OracleReport{}, err
	}
	o.mu.Lock()
	obs, ok := o.observations[marketID]
	o.mu.Unlock()
	if !ok {
		return OracleReport{}, errs.New(errs.CodeNotFound, "predmarket: the oracle has no observation for this market yet").
			WithField("market_id", marketID.String())
	}
	return o.Sign(OracleReport{
		MarketID: marketID, Outcome: obs.outcome, Source: o.source, ObservedAt: obs.observedAt,
		Evidence: append([]byte(nil), obs.evidence...),
	})
}

// Sign returns r with Source set to this oracle and a fresh signature by its
// key. The sandbox uses it for its own reports; tests use it to build the
// conflicting and misdirected reports the service must refuse.
func (o *SandboxOracle) Sign(r OracleReport) (OracleReport, error) {
	r.Source = o.source
	r.ObservedAt = r.ObservedAt.UTC()
	r.Signature = make([]byte, ed25519.SignatureSize)
	if err := r.validateShape(); err != nil {
		return OracleReport{}, err
	}
	r.Signature = ed25519.Sign(o.key, r.SigningBytes())
	return r, nil
}
