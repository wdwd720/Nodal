package devnethost

import (
	"sort"
	"strconv"

	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/errs"
)

// The two simulated public chains every devnet-mode subsystem uses.
const (
	ChainSolana   devnet.ChainID = "sbx-solana"
	ChainEthereum devnet.ChainID = "sbx-ethereum"
)

// Chains returns the host's chain ids in ascending order.
func Chains() []devnet.ChainID { return []devnet.ChainID{ChainEthereum, ChainSolana} }

// Bounds on one call, so a single request cannot pin the host.
const (
	// MaxBlocksPerCall bounds ProduceBlocks.
	MaxBlocksPerCall = 1000
	// MaxFaultCount bounds DropNext and DelayInclusion.
	MaxFaultCount = 1000
)

// ChainParams configures one chain of the host.
type ChainParams struct {
	ID              devnet.ChainID
	BlockhashWindow uint64
	FinalityDepth   uint64
	FeePerTx        uint64
	// BridgeAuthority is the key whose signature authorises BRIDGE_RELEASE on
	// this chain (the attester a relayer must get to sign). The zero key
	// leaves the bridge programs disabled on the chain.
	BridgeAuthority devnet.PublicKey
}

// Config configures a Host. Everything that decides how the chains behave
// is recorded in the GENESIS event, and a host refuses to open a history
// that was produced under a different configuration.
type Config struct {
	// Network names the event stream. Two hosts with the same network share
	// one chain history; different networks are independent. 1..63 bytes of
	// [a-z0-9-], starting with a letter or digit.
	Network string
	// Environment decides one thing only: whether fault injection is
	// available (LOCAL, TEST and DEV). It is not part of the chain history.
	Environment config.Environment
	// Chains must name exactly sbx-solana and sbx-ethereum.
	Chains []ChainParams
}

// DefaultConfig returns the standard host: both chains with devnet's default
// window (150 blocks), finality depth (32 blocks) and fee (5000 lamports),
// and the bridge enabled on both with bridgeAuthority as the attester.
func DefaultConfig(network string, env config.Environment, bridgeAuthority devnet.PublicKey) Config {
	params := func(id devnet.ChainID) ChainParams {
		return ChainParams{
			ID:              id,
			BlockhashWindow: devnet.DefaultBlockhashWindow,
			FinalityDepth:   devnet.DefaultFinalityDepth,
			FeePerTx:        devnet.DefaultFeePerTx,
			BridgeAuthority: bridgeAuthority,
		}
	}
	return Config{Network: network, Environment: env, Chains: []ChainParams{params(ChainSolana), params(ChainEthereum)}}
}

// Validate checks the configuration.
func (c Config) Validate() error {
	var problems []string
	if !validNetwork(c.Network) {
		problems = append(problems, "network must be 1..63 bytes of [a-z0-9-] starting with a letter or digit")
	}
	if _, err := config.ParseEnvironment(string(c.Environment)); err != nil {
		problems = append(problems, "environment must be LOCAL, TEST, DEV, STAGING or PROD")
	}
	seen := map[devnet.ChainID]bool{}
	for _, p := range c.Chains {
		if p.ID != ChainSolana && p.ID != ChainEthereum {
			problems = append(problems, "unknown chain "+string(p.ID)+"; the host runs sbx-solana and sbx-ethereum")
			continue
		}
		if seen[p.ID] {
			problems = append(problems, "chain "+string(p.ID)+" is configured twice")
		}
		seen[p.ID] = true
		if err := devnetConfig(p).Validate(); err != nil {
			problems = append(problems, "chain "+string(p.ID)+": "+err.Error())
		}
	}
	if !seen[ChainSolana] || !seen[ChainEthereum] {
		problems = append(problems, "both sbx-solana and sbx-ethereum must be configured")
	}
	if len(problems) > 0 {
		return errs.New(errs.CodeValidationFailed, "devnethost: invalid configuration").WithField("problems", problems)
	}
	return nil
}

// FaultInjectionAllowed reports whether the fault hooks are available: in
// LOCAL, TEST and DEV only.
func (c Config) FaultInjectionAllowed() bool { return c.Environment.AllowsFakeProviders() }

// Params returns the configured parameters of chain id.
func (c Config) Params(id devnet.ChainID) (ChainParams, bool) {
	for _, p := range c.Chains {
		if p.ID == id {
			return p, true
		}
	}
	return ChainParams{}, false
}

func validNetwork(s string) bool {
	if len(s) == 0 || len(s) > 63 {
		return false
	}
	for i := 0; i < len(s); i++ {
		ch := s[i]
		alnum := (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9')
		if !alnum && (ch != '-' || i == 0) {
			return false
		}
	}
	return true
}

// devnetConfig turns host parameters into a devnet chain configuration. The
// fee collector is devnet's derived address, which no key controls.
func devnetConfig(p ChainParams) devnet.ChainConfig {
	cfg := devnet.ChainConfig{
		ID:              p.ID,
		BlockhashWindow: p.BlockhashWindow,
		FinalityDepth:   p.FinalityDepth,
		FeePerTx:        p.FeePerTx,
		FeeCollector:    devnet.FeeCollectorAddress(p.ID),
	}
	if !p.BridgeAuthority.IsZero() {
		cfg.Bridge = &devnet.BridgeConfig{Authority: p.BridgeAuthority}
	}
	return cfg
}

// genesisChain is one chain in the GENESIS document. Every integer is a
// decimal string so the document is canonical JSON whatever its magnitude.
type genesisChain struct {
	ID              string `json:"id"`
	BlockhashWindow string `json:"blockhash_window"`
	FinalityDepth   string `json:"finality_depth"`
	FeePerTx        string `json:"fee_per_tx"`
	FeeCollector    string `json:"fee_collector"`
	BridgeAuthority string `json:"bridge_authority"`
}

// genesisArgs is the GENESIS event's document: the chain configurations the
// history was produced under, in chain-id order.
type genesisArgs struct {
	Version string         `json:"version"`
	Chains  []genesisChain `json:"chains"`
}

const genesisVersion = "nodal-devnethost/genesis/v1"

func (c Config) genesis() genesisArgs {
	chains := make([]genesisChain, 0, len(c.Chains))
	for _, p := range c.Chains {
		g := genesisChain{
			ID:              string(p.ID),
			BlockhashWindow: strconv.FormatUint(p.BlockhashWindow, 10),
			FinalityDepth:   strconv.FormatUint(p.FinalityDepth, 10),
			FeePerTx:        strconv.FormatUint(p.FeePerTx, 10),
			FeeCollector:    devnet.FeeCollectorAddress(p.ID).String(),
		}
		if !p.BridgeAuthority.IsZero() {
			g.BridgeAuthority = p.BridgeAuthority.String()
		}
		chains = append(chains, g)
	}
	sort.Slice(chains, func(i, j int) bool { return chains[i].ID < chains[j].ID })
	return genesisArgs{Version: genesisVersion, Chains: chains}
}

// newNetwork builds the network a fresh history starts from.
func (c Config) newNetwork() (*devnet.Network, error) {
	net := devnet.NewNetwork()
	params := append([]ChainParams(nil), c.Chains...)
	sort.Slice(params, func(i, j int) bool { return params[i].ID < params[j].ID })
	for _, p := range params {
		if _, err := net.AddChain(devnetConfig(p)); err != nil {
			return nil, err
		}
	}
	return net, nil
}
