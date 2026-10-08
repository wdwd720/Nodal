package stableasset

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"time"

	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// Plan document versions.
const (
	mintPlanVersion    = "nodal-sandbox/stable-mint-plan/v1"
	burnPlanVersion    = "nodal-sandbox/stable-burn-plan/v1"
	signingPlanVersion = "nodal-sandbox/stable-signing-plan/v1"
	planHashDomain     = "nodal-sandbox/stable-plan/v1\x00"
)

// PriceDoc is a price as a plan records it: exact strings.
type PriceDoc struct {
	Mantissa string    `json:"mantissa"`
	Scale    int32     `json:"scale"`
	Quote    string    `json:"quote"`
	Source   string    `json:"source"`
	At       time.Time `json:"at"`
}

func priceDoc(p money.Price) PriceDoc {
	return PriceDoc{Mantissa: p.Mantissa.String(), Scale: p.Scale, Quote: p.QuoteAsset, Source: p.Source, At: p.At.UTC()}
}

// MintPlan is the canonical description of one issuance: what is minted, to
// whom, by which authority, against which reserve payment. Its hash is the
// transaction's MEMO, so the signed transaction is bound to exactly this plan
// and the inspector can prove it.
type MintPlan struct {
	Version         string   `json:"version"`
	RequestID       string   `json:"request_id"`
	StableAssetID   string   `json:"stable_asset_id"`
	AccountID       string   `json:"account_id"`
	Chain           string   `json:"chain"`
	Mint            string   `json:"mint"`
	Recipient       string   `json:"recipient"`
	Amount          string   `json:"amount"`
	MintAuthority   string   `json:"mint_authority"`
	FeePayer        string   `json:"fee_payer"`
	RecentBlockhash string   `json:"recent_blockhash"`
	BlockhashHeight string   `json:"blockhash_height"`
	ReserveAssetID  string   `json:"reserve_asset_id"`
	ReserveQuantity string   `json:"reserve_quantity"`
	ReserveFactor   string   `json:"reserve_factor_bps"`
	ReservePrice    PriceDoc `json:"reserve_price"`
	Peg             PriceDoc `json:"peg"`
}

// BurnPlan is the canonical description of one burn and its redemption.
type BurnPlan struct {
	Version         string   `json:"version"`
	RequestID       string   `json:"request_id"`
	StableAssetID   string   `json:"stable_asset_id"`
	AccountID       string   `json:"account_id"`
	Chain           string   `json:"chain"`
	Mint            string   `json:"mint"`
	Holder          string   `json:"holder"`
	Amount          string   `json:"amount"`
	FeePayer        string   `json:"fee_payer"`
	RecentBlockhash string   `json:"recent_blockhash"`
	BlockhashHeight string   `json:"blockhash_height"`
	PayoutAssetID   string   `json:"payout_asset_id"`
	PayoutQuantity  string   `json:"payout_quantity"`
	RedemptionMode  string   `json:"redemption_mode"`
	Peg             PriceDoc `json:"peg"`
}

// SigningPlan states, before anything is signed, exactly which key signs
// which program set for which plan. The signer refuses a transaction that
// needs any other key.
type SigningPlan struct {
	Version  string   `json:"version"`
	PlanHash string   `json:"plan_hash"`
	Chain    string   `json:"chain"`
	Role     string   `json:"role"`
	Signers  []string `json:"signers"`
	FeePayer string   `json:"fee_payer"`
	Programs []string `json:"programs"`
}

// Signing roles.
const (
	RoleMintAuthority = "MINT_AUTHORITY"
	RoleHolder        = "HOLDER"
)

// hashPlan is SHA-256 of a domain tag and the plan's canonical JSON.
func hashPlan(plan any) ([32]byte, []byte, error) {
	b, err := audit.CanonicalJSON(plan)
	if err != nil {
		return [32]byte{}, nil, errs.Wrap(err, errs.CodeInternal, "stableasset: plan is not canonically encodable")
	}
	return sha256.Sum256(append([]byte(planHashDomain), b...)), b, nil
}

// Hash returns the plan hash and the canonical bytes it covers.
func (p MintPlan) Hash() ([32]byte, []byte, error) { return hashPlan(p) }

// Hash returns the plan hash and the canonical bytes it covers.
func (p BurnPlan) Hash() ([32]byte, []byte, error) { return hashPlan(p) }

// memoFor is the MEMO payload that binds a transaction to a plan: the plan
// hash in lowercase hex.
func memoFor(planHash [32]byte) devnet.Memo {
	return devnet.Memo{Data: []byte(hex.EncodeToString(planHash[:]))}
}

// mintParts are a mint plan's typed fields.
type mintParts struct {
	chain     devnet.ChainID
	mint      devnet.PublicKey
	recipient devnet.PublicKey
	amount    uint64
	authority devnet.PublicKey
	feePayer  devnet.PublicKey
	blockhash devnet.Hash
}

