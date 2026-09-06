package devidp_test

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/nodal/controlplane/internal/auth"
	"github.com/nodal/controlplane/internal/auth/devidp"
	"github.com/nodal/controlplane/internal/security"
)

var t0 = time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

// TestDevIdPRefusesProd is the guard the rest of the system relies on:
// construction fails closed for every env except the three dev ones.
func TestDevIdPRefusesProd(t *testing.T) {
	refused := []string{"PROD", "STAGING", "", "prod", "Prod", "staging", "local", "Local", " LOCAL", "LOCAL ", "LOCAL\n", "DEVELOPMENT", "PRODUCTION", "UNKNOWN", "LOCAL,PROD"}
	for _, env := range refused {
		t.Run("refuses "+strings.ReplaceAll(env, "\n", "\\n"), func(t *testing.T) {
			p, err := devidp.New(env, devidp.Config{})
			if !errors.Is(err, auth.ErrDevIdPNotAllowed) {
				t.Fatalf("env %q: err = %v", env, err)
			}
			if p != nil {
				t.Fatal("provider returned alongside error")
			}
			if devidp.Allowed(env) {
				t.Fatal("Allowed reported true")
			}
		})
	}
	for _, env := range []string{"LOCAL", "TEST", "DEV"} {
		t.Run("allows "+env, func(t *testing.T) {
			p, err := devidp.New(env, devidp.Config{})
			if err != nil || p == nil || p.Env() != env || p.Name() != devidp.Name {
				t.Fatalf("env %q: p=%v err=%v", env, p, err)
			}
		})
	}
}

// TestNoInitFunction enforces the init()-free design: the package cannot
// activate itself at import time, so New's env gate is the only path.
func TestNoInitFunction(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	initRe := regexp.MustCompile(`(?m)^func\s+init\s*\(`)
	registerRe := regexp.MustCompile(`(?m)^var\s+_\s*=\s*\w+\(`)
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatal(err)
		}
		if initRe.Match(src) {
			t.Errorf("%s declares an init function", name)
		}
		if registerRe.Match(src) {
			t.Errorf("%s registers something at package init via var _ = f()", name)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no source files checked")
	}
}

