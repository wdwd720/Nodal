package observability

import (
	"context"
	"io"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/nodal/controlplane/internal/config"
)

// RedactedMarker replaces secret material in log output.
const RedactedMarker = config.RedactedMarker

// LoggerOptions tunes NewLoggerWithOptions.
type LoggerOptions struct {
	// Level defaults to Debug in LOCAL and Info elsewhere.
	Level slog.Leveler
	// AddSource records the caller file:line.
	AddSource bool
}

// NewLogger returns the standard logger: a JSON handler (text in LOCAL for
// readability) wrapped by the redaction handler and the context-attribute
// handler. Redaction is never optional.
func NewLogger(env config.Environment, w io.Writer) *slog.Logger {
	return NewLoggerWithOptions(env, w, LoggerOptions{})
}

// NewLoggerWithOptions is NewLogger with explicit level/source options.
func NewLoggerWithOptions(env config.Environment, w io.Writer, opts LoggerOptions) *slog.Logger {
	if w == nil {
		w = io.Discard
	}
	level := opts.Level
	if level == nil {
		level = slog.LevelInfo
		if env == config.EnvLocal {
			level = slog.LevelDebug
		}
	}
	hopts := &slog.HandlerOptions{Level: level, AddSource: opts.AddSource}
	var base slog.Handler
	if env == config.EnvLocal {
		base = slog.NewTextHandler(w, hopts)
	} else {
		base = slog.NewJSONHandler(w, hopts)
	}
	return slog.New(NewContextHandler(NewRedactHandler(base)))
}

// Secret is a string that never renders: as a slog value, via fmt, or via
// encoding.TextMarshaler it prints RedactedMarker. Use Reveal to read it.
type Secret string

// LogValue implements slog.LogValuer.
func (Secret) LogValue() slog.Value { return slog.StringValue(RedactedMarker) }

// String implements fmt.Stringer.
func (Secret) String() string { return RedactedMarker }

// MarshalText implements encoding.TextMarshaler.
func (Secret) MarshalText() ([]byte, error) { return []byte(RedactedMarker), nil }

// Reveal returns the underlying value.
func (s Secret) Reveal() string { return string(s) }

// ---- redaction handler ----------------------------------------------------

// deniedKeys are the attribute keys (goal PART 190) that are always
// redacted. Matching is on normalised key segments: keys are lower-cased and
// split on '_', '-', '.', ' ' and camelCase boundaries, and a key is denied
// when the entry's segments appear contiguously in it. So "private_key",
// "PrivateKey", "wallet-private-key" and "x-api-key" all match, while
// "cardinality", "company" or "tokens_used" do not.
var deniedKeys = []string{
	"private_key", "privatekey", "seed", "seed_phrase", "mnemonic", "secret", "token",
	"access_token", "refresh_token", "id_token", "authorization", "cookie", "set-cookie",
	"password", "passwd", "api_key", "apikey", "card", "pan", "cvv", "ssn", "signing_token",
	"webhook_secret",
	// Connection strings. PART 190 does not name them, but a DSN carries a
	// password inline, so a single `log.Info("connecting", "dsn", url)` anywhere
	// would print a live database credential. Nothing does that today —
	// internal/config holds every DSN as a SecretRef, internal/db never echoes
	// the URL into an error, and pgconn redacts the password in its own parse
	// error — but that is three separate behaviors all continuing to hold, and
	// the denial costs nothing.
	"dsn", "database_url", "connection_string", "conn_string", "conninfo",
}

var deniedSequences = func() [][]string {
	out := make([][]string, 0, len(deniedKeys))
	for _, k := range deniedKeys {
		out = append(out, splitKey(k))
	}
	return out
}()

// IsDeniedKey reports whether an attribute key is on the redaction denylist.
func IsDeniedKey(key string) bool {
	segs := splitKey(key)
	if len(segs) == 0 {
		return false
	}
	for _, seq := range deniedSequences {
		if containsSeq(segs, seq) {
			return true
		}
	}
	return false
}

func containsSeq(hay, needle []string) bool {
	if len(needle) == 0 || len(needle) > len(hay) {
		return false
	}
	for i := 0; i+len(needle) <= len(hay); i++ {
		if slices.Equal(hay[i:i+len(needle)], needle) {
			return true
		}
	}
	return false
}

func splitKey(key string) []string {
	var segs []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			segs = append(segs, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	runes := []rune(key)
	for i, r := range runes {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			flush()
			continue
		}
		if i > 0 && unicode.IsUpper(r) && len(cur) > 0 {
			prev := runes[i-1]
			var next rune
			if i+1 < len(runes) {
				next = runes[i+1]
			}
			if unicode.IsLower(prev) || unicode.IsDigit(prev) || (unicode.IsUpper(prev) && unicode.IsLower(next)) {
				flush()
			}
		}
		cur = append(cur, r)
	}
	flush()
	return segs
}