func (p MintPlan) parts() (mintParts, error) {
	var out mintParts
	var err error
	out.chain = devnet.ChainID(p.Chain)
	if out.mint, err = devnet.ParsePublicKey(p.Mint); err != nil {
		return out, err
	}
	if out.recipient, err = devnet.ParsePublicKey(p.Recipient); err != nil {
		return out, err
	}
	if out.authority, err = devnet.ParsePublicKey(p.MintAuthority); err != nil {
		return out, err
	}
	if out.feePayer, err = devnet.ParsePublicKey(p.FeePayer); err != nil {
		return out, err
	}
	if out.blockhash, err = devnet.ParseHash(p.RecentBlockhash); err != nil {
		return out, err
	}
	if out.amount, err = strconv.ParseUint(p.Amount, 10, 64); err != nil || out.amount == 0 {
		return out, errs.New(errs.CodeValidationFailed, "stableasset: mint amount is not a positive u64")
	}
	return out, nil
}

// Message builds the exact devnet message the plan describes:
// [TOKEN_MINT_TO, MEMO(plan hash)], paid by the plan's fee payer.
func (p MintPlan) Message() (devnet.Message, error) {
	parts, err := p.parts()
	if err != nil {
		return devnet.Message{}, err
	}
	h, _, err := p.Hash()
	if err != nil {
		return devnet.Message{}, err
	}
	return devnet.Message{
		ChainID: parts.chain, RecentBlockhash: parts.blockhash, FeePayer: parts.feePayer,
		Instructions: []devnet.Instruction{
			devnet.TokenMintTo{Mint: parts.mint, ToOwner: parts.recipient, Amount: parts.amount, Authority: parts.authority},
			memoFor(h),
		},
	}, nil
}

// SigningPlan returns the plan's signing plan: the mint authority alone signs,
// as fee payer and authority.
func (p MintPlan) SigningPlan() (SigningPlan, error) {
	h, _, err := p.Hash()
	if err != nil {
		return SigningPlan{}, err
	}
	return SigningPlan{
		Version: signingPlanVersion, PlanHash: hex.EncodeToString(h[:]), Chain: p.Chain, Role: RoleMintAuthority,
		Signers: []string{p.MintAuthority}, FeePayer: p.FeePayer,
		Programs: []string{devnet.ProgramTokenMintTo.String(), devnet.ProgramMemo.String()},
	}, nil
}

type burnParts struct {
	chain     devnet.ChainID
	mint      devnet.PublicKey
	holder    devnet.PublicKey
	amount    uint64
	feePayer  devnet.PublicKey
	blockhash devnet.Hash
}

func (p BurnPlan) parts() (burnParts, error) {
	var out burnParts
	var err error
	out.chain = devnet.ChainID(p.Chain)
	if out.mint, err = devnet.ParsePublicKey(p.Mint); err != nil {
		return out, err
	}
	if out.holder, err = devnet.ParsePublicKey(p.Holder); err != nil {
		return out, err
	}
	if out.feePayer, err = devnet.ParsePublicKey(p.FeePayer); err != nil {
		return out, err
	}
	if out.blockhash, err = devnet.ParseHash(p.RecentBlockhash); err != nil {
		return out, err
	}
	if out.amount, err = strconv.ParseUint(p.Amount, 10, 64); err != nil || out.amount == 0 {
		return out, errs.New(errs.CodeValidationFailed, "stableasset: burn amount is not a positive u64")
	}
	return out, nil
}

// Message builds [TOKEN_BURN, MEMO(plan hash)], paid by the holder.
func (p BurnPlan) Message() (devnet.Message, error) {
	parts, err := p.parts()
	if err != nil {
		return devnet.Message{}, err
	}
	h, _, err := p.Hash()
	if err != nil {
		return devnet.Message{}, err
	}
	return devnet.Message{
		ChainID: parts.chain, RecentBlockhash: parts.blockhash, FeePayer: parts.feePayer,
		Instructions: []devnet.Instruction{
			devnet.TokenBurn{Mint: parts.mint, Owner: parts.holder, Amount: parts.amount},
			memoFor(h),
		},
	}, nil
}

// SigningPlan returns the burn's signing plan: the holder alone signs.
func (p BurnPlan) SigningPlan() (SigningPlan, error) {
	h, _, err := p.Hash()
	if err != nil {
		return SigningPlan{}, err
	}
	return SigningPlan{
		Version: signingPlanVersion, PlanHash: hex.EncodeToString(h[:]), Chain: p.Chain, Role: RoleHolder,
		Signers: []string{p.Holder}, FeePayer: p.FeePayer,
		Programs: []string{devnet.ProgramTokenBurn.String(), devnet.ProgramMemo.String()},
	}, nil
}
