package idempotency

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStore_ValidatesBeforeTouchingTheTransaction(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := NewStore(nil)
	long := strings.Repeat("k", MaxKeyLength+1)

	cases := []struct {
		name                       string
		actor, endpoint, key, hash string
		ttl                        time.Duration
	}{
		{"empty actor", "", "POST /v1/x", "k", "h", time.Hour},
		{"empty endpoint", "a", "", "k", "h", time.Hour},
		{"empty key", "a", "e", "", "h", time.Hour},
		{"oversized key", "a", "e", long, "h", time.Hour},
		{"empty hash", "a", "e", "k", "", time.Hour},
		{"zero ttl", "a", "e", "k", "h", 0},
		{"negative ttl", "a", "e", "k", "h", -time.Second},
		{"nil tx", "a", "e", "k", "h", time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := s.Begin(ctx, nil, tc.actor, tc.endpoint, tc.key, tc.hash, tc.ttl)
			require.ErrorIs(t, err, ErrInvalidArgument)
			assert.Nil(t, got)
		})
	}

	require.ErrorIs(t, s.Complete(ctx, nil, "", "e", "k", 200, "", "", nil), ErrInvalidArgument)
	require.ErrorIs(t, s.Complete(ctx, nil, "a", "e", "k", 200, "", "", []byte("{not json")), ErrInvalidArgument)
	require.ErrorIs(t, s.Complete(ctx, nil, "a", "e", "k", 200, "", "", []byte("{}")), ErrInvalidArgument, "nil tx")
	require.ErrorIs(t, s.Fail(ctx, nil, "a", "", "k", 500, nil), ErrInvalidArgument)
	require.ErrorIs(t, s.Fail(ctx, nil, "a", "e", "k", 500, []byte("x")), ErrInvalidArgument)
	_, _, err := s.Get(ctx, nil, "a", "e", "")
	require.ErrorIs(t, err, ErrInvalidArgument)
}

func TestNewStore_ClockIsUTC(t *testing.T) {
	t.Parallel()
	loc := time.FixedZone("X", 3600)
	fixed := time.Date(2026, 9, 5, 12, 0, 0, 0, loc)
	s := NewStore(func() time.Time { return fixed })
	assert.Equal(t, time.UTC, s.now().Location())
	assert.True(t, s.now().Equal(fixed))
	assert.Equal(t, time.UTC, NewStore(nil).now().Location())
}

func TestBegunIsSealed(t *testing.T) {
	t.Parallel()
	var b Begun = Acquired{}
	_, ok := b.(Acquired)
	assert.True(t, ok)
	b = Replay{}
	_, ok = b.(Replay)
	assert.True(t, ok)
	b = InProgress{}
	_, ok = b.(InProgress)
	assert.True(t, ok)
}
