package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"unicode"
)

// SecretRef is a reference to a secret value. Supported forms:
//
//	env://NAME          value of the environment variable NAME
//	aws-sm://name|arn   AWS Secrets Manager secret (resolver lives in a future package)
//	file://path         contents of a file (LOCAL, TEST and DEV only)
//	<plain value>       the literal value (LOCAL and TEST only)
//
// A SecretRef is never included in Config.Hash and plain values are masked by
// Config.Redacted. Only the three reference prefixes above are recognized
// (case-insensitively); any other value, including URLs such as
// postgres://..., is a plain value, and plain values are rejected outside
// LOCAL/TEST so a mistyped reference can never reach a deployed environment.
type SecretRef string

// SecretScheme identifies how a SecretRef is resolved.
type SecretScheme string

const (
	// SecretSchemePlain is a literal value (LOCAL/TEST only).
	SecretSchemePlain SecretScheme = "plain"
	// SecretSchemeEnv reads an environment variable.
	SecretSchemeEnv SecretScheme = "env"
	// SecretSchemeAWSSM reads from AWS Secrets Manager.
	SecretSchemeAWSSM SecretScheme = "aws-sm"
	// SecretSchemeFile reads a file (LOCAL/TEST/DEV only).
	SecretSchemeFile SecretScheme = "file"
)

// ParsedSecretRef is the decoded form of a SecretRef.
type ParsedSecretRef struct {
	Scheme SecretScheme
	// Target is the variable name, secret name/ARN, file path, or the plain
	// value itself. Empty only for the zero reference.
	Target string
}

// String re-encodes the reference. ParseSecretRef(p.String()) == p.
func (p ParsedSecretRef) String() string {
	if p.Scheme == SecretSchemePlain || p.Scheme == "" {
		return p.Target
	}
	return string(p.Scheme) + "://" + p.Target
}

// IsZero reports whether the reference is empty.
func (p ParsedSecretRef) IsZero() bool { return p.Target == "" }

// Sentinel errors for SecretRef handling.
var (
	ErrInvalidSecretRef       = errors.New("config: invalid secret reference")
	ErrSecretSchemeNotAllowed = errors.New("config: secret reference scheme not allowed in this environment")
	ErrEmptySecretRef         = errors.New("config: empty secret reference")
	ErrSecretNotFound         = errors.New("config: secret not found")
	ErrUnsupportedSecretRef   = errors.New("config: resolver does not support this secret reference")
)

const maxSecretRefLen = 4096

var (
	envNameRe   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	awsSMNameRe = regexp.MustCompile(`^[A-Za-z0-9/_+=.@:-]+$`)
)

// referenceSchemes are the only prefixes that make a value a reference.
var referenceSchemes = []SecretScheme{SecretSchemeEnv, SecretSchemeAWSSM, SecretSchemeFile}

func splitScheme(s string) (SecretScheme, string, bool) {
	for _, sc := range referenceSchemes {
		prefix := string(sc) + "://"
		if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
			return sc, s[len(prefix):], true
		}
	}
	return SecretSchemePlain, s, false
}

// ParseSecretRef decodes s. It never panics on any input. Plain values are
// accepted here; whether they are permitted is decided by ValidateFor.
func ParseSecretRef(s string) (ParsedSecretRef, error) {
	if s == "" {
		return ParsedSecretRef{Scheme: SecretSchemePlain}, nil
	}
	if len(s) > maxSecretRefLen {
		return ParsedSecretRef{}, fmt.Errorf("%w: longer than %d bytes", ErrInvalidSecretRef, maxSecretRefLen)
	}
	if strings.ContainsRune(s, 0) {
		return ParsedSecretRef{}, fmt.Errorf("%w: contains NUL", ErrInvalidSecretRef)
	}
	scheme, target, isRef := splitScheme(s)
	if !isRef {
		return ParsedSecretRef{Scheme: SecretSchemePlain, Target: s}, nil
	}
	switch scheme {
	case SecretSchemeEnv:
		if !envNameRe.MatchString(target) {
			return ParsedSecretRef{}, fmt.Errorf("%w: env:// requires a variable name matching %s", ErrInvalidSecretRef, envNameRe)
		}
	case SecretSchemeAWSSM:
		if target == "" || len(target) > 2048 || !awsSMNameRe.MatchString(target) {
			return ParsedSecretRef{}, fmt.Errorf("%w: aws-sm:// requires a secret name or ARN", ErrInvalidSecretRef)
		}
	case SecretSchemeFile:
		if target == "" || hasControl(target) {
			return ParsedSecretRef{}, fmt.Errorf("%w: file:// requires a path without control characters", ErrInvalidSecretRef)
		}
	}
	return ParsedSecretRef{Scheme: scheme, Target: target}, nil
}

