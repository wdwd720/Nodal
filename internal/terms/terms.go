// Package terms is the registry of the legal documents Nodal asks a user to
// accept, and the only place their versions are declared.
//
// Goal PART 48 asks for terms, privacy, risk, Credits and withdrawal documents,
// forbids fabricating final legal copy, requires documents needing counsel
// review to be marked as such, and requires the copy to be replaceable without a
// code rewrite. This package does all four: the text is Markdown embedded from
// documents/, the version is a constant beside it, and CounselReviewRequired is
// a field rather than a comment.
//
// It is a registry in CODE and not configuration on purpose. A deployment that
// could redefine which documents exist, or claim a version it does not serve,
// could obtain an acceptance of something nobody wrote. The bytes a user
// accepted are hashed into the acceptance record, so the document that was
// shown is identifiable from the record alone.
//
// This package has no dependencies beyond the standard library and holds no
// state: it is a lookup over compiled-in bytes.
package terms

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
)

//go:embed documents/*.md
var documents embed.FS

// DocumentID identifies a legal document. The set is closed: a document the
// database CHECK does not know cannot be accepted, and test/integration/enums
// keeps the two lists identical.
type DocumentID string

// The documents Nodal serves.
const (
	TermsOfService       DocumentID = "TERMS_OF_SERVICE"
	PrivacyPolicy        DocumentID = "PRIVACY_POLICY"
	RiskDisclosure       DocumentID = "RISK_DISCLOSURE"
	CreditsTerms         DocumentID = "CREDITS_TERMS"
	WithdrawalDisclosure DocumentID = "WITHDRAWAL_DISCLOSURE"
)

// Requirement says when a document must have been accepted.
//
// It is not a boolean because "required" is not one question. The withdrawal
// disclosure is required, but asking for it at signup would be exactly the
// "frontloaded KYC" the goal's onboarding section forbids: it applies at the
// moment somebody asks to take value out, and not before.
type Requirement string

// When a document is required.
const (
	// AtOnboarding must be accepted before onboarding is complete.
	AtOnboarding Requirement = "ONBOARDING"
	// BeforeWithdrawal must be accepted before a conversion or payout request.
	// Nothing in this package enforces that; the withdrawal surface asks.
	BeforeWithdrawal Requirement = "WITHDRAWAL"
)

// Document is one legal document at one version.
type Document struct {
	ID      DocumentID
	Version string
	Title   string
	// Body is the exact Markdown a user is shown. ContentHash is sha256 over
	// these bytes, and is what an acceptance records.
	Body        string
	ContentHash string
	// Requirement says when acceptance is required.
	Requirement Requirement
	// CounselReviewRequired marks a document that has not been reviewed by a
	// lawyer. Every document in this registry is currently marked, and the API
	// reports the flag so no surface can present a draft as settled.
	CounselReviewRequired bool
}

// Version is the version of every document in this registry.
//
// One version for all of them, bumped together, because the alternative --
// five independently drifting version strings -- buys nothing here and makes
// "which set of documents did this person accept" a five-part question. The
// format is date.revision and the database CHECK pins it.
const Version = "2026-09-10.1"

type source struct {
	id          DocumentID
	file        string
	title       string
	requirement Requirement
}

// sources is the declaration. Adding a document means adding a line here, a
// file beside it, and the value to the database CHECK; the enum test refuses
// the third being forgotten.
var sources = []source{
	{TermsOfService, "documents/terms_of_service.md", "Terms of Service", AtOnboarding},
	{PrivacyPolicy, "documents/privacy_policy.md", "Privacy Notice", AtOnboarding},
	{RiskDisclosure, "documents/risk_disclosure.md", "Risk Disclosure", AtOnboarding},
	{CreditsTerms, "documents/credits_terms.md", "Credits Terms", AtOnboarding},
	{WithdrawalDisclosure, "documents/withdrawal_disclosure.md", "Withdrawal and Verification Disclosure", BeforeWithdrawal},
}

var (
	once    sync.Once
	loaded  []Document
	byID    map[DocumentID]Document
	loadErr error
)

func load() {
	once.Do(func() {
		byID = make(map[DocumentID]Document, len(sources))
		for _, s := range sources {
			raw, err := documents.ReadFile(s.file)
			if err != nil {
				loadErr = fmt.Errorf("terms: %s: %w", s.file, err)
				return
			}
			body := strings.ReplaceAll(string(raw), "\r\n", "\n")
			sum := sha256.Sum256([]byte(body))
			d := Document{
				ID: s.id, Version: Version, Title: s.title, Body: body,
				ContentHash: hex.EncodeToString(sum[:]), Requirement: s.requirement,
				// Every draft in this registry says so on its first line, and
				// this field is that statement in a form a surface can read.
				CounselReviewRequired: true,
			}
			loaded = append(loaded, d)
			byID[s.id] = d
		}
	})
}

// Current returns every document, in declaration order.
func Current() ([]Document, error) {
	load()
	if loadErr != nil {
		return nil, loadErr
	}
	return append([]Document(nil), loaded...), nil
}

// MustCurrent is Current for a caller that cannot proceed without the registry
// -- which is every caller, since the bytes are compiled in and a failure here
// means the binary is malformed.
func MustCurrent() []Document {
	d, err := Current()
	if err != nil {
		panic(err)
	}
	return d
}

// Get returns one document.
func Get(id DocumentID) (Document, bool) {
	load()
	if loadErr != nil {
		return Document{}, false
	}
	d, ok := byID[id]
	return d, ok
}

// RequiredAt returns the documents that must be accepted at the given point.
func RequiredAt(r Requirement) []Document {
	out := make([]Document, 0, len(sources))
	for _, d := range MustCurrent() {
		if d.Requirement == r {
			out = append(out, d)
		}
	}
	return out
}

// AllDocumentIDs returns every declared id, sorted. It is the Go half of the
// pair test/integration/enums holds against terms_acceptances_document_id_check.
func AllDocumentIDs() []DocumentID {
	out := make([]DocumentID, 0, len(sources))
	for _, s := range sources {
		out = append(out, s.id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// AllRequirements returns every declared requirement, sorted.
func AllRequirements() []Requirement {
	return []Requirement{AtOnboarding, BeforeWithdrawal}
}

// Valid reports whether id is declared.
func (d DocumentID) Valid() bool {
	_, ok := Get(d)
	return ok
}
