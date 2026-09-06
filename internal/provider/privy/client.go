package privy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gagliardetto/solana-go"
	privyclient "github.com/privy-io/go-sdk"
	"github.com/privy-io/go-sdk/authorization"
	"github.com/privy-io/go-sdk/option"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/provider"
	"github.com/nodal/controlplane/internal/wallet"
)

// Adapter implements wallet.WalletProvider and wallet.SigningProvider over
// the Privy SDK.
type Adapter struct {
	client  *privyclient.PrivyClient
	auth    authorization.AuthorizationContext
	opts    Options
	timeout time.Duration
	clk     clock.Clock
}

var (
	_ wallet.WalletProvider  = (*Adapter)(nil)
	_ wallet.SigningProvider = (*Adapter)(nil)
)

// New builds the adapter. Secrets are resolved once through resolver and
// kept only inside the SDK client / authorization context; they are never
// logged. It fails closed on mode fake, missing configuration, insecure
// base URLs in STAGING/PROD and unresolvable secrets.
func New(ctx context.Context, cfg config.ProviderConfig, opts Options, resolver config.Resolver, clk clock.Clock) (*Adapter, error) {
	opts = opts.withDefaults(cfg)
	if err := opts.validate(cfg); err != nil {
		return nil, err
	}
	if resolver == nil {
		return nil, errs.New(errs.CodeValidationFailed, "privy: secret resolver required")
	}
	if clk == nil {
		clk = clock.System()
	}
	secret, err := resolver.Resolve(ctx, opts.AppSecretRef)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeValidationFailed, "privy: app secret could not be resolved")
	}
	if strings.TrimSpace(secret) == "" {
		return nil, errs.New(errs.CodeValidationFailed, "privy: app secret is empty")
	}
	auth := authorization.AuthorizationContext{}
	if opts.Signer != nil {
		auth.Signers = []authorization.AuthorizationSigner{opts.Signer}
	}
	if !opts.AuthorizationKeyRef.IsZero() {
		key, err := resolver.Resolve(ctx, opts.AuthorizationKeyRef)
		if err != nil {
			return nil, errs.Wrap(err, errs.CodeValidationFailed, "privy: authorization key could not be resolved")
		}
		key = strings.TrimSpace(key)
		if _, err := base64.StdEncoding.DecodeString(key); err != nil || key == "" {
			return nil, errs.New(errs.CodeValidationFailed, "privy: authorization key must be base64 PKCS#8")
		}
		auth.PrivateKeys = []string{key}
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: timeout}
	}
	client := privyclient.NewPrivyClient(privyclient.PrivyClientOptions{
		AppID:      opts.AppID,
		AppSecret:  secret,
		APIUrl:     cfg.BaseURL,
		HTTPClient: httpClient,
		RequestExpiry: privyclient.PrivyRequestExpiryOptions{
			DefaultMs: timeout.Milliseconds() * 2,
		},
	})
	return &Adapter{client: client, auth: auth, opts: opts, timeout: timeout, clk: clk}, nil
}

// Name implements wallet.WalletProvider and wallet.SigningProvider.
func (a *Adapter) Name() string { return Name }

// VerificationLabel is the honest integration status (PART 208).
func (a *Adapter) VerificationLabel() provider.VerificationLabel { return provider.CodeComplete }

// Chain returns the platform chain name wallets are created on.
func (a *Adapter) Chain() string { return a.opts.Chain }

func (a *Adapter) callCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, a.timeout)
}

// requestOptions are the transport options applied to every SDK call:
// SDK-side retries off (retry policy is the caller's), per-call timeout.
func (a *Adapter) requestOptions() []option.RequestOption {
	return []option.RequestOption{option.WithMaxRetries(0), option.WithRequestTimeout(a.timeout)}
}

// CreateWalletIdempotencyKey is the privy-idempotency-key for (account, chain).
func CreateWalletIdempotencyKey(accountID, chain string) string {
	h := sha256.Sum256([]byte("wallet:" + accountID + ":" + chain))
	return "wallet-" + hex.EncodeToString(h[:16])
}

