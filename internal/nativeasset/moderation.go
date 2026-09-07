package nativeasset

import (
	"net/url"
	"sort"
	"strings"
	"unicode"

	"github.com/nodal/controlplane/internal/errs"
)

// Screening of user-supplied asset content (PARTS XIII, LI).
//
// Everything here is deterministic, local and explainable: the same name gets
// the same verdict on every machine, forever, and every finding names the rule
// that produced it. That matters more than sophistication, because this is the
// layer that has to be auditable when someone asks why their asset was
// refused.
//
// It is a floor. Illegal-content classification, trademark disputes and
// coordinated-impersonation detection need judgement and external data;
// Screener is where those attach, and their verdicts are recorded separately
// rather than merged into the local one.

// Limits on user-supplied content.
const (
	MinNameLen        = 2
	MaxNameLen        = 64
	MinSymbolLen      = 2
	MaxSymbolLen      = 10
	MaxDescriptionLen = 2_000
	MaxImageURLLen    = 512
	MaxMetadataKeys   = 32
	MaxMetadataBytes  = 8_192
)

// FindingCode names a specific screening rule.
type FindingCode string

// Screening findings.
const (
	FindingNameLength          FindingCode = "NAME_LENGTH"
	FindingNameCharacters      FindingCode = "NAME_CHARACTERS"
	FindingNameBidiControl     FindingCode = "NAME_BIDI_CONTROL"
	FindingSymbolFormat        FindingCode = "SYMBOL_FORMAT"
	FindingSymbolReserved      FindingCode = "SYMBOL_RESERVED"
	FindingSymbolTaken         FindingCode = "SYMBOL_TAKEN"
	FindingNameConfusable      FindingCode = "NAME_CONFUSABLE_WITH_EXISTING"
	FindingImpersonation       FindingCode = "IMPERSONATION_OF_PROTECTED_NAME"
	FindingDescriptionLength   FindingCode = "DESCRIPTION_LENGTH"
	FindingDescriptionControl  FindingCode = "DESCRIPTION_CONTROL_CHARACTERS"
	FindingReturnClaim         FindingCode = "PROHIBITED_RETURN_CLAIM"
	FindingURLScheme           FindingCode = "URL_SCHEME"
	FindingURLLength           FindingCode = "URL_LENGTH"
	FindingURLMalformed        FindingCode = "URL_MALFORMED"
	FindingMetadataTooLarge    FindingCode = "METADATA_TOO_LARGE"
	FindingMetadataTooManyKeys FindingCode = "METADATA_TOO_MANY_KEYS"
)

// Severity says what a finding does to the verdict.
type Severity string

// Severities.
const (
	// SeverityBlock refuses creation outright.
	SeverityBlock Severity = "BLOCK"
	// SeverityFlag allows creation and asks for human review.
	SeverityFlag Severity = "FLAG"
)

// Finding is one screening result.
type Finding struct {
	Code     FindingCode
	Severity Severity
	Detail   string
	// Field names the user-supplied field at fault, so a UI can point at it.
	Field string
}

// Verdict is the outcome of screening.
type Verdict struct {
	State    ModerationState
	Findings []Finding
}

// Blocked reports whether any finding refuses creation.
func (v Verdict) Blocked() bool { return v.State == ModerationRejected }

// Explain renders the findings as operator- and user-facing text.
func (v Verdict) Explain() string {
	if len(v.Findings) == 0 {
		return "no findings"
	}
	parts := make([]string, 0, len(v.Findings))
	for _, f := range v.Findings {
		parts = append(parts, string(f.Severity)+" "+string(f.Code)+": "+f.Detail)
	}
	return strings.Join(parts, "; ")
}

// reservedSymbols are symbols a user may not take. They are reserved because
// taking one would let an internal asset masquerade as the unit of account, a
// major external asset, or a fiat currency in a UI that shows both.
var reservedSymbols = map[string]bool{
	"CREDIT": true, "CREDITS": true, "NODAL": true, "USD": true, "USDC": true,
	"USDT": true, "EUR": true, "GBP": true, "JPY": true, "SOL": true, "BTC": true,
	"ETH": true, "XRP": true, "ADA": true, "DOGE": true, "CASH": true, "MONEY": true,
	"NULL": true, "NONE": true, "TEST": true, "ADMIN": true, "SYSTEM": true,
}

// protectedNames are names an internal asset may not adopt, because doing so
// would claim to be the platform, its unit of account, or a real institution.
// The list is short and specific on purpose: a long list of brand names would
// be a trademark judgement this code is not entitled to make.
var protectedNames = []string{
	"nodal", "nodal credit", "nodal credits", "nodal exchange",
	"official", "verified", "support", "admin", "administrator", "moderator",
	"customer support", "helpdesk",
}

