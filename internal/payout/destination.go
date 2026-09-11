package payout

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/security"
)

// Payout destinations (goal §25).
//
// The rule this file exists to enforce: **Nodal never stores a bank account
// number, a card number, an IBAN, a routing number, a private key or a seed
// phrase.** What it stores is the PROVIDER'S token for a destination, plus
// enough display information for a person to recognise which of their accounts
// it is.
//
// That is not a preference. PART LXXXIV says not to duplicate identity data the
// provider already holds, and a bank account number in this table is a
// liability with no compensating benefit: it cannot be used to send money — only
// the provider can do that — and it can be stolen.
//
// So the input is validated to BE a token, not merely trusted to be one. A
// string that looks like a raw account number is refused with the reason,
// rather than stored and hoped about.

// AllDestinationStatuses returns every declared status in lifecycle order (a
// copy). It is compared against payout_destinations_status_check by
// test/integration/enums.
func AllDestinationStatuses() []DestinationStatus {
	return []DestinationStatus{
		DestinationUnverified, DestinationVerified, DestinationRejected, DestinationDisabled,
	}
}

// destinationTransitions is the explicit legal transition table (00763).
//
// A destination never returns from DISABLED or REJECTED. Re-adding one is a NEW
// row with its own creation time, which is what makes §25's cooldown on a
// changed destination a fact the database can state rather than a field
// somebody remembers to reset.
var destinationTransitions = map[DestinationStatus][]DestinationStatus{
	DestinationUnverified: {DestinationVerified, DestinationRejected, DestinationDisabled},
	DestinationVerified:   {DestinationDisabled, DestinationRejected},
	DestinationRejected:   {},
	DestinationDisabled:   {},
}