// CreateWallet implements wallet.WalletProvider (IDEMPOTENT_WRITE).
func (a *Adapter) CreateWallet(ctx context.Context, accountID, chain string) (wallet.Wallet, error) {
	acct, err := accounts.ParseAccountID(accountID)
	if err != nil {
		return wallet.Wallet{}, errs.New(errs.CodeValidationFailed, "privy: account id must be a canonical uuid")
	}
	if chain != a.opts.Chain {
		return wallet.Wallet{}, errs.Newf(errs.CodeUnsupported, "privy: adapter is configured for %s, not %s", a.opts.Chain, chain)
	}
	ctx, cancel := a.callCtx(ctx)
	defer cancel()
	params := privyclient.WalletNewParams{
		ChainType:           privyclient.WalletChainTypeSolana,
		OwnerID:             privyclient.String(a.opts.OwnerQuorumID),
		PolicyIDs:           privyclient.PolicyInput{a.opts.PolicyID},
		ExternalID:          privyclient.String(acct.String()),
		DisplayName:         privyclient.String("account " + acct.String()),
		PrivyIdempotencyKey: privyclient.String(CreateWalletIdempotencyKey(acct.String(), chain)),
	}
	w, err := a.client.Wallets.New(ctx, params, a.requestOptions()...)
	if err != nil {
		return wallet.Wallet{}, mapError("CreateWallet", provider.IdempotentWrite, err)
	}
	out, err := a.toWallet(w)
	if err != nil {
		return wallet.Wallet{}, err
	}
	out.AccountID = acct
	return out, nil
}

// GetWallet implements wallet.WalletProvider (SAFE_RETRY).
func (a *Adapter) GetWallet(ctx context.Context, providerWalletID string) (wallet.Wallet, error) {
	if providerWalletID == "" {
		return wallet.Wallet{}, errs.New(errs.CodeValidationFailed, "privy: provider wallet id required")
	}
	ctx, cancel := a.callCtx(ctx)
	defer cancel()
	w, err := a.client.Wallets.Get(ctx, providerWalletID, privyclient.WalletGetParams{}, a.requestOptions()...)
	if err != nil {
		return wallet.Wallet{}, mapError("GetWallet", provider.SafeRetry, err)
	}
	return a.toWallet(w)
}

func (a *Adapter) toWallet(w *privyclient.Wallet) (wallet.Wallet, error) {
	if w == nil || w.ID == "" || w.Address == "" {
		return wallet.Wallet{}, errs.New(errs.CodeInternal, "privy: wallet response missing id or address").
			WithField(FieldProvider, Name).WithField(FieldRetryClass, provider.SafeRetry)
	}
	if w.ChainType != privyclient.WalletChainTypeSolana {
		return wallet.Wallet{}, errs.Newf(errs.CodeUnsupported, "privy: wallet chain type %q is not solana", w.ChainType)
	}
	if _, err := solana.PublicKeyFromBase58(w.Address); err != nil {
		return wallet.Wallet{}, errs.New(errs.CodeInternal, "privy: wallet address is not a base58 public key")
	}
	status := wallet.StatusActive
	if w.ArchivedAt != 0 {
		status = wallet.StatusRevoked
	}
	return wallet.Wallet{
		Provider:         Name,
		ProviderWalletID: w.ID,
		Chain:            a.opts.Chain,
		Address:          w.Address,
		Kind:             wallet.KindEmbeddedDelegated,
		Status:           status,
	}, nil
}