var (
	// bearerRe matches "Bearer <token>" with a token of 20+ characters.
	bearerRe = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9\-._~+/]{20,}=*`)
	// solanaKeyRe matches base58 strings of 87-88 characters (a 64-byte
	// ed25519 secret key as exported by Solana wallets).
	solanaKeyRe = regexp.MustCompile(`\b[1-9A-HJ-NP-Za-km-z]{87,88}\b`)
	// pemRe matches a PEM block, terminated or truncated.
	pemRe = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]+-----[\s\S]*?(?:-----END [A-Z0-9 ]+-----|\z)`)

	// dsnUserinfoRe matches the "user:password@" of a URL, capturing any
	// leading text and the scheme+user so only the password is replaced. The
	// password class excludes "/" and "@" so it cannot run past the host into
	// a path, and the host must be non-empty so "http://x" is left alone.
	dsnUserinfoRe = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)([^:/@\s]+):[^@/\s]+@`)
)

// MaskString masks values that look like secrets regardless of key: bearer
// tokens, Solana private keys, PEM blocks, and the userinfo credential of a
// connection string.
//
// The key-based denylist above is not sufficient on its own for a DSN: the
// credential travels inside a value that may be logged under any key at all,
// or embedded in a longer message. Masking the userinfo keeps the host, port
// and database name — which are what an operator actually needs to read from a
// log line — while removing the password.
func MaskString(s string) string {
	if len(s) < 16 {
		return s
	}
	if strings.Contains(strings.ToLower(s), "bearer") {
		s = bearerRe.ReplaceAllString(s, "Bearer "+RedactedMarker)
	}
	if len(s) >= 87 {
		s = solanaKeyRe.ReplaceAllString(s, RedactedMarker)
	}
	if strings.Contains(s, "-----BEGIN ") {
		s = pemRe.ReplaceAllString(s, RedactedMarker)
	}
	if strings.Contains(s, "://") {
		s = dsnUserinfoRe.ReplaceAllString(s, "${1}${2}:"+RedactedMarker+"@")
	}
	return s
}

// RedactHandler is a slog.Handler that rewrites records before delegating:
// attributes with denied keys (at any group depth, including groups opened
// with WithGroup) become RedactedMarker, and string values pass through
// MaskString. Values of kind Any other than errors are passed through
// untouched; log secrets through Secret or explicit attributes, never by
// dumping structs.
type RedactHandler struct {
	inner     slog.Handler
	groups    []string
	redactAll bool
}

// NewRedactHandler wraps inner.
func NewRedactHandler(inner slog.Handler) *RedactHandler {
	if r, ok := inner.(*RedactHandler); ok {
		return r
	}
	return &RedactHandler{inner: inner}
}

// Enabled implements slog.Handler.
func (h *RedactHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

// Handle implements slog.Handler.
func (h *RedactHandler) Handle(ctx context.Context, r slog.Record) error {
	nr := slog.NewRecord(r.Time, r.Level, MaskString(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		nr.AddAttrs(RedactAttr(a, h.redactAll))
		return true
	})
	return h.inner.Handle(ctx, nr)
}

// WithAttrs implements slog.Handler.
func (h *RedactHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make([]slog.Attr, 0, len(attrs))
	for _, a := range attrs {
		out = append(out, RedactAttr(a, h.redactAll))
	}
	return &RedactHandler{inner: h.inner.WithAttrs(out), groups: h.groups, redactAll: h.redactAll}
}

// WithGroup implements slog.Handler. Opening a denied group redacts every
// attribute logged beneath it.
func (h *RedactHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return &RedactHandler{
		inner:     h.inner.WithGroup(name),
		groups:    append(slices.Clone(h.groups), name),
		redactAll: h.redactAll || IsDeniedKey(name),
	}
}

// RedactAttr returns a copy of a with secrets removed. When forced is true
// the whole attribute is redacted regardless of key.
func RedactAttr(a slog.Attr, forced bool) slog.Attr {
	a.Value = a.Value.Resolve()
	if forced || (a.Key != "" && IsDeniedKey(a.Key)) {
		return slog.String(a.Key, RedactedMarker)
	}
	switch a.Value.Kind() {
	case slog.KindGroup:
		members := a.Value.Group()
		out := make([]slog.Attr, len(members))
		for i, m := range members {
			out[i] = RedactAttr(m, false)
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(out...)}
	case slog.KindString:
		return slog.String(a.Key, MaskString(a.Value.String()))
	case slog.KindAny:
		if err, ok := a.Value.Any().(error); ok && err != nil {
			return slog.String(a.Key, MaskString(err.Error()))
		}
	}
	return a
}

// ---- context handler ------------------------------------------------------

// ContextHandler adds ContextAttrs(ctx) to every record. Attributes are added
// at the handler's current group level.
type ContextHandler struct {
	inner slog.Handler
}

// NewContextHandler wraps inner.
func NewContextHandler(inner slog.Handler) *ContextHandler {
	if c, ok := inner.(*ContextHandler); ok {
		return c
	}
	return &ContextHandler{inner: inner}
}

// Enabled implements slog.Handler.
func (h *ContextHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

// Handle implements slog.Handler.
func (h *ContextHandler) Handle(ctx context.Context, r slog.Record) error {
	if attrs := ContextAttrs(ctx); len(attrs) > 0 {
		r = r.Clone()
		r.AddAttrs(attrs...)
	}
	return h.inner.Handle(ctx, r)
}

// WithAttrs implements slog.Handler.
func (h *ContextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &ContextHandler{inner: h.inner.WithAttrs(attrs)}
}

// WithGroup implements slog.Handler.
func (h *ContextHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return &ContextHandler{inner: h.inner.WithGroup(name)}
}
