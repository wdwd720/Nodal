package sandboxworld

import (
	"context"
	"fmt"
	"math/big"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/compliance"
	"github.com/nodal/controlplane/internal/delegation"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/devnethost"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/httpapi"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/reconciliation"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/sandboxchain"
	"github.com/nodal/controlplane/internal/valuation"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// Delegation is delegated agent authority over the world, wired as a
// deployment wires it.
//
// The platform's side is cmd/api's ("advanced:delegation services" in
// cmd/api/wire.go): the delegated wallet signer from the world's sandbox
// chain seed (sandboxchain.PurposeDelegatedSigner), one venue co-signer per
// httpapi.DelegationSandboxVenues venue from the venue's name
// (sandboxchain.Seed.VenueSeed), the service over the world's ledger,
// capital, risk, prices, legal router and identity rule, and the first
// sandbox venue registered at startup by httpapi.EnsureDelegationVenue, at
// its co-signer's address, on a LIQUIDITY_PROVISION house book of its own.
//
// The agent's side is cmd/agent-worker's (wireDelegation): the admission
// facade (delegation.NewAgentAdmission), built from the same seed and the
// registered venues' co-signer seeds, which is all of delegated authority an
// agent tree may hold. It admits; it never signs, decides or settles.
type Delegation struct {
	Service   *delegation.Service
	Admission *delegation.AgentAdmission
	// Signer is the platform's delegated wallet signer. The world holds it
	// only as cmd/api does, to hand it to the service; its one public use
	// here is the delegated wallet's address.
	Signer    *delegation.DelegatedSigner
	Venue     delegation.Venue
	CoSigners []httpapi.DelegationCoSigner
}

// OpenDelegation wires delegated authority over the world and registers the
// startup sandbox venue.
func (w *World) OpenDelegation(ctx context.Context) (*Delegation, error) {
	seed := w.chainSeed
	signer, err := delegation.NewDelegatedSigner(seed.Derive(sandboxchain.PurposeDelegatedSigner), delegation.SignerConfig{
		Environment: w.Env, Clock: w.Clock, Policy: delegation.DefaultSigningPolicy(), Fees: delegation.HostFees(w.Host),
	})
	if err != nil {
		return nil, fmt.Errorf("sandboxworld: delegated signer: %w", err)
	}
	venues := httpapi.DelegationSandboxVenues()
	d := &Delegation{Signer: signer}
	venueSigners := make([]*delegation.VenueSigner, 0, len(venues))
	for _, v := range venues {
		vs := delegation.NewVenueSigner(seed.VenueSeed(v.Name))
		venueSigners = append(venueSigners, vs)
		d.CoSigners = append(d.CoSigners, httpapi.DelegationCoSigner{Name: v.Name, Chain: v.Chain, Address: vs.PublicKey()})
	}
	if d.Service, err = delegation.New(delegation.Deps{
		DB: w.DB, Clock: w.Clock, Environment: w.Env, Host: w.Host,
		Intents: econ.NewIntentStore(w.Clock, w.Outbox, w.Audit), Effects: econ.NewEffectStore(w.Clock, w.Outbox, w.Audit),
		Authorizer: econ.NewAuthorizer(w.Gates, w.Kills), Kills: w.Kills, Ledger: w.Ledger, Capital: w.Capital,
		Risk: risk.NewStore(), RiskPolicy: risk.DefaultAdvancedPolicy(), Router: w.Router, Prices: w.Prices,
		Identity: compliance.NewAccountVerification(w.Clock), Profiles: compliance.NewRepository(w.Audit),
		Reconciliation: reconciliation.NewRepository(w.Clock, w.Outbox, w.Audit), Audit: w.Audit,
		Accounts: accounts.NewRepository(), Assets: w.Assets, Signer: signer, Venues: venueSigners,
	}); err != nil {
		return nil, fmt.Errorf("sandboxworld: delegation service: %w", err)
	}
	if d.Venue, err = httpapi.EnsureDelegationVenue(ctx, w.DB, httpapi.DelegationDeps{Service: d.Service, CoSigners: d.CoSigners},
		venues[0]); err != nil {
		return nil, fmt.Errorf("sandboxworld: register the sandbox venue: %w", err)
	}
	names, err := registeredVenueNames(ctx, w)
	if err != nil {
		return nil, err
	}
	seeds := make(map[string][devnet.SeedSize]byte, len(names))
	for _, name := range names {
		seeds[name] = seed.VenueSeed(name)
	}
	// No capability is enabled by configuration and the legal router is the
	// conservative one, as for every other service of the world.
	if d.Admission, err = delegation.NewAgentAdmission(delegation.AdmissionConfig{
		DB: w.DB, Clock: w.Clock, Environment: w.Env, Host: w.Host, BuildVersion: w.component,
		SignerSeed: seed.Derive(sandboxchain.PurposeDelegatedSigner), VenueSeeds: seeds,
	}); err != nil {
		return nil, fmt.Errorf("sandboxworld: delegated authority admission: %w", err)
	}
	return d, nil
}

// registeredVenueNames reads the registered delegation venues by name, as
// cmd/agent-worker does: the agent's side co-signs for exactly those.
func registeredVenueNames(ctx context.Context, w *World) ([]string, error) {
	rows, err := w.DB.Query(ctx, `SELECT name FROM delegation_venues ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("sandboxworld: delegation venues: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("sandboxworld: delegation venues: %w", err)
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// Sandbox tokens. A delegated swap moves devnet tokens: the delegated wallet
// pays one and the venue another, on chain, in one transaction. No service of
// this build issues an arbitrary devnet token (the stable asset's issuer
// mints only its own asset, to custodial wallets), so the world holds a mint
// authority for the tokens it lists -- as every delegation integration test
// does -- with its key derived from the world's seed through
// internal/sandboxchain under a fixed purpose, never from anything else.
const (
	purposeSandboxTokenMint = "sandbox-token-mint-authority"
	purposeSandboxTokenAddr = "sandbox-token-mint/"
)

func (w *World) tokenMintAuthority() devnet.Keypair {
	return devnet.NewKeypairFromSeed(w.chainSeed.Derive(purposeSandboxTokenMint))
}

// SandboxToken lists a SIMULATED devnet token on sbx-solana: its mint is
// created on chain under the world's token mint authority and the asset is
// registered with the mint as its address. A USD-pegged token is a stable
// unit valued at par (STABLE_VALUE to delegated authority); any other needs a
// recorded mark.
func (w *World) SandboxToken(ctx context.Context, symbol, name string, decimals uint8, rc assets.RiskClass, usdPegged bool) (assets.Asset, error) {
	mint := devnet.NewKeypairFromSeed(w.chainSeed.Derive(purposeSandboxTokenAddr + symbol)).PublicKey()
	if err := w.Host.EnsureMint(ctx, devnethost.ChainSolana, devnet.MintConfig{
		Address: mint, Decimals: decimals, Authority: w.tokenMintAuthority().PublicKey(),
	}); err != nil {
		return assets.Asset{}, fmt.Errorf("sandboxworld: create the %s mint: %w", symbol, err)
	}
	a := assets.Asset{
		Chain: string(devnethost.ChainSolana), MintAddress: mint.String(), Kind: assets.KindSimulated,
		ValueDomain: valuedomain.Simulated, Symbol: symbol, Name: name, Decimals: decimals, RiskClass: rc, Status: assets.StatusActive,
	}
	if usdPegged {
		a.IsStablecoin, a.PegCurrency = true, valuation.USDPeg
	}
	return w.Assets.Create(ctx, w.DB, a)
}

// MintSandboxToken issues amount base units of a sandbox token to owner on
// chain, signed by the world's token mint authority, and produces blocks
// until the chain has finalized it. memo makes the transaction unique.
func (w *World) MintSandboxToken(ctx context.Context, token assets.Asset, owner devnet.PublicKey, amount uint64, memo string) (devnet.TxID, error) {
	chain := devnethost.ChainSolana
	auth := w.tokenMintAuthority()
	if err := w.Host.EnsureNativeBalance(ctx, chain, auth.PublicKey(), 1_000_000, 100_000_000); err != nil {
		return "", fmt.Errorf("sandboxworld: fund the mint authority's fees: %w", err)
	}
	mint, err := devnet.ParsePublicKey(token.MintAddress)
	if err != nil {
		return "", fmt.Errorf("sandboxworld: %s has no devnet mint: %w", token.Symbol, err)
	}
	bh, err := w.Host.LatestBlockhash(ctx, chain)
	if err != nil {
		return "", err
	}
	tx, err := devnet.Sign(devnet.Message{ChainID: chain, RecentBlockhash: bh, FeePayer: auth.PublicKey(),
		Instructions: []devnet.Instruction{
			devnet.TokenMintTo{Mint: mint, ToOwner: owner, Amount: amount, Authority: auth.PublicKey()},
			devnet.Memo{Data: []byte("sandbox-token-mint:" + memo)},
		}}, auth)
	if err != nil {
		return "", err
	}
	raw, err := tx.Encode()
	if err != nil {
		return "", err
	}
	txID, err := w.Host.Submit(ctx, chain, raw)
	if err != nil {
		return "", fmt.Errorf("sandboxworld: submit the %s mint: %w", token.Symbol, err)
	}
	depth, err := w.finalityDepth(chain)
	if err != nil {
		return "", err
	}
	if _, err := w.Host.ProduceBlocks(ctx, chain, 1+depth); err != nil {
		return "", err
	}
	st, err := w.Host.Status(ctx, chain, txID)
	if err != nil {
		return "", err
	}
	if st.State != devnet.TxFinalized {
		return "", fmt.Errorf("sandboxworld: the %s mint did not finalize: %s %s", token.Symbol, st.State, st.Err)
	}
	return txID, nil
}

func (w *World) finalityDepth(chain devnet.ChainID) (uint64, error) {
	for _, c := range w.Host.Config().Chains {
		if c.ID == chain {
			return c.FinalityDepth, nil
		}
	}
	return 0, fmt.Errorf("sandboxworld: the host does not run %s", chain)
}

// FundVenue gives a delegation venue inventory of a sandbox token: its house
// book is funded in the books (HOUSE_FUNDING: Dr HOUSE_INVENTORY / Cr
// HOUSE_CAPITAL) and the same units are minted to the venue's address on
// chain, as the delegation integration tests back their venues. key names
// the funding and makes it idempotent.
func (w *World) FundVenue(ctx context.Context, v delegation.Venue, token assets.Asset, amount uint64, key string) error {
	qty := money.QuantityFromBigInt(new(big.Int).SetUint64(amount))
	house := econ.HouseAccount{ID: v.HouseAccountID}
	if err := w.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := w.Ledger.Post(ctx, tx, ledger.Posting{
			Kind: ledger.KindHouseFunding, IdempotencyKey: "sandbox-venue-funding:" + key,
			Reference:   ledger.FinancialEventReference{Type: "house_funding", ID: key},
			EffectiveAt: w.Clock.Now(), Description: "simulated inventory of delegation venue " + v.Name,
			Entries: []ledger.Entry{
				{Account: house.Account(ledger.CodeHouseInventory, token.ID), Side: ledger.Debit, Quantity: qty},
				{Account: house.Account(ledger.CodeHouseCapital, token.ID), Side: ledger.Credit, Quantity: qty},
			},
		})
		return err
	}); err != nil {
		return fmt.Errorf("sandboxworld: fund venue %s with %s: %w", v.Name, token.Symbol, err)
	}
	_, err := w.MintSandboxToken(ctx, token, v.Address, amount, "venue:"+key)
	return err
}

// FundDelegatedWallet gives a customer amount base units of a sandbox token
// in their delegated wallet: the sandbox faucet grants them in the books, as
// the customer, and the same units are minted to the account's delegated
// wallet on chain.
func (w *World) FundDelegatedWallet(ctx context.Context, d *Delegation, c Customer, token assets.Asset, amount uint64, key string) (devnet.PublicKey, error) {
	wallet := d.Signer.PublicKey(c.Account, devnethost.ChainSolana)
	if _, err := w.Fund(ctx, c, token.ID, money.QuantityFromBigInt(new(big.Int).SetUint64(amount)), key); err != nil {
		return devnet.PublicKey{}, fmt.Errorf("sandboxworld: faucet grant of %s: %w", token.Symbol, err)
	}
	if _, err := w.MintSandboxToken(ctx, token, wallet, amount, "wallet:"+key); err != nil {
		return devnet.PublicKey{}, err
	}
	return wallet, nil
}
