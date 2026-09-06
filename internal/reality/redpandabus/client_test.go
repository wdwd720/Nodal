package redpandabus_test

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/reality/redpandabus"
)

// closedAddr returns a host:port nothing is listening on.
func closedAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())
	return addr
}

func testResolver() config.Resolver {
	return config.NewResolver(config.EnvTest, func(string) (string, bool) { return "", false })
}

// A broker that does not answer is a startup failure. This is the property
// the whole no-fallback posture rests on: if New could return a client whose
// brokers are down, the worker would come up and publish into nothing.
func TestNew_UnreachableBrokerIsAStartupFailure(t *testing.T) {
	t.Parallel()
	_, err := redpandabus.New(context.Background(),
		config.RedpandaConfig{Brokers: []string{closedAddr(t)}}, testResolver(),
		redpandabus.Options{ProduceTimeout: 3 * time.Second})
	require.Error(t, err)
	require.Equal(t, errs.CodeProviderUnavailable, errs.CodeOf(err))
	require.Contains(t, err.Error(), "unreachable")
}

func TestNew_RequiresBrokers(t *testing.T) {
	t.Parallel()
	_, err := redpandabus.New(context.Background(), config.RedpandaConfig{}, testResolver(), redpandabus.Options{})
	require.ErrorIs(t, err, redpandabus.ErrBrokersRequired)
}

func TestNew_UnsupportedSASLMechanismIsRefusedAndNeverEchoesTheSecret(t *testing.T) {
	t.Parallel()
	const password = "s3cr3t-do-not-log"
	_, err := redpandabus.New(context.Background(), config.RedpandaConfig{
		Brokers:         []string{closedAddr(t)},
		SASLMechanism:   "GSSAPI",
		SASLUsernameRef: config.SecretRef("cp"),
		SASLPasswordRef: config.SecretRef(password),
	}, config.NewResolver(config.EnvTest, func(string) (string, bool) { return "", false }),
		redpandabus.Options{ProduceTimeout: time.Second})
	require.Error(t, err)
	require.Contains(t, err.Error(), "GSSAPI")
	require.NotContains(t, err.Error(), password, "credentials never reach an error message")
}

// Open is the composition root's only choice point, and it never substitutes
// one bus for the other.
func TestOpen_SelectsByModeAndNeverFallsBack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	bus, err := redpandabus.Open(ctx, config.EnvLocal, config.ProviderModeFake, config.RedpandaConfig{}, nil, redpandabus.Options{})
	require.NoError(t, err)
	require.IsType(t, &redpandabus.Loopback{}, bus, "fake mode is the loopback")
	require.NoError(t, bus.Close(ctx))

	_, err = redpandabus.Open(ctx, config.EnvProd, config.ProviderModeFake, config.RedpandaConfig{}, nil, redpandabus.Options{})
	require.ErrorIs(t, err, redpandabus.ErrFakeModeForbidden, "production never gets a fake bus")

	// Live mode against a dead broker fails; it does not quietly become a
	// loopback that swallows every event.
	_, err = redpandabus.Open(ctx, config.EnvDev, config.ProviderModeLive,
		config.RedpandaConfig{Brokers: []string{closedAddr(t)}}, testResolver(),
		redpandabus.Options{ProduceTimeout: 3 * time.Second})
	require.Error(t, err)
	require.Equal(t, errs.CodeProviderUnavailable, errs.CodeOf(err))

	_, err = redpandabus.Open(ctx, config.EnvLocal, config.ProviderMode("shadow"), config.RedpandaConfig{}, nil, redpandabus.Options{})
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "unknown provider mode"), err.Error())
}