// VerifyDelegation implements wallet.WalletProvider (SAFE_RETRY): the
// configured policy must be attached, our signer must be the owner or an
// additional signer, and the policy must verify.
func (a *Adapter) VerifyDelegation(ctx context.Context, providerWalletID string) (wallet.DelegationStatus, error) {
	if providerWalletID == "" {
		return wallet.DelegationStatus{}, errs.New(errs.CodeValidationFailed, "privy: provider wallet id required")
	}
	ctx, cancel := a.callCtx(ctx)
	defer cancel()
	w, err := a.client.Wallets.Get(ctx, providerWalletID, privyclient.WalletGetParams{}, a.requestOptions()...)
	if err != nil {
		return wallet.DelegationStatus{}, mapError("VerifyDelegation", provider.SafeRetry, err)
	}
	st := wallet.DelegationStatus{
		ProviderWalletID: providerWalletID,
		State:            wallet.DelegationUnverified,
		PolicyID:         a.opts.PolicyID,
		SignerID:         a.opts.SignerQuorumID,
		EvidenceRef:      "privy://wallets/" + providerWalletID,
		CheckedAt:        a.clk.Now(),
	}
	var problems []string
	if w == nil || w.ID != providerWalletID {
		return st, errs.New(errs.CodeInternal, "privy: wallet response id mismatch")
	}
	if w.ArchivedAt != 0 {
		st.State = wallet.DelegationRevoked
		st.Detail = "wallet is archived"
		return st, nil
	}
	if !containsString(w.PolicyIDs, a.opts.PolicyID) {
		problems = append(problems, "configured policy is not attached to the wallet")
	}
	signer := w.OwnerID == a.opts.SignerQuorumID
	for _, s := range w.AdditionalSigners {
		if s.SignerID == a.opts.SignerQuorumID {
			signer = true
			// An override policy other than ours would replace the wallet policy for our signer.
			for _, p := range s.OverridePolicyIDs {
				if p != a.opts.PolicyID {
					problems = append(problems, "our signer carries an override policy "+p)
				}
			}
		}
	}
	if !signer {
		problems = append(problems, "our signer is neither the owner nor an additional signer")
	}
	policy, err := a.client.Policies.Get(ctx, a.opts.PolicyID, a.requestOptions()...)
	if err != nil {
		return st, mapError("VerifyDelegation", provider.SafeRetry, err)
	}
	chainType, rules := fromSDKPolicy(policy)
	allowed, policyProblems := verifyPolicy(chainType, rules, a.opts.AllowedProgramIDs)
	problems = append(problems, policyProblems...)
	st.AllowedPrograms = allowed
	st.PolicyVersion = policyVersion(policy)
	if len(problems) == 0 {
		st.State = wallet.DelegationVerified
		st.Detail = "policy verified: programId allow-list + deny-all; provider policy does not resolve lookup-table accounts (inspector is authoritative)"
	} else {
		st.Detail = strings.Join(problems, "; ")
	}
	return st, nil
}

// Capabilities implements wallet.WalletProvider: the PART 96 probe verifies
// the configured policy without a wallet.
func (a *Adapter) Capabilities(ctx context.Context) (wallet.Capability, error) {
	ctx, cancel := a.callCtx(ctx)
	defer cancel()
	cap := wallet.Capability{
		Provider:          Name,
		DelegatedSigning:  wallet.CapabilityUnverified,
		VerificationLabel: provider.CodeComplete,
		SupportedChains:   []string{a.opts.Chain},
		ProbedAt:          a.clk.Now(),
	}
	policy, err := a.client.Policies.Get(ctx, a.opts.PolicyID, a.requestOptions()...)
	if err != nil {
		return cap, mapError("Capabilities", provider.SafeRetry, err)
	}
	chainType, rules := fromSDKPolicy(policy)
	_, problems := verifyPolicy(chainType, rules, a.opts.AllowedProgramIDs)
	if len(problems) == 0 {
		cap.DelegatedSigning = wallet.CapabilityVerified
		cap.Detail = "policy " + a.opts.PolicyID + " verified (" + policyVersion(policy) + "); label CODE_COMPLETE: no live signing exercised in this build"
	} else {
		cap.Detail = strings.Join(problems, "; ")
	}
	return cap, nil
}

func policyVersion(p *privyclient.Policy) string {
	if p == nil {
		return ""
	}
	return p.ID + "@" + string(p.Version)
}

