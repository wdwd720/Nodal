package assets

import (
	"encoding/json"
	"fmt"
	"sort"
)

// Token-2022 extensions that change balance or transfer semantics. Any asset
// carrying one of these is rejected by the signing inspector by default
// (PART 34) until explicit, per-extension support exists.
const (
	ExtTransferFee          = "transfer_fee"
	ExtTransferHook         = "transfer_hook"
	ExtConfidentialTransfer = "confidential_transfer"
	ExtPermanentDelegate    = "permanent_delegate"
	ExtNonTransferable      = "non_transferable"
	ExtDefaultAccountState  = "default_account_state"
	ExtCPIGuard             = "cpi_guard"
	ExtInterestBearing      = "interest_bearing"
	ExtMintCloseAuthority   = "mint_close_authority"
	ExtMetadataPointer      = "metadata_pointer"
	ExtGroupPointer         = "group_pointer"
	ExtMemberPointer        = "member_pointer"
	ExtPausable             = "pausable"
	ExtScaledUIAmount       = "scaled_ui_amount"
)

// HasUnsupportedExtensions reports whether the asset carries any Token-2022
// extension. V1 supports none; the list is kept so later enablement is
// explicit per extension.
func (a Asset) HasUnsupportedExtensions() bool { return len(a.TokenExtensions) > 0 }

func encodeExtensions(ext []string) ([]byte, error) {
	if ext == nil {
		ext = []string{}
	}
	cp := append([]string(nil), ext...)
	sort.Strings(cp)
	b, err := json.Marshal(cp)
	if err != nil {
		return nil, fmt.Errorf("assets: encode extensions: %w", err)
	}
	return b, nil
}

func decodeExtensions(b []byte) ([]string, error) {
	if len(b) == 0 {
		return []string{}, nil
	}
	var out []string
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("assets: decode extensions: %w", err)
	}
	if out == nil {
		out = []string{}
	}
	return out, nil
}
