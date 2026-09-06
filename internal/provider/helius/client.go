package helius

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/nodal/controlplane/internal/chain"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/observability"
	"github.com/nodal/controlplane/internal/provider"
	"github.com/nodal/controlplane/internal/provider/solanarpc"
)

// Documented constants (helius.md, "Endpoint formats and auth").
const (
	// ProviderName is the observer name and the source label persisted with
	// observations.
	ProviderName = "helius"
	// DefaultMainnetURL / DefaultDevnetURL are the documented RPC hosts.
	DefaultMainnetURL = "https://mainnet.helius-rpc.com"
	DefaultDevnetURL  = "https://devnet.helius-rpc.com"
	// APIKeyQueryParam is the name of the query parameter that carries the
	// key on every Helius host (the value comes from config.SecretRef).
	APIKeyQueryParam = "api-key" //nolint:gosec // G101: parameter name, not a credential
	// DASPageLimit is the documented maximum page size of getTokenAccounts.
	DASPageLimit = 1000
	// maxDASPages bounds balance enumeration (1000 accounts per page).
	maxDASPages = 10
)

// Options are the adapter tunables.
type Options struct {
	// RPC tunables; Name defaults to ProviderName and the auth placement is
	// always the api-key query parameter.
	RPC solanarpc.Options
	// Stream tunables.
	Stream StreamOptions
	// WSURL overrides the WebSocket endpoint (derived from BaseURL when
	// empty: wss://<host>/?api-key=…).
	WSURL string
}

// Deps are the injected collaborators.
type Deps struct {
	solanarpc.Deps
	// Dialer is the WebSocket transport (default: golang.org/x/net/websocket).
	Dialer Dialer
}

// Client is the Helius SolanaDataProvider.
type Client struct {
	name     string
	rpc      *solanarpc.Client
	wsURL    observability.Secret
	wsOrigin string
	wsRedact string
	dialer   Dialer
	stream   StreamOptions
	clk      clock.Clock
	log      *slog.Logger
}

var _ chain.SolanaDataProvider = (*Client)(nil)

// New builds the Helius adapter. Fake mode is refused (chaintest fakes are
// wired by the composition root); sandbox targets devnet, live mainnet; an
// API key is required in both because Helius has no unauthenticated host.
func New(ctx context.Context, cfg config.ProviderConfig, opts Options, deps Deps) (*Client, error) {
	if deps.Archive == nil {
		return nil, errors.New("helius: raw archive is required")
	}
	var base string
	switch cfg.Mode {
	case config.ProviderModeFake:
		return nil, errs.New(errs.CodeUnsupported, "helius: fake mode is wired through chain/chaintest by the composition root")
	case config.ProviderModeSandbox:
		base = DefaultDevnetURL
	case config.ProviderModeLive:
		base = DefaultMainnetURL
	default:
		return nil, fmt.Errorf("helius: unknown provider mode %q", cfg.Mode)
	}
	if cfg.BaseURL != "" {
		base = cfg.BaseURL
	}
	if cfg.APIKeyRef.IsZero() {
		return nil, errors.New("helius: APIKeyRef is required")
	}
	if deps.Resolver == nil {
		return nil, errors.New("helius: secret resolver is required")
	}
	key, err := deps.Resolver.Resolve(ctx, cfg.APIKeyRef)
	if err != nil {
		return nil, fmt.Errorf("helius: resolve api key: %w", err)
	}
	if strings.TrimSpace(key) == "" {
		return nil, errors.New("helius: resolved api key is empty")
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, errors.New("helius: BaseURL must be an absolute http(s) URL")
	}
	q := u.Query()
	q.Set(APIKeyQueryParam, key)
	u.RawQuery = q.Encode()

	name := cfg.Name
	if name == "" {
		name = ProviderName
	}
	rpcCfg := cfg
	rpcCfg.Name = name
	rpcCfg.BaseURL = u.String()
	rpcCfg.APIKeyRef = ""
	rpcOpts := opts.RPC
	rpcOpts.Name = name
	rpcOpts.AuthHeader, rpcOpts.AuthQueryParam = "", ""
	rpc, err := solanarpc.New(ctx, rpcCfg, rpcOpts, deps.Deps)
	if err != nil {
		return nil, err
	}
	c := &Client{name: name, rpc: rpc, dialer: deps.Dialer, stream: opts.Stream.withDefaults(), clk: deps.Clock, log: deps.Logger}
	if c.clk == nil {
		c.clk = clock.System()
	}
	if c.log == nil {
		c.log = slog.New(slog.DiscardHandler)
	}
	if c.dialer == nil {
		c.dialer = xnetDialer{}
	}
	ws := opts.WSURL
	if ws == "" {
		wu := *u
		wu.Path = "/"
		switch wu.Scheme {
		case "https":
			wu.Scheme = "wss"
		default:
			wu.Scheme = "ws"
		}
		ws = wu.String()
	} else {
		wu, err := url.Parse(ws)
		if err != nil || (wu.Scheme != "wss" && wu.Scheme != "ws") || wu.Host == "" {
			return nil, errors.New("helius: WSURL must be an absolute ws(s) URL")
		}
		if wu.Query().Get(APIKeyQueryParam) == "" {
			wq := wu.Query()
			wq.Set(APIKeyQueryParam, key)
			wu.RawQuery = wq.Encode()
		}
		ws = wu.String()
	}
	wu, _ := url.Parse(ws)
	origin := "https://" + wu.Host
	if wu.Scheme == "ws" {
		origin = "http://" + wu.Host
	}
	c.wsURL = observability.Secret(ws)
	c.wsOrigin = origin
	c.wsRedact = solanarpc.RedactURL(ws)
	c.log = c.log.With(slog.String("provider", name), slog.String("endpoint", rpc.RedactedEndpoint()))
	return c, nil
}