// SignTransaction implements wallet.SigningProvider (UNKNOWN_EFFECT_WRITE,
// see doc.go). The signed bytes must carry the identical message and a
// valid signature by the wallet's key, otherwise SIGNING_REJECTED.
func (a *Adapter) SignTransaction(ctx context.Context, req wallet.SignRequest) (wallet.SignResult, error) {
	if err := req.Validate(); err != nil {
		return wallet.SignResult{}, err
	}
	if req.Chain != a.opts.Chain {
		return wallet.SignResult{}, errs.Newf(errs.CodeUnsupported, "privy: adapter is configured for %s, not %s", a.opts.Chain, req.Chain)
	}
	unsigned, err := solana.TransactionFromBytes(req.UnsignedTx)
	if err != nil {
		return wallet.SignResult{}, errs.Wrap(err, errs.CodeValidationFailed, "privy: unsigned transaction does not decode")
	}
	unsignedMsg, err := unsigned.Message.MarshalBinary()
	if err != nil {
		return wallet.SignResult{}, errs.Wrap(err, errs.CodeValidationFailed, "privy: unsigned message does not encode")
	}
	ctx, cancel := a.callCtx(ctx)
	defer cancel()
	auth := a.auth
	data, err := a.client.Wallets.Solana.SignTransactionBytes(ctx, req.ProviderWalletID, req.UnsignedTx,
		privyclient.WithIdempotencyKey(req.IdempotencyKey),
		privyclient.WithAuthorizationContext(&auth),
		privyclient.WithRequestOptions(a.requestOptions()...),
	)
	if err != nil {
		return wallet.SignResult{}, mapError("SignTransaction", provider.UnknownEffectWrite, err)
	}
	if data == nil || data.SignedTransaction == "" {
		return wallet.SignResult{}, errs.New(errs.CodeInternal, "privy: signTransaction response missing signed_transaction").
			WithField(FieldProvider, Name).WithField(FieldOperation, "SignTransaction").WithField(FieldRetryClass, provider.UnknownEffectWrite)
	}
	if data.Encoding != "" && data.Encoding != privyclient.SolanaSignTransactionRpcResponseDataEncodingBase64 {
		return wallet.SignResult{}, errs.Newf(errs.CodeInternal, "privy: unexpected signed transaction encoding %q", data.Encoding)
	}
	signed, err := base64.StdEncoding.DecodeString(data.SignedTransaction)
	if err != nil {
		return wallet.SignResult{}, errs.Wrap(err, errs.CodeInternal, "privy: signed transaction is not base64")
	}
	stx, err := solana.TransactionFromBytes(signed)
	if err != nil {
		return wallet.SignResult{}, errs.Wrap(err, errs.CodeSigningRejected, "privy: signed transaction does not decode")
	}
	signedMsg, err := stx.Message.MarshalBinary()
	if err != nil {
		return wallet.SignResult{}, errs.Wrap(err, errs.CodeSigningRejected, "privy: signed message does not encode")
	}
	if !bytes.Equal(unsignedMsg, signedMsg) {
		return wallet.SignResult{}, errs.New(errs.CodeSigningRejected, "privy: provider returned a different message than the one submitted")
	}
	if err := stx.VerifySignatures(); err != nil {
		return wallet.SignResult{}, errs.Wrap(err, errs.CodeSigningRejected, "privy: provider signature does not verify")
	}
	if len(stx.Signatures) == 0 {
		return wallet.SignResult{}, errs.New(errs.CodeSigningRejected, "privy: no signature on the returned transaction")
	}
	h := sha256.Sum256(signed)
	return wallet.SignResult{
		SignedTx:    signed,
		Signature:   append([]byte(nil), stx.Signatures[0][:]...),
		ProviderRef: fmt.Sprintf("privy:signTransaction:%s:%s", req.IdempotencyKey, hex.EncodeToString(h[:8])),
		RetryClass:  provider.UnknownEffectWrite,
		SignedAt:    a.clk.Now(),
	}, nil
}

func containsString(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}
