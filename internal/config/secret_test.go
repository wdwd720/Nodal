package config

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

func TestParseSecretRef(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		in      string
		want    ParsedSecretRef
		wantErr error
	}{
		{"empty", "", ParsedSecretRef{Scheme: SecretSchemePlain}, nil},
		{"env", "env://DB_URL", ParsedSecretRef{SecretSchemeEnv, "DB_URL"}, nil},
		{"env underscore", "env://_x1", ParsedSecretRef{SecretSchemeEnv, "_x1"}, nil},
		{"env scheme case-insensitive", "ENV://DB_URL", ParsedSecretRef{SecretSchemeEnv, "DB_URL"}, nil},
		{"env empty name", "env://", ParsedSecretRef{}, ErrInvalidSecretRef},
		{"env bad name", "env://9abc", ParsedSecretRef{}, ErrInvalidSecretRef},
		{"env name with space", "env://A B", ParsedSecretRef{}, ErrInvalidSecretRef},
		{"aws name", "aws-sm://cp/prod/db", ParsedSecretRef{SecretSchemeAWSSM, "cp/prod/db"}, nil},
		{
			"aws arn", "aws-sm://arn:aws:secretsmanager:us-east-1:123456789012:secret:cp/prod/db-AbCdEf",
			ParsedSecretRef{SecretSchemeAWSSM, "arn:aws:secretsmanager:us-east-1:123456789012:secret:cp/prod/db-AbCdEf"},
			nil,
		},
		{"aws empty", "aws-sm://", ParsedSecretRef{}, ErrInvalidSecretRef},
		{"aws space", "aws-sm://a b", ParsedSecretRef{}, ErrInvalidSecretRef},
		{"file posix", "file:///run/secrets/db", ParsedSecretRef{SecretSchemeFile, "/run/secrets/db"}, nil},
		{"file windows", "file://C:/secrets/db.txt", ParsedSecretRef{SecretSchemeFile, "C:/secrets/db.txt"}, nil},
		{"file empty", "file://", ParsedSecretRef{}, ErrInvalidSecretRef},
		{"file newline", "file:///a\nb", ParsedSecretRef{}, ErrInvalidSecretRef},
		{"plain postgres url", "postgres://u:p@h/db", ParsedSecretRef{SecretSchemePlain, "postgres://u:p@h/db"}, nil},
		{"plain word", "hunter2", ParsedSecretRef{SecretSchemePlain, "hunter2"}, nil},
		{"plain with colon", "user:pass", ParsedSecretRef{SecretSchemePlain, "user:pass"}, nil},
		{"plain slashes", "a//b", ParsedSecretRef{SecretSchemePlain, "a//b"}, nil},
		{"unrecognised prefix is plain", "vault://kv/db", ParsedSecretRef{SecretSchemePlain, "vault://kv/db"}, nil},
		{"near miss prefix is plain", "envs://X", ParsedSecretRef{SecretSchemePlain, "envs://X"}, nil},
		{"nul", "abc\x00def", ParsedSecretRef{}, ErrInvalidSecretRef},
		{"too long", strings.Repeat("x", maxSecretRefLen+1), ParsedSecretRef{}, ErrInvalidSecretRef},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseSecretRef(tc.in)
			if tc.wantErr != nil {
				require.Error(t, err)
				assert.ErrorIs(t, err, tc.wantErr)
				assert.Equal(t, ParsedSecretRef{}, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
			again, err := ParseSecretRef(got.String())
			require.NoError(t, err)
			assert.Equal(t, got, again)
		})
	}
}

func TestSecretRef_ValidateFor(t *testing.T) {
	t.Parallel()
	type row struct {
		ref  SecretRef
		envs map[Environment]bool // allowed?
	}
	all := func(v bool) map[Environment]bool {
		return map[Environment]bool{EnvLocal: v, EnvTest: v, EnvDev: v, EnvStaging: v, EnvProd: v}
	}
	localTestOnly := map[Environment]bool{EnvLocal: true, EnvTest: true, EnvDev: false, EnvStaging: false, EnvProd: false}
	rows := []row{
		{"", all(true)},
		{"env://X", all(true)},
		{"aws-sm://cp/x", all(true)},
		{"file:///run/secrets/x", map[Environment]bool{EnvLocal: true, EnvTest: true, EnvDev: true, EnvStaging: false, EnvProd: false}},
		{"plain-value", localTestOnly},
		{"postgres://cp_app:cp_app_local@127.0.0.1:5433/controlplane?sslmode=disable", localTestOnly},
		{"vault://x", localTestOnly},
		{"env://", all(false)},
	}
	for _, r := range rows {
		for env, ok := range r.envs {
			t.Run(string(r.ref)+"/"+string(env), func(t *testing.T) {
				t.Parallel()
				err := r.ref.ValidateFor(env)
				if ok {
					assert.NoError(t, err)
				} else {
					assert.Error(t, err)
				}
			})
		}
	}
}

