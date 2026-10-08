package transfers

import (
	"context"
	"sort"
	"strings"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// ScreenDecision is a compliance screen's answer.
type ScreenDecision string

// Screen decisions. Anything other than CLEAR stops the transfer.
const (
	ScreenClear ScreenDecision = "CLEAR"
	ScreenBlock ScreenDecision = "BLOCK"
)

// ScreenRequest is what a sanctions / fraud screen sees about one transfer.
// The memo is represented by its digest only.
type ScreenRequest struct {
	TransferID         string
	SenderAccountID    accounts.AccountID
	SenderUserID       accounts.UserID
	RecipientAccountID accounts.AccountID
	RecipientUserID    accounts.UserID
	AssetID            assets.AssetID
	Quantity           money.Quantity
	NotionalUSD        money.USD
	MemoSHA256         string
}

// ScreenResult is a screen's decision, with the screen's identity so the
// evidence can say which screen decided.
type ScreenResult struct {
	Decision ScreenDecision `json:"decision"`
	// Code is a stable reason for a BLOCK (e.g. SANCTIONS_LIST_MATCH).
	Code    string `json:"code"`
	Screen  string `json:"screen"`
	Version string `json:"version"`
	// Matched names which party matched, for a BLOCK.
	Matched string `json:"matched"`
}

// ComplianceScreen is the sanctions / fraud hook every transfer passes
// through before value moves. An error is not a pass: the transfer is
// refused (TRANSFER_COMPLIANCE_UNAVAILABLE) and nothing is posted.
type ComplianceScreen interface {
	Screen(ctx context.Context, req ScreenRequest) (ScreenResult, error)
}

// ListScreen is the deterministic default screen: it blocks a configured
// list of account ids and user ids, each with a stable reason code. The same
// request always gets the same answer; the list is copied at construction
// and never changes afterwards.
type ListScreen struct {
	version string
	blocked map[string]string
}

// ListScreenName is the Screen value a ListScreen reports.
const ListScreenName = "list"

// NewListScreen builds a ListScreen. Keys are canonical account or user ids
// (case-insensitive); values are reason codes such as SANCTIONS_LIST_MATCH.
func NewListScreen(version string, blocked map[string]string) (*ListScreen, error) {
	if strings.TrimSpace(version) == "" {
		return nil, errs.New(errs.CodeValidationFailed, "a compliance screen needs a version")
	}
	m := make(map[string]string, len(blocked))
	keys := make([]string, 0, len(blocked))
	for k := range blocked {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		code := blocked[k]
		if !validReasonCode(code) {
			return nil, errs.Newf(errs.CodeValidationFailed, "block reason %q is not a stable code", code)
		}
		m[strings.ToLower(strings.TrimSpace(k))] = code
	}
	return &ListScreen{version: version, blocked: m}, nil
}

// Screen implements ComplianceScreen. Parties are checked in a fixed order
// (sender account, sender user, recipient account, recipient user) and the
// first match decides.
func (l *ListScreen) Screen(_ context.Context, req ScreenRequest) (ScreenResult, error) {
	res := ScreenResult{Decision: ScreenClear, Screen: ListScreenName, Version: l.version}
	for _, p := range []struct{ role, id string }{
		{"sender_account", req.SenderAccountID.String()},
		{"sender_user", req.SenderUserID.String()},
		{"recipient_account", req.RecipientAccountID.String()},
		{"recipient_user", req.RecipientUserID.String()},
	} {
		if code, ok := l.blocked[strings.ToLower(p.id)]; ok {
			res.Decision, res.Code, res.Matched = ScreenBlock, code, p.role
			return res, nil
		}
	}
	return res, nil
}