// CanTransitionDestination reports whether from → to is legal.
func CanTransitionDestination(from, to DestinationStatus) bool {
	for _, t := range destinationTransitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// DestinationTransitionsFrom returns the legal destinations of a status (a copy).
func DestinationTransitionsFrom(s DestinationStatus) []DestinationStatus {
	return append([]DestinationStatus(nil), destinationTransitions[s]...)
}

// DestinationChange describes who moved a destination's status and why.
type DestinationChange struct {
	ActorType     security.ActorType
	ActorID       string
	Reason        string
	ProviderEvent string
	CorrelationID string
	OccurredAt    time.Time
}

var (
	// bareNumber matches a stripped string that is NOTHING BUT digits, at the
	// length a financial account identifier has. A United States routing number
	// is nine, an account number eight to seventeen, a card number thirteen to
	// nineteen; no provider issues a token that is a bare run of digits,
	// because a token has to be distinguishable from the thing it replaces.
	//
	// It is deliberately a WHOLE-STRING match rather than a search for a long
	// digit run. A search would refuse a legitimate token that happened to
	// contain one -- a UUID's last group is twelve characters and is all digits
	// about once in three hundred -- and a validator that rejects valid input
	// at random is a validator somebody turns off.
	bareNumber = regexp.MustCompile(`^[0-9]{6,34}$`)
	// ibanLike matches the IBAN shape over the whole stripped string: two
	// letters, two check digits, then eleven to thirty alphanumerics.
	ibanLike = regexp.MustCompile(`^[A-Z]{2}[0-9]{2}[A-Z0-9]{11,30}$`)
	// cardRun finds a thirteen-to-nineteen digit run ANYWHERE, which is then
	// Luhn-checked. That combination is specific enough to be safe: a run that
	// long AND passing the checksum is a card number about one time in ten by
	// chance, and the two together are vanishingly unlikely in a token.
	cardRun = regexp.MustCompile(`[0-9]{13,19}`)
	// digitRun is the mask rule: a masked display shows the last few
	// characters, so seven consecutive digits in one is the whole number.
	digitRun = regexp.MustCompile(`[0-9]{7,}`)
	// maskShape is what a masked display may be: mask characters, digits,
	// letters, spaces and a few separators. It is bounded so a "display label"
	// cannot become a place to smuggle a document.
	maskShape = regexp.MustCompile(`^[\p{L}\p{N} \x{2022}*\x{00B7}\-_.()#/]{1,64}$`)
	// seedPhraseWords are the give-away words of a wallet secret. Section 25 is
	// explicit: never request a private key or a seed phrase. Refusing them on
	// input is what makes that true even when a person pastes one into the
	// wrong box.
	seedPhraseWords = []string{"seed phrase", "mnemonic", "private key", "secret key", "xprv", "-----begin"}
)

// luhn reports whether a run of digits satisfies the Luhn checksum, which every
// payment card number does and an arbitrary run of digits does one time in ten.
func luhn(digits string) bool {
	sum, double := 0, false
	for i := len(digits) - 1; i >= 0; i-- {
		d := int(digits[i] - '0')
		if double {
			if d *= 2; d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
	}
	return sum%10 == 0
}

// ValidateDestinationToken refuses an input that is a raw account number rather
// than a provider token.
//
// It is deliberately a refusal rather than a redaction. Redacting would mean
// the value reached this process, was inspected, and was written to a log line
// somewhere on the way; refusing at the boundary means it never becomes ours.
//
// The check is necessarily heuristic — a provider is free to issue a token that
// happens to be all digits — and it is calibrated in the safe direction: a
// provider whose token is refused is a configuration problem somebody notices
// in a minute, and a bank account number this system stores is a liability
// nobody notices for a year.
func ValidateDestinationToken(token string) error {
	t := strings.TrimSpace(token)
	switch {
	case t == "":
		return errs.New(errs.CodeValidationFailed,
			"a destination is identified by the provider's token for it")
	case len(t) > 255:
		return errs.New(errs.CodeValidationFailed, "that provider token is implausibly long")
	}
	lower := strings.ToLower(t)
	for _, word := range seedPhraseWords {
		if strings.Contains(lower, word) {
			return errs.New(errs.CodeValidationFailed,
				"Nodal never accepts a private key or a seed phrase; send the provider's token instead")
		}
	}
	stripped := strings.Map(func(r rune) rune {
		if r == ' ' || r == '-' {
			return -1
		}
		return r
	}, t)
	if ibanLike.MatchString(strings.ToUpper(stripped)) {
		return errs.New(errs.CodeValidationFailed,
			"that looks like an IBAN; Nodal stores the provider's token for a destination, never the account itself")
	}
	if bareNumber.MatchString(stripped) {
		return errs.New(errs.CodeValidationFailed,
			"that looks like an account, routing or card number; Nodal stores the provider's token for a destination, never the account itself").
			WithField("expected", "a provider-issued token or, on a sandbox tier, a sandbox handle")
	}
	for _, run := range cardRun.FindAllString(stripped, -1) {
		if luhn(run) {
			return errs.New(errs.CodeValidationFailed,
				"that contains what looks like a payment card number; Nodal stores the provider's token for a destination, never the card").
				WithField("expected", "a provider-issued token or, on a sandbox tier, a sandbox handle")
		}
	}
	return nil
}

// ValidateMaskedDisplay refuses a display string that carries more than a mask.
func ValidateMaskedDisplay(masked string) error {
	m := strings.TrimSpace(masked)
	if m == "" {
		return nil
	}
	if !maskShape.MatchString(m) {
		return errs.New(errs.CodeValidationFailed,
			"a masked display is a short recognisable label such as \"••••4242\"")
	}
	if digitRun.MatchString(strings.ReplaceAll(m, " ", "")) {
		return errs.New(errs.CodeValidationFailed,
			"a masked display shows the last few characters, never the whole number")
	}
	return nil
}

// TransitionDestination moves a destination's status by inserting the row that
// licenses the move. The trigger in migration 00763 writes the status and
// verified_at; nothing here does, and the application role has no privilege to.
//
// USER actors are permitted for exactly one edge — disabling one's own
// destination — because turning off a destination is a person's own decision
// and a control that only staff can exercise is a control nobody exercises.
// Everything else is the provider's decision or an operator's.
func (s *Service) TransitionDestination(ctx context.Context, tx pgx.Tx, id DestinationID, to DestinationStatus, ch DestinationChange) (Destination, error) {
	if !to.Valid() {
		return Destination{}, errs.Newf(errs.CodeValidationFailed, "unknown destination status %q", to)
	}
	if strings.TrimSpace(ch.ActorID) == "" || strings.TrimSpace(ch.Reason) == "" {
		return Destination{}, errs.New(errs.CodeValidationFailed,
			"a destination status change names the principal that made it and why")
	}
	if ch.OccurredAt.IsZero() {
		return Destination{}, errs.New(errs.CodeValidationFailed, "a destination status change needs a time")
	}
	switch ch.ActorType {
	case security.ActorSystem, security.ActorOperator:
	case security.ActorUser:
		if to != DestinationDisabled {
			return Destination{}, errs.New(errs.CodeForbidden,
				"a person may disable their own destination; whether it may receive value is the provider's decision")
		}
	default:
		return Destination{}, errs.Newf(errs.CodeForbidden,
			"%s actors do not change a payout destination", ch.ActorType)
	}

	current, err := s.lockDestination(ctx, tx, id)
	if err != nil {
		return Destination{}, err
	}
	if current.Status == to {
		return current, nil
	}
	if !CanTransitionDestination(current.Status, to) {
		return Destination{}, errs.Newf(errs.CodeInvalidStateTransition,
			"a payout destination cannot go %s -> %s", current.Status, to).
			WithField("destination_id", id.String()).
			WithField("from", string(current.Status)).
			WithField("to", string(to))
	}
	if _, err := tx.Exec(ctx, `INSERT INTO payout_destination_transitions
		(id, destination_id, from_status, to_status, actor_type, actor_id, reason, provider_event, correlation_id, occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''),NULLIF($9,''),$10)`,
		NewTransitionID(), id, string(current.Status), string(to),
		string(ch.ActorType), ch.ActorID, ch.Reason, ch.ProviderEvent, ch.CorrelationID, ch.OccurredAt.UTC()); err != nil {
		return Destination{}, mapError(err)
	}
	return s.Destination(ctx, tx, id)
}