// Name implements chain.ChainObserver.
func (c *Client) Name() string { return c.name }

// Health implements chain.ChainObserver.
func (c *Client) Health() provider.Health { return c.rpc.Health() }

// Tracker exposes the health tracker.
func (c *Client) Tracker() *provider.Tracker { return c.rpc.Tracker() }

// VerificationLabel is the honest integration status (PART 208).
func (c *Client) VerificationLabel() provider.VerificationLabel { return provider.CodeComplete }

// RPC exposes the underlying JSON-RPC client.
func (c *Client) RPC() *solanarpc.Client { return c.rpc }

// RedactedWSURL is the stream endpoint without credentials.
func (c *Client) RedactedWSURL() string { return c.wsRedact }

// GetTransaction implements chain.ChainObserver.
func (c *Client) GetTransaction(ctx context.Context, sig string) (chain.TxObservation, error) {
	return c.rpc.GetTransaction(ctx, sig)
}

// GetSignatureStatuses implements chain.ChainObserver.
func (c *Client) GetSignatureStatuses(ctx context.Context, sigs []string) ([]chain.SignatureStatus, error) {
	return c.rpc.GetSignatureStatuses(ctx, sigs)
}

// GetBlockHeight implements chain.ChainObserver.
func (c *Client) GetBlockHeight(ctx context.Context) (uint64, error) {
	return c.rpc.GetBlockHeight(ctx)
}

// GetLatestBlockhash implements chain.ChainObserver.
func (c *Client) GetLatestBlockhash(ctx context.Context) (chain.Blockhash, error) {
	return c.rpc.GetLatestBlockhash(ctx)
}

// IsBlockhashValid implements chain.ChainObserver.
func (c *Client) IsBlockhashValid(ctx context.Context, blockhash string) (bool, error) {
	return c.rpc.IsBlockhashValid(ctx, blockhash)
}

// Simulate implements chain.ChainObserver.
func (c *Client) Simulate(ctx context.Context, rawTx []byte, opts chain.SimulateOptions) (chain.SimulationResult, error) {
	return c.rpc.Simulate(ctx, rawTx, opts)
}

// SearchWalletActivity implements chain.ChainObserver through the standard
// getSignaturesForAddress (wallet and its token accounts) + getTransaction
// path, identical to the fallback observer's.
func (c *Client) SearchWalletActivity(ctx context.Context, wallet string, since time.Time, limit int) ([]chain.TxObservation, error) {
	return c.rpc.SearchWalletActivity(ctx, wallet, since, limit)
}
