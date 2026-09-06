package ledger

import (
	"strings"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// Limits on a single posting. They bound the work of the deferred balance
// trigger (which rescans the transaction per entry) and keep seq in int32.
const (
	MaxEntries           = 1000
	MaxIdempotencyKeyLen = 512
)

// Validate checks the posting without touching the database: known kind,
// non-empty idempotency key and reference, effective time, correction rules,
// well-formed entries with positive quantities, and Σdebit == Σcredit for
// every asset. Structural problems fail with VALIDATION_FAILED; fewer than
// two entries or a per-asset imbalance fail with LEDGER_UNBALANCED.
func (p Posting) Validate() error { return validatePosting(p) }

// validatePosting is the application-level half of the balanced-per-asset
// invariant (FINANCIAL_MODEL §8); the deferred trigger is the other half.
func validatePosting(p Posting) error {
	if !p.Kind.Valid() {
		return errs.Newf(errs.CodeValidationFailed, "unknown journal kind %q", p.Kind).WithField("kind", string(p.Kind))
	}
	key := strings.TrimSpace(p.IdempotencyKey)
	if key == "" || key != p.IdempotencyKey {
		return errs.New(errs.CodeValidationFailed, "idempotency key is required and may not have surrounding whitespace")
	}
	if len(key) > MaxIdempotencyKeyLen {
		return errs.Newf(errs.CodeValidationFailed, "idempotency key exceeds %d bytes", MaxIdempotencyKeyLen)
	}
	if p.Reference.Type == "" || p.Reference.ID == "" {
		return errs.New(errs.CodeValidationFailed, "financial event reference type and id are required")
	}
	if p.EffectiveAt.IsZero() {
		return errs.New(errs.CodeValidationFailed, "effective_at is required")
	}
	if p.ReversalOf != nil && p.ReversalOf.IsZero() {
		return errs.New(errs.CodeValidationFailed, "reversal_of is set but empty")
	}
	if p.Kind.RequiresReversalOf() && p.ReversalOf == nil {
		return errs.Newf(errs.CodeValidationFailed, "%s transactions must set reversal_of", p.Kind)
	}
	if p.Kind.RequiresReasonCode() && p.ReasonCode() == "" {
		return errs.Newf(errs.CodeValidationFailed, "%s transactions must carry metadata.%s", p.Kind, MetadataReasonCode)
	}
	if len(p.Entries) < 2 {
		return errs.Newf(errs.CodeLedgerUnbalanced, "a journal transaction needs at least two entries, got %d", len(p.Entries)).
			WithField("entries", len(p.Entries))
	}
	if len(p.Entries) > MaxEntries {
		return errs.Newf(errs.CodeValidationFailed, "a journal transaction may carry at most %d entries", MaxEntries)
	}
	for i, e := range p.Entries {
		if err := e.Account.Validate(); err != nil {
			if ee, ok := errs.As(err); ok {
				return ee.WithField("entry", i)
			}
			return err
		}
		if !e.Side.Valid() {
			return errs.Newf(errs.CodeValidationFailed, "entry %d: unknown side %q", i, e.Side).WithField("entry", i)
		}
		if e.Quantity.Sign() <= 0 {
			return errs.Newf(errs.CodeValidationFailed, "entry %d: quantity must be positive, got %s", i, e.Quantity).WithField("entry", i)
		}
	}
	for _, im := range assetImbalances(p.Entries) {
		if !im.net.IsZero() {
			return errs.Newf(errs.CodeLedgerUnbalanced, "asset %s does not balance: debits %s, credits %s", im.asset, im.debits, im.credits).
				WithField("asset_id", im.asset.String()).
				WithField("debits", im.debits.String()).
				WithField("credits", im.credits.String())
		}
	}
	return nil
}

// assetImbalance is the per-asset totals of a set of entries.
type assetImbalance struct {
	asset   assets.AssetID
	debits  money.Quantity
	credits money.Quantity
	net     money.Quantity // debits − credits
}

// assetImbalances sums debits and credits per asset in first-seen order.
// It never balances across assets.
func assetImbalances(entries []Entry) []assetImbalance {
	index := make(map[assets.AssetID]int)
	var out []assetImbalance
	for _, e := range entries {
		i, ok := index[e.Account.AssetID]
		if !ok {
			i = len(out)
			index[e.Account.AssetID] = i
			out = append(out, assetImbalance{asset: e.Account.AssetID})
		}
		switch e.Side {
		case Debit:
			out[i].debits = out[i].debits.Add(e.Quantity)
			out[i].net = out[i].net.Add(e.Quantity)
		case Credit:
			out[i].credits = out[i].credits.Add(e.Quantity)
			out[i].net = out[i].net.Sub(e.Quantity)
		}
	}
	return out
}