func TestSecretRef_Redacted(t *testing.T) {
	t.Parallel()
	assert.Equal(t, SecretRef(""), SecretRef("").Redacted())
	assert.Equal(t, SecretRef("env://X"), SecretRef("env://X").Redacted())
	assert.Equal(t, SecretRef("aws-sm://a/b"), SecretRef("aws-sm://a/b").Redacted())
	assert.Equal(t, SecretRef("file:///x"), SecretRef("file:///x").Redacted())
	assert.Equal(t, SecretRef(RedactedMarker), SecretRef("hunter2").Redacted())
	assert.Equal(t, SecretRef(RedactedMarker), SecretRef("postgres://u:p@h/db").Redacted())
	assert.Equal(t, SecretRef(RedactedMarker), SecretRef("env://").Redacted())
	assert.Equal(t, RedactedMarker, SecretRef("hunter2").LogValue().String())
	assert.Equal(t, "env://X", SecretRef("env://X").LogValue().String())
	assert.Equal(t, SecretSchemeEnv, SecretRef("env://X").Scheme())
	assert.Equal(t, SecretSchemePlain, SecretRef("vault://X").Scheme())
	assert.Equal(t, SecretScheme(""), SecretRef("env://").Scheme())
}

func TestResolvers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lookup := LookupFromMap(map[string]string{"DB_URL": "postgres://x"})

	t.Run("env resolves", func(t *testing.T) {
		t.Parallel()
		v, err := EnvResolver{Lookup: lookup}.Resolve(ctx, "env://DB_URL")
		require.NoError(t, err)
		assert.Equal(t, "postgres://x", v)
	})
	t.Run("env missing", func(t *testing.T) {
		t.Parallel()
		_, err := EnvResolver{Lookup: lookup}.Resolve(ctx, "env://NOPE")
		assert.ErrorIs(t, err, ErrSecretNotFound)
	})
	t.Run("env unsupported scheme", func(t *testing.T) {
		t.Parallel()
		_, err := EnvResolver{Lookup: lookup}.Resolve(ctx, "aws-sm://x")
		assert.ErrorIs(t, err, ErrUnsupportedSecretRef)
	})
	t.Run("empty ref", func(t *testing.T) {
		t.Parallel()
		_, err := EnvResolver{Lookup: lookup}.Resolve(ctx, "")
		assert.ErrorIs(t, err, ErrEmptySecretRef)
	})
	t.Run("plain allowed in LOCAL", func(t *testing.T) {
		t.Parallel()
		pr, err := NewPlainResolver(EnvLocal)
		require.NoError(t, err)
		v, err := pr.Resolve(ctx, "hunter2")
		require.NoError(t, err)
		assert.Equal(t, "hunter2", v)
		_, err = pr.Resolve(ctx, "env://X")
		assert.ErrorIs(t, err, ErrUnsupportedSecretRef)
	})
	t.Run("plain refused in DEV/STAGING/PROD", func(t *testing.T) {
		t.Parallel()
		for _, env := range []Environment{EnvDev, EnvStaging, EnvProd} {
			_, err := NewPlainResolver(env)
			assert.ErrorIs(t, err, ErrSecretSchemeNotAllowed, env)
		}
		_, err := PlainResolver{}.Resolve(ctx, "x")
		assert.ErrorIs(t, err, ErrSecretSchemeNotAllowed)
	})
	t.Run("file resolves and trims newline", func(t *testing.T) {
		t.Parallel()
		fr, err := NewFileResolver(EnvDev)
		require.NoError(t, err)
		fr.ReadFile = func(p string) ([]byte, error) {
			assert.Equal(t, "/run/secrets/x", p)
			return []byte("value\n"), nil
		}
		v, err := fr.Resolve(ctx, "file:///run/secrets/x")
		require.NoError(t, err)
		assert.Equal(t, "value", v)
		_, err = fr.Resolve(ctx, "env://X")
		assert.ErrorIs(t, err, ErrUnsupportedSecretRef)
	})
	t.Run("file refused in STAGING/PROD", func(t *testing.T) {
		t.Parallel()
		for _, env := range []Environment{EnvStaging, EnvProd} {
			_, err := NewFileResolver(env)
			assert.ErrorIs(t, err, ErrSecretSchemeNotAllowed, env)
		}
		var nilFR *FileResolver
		_, err := nilFR.Resolve(ctx, "file:///x")
		assert.ErrorIs(t, err, ErrSecretSchemeNotAllowed)
	})
	t.Run("chain falls through unsupported and stops on real errors", func(t *testing.T) {
		t.Parallel()
		pr, _ := NewPlainResolver(EnvTest)
		chain := ChainResolver{nil, pr, EnvResolver{Lookup: lookup}}
		v, err := chain.Resolve(ctx, "env://DB_URL")
		require.NoError(t, err)
		assert.Equal(t, "postgres://x", v)
		v, err = chain.Resolve(ctx, "plain")
		require.NoError(t, err)
		assert.Equal(t, "plain", v)
		_, err = chain.Resolve(ctx, "env://MISSING")
		assert.ErrorIs(t, err, ErrSecretNotFound)
		_, err = chain.Resolve(ctx, "aws-sm://x")
		assert.ErrorIs(t, err, ErrUnsupportedSecretRef)
		_, err = chain.Resolve(ctx, "env://")
		assert.ErrorIs(t, err, ErrInvalidSecretRef)
		_, err = chain.Resolve(ctx, "")
		assert.ErrorIs(t, err, ErrEmptySecretRef)
	})
	t.Run("NewResolver composition per environment", func(t *testing.T) {
		t.Parallel()
		assert.Len(t, NewResolver(EnvLocal, lookup), 3)
		assert.Len(t, NewResolver(EnvTest, lookup), 3)
		assert.Len(t, NewResolver(EnvDev, lookup), 2)
		assert.Len(t, NewResolver(EnvStaging, lookup), 1)
		assert.Len(t, NewResolver(EnvProd, lookup), 1)
		_, err := NewResolver(EnvProd, lookup).Resolve(ctx, "plain")
		assert.ErrorIs(t, err, ErrUnsupportedSecretRef)
		_, err = NewResolver(EnvProd, lookup).Resolve(ctx, "file:///x")
		assert.ErrorIs(t, err, ErrUnsupportedSecretRef)
	})
}