func newProvider(t *testing.T) *devidp.Provider {
	t.Helper()
	p, err := devidp.New("TEST", devidp.Config{Now: func() time.Time { return t0 }})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAuthCodeURL(t *testing.T) {
	p := newProvider(t)
	u, err := url.Parse(p.AuthCodeURL("st", "nn", "ch", false))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Path != devidp.DefaultLoginURL || q.Get("state") != "st" || q.Get("nonce") != "nn" || q.Get("code_challenge") != "ch" || q.Get("code_challenge_method") != "S256" {
		t.Fatalf("url %s", u)
	}
	if q.Has("prompt") || q.Has("acr_values") {
		t.Fatal("step-up params without stepUp")
	}
	u, _ = url.Parse(p.AuthCodeURL("st", "nn", "ch", true))
	if u.Query().Get("prompt") != "login" || u.Query().Get("acr_values") != "phr" {
		t.Fatalf("step-up params missing: %s", u)
	}
	custom, _ := devidp.New("LOCAL", devidp.Config{LoginURL: "http://localhost:3000/dev?x=1"})
	if got := custom.AuthCodeURL("s", "n", "c", false); !strings.HasPrefix(got, "http://localhost:3000/dev?x=1&") {
		t.Fatalf("custom login url: %s", got)
	}
}

func TestExchange_FixedIdentities(t *testing.T) {
	p := newProvider(t)
	ctx := context.Background()
	wantRoles := map[string]security.Role{
		"customer-a": security.RoleCustomer, "customer-b": security.RoleCustomer, "support": security.RoleSupportReadOnly,
		"operations": security.RoleOperations, "risk": security.RoleRisk, "compliance": security.RoleCompliance,
		"finance": security.RoleFinance, "security": security.RoleSecurity, "admin": security.RoleAdmin,
	}
	names := devidp.Identities()
	if len(names) != len(wantRoles) {
		t.Fatalf("identities %v", names)
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			id, err := p.Exchange(ctx, name, "verifier", "nonce-1")
			if err != nil {
				t.Fatal(err)
			}
			if id.Subject != "dev:"+name || !id.EmailVerified || !id.AuthTime.Equal(t0) || id.Claims["nonce"] != "nonce-1" {
				t.Fatalf("identity %+v", id)
			}
			if security.HasStrongAMR(id.AMR) || id.ACR != "" {
				t.Fatalf("plain code must not be strong: %+v", id)
			}
			roles := devidp.Roles(id)
			if len(roles) != 1 || roles[0] != wantRoles[name] {
				t.Fatalf("roles %v", roles)
			}
			accts := devidp.AccountIDs(id)
			if strings.HasPrefix(name, "customer") && (len(accts) != 1 || accts[0] != "acct-dev-"+strings.TrimPrefix(name, "customer-")) {
				t.Fatalf("accounts %v", accts)
			}
			if !strings.HasPrefix(name, "customer") && len(accts) != 0 {
				t.Fatalf("operator has accounts %v", accts)
			}
			pr := security.Principal{SubjectID: id.Subject, ActorType: security.ActorUser, Roles: roles, AccountIDs: accts}
			if err := pr.Validate(); err != nil {
				t.Fatal(err)
			}
		})
	}
	// Customers cannot see each other; admin sees both.
	a, _ := p.Exchange(ctx, "customer-a", "v", "n")
	adm, _ := p.Exchange(ctx, "admin", "v", "n")
	pa := security.Principal{SubjectID: a.Subject, ActorType: security.ActorUser, Roles: devidp.Roles(a), AccountIDs: devidp.AccountIDs(a)}
	padm := security.Principal{SubjectID: adm.Subject, ActorType: security.ActorOperator, Roles: devidp.Roles(adm)}
	if err := security.RequireAccount(security.WithPrincipal(ctx, pa), "acct-dev-b"); !errors.Is(err, security.ErrCrossTenant) {
		t.Fatalf("customer-a reached acct-dev-b: %v", err)
	}
	if err := security.RequireAccount(security.WithPrincipal(ctx, padm), "acct-dev-b"); err != nil {
		t.Fatal(err)
	}
}

func TestExchange_MFAAndErrors(t *testing.T) {
	p := newProvider(t)
	ctx := context.Background()
	id, err := p.Exchange(ctx, "admin:mfa", "v", "n")
	if err != nil {
		t.Fatal(err)
	}
	if !security.HasStrongAMR(id.AMR) || id.ACR != "phr" || id.Subject != "dev:admin" {
		t.Fatalf("mfa identity %+v", id)
	}
	pr := security.Principal{SubjectID: id.Subject, ActorType: security.ActorOperator, Roles: devidp.Roles(id), AuthTime: id.AuthTime, AMR: id.AMR}
	if err := security.RequireStepUp(security.WithPrincipal(ctx, pr), time.Minute, func() time.Time { return t0 }); err != nil {
		t.Fatal(err)
	}

	for name, args := range map[string][3]string{
		"unknown identity": {"root", "v", "n"},
		"unknown suffix":   {"admin:sudo", "v", "n"},
		"empty code":       {"", "v", "n"},
		"empty verifier":   {"admin", "", "n"},
		"empty nonce":      {"admin", "v", ""},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := p.Exchange(ctx, args[0], args[1], args[2]); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	if _, err := p.Exchange(ctx, "root", "v", "n"); !errors.Is(err, devidp.ErrUnknownIdentity) {
		t.Fatalf("unknown identity error: %v", err)
	}
	if len(devidp.Roles(auth.Identity{})) != 0 || len(devidp.AccountIDs(auth.Identity{})) != 0 {
		t.Fatal("helpers leaked roles for a foreign identity")
	}
}