// returnClaimPhrases are phrases that assert a financial return. PART XIII and
// PART LIII both forbid an internal speculative asset being marketed as an
// investment with an expected outcome.
var returnClaimPhrases = []string{
	"guaranteed profit", "guaranteed return", "guaranteed returns",
	"risk free", "risk-free", "no risk", "cannot lose", "can't lose",
	"guaranteed gains", "assured return", "principal protected",
	"guaranteed 100x", "guaranteed moon",
}

// allowedImageSchemes is the complete set of URL schemes an image may use.
// data: is excluded deliberately: it is a content channel wearing a URL's
// clothes, and the size and type of what it carries cannot be checked here.
var allowedImageSchemes = map[string]bool{"https": true}

// ScreenInput is everything the local screener needs.
type ScreenInput struct {
	Name        string
	Symbol      string
	Description string
	ImageURL    string
	Metadata    map[string]any

	// ExistingNames and ExistingSymbols are the live native assets to compare
	// against for confusability and collision. The caller supplies them so
	// this stays a pure function.
	ExistingNames   []string
	ExistingSymbols []string
}

// Screen applies every local rule and returns a verdict.
//
// Findings are returned sorted by code so the same input produces the same
// list in the same order — a screening record that reorders itself between
// runs cannot be diffed, and this one is stored.
func Screen(in ScreenInput) Verdict {
	var findings []Finding
	add := func(c FindingCode, s Severity, field, detail string) {
		findings = append(findings, Finding{Code: c, Severity: s, Field: field, Detail: detail})
	}

	name := strings.TrimSpace(in.Name)
	symbol := strings.ToUpper(strings.TrimSpace(in.Symbol))

	// --- name ---
	if n := len([]rune(name)); n < MinNameLen || n > MaxNameLen {
		add(FindingNameLength, SeverityBlock, "name",
			"a name must be between 2 and 64 characters")
	}
	if hasControlOrFormat(name) {
		add(FindingNameCharacters, SeverityBlock, "name",
			"a name may not contain control or formatting characters")
	}
	if hasBidiOverride(name) {
		add(FindingNameBidiControl, SeverityBlock, "name",
			"a name may not contain bidirectional override characters, which can make it render as something else entirely")
	}

	// --- symbol ---
	if !validSymbol(symbol) {
		add(FindingSymbolFormat, SeverityBlock, "symbol",
			"a symbol must be 2 to 10 characters, A-Z and 0-9 only, and must start with a letter")
	} else if reservedSymbols[symbol] {
		add(FindingSymbolReserved, SeverityBlock, "symbol",
			"the symbol "+symbol+" is reserved and cannot be used by a user-created asset")
	}
	for _, existing := range in.ExistingSymbols {
		if strings.EqualFold(strings.TrimSpace(existing), symbol) {
			add(FindingSymbolTaken, SeverityBlock, "symbol",
				"the symbol "+symbol+" is already in use by another asset")
			break
		}
	}

	// --- impersonation ---
	normName := normalizeConfusables(name)
	for _, p := range protectedNames {
		if normName == normalizeConfusables(p) {
			add(FindingImpersonation, SeverityBlock, "name",
				"the name is reserved to the platform or implies an official role")
			break
		}
	}
	for _, existing := range in.ExistingNames {
		if strings.EqualFold(strings.TrimSpace(existing), name) {
			continue // an exact duplicate name is allowed; the symbol is the identity
		}
		if normalizeConfusables(existing) == normName && normName != "" {
			add(FindingNameConfusable, SeverityFlag, "name",
				"the name is visually confusable with the existing asset "+strings.TrimSpace(existing))
			break
		}
	}

	// --- description ---
	if len(in.Description) > MaxDescriptionLen {
		add(FindingDescriptionLength, SeverityBlock, "description",
			"a description may be at most 2,000 characters")
	}
	if hasControlOrFormat(in.Description) {
		add(FindingDescriptionControl, SeverityBlock, "description",
			"a description may not contain control or formatting characters")
	}
	lowerDesc := strings.ToLower(in.Description)
	for _, phrase := range returnClaimPhrases {
		if strings.Contains(lowerDesc, phrase) {
			add(FindingReturnClaim, SeverityBlock, "description",
				"a description may not promise a return; this is a speculative internal asset, not an investment")
			break
		}
	}

	// --- image ---
	if in.ImageURL != "" {
		switch {
		case len(in.ImageURL) > MaxImageURLLen:
			add(FindingURLLength, SeverityBlock, "image_url", "the image URL is too long")
		default:
			u, err := url.Parse(in.ImageURL)
			if err != nil || u.Host == "" {
				add(FindingURLMalformed, SeverityBlock, "image_url", "the image URL could not be parsed")
			} else if !allowedImageSchemes[strings.ToLower(u.Scheme)] {
				add(FindingURLScheme, SeverityBlock, "image_url",
					"an image URL must use https; "+u.Scheme+" is not permitted")
			}
		}
	}

	// --- metadata ---
	if len(in.Metadata) > MaxMetadataKeys {
		add(FindingMetadataTooManyKeys, SeverityBlock, "metadata",
			"metadata may have at most 32 keys")
	}
	if metadataSize(in.Metadata) > MaxMetadataBytes {
		add(FindingMetadataTooLarge, SeverityBlock, "metadata",
			"metadata may be at most 8 KB")
	}

	sort.SliceStable(findings, func(i, j int) bool { return findings[i].Code < findings[j].Code })

	state := ModerationApproved
	for _, f := range findings {
		if f.Severity == SeverityBlock {
			state = ModerationRejected
			break
		}
		state = ModerationFlagged
	}
	return Verdict{State: state, Findings: findings}
}

