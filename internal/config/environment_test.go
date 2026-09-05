package config

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

func TestParseEnvironment(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in      string
		want    Environment
		wantErr bool
	}{
		{"LOCAL", EnvLocal, false},
		{"TEST", EnvTest, false},
		{"DEV", EnvDev, false},
		{"STAGING", EnvStaging, false},
		{"PROD", EnvProd, false},
		{"prod", EnvProd, false},
		{"  Staging\n", EnvStaging, false},
		{"", "", true},
		{"   ", "", true},
		{"PRODUCTION", "", true},
		{"production", "", true},
		{"LOCAL-DEV", "", true},
		{"DEVELOPMENT", "", true},
		{"PROD;", "", true},
		{"LOCALX", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got, err := ParseEnvironment(tc.in)
			if tc.wantErr {
				require.Error(t, err)
				assert.ErrorIs(t, err, ErrUnknownEnvironment)
				assert.Equal(t, Environment(""), got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
			assert.True(t, got.IsValid())
		})
	}
}

func TestEnvironment_Predicates(t *testing.T) {
	t.Parallel()
	cases := []struct {
		env                                                                   Environment
		prodLike, defaults, fakeProviders, plainSecrets, fileSecrets, devAuth bool
	}{
		{EnvLocal, false, true, true, true, true, true},
		{EnvTest, false, true, true, true, true, true},
		{EnvDev, false, false, true, false, true, true},
		{EnvStaging, true, false, false, false, false, false},
		{EnvProd, true, false, false, false, false, false},
	}
	for _, tc := range cases {
		t.Run(string(tc.env), func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.prodLike, tc.env.IsProductionLike(), "IsProductionLike")
			assert.Equal(t, tc.defaults, tc.env.AllowsDefaults(), "AllowsDefaults")
			assert.Equal(t, tc.fakeProviders, tc.env.AllowsFakeProviders(), "AllowsFakeProviders")
			assert.Equal(t, tc.plainSecrets, tc.env.AllowsPlainSecrets(), "AllowsPlainSecrets")
			assert.Equal(t, tc.fileSecrets, tc.env.AllowsFileSecrets(), "AllowsFileSecrets")
			assert.Equal(t, tc.devAuth, tc.env.AllowsDevAuth(), "AllowsDevAuth")
		})
	}
	// The zero value is never valid and never permissive.
	var zero Environment
	assert.False(t, zero.IsValid())
	assert.False(t, zero.AllowsDefaults())
	assert.False(t, zero.AllowsFakeProviders())
	assert.False(t, zero.AllowsPlainSecrets())
	assert.False(t, zero.IsProductionLike())
}

func TestProp_ParseEnvironmentFailsClosed(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		s := rapid.String().Draw(rt, "s")
		got, err := ParseEnvironment(s)
		if err != nil {
			if !errors.Is(err, ErrUnknownEnvironment) {
				rt.Fatalf("unexpected error type: %v", err)
			}
			if got != "" {
				rt.Fatalf("error with non-empty value %q", got)
			}
			return
		}
		if !got.IsValid() {
			rt.Fatalf("parsed %q into invalid environment %q", s, got)
		}
	})
}
