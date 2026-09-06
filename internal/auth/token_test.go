package auth_test

import (
	"encoding/base64"
	"errors"
	"regexp"
	"testing"

	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/auth"
	"github.com/nodal/controlplane/internal/security"
)

var hexRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

func TestNewToken_Shape(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		raw, err := auth.NewToken()
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) != 43 {
			t.Fatalf("token length %d, want 43", len(raw))
		}
		b, err := base64.RawURLEncoding.DecodeString(raw)
		if err != nil || len(b) != 32 {
			t.Fatalf("token is not base64url of 32 bytes: %v", err)
		}
		if err := auth.ValidateToken(raw); err != nil {
			t.Fatalf("fresh token rejected: %v", err)
		}
		if seen[raw] {
			t.Fatal("duplicate token")
		}
		seen[raw] = true
		h := auth.HashToken(raw)
		if !hexRe.MatchString(h) || h == raw {
			t.Fatalf("bad hash %q", h)
		}
	}
}

func TestValidateToken_Rejects(t *testing.T) {
	raw, _ := auth.NewToken()
	for name, bad := range map[string]string{
		"empty":         "",
		"short":         raw[:42],
		"long":          raw + "A",
		"padded":        raw[:42] + "=",
		"std base64":    "+" + raw[1:],
		"whitespace":    " " + raw[1:],
		"slash":         "/" + raw[1:],
		"non-canonical": raw[:42] + "/",
	} {
		t.Run(name, func(t *testing.T) {
			err := auth.ValidateToken(bad)
			if !errors.Is(err, auth.ErrInvalidToken) || !errors.Is(err, security.ErrUnauthenticated) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestHashToken_Deterministic(t *testing.T) {
	a := auth.HashToken("abc")
	if a != auth.HashToken("abc") || a == auth.HashToken("abd") {
		t.Fatal("hash not deterministic or not distinct")
	}
	if a != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("sha256 mismatch: %s", a)
	}
}

func TestProp_ValidateTokenOnlyAcceptsCanonical(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		s := rapid.OneOf(
			rapid.String(),
			rapid.StringMatching(`[A-Za-z0-9_-]{40,46}`),
			rapid.StringMatching(`[A-Za-z0-9_-]{43}`),
		).Draw(rt, "s")
		err := auth.ValidateToken(s)
		b, decErr := base64.RawURLEncoding.DecodeString(s)
		canonical := len(s) == 43 && decErr == nil && len(b) == 32
		if canonical != (err == nil) {
			rt.Fatalf("ValidateToken(%q) = %v, canonical=%v", s, err, canonical)
		}
		if h := auth.HashToken(s); !hexRe.MatchString(h) {
			rt.Fatalf("hash %q not lowercase hex", h)
		}
	})
}