func TestProp_SecretRefRoundTrip(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		s := rapid.String().Draw(rt, "s")
		p, err := ParseSecretRef(s)
		if err != nil {
			return
		}
		again, err := ParseSecretRef(p.String())
		if err != nil {
			rt.Fatalf("re-parse of %q failed: %v", p.String(), err)
		}
		if again != p {
			rt.Fatalf("round trip mismatch: %#v vs %#v", p, again)
		}
		if p.Scheme == SecretSchemePlain && p.Target != s {
			rt.Fatalf("plain value altered: %q -> %q", s, p.Target)
		}
	})
}

func FuzzParseSecretRef(f *testing.F) {
	for _, seed := range []string{
		"", "env://X", "ENV://x_1", "aws-sm://cp/prod/db", "aws-sm://arn:aws:secretsmanager:us-east-1:1:secret:a-b",
		"file:///run/secrets/x", "file://C:/x", "vault://x", "plain", "a://", "://", "env://",
		"aws-sm://", "file://", "\x00", "env://a\x00", "x\ny", "postgres://u:p@h/db", strings.Repeat("a", 5000),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		p, err := ParseSecretRef(s)
		if err != nil {
			if p != (ParsedSecretRef{}) {
				t.Fatalf("error with non-zero result: %#v", p)
			}
			if !errors.Is(err, ErrInvalidSecretRef) {
				t.Fatalf("unexpected error kind: %v", err)
			}
			return
		}
		if !utf8.ValidString(s) {
			// Invalid UTF-8 must be handled without panicking; the
			// round-trip guarantee below applies to valid strings.
			return
		}
		again, err := ParseSecretRef(p.String())
		if err != nil {
			t.Fatalf("re-parse failed for %q: %v", p.String(), err)
		}
		if again != p {
			t.Fatalf("round trip mismatch for %q: %#v vs %#v", s, p, again)
		}
		if p.Scheme == SecretSchemePlain && s != "" && SecretRef(s).Redacted() != RedactedMarker {
			t.Fatalf("plain value leaked through Redacted: %q", s)
		}
		for _, env := range append(Environments(), Environment("bogus")) {
			_ = SecretRef(s).ValidateFor(env)
		}
	})
}