// validSymbol enforces the symbol shape: uppercase alphanumeric, starting with
// a letter. Starting with a letter keeps a symbol from being read as a number
// in a table of prices.
func validSymbol(s string) bool {
	if len(s) < MinSymbolLen || len(s) > MaxSymbolLen {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func hasControlOrFormat(s string) bool {
	for _, r := range s {
		if r == '\n' || r == '\t' {
			continue
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return true
		}
	}
	return false
}

// hasBidiOverride looks for the characters that let a name render right to
// left, which is how "GOOD" becomes something else on screen while comparing
// equal to nothing suspicious.
func hasBidiOverride(s string) bool {
	for _, r := range s {
		switch r {
		case '‪', '‫', '‬', '‭', '‮',
			'⁦', '⁧', '⁨', '⁩', '‏', '‎':
			return true
		}
	}
	return false
}

// confusables maps characters that render alike to a single representative.
// It is small and covers the substitutions actually used to impersonate a
// ticker or a brand: digits for letters, and the Cyrillic and Greek letters
// that are visually identical to Latin ones in most fonts.
var confusables = map[rune]rune{
	'0': 'o', '1': 'l', '3': 'e', '4': 'a', '5': 's', '7': 't', '8': 'b',
	'а': 'a', 'е': 'e', 'о': 'o', 'р': 'p', 'с': 'c', 'х': 'x', 'у': 'y',
	'і': 'i', 'ѕ': 's', 'ԁ': 'd', 'һ': 'h', 'ӏ': 'l', 'ν': 'v', 'κ': 'k',
	'ο': 'o', 'ρ': 'p', 'τ': 't', 'α': 'a', 'ε': 'e', 'ι': 'i',
}

// normalizeConfusables folds a string to a comparison form: lowercase, with
// confusable characters mapped together and everything that is not a letter or
// digit removed. "N0dal", "Nodal" and "N o d a l" all fold to "nodal".
func normalizeConfusables(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if rep, ok := confusables[r]; ok {
			b.WriteRune(rep)
			continue
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// metadataSize is a cheap upper bound on the serialised size of the metadata
// map. It walks the structure rather than marshalling, so a hostile map cannot
// make the check itself expensive.
func metadataSize(m map[string]any) int {
	total := 0
	var walk func(v any, depth int)
	walk = func(v any, depth int) {
		if depth > 8 || total > MaxMetadataBytes {
			total = MaxMetadataBytes + 1
			return
		}
		switch t := v.(type) {
		case string:
			total += len(t)
		case map[string]any:
			for k, vv := range t {
				total += len(k)
				walk(vv, depth+1)
			}
		case []any:
			for _, vv := range t {
				walk(vv, depth+1)
			}
		default:
			total += 8
		}
	}
	for k, v := range m {
		total += len(k)
		walk(v, 1)
	}
	return total
}

// Screener is an optional external classifier.
//
// Its verdict is recorded alongside the local one and never replaces it: a
// provider that returns "approved" cannot un-reject something the local rules
// blocked, because the local rules encode commitments the platform has made
// rather than opinions it holds.
type Screener interface {
	Screen(name, symbol, description string) (ModerationState, string, error)
}

// Combine merges a local verdict with an external one, taking the stricter.
func Combine(local Verdict, external ModerationState, externalNote string) Verdict {
	if !external.Valid() {
		return local
	}
	out := local
	if external == ModerationRejected {
		out.State = ModerationRejected
		out.Findings = append(out.Findings, Finding{
			Code: "EXTERNAL_SCREENING", Severity: SeverityBlock,
			Field: "content", Detail: externalNote,
		})
		return out
	}
	if external == ModerationFlagged && out.State == ModerationApproved {
		out.State = ModerationFlagged
		out.Findings = append(out.Findings, Finding{
			Code: "EXTERNAL_SCREENING", Severity: SeverityFlag,
			Field: "content", Detail: externalNote,
		})
	}
	return out
}

// ErrRejected is returned when screening blocks creation.
func ErrRejected(v Verdict) error {
	return errs.New(errs.CodeValidationFailed, "this asset cannot be created: "+v.Explain()).
		WithField("moderation_state", string(v.State)).
		WithField("findings", findingCodes(v))
}

func findingCodes(v Verdict) []string {
	out := make([]string, 0, len(v.Findings))
	for _, f := range v.Findings {
		out = append(out, string(f.Code))
	}
	return out
}