func hasControl(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

// Parse decodes the reference.
func (r SecretRef) Parse() (ParsedSecretRef, error) { return ParseSecretRef(string(r)) }

// IsZero reports whether the reference is empty.
func (r SecretRef) IsZero() bool { return r == "" }

// Scheme returns the scheme, or "" if the reference does not parse.
func (r SecretRef) Scheme() SecretScheme {
	p, err := r.Parse()
	if err != nil {
		return ""
	}
	return p.Scheme
}

// ValidateFor checks that the reference parses and that its scheme is
// permitted in env: plain values only in LOCAL/TEST, file:// only in
// LOCAL/TEST/DEV. The zero reference is always valid (presence is checked by
// the owning field's rule).
func (r SecretRef) ValidateFor(env Environment) error {
	if r.IsZero() {
		return nil
	}
	p, err := r.Parse()
	if err != nil {
		return err
	}
	switch p.Scheme {
	case SecretSchemePlain:
		if !env.AllowsPlainSecrets() {
			return fmt.Errorf("%w: plain values are only allowed in LOCAL/TEST (use env:// or aws-sm://)", ErrSecretSchemeNotAllowed)
		}
	case SecretSchemeFile:
		if !env.AllowsFileSecrets() {
			return fmt.Errorf("%w: file:// is only allowed in LOCAL/TEST/DEV", ErrSecretSchemeNotAllowed)
		}
	}
	return nil
}

// RedactedMarker replaces secret material in logs and redacted copies.
const RedactedMarker = "[REDACTED]"

// Redacted returns a form safe for logs: references stay as they are (they
// name a location, not a value); plain or unparsable values become
// RedactedMarker.
func (r SecretRef) Redacted() SecretRef {
	if r.IsZero() {
		return r
	}
	p, err := r.Parse()
	if err != nil || p.Scheme == SecretSchemePlain {
		return RedactedMarker
	}
	return r
}

// LogValue implements slog.LogValuer so that a SecretRef logged directly is
// always redacted.
func (r SecretRef) LogValue() fmt.Stringer { return redactedStringer(r.Redacted()) }

type redactedStringer string

func (s redactedStringer) String() string { return string(s) }

// Resolver turns a SecretRef into its value. Implementations return
// ErrUnsupportedSecretRef (wrapped) for schemes they do not handle so that a
// ChainResolver can try the next one. An AWS Secrets Manager resolver lives
// in a separate package; only the interface is defined here.
type Resolver interface {
	Resolve(ctx context.Context, ref SecretRef) (string, error)
}

// EnvResolver resolves env:// references through Lookup (os.LookupEnv when
// nil).
type EnvResolver struct {
	Lookup func(string) (string, bool)
}

// Resolve implements Resolver.
func (e EnvResolver) Resolve(_ context.Context, ref SecretRef) (string, error) {
	p, err := prepareRef(ref)
	if err != nil {
		return "", err
	}
	if p.Scheme != SecretSchemeEnv {
		return "", fmt.Errorf("%w: EnvResolver: %s", ErrUnsupportedSecretRef, p.Scheme)
	}
	lookup := e.Lookup
	if lookup == nil {
		lookup = os.LookupEnv
	}
	v, ok := lookup(p.Target)
	if !ok {
		return "", fmt.Errorf("%w: environment variable %s is not set", ErrSecretNotFound, p.Target)
	}
	return v, nil
}

// PlainResolver returns plain values verbatim. NewPlainResolver refuses to
// construct one outside LOCAL/TEST, and the zero value refuses to resolve.
type PlainResolver struct{ env Environment }

// NewPlainResolver returns a PlainResolver or an error if env does not allow
// plain secrets.
func NewPlainResolver(env Environment) (PlainResolver, error) {
	if !env.AllowsPlainSecrets() {
		return PlainResolver{}, fmt.Errorf("%w: PlainResolver in %s", ErrSecretSchemeNotAllowed, env)
	}
	return PlainResolver{env: env}, nil
}

// Resolve implements Resolver.
func (p PlainResolver) Resolve(_ context.Context, ref SecretRef) (string, error) {
	if !p.env.AllowsPlainSecrets() {
		return "", fmt.Errorf("%w: PlainResolver in %q", ErrSecretSchemeNotAllowed, p.env)
	}
	parsed, err := prepareRef(ref)
	if err != nil {
		return "", err
	}
	if parsed.Scheme != SecretSchemePlain {
		return "", fmt.Errorf("%w: PlainResolver: %s", ErrUnsupportedSecretRef, parsed.Scheme)
	}
	return parsed.Target, nil
}

// FileResolver reads file:// references. NewFileResolver refuses to construct
// one in STAGING/PROD. ReadFile defaults to os.ReadFile.
type FileResolver struct {
	env      Environment
	ReadFile func(string) ([]byte, error)
}

// NewFileResolver returns a FileResolver or an error if env forbids file
// secrets.
func NewFileResolver(env Environment) (*FileResolver, error) {
	if !env.AllowsFileSecrets() {
		return nil, fmt.Errorf("%w: FileResolver in %s", ErrSecretSchemeNotAllowed, env)
	}
	return &FileResolver{env: env, ReadFile: os.ReadFile}, nil
}

// Resolve implements Resolver. Trailing newlines are trimmed.
func (f *FileResolver) Resolve(_ context.Context, ref SecretRef) (string, error) {
	if f == nil || !f.env.AllowsFileSecrets() {
		return "", fmt.Errorf("%w: FileResolver", ErrSecretSchemeNotAllowed)
	}
	p, err := prepareRef(ref)
	if err != nil {
		return "", err
	}
	if p.Scheme != SecretSchemeFile {
		return "", fmt.Errorf("%w: FileResolver: %s", ErrUnsupportedSecretRef, p.Scheme)
	}
	read := f.ReadFile
	if read == nil {
		read = os.ReadFile
	}
	b, err := read(p.Target)
	if err != nil {
		return "", fmt.Errorf("%w: file %s: %w", ErrSecretNotFound, p.Target, err)
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}

// ChainResolver tries each resolver in order, moving on when a resolver
// reports ErrUnsupportedSecretRef. Any other error stops the chain.
type ChainResolver []Resolver

// Resolve implements Resolver.
func (c ChainResolver) Resolve(ctx context.Context, ref SecretRef) (string, error) {
	if _, err := prepareRef(ref); err != nil {
		return "", err
	}
	for _, r := range c {
		if r == nil {
			continue
		}
		v, err := r.Resolve(ctx, ref)
		if err == nil {
			return v, nil
		}
		if !errors.Is(err, ErrUnsupportedSecretRef) {
			return "", err
		}
	}
	return "", fmt.Errorf("%w: no resolver in chain handles %s", ErrUnsupportedSecretRef, ref.Scheme())
}

// NewResolver builds the resolver chain appropriate for env from the schemes
// that can be resolved without external services: plain (LOCAL/TEST), env://
// (all), file:// (LOCAL/TEST/DEV). Append an aws-sm:// resolver for deployed
// environments.
func NewResolver(env Environment, lookup func(string) (string, bool)) ChainResolver {
	var chain ChainResolver
	if pr, err := NewPlainResolver(env); err == nil {
		chain = append(chain, pr)
	}
	chain = append(chain, EnvResolver{Lookup: lookup})
	if fr, err := NewFileResolver(env); err == nil {
		chain = append(chain, fr)
	}
	return chain
}

func prepareRef(ref SecretRef) (ParsedSecretRef, error) {
	if ref.IsZero() {
		return ParsedSecretRef{}, ErrEmptySecretRef
	}
	return ref.Parse()
}
