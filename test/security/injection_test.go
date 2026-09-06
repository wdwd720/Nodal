//go:build integration

package security

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The payload sweep is the empirical half of the injection argument; the
// constant-derivation scan in sqlsource_test.go is the structural half. This
// file fires hostile strings at every place a client can put one and asserts
// four things about each answer: it came from a handler and not from the
// authenticator, it is not a 5xx, it describes nothing of the implementation,
// and it changed nothing in the database.
//
// The last of those is what makes the sweep more than a fuzz run. A payload
// that is quietly accepted and stored is worse than one that produces a 500,
// so the sweep brackets itself with a census of the tables an injection would
// damage and compares it afterwards.

// injectionPayloads are the strings under test. Each is chosen for a specific
// interpreter: the SQL ones close a quote or a statement, the shell and
// template ones test whether any value is passed to an evaluator, the encoding
// ones test whether a decoder runs before validation does.
var injectionPayloads = []struct{ name, value string }{
	{"sql_or_true", "' OR '1'='1"},
	{"sql_comment", "admin'--"},
	{"sql_union", "' UNION SELECT id, owner_user_id, status FROM accounts--"},
	{"sql_stacked_drop", "'; DROP TABLE trade_intents; --"},
	{"sql_stacked_delete", "'; DELETE FROM accounts WHERE 1=1; --"},
	{"sql_stacked_update", "'; UPDATE accounts SET status = 'ACTIVE'; --"},
	{"sql_sleep", "'; SELECT pg_sleep(10); --"},
	{"sql_boolean_blind", "' AND (SELECT count(*) FROM accounts) > 0 --"},
	{"sql_cast_error", "' AND 1=CAST((SELECT version()) AS int)--"},
	{"sql_quote_escape", `\' OR 1=1--`},
	{"sql_double_quote", `" OR ""="`},
	{"sql_null_byte", "abc\x00' OR 1=1--"},
	{"sql_unicode_quote", "’ OR 1=1--"},
	{"pg_dollar_quote", "$$; DROP TABLE accounts; $$"},
	{"pg_array_literal", "{1,2}'::int[] --"},
	{"like_wildcards", "%_%"},
	{"json_injection", `{"$ne":null}`},
	{"template_injection", "{{.}}${jndi:ldap://127.0.0.1/x}"},
	{"shell_injection", "$(id); `id`; | id"},
	{"path_traversal", "../../../../etc/passwd"},
	{"crlf_header", "x\r\nX-Injected: yes"},
	{"xss_script", "<script>alert(document.cookie)</script>"},
	{"very_long", strings.Repeat("A' OR 1=1--", 400)},
}

// implementationMarkers are strings that would tell an attacker how the system
// is built. Any of them in a response body is a finding, whatever the status.
var implementationMarkers = []string{
	"SQLSTATE", "sqlstate", "pgx", "pgconn", "pq:", "syntax error at or near",
	"relation \"", "column \"", "postgres://", "sslmode", "goroutine ",
	"panic:", "/c/Dev/", "internal/httpapi", "duplicate key value",
	"pg_sleep", "DROP TABLE", "UNION SELECT",
}

// TestInjection_PayloadsNeverReachAnInterpreter sweeps every payload through
// every client-controlled value: query parameters, JSON body fields and the
// pagination cursor.
//
// The negative control drops the session cookie, so every probe is answered
// 401 by the authenticator before any handler sees it. That is the vacuous
// version of this test — an anonymous sweep proves only that the front door is
// shut — and the "reached a handler" assertion is what rules it out.
func TestInjection_PayloadsNeverReachAnInterpreter(t *testing.T) {
	requireAPI(t)
	a := mustLogin(t, "customer-a")
	acct := firstAccount(t, a)
	inst := instrumentID(t, a)

	token := a.Token
	if secBreak(t, "injection_probes_unauthenticated") {
		token = ""
	}

	// The census an injection would disturb. Counted before and after, with
	// the sweep in between.
	before := databaseCensus(t)
	require.Positive(t, before["accounts"], "there must be accounts to destroy, or the census proves nothing")
	require.Positive(t, before["instruments"], "there must be instruments to destroy")

	reachedAHandler, expectedToReach, probes := 0, 0, 0
	for _, p := range injectionPayloads {
		for _, probe := range injectionProbes(t, acct, inst, p.name, p.value) {
			probes++
			if probe.reachesHandler {
				expectedToReach++
			}
			t.Run(p.name+"/"+probe.where, func(t *testing.T) {
				r := probe.send(token)

				// The precondition, and the only thing the negative control
				// touches. It is asserted for the positions the payload can
				// legally occupy: the generated request binder runs before
				// authorization (it is a strict-handler middleware, the binder
				// is not), so a payload in a UUID- or integer-typed position is
				// refused at the transport layer whether or not a session was
				// sent. Those probes still assert everything else below; they
				// simply cannot witness "an authenticated caller got through",
				// and pretending otherwise is how a sweep passes vacuously.
				if probe.reachesHandler {
					require.NotEqualf(t, http.StatusUnauthorized, r.Status,
						"%s was answered by the authenticator, not a handler, so it proves nothing about %s",
						probe.where, p.name)
					reachedAHandler++
				}

				require.Lessf(t, r.Status, 500,
					"%s with %s produced %d: %s", probe.where, p.name, r.Status, r.text())
				if r.Status >= 400 {
					require.NotEmptyf(t, r.Problem.Code,
						"%s with %s was refused with no stable code: %s", probe.where, p.name, r.text())
				}
				body := r.text()
				for _, marker := range implementationMarkers {
					require.NotContainsf(t, body, marker,
						"%s with %s leaked %q: %s", probe.where, p.name, marker, truncate(body, 400))
				}
				require.NotContains(t, r.Header.Get("X-Injected"), "yes",
					"a header was injected through a request value")
			})
		}
	}
	require.Positive(t, probes, "no probe was constructed")
	require.Equal(t, expectedToReach, reachedAHandler,
		"only %d of %d payload positions reached a handler", reachedAHandler, expectedToReach)
	// The handler-reaching probes must be a real share of the sweep, or the
	// precondition above could be carried by a single position while the rest
	// of the sweep quietly measured the request binder.
	require.Greaterf(t, expectedToReach*2, probes,
		"only %d of %d probes can reach a handler; most of the sweep is measuring the binder", expectedToReach, probes)

	after := databaseCensus(t)
	for table, n := range before {
		require.GreaterOrEqualf(t, after[table], n,
			"the sweep destroyed rows in %s: %d before, %d after", table, n, after[table])
	}
	// The rows an injection would forge: nothing may have been written for an
	// account other than the caller's, and nothing may carry a payload.
	require.Zero(t, countRows(t,
		`SELECT count(*) FROM trade_intents WHERE idempotency_key LIKE $1 AND account_id <> $2`,
		key("inj")+"%", acct),
		"the sweep created an intent on an account the caller does not own")
	require.Zero(t, countRows(t,
		`SELECT count(*) FROM trade_intents WHERE idempotency_key LIKE $1 AND idempotency_key LIKE '%DROP%'`,
		key("inj")+"%"),
		"a payload was stored as an idempotency key")

	// The system still works: the sweep must not have degraded it.
	ok := getAs(t, a.Token, "/v1/accounts/"+acct)
	require.Equal(t, http.StatusOK, ok.Status, "the account is unreadable after the sweep: %s", ok.text())
	require.Equal(t, inst, instrumentID(t, a), "the seeded instrument changed during the sweep")
}

// injectionProbe is one payload in one position. reachesHandler records
// whether the position accepts an arbitrary string in the contract: a
// UUID- or integer-typed parameter is refused by the generated binder before
// authorization runs, so such a probe cannot witness that an authenticated
// caller's payload got as far as the query layer.
type injectionProbe struct {
	where          string
	reachesHandler bool
	send           func(token string) response
}

// injectionProbes places the payload everywhere a client controls a value:
// each account-scoped query parameter, the pagination cursor, and each field
// of the intent command body.
func injectionProbes(t *testing.T, acct, inst, name, payload string) []injectionProbe {
	t.Helper()
	esc := escapeQuery(payload)
	idem := func(suffix string) string { return key("inj-" + sanitiseKeyPart(name+"-"+suffix)) }

	queries := []struct {
		where, path string
		freeForm    bool
	}{
		{"query_account_id", "/v1/intents?account_id=" + esc, false},
		{"query_orders_account_id", "/v1/orders?account_id=" + esc, false},
		{"query_deposits_account_id", "/v1/funding/deposits?account_id=" + esc, false},
		{"query_limit", "/v1/intents?account_id=" + acct + "&limit=" + esc, false},
		{"query_cursor", "/v1/intents?account_id=" + acct + "&cursor=" + esc, true},
		{"query_ledger_cursor", "/v1/accounts/" + acct + "/ledger/transactions?cursor=" + esc, true},
		{"query_activity_cursor", "/v1/accounts/" + acct + "/activity?cursor=" + esc, true},
		{"query_unknown_param", "/v1/intents?account_id=" + acct + "&order_by=" + esc, true},
	}
	probes := make([]injectionProbe, 0, len(queries)+5)
	for _, q := range queries {
		probes = append(probes, injectionProbe{
			where:          q.where,
			reachesHandler: q.freeForm,
			send:           func(token string) response { return getAs(t, token, q.path) },
		})
	}

	// Body fields. Each holds the payload while the rest of the document stays
	// well-formed, so a refusal is attributable to the field under test.
	bodies := []struct {
		where, body string
		freeForm    bool
	}{
		{"body_account_id", intentBodyWith(payload, inst, "ACQUIRE_NOTIONAL", "PAPER", "10.00"), false},
		{"body_instrument_id", intentBodyWith(acct, payload, "ACQUIRE_NOTIONAL", "PAPER", "10.00"), false},
		{"body_action", intentBodyWith(acct, inst, payload, "PAPER", "10.00"), true},
		{"body_mode", intentBodyWith(acct, inst, "ACQUIRE_NOTIONAL", payload, "10.00"), true},
		{"body_notional", intentBodyWith(acct, inst, "ACQUIRE_NOTIONAL", "PAPER", payload), true},
	}
	for _, b := range bodies {
		probes = append(probes, injectionProbe{
			where:          b.where,
			reachesHandler: b.freeForm,
			send: func(token string) response {
				return postAs(t, token, "/v1/intents", idem(b.where), b.body)
			},
		})
	}
	return probes
}

// intentBodyWith builds an intent document with one field replaced by an
// arbitrary string. It is written by hand rather than with encoding/json so
// the payload reaches the server as the client wrote it.
func intentBodyWith(account, instrument, action, mode, notional string) string {
	return fmt.Sprintf(`{"account_id":%s,"instrument_id":%s,"action":%s,"mode":%s,"notional_usd":%s}`,
		jsonString(account), jsonString(instrument), jsonString(action), jsonString(mode), jsonString(notional))
}

// jsonString quotes s for a JSON document, escaping exactly what RFC 8259
// requires and nothing else, so a payload survives intact.
func jsonString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, r)
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// escapeQuery percent-encodes everything a query value may not carry
// literally. url.QueryEscape would also turn a space into '+', which some
// payloads rely on being a space.
func escapeQuery(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := range len(s) {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&0x0f])
	}
	return b.String()
}

// sanitiseKeyPart reduces a probe name to the Idempotency-Key charset, so the
// key under test is the sweep's own and never a malformed-key probe by
// accident (that contract is measured in idempotency_abuse_test.go).
func sanitiseKeyPart(s string) string {
	var b strings.Builder
	for i := range len(s) {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// databaseCensus counts the tables an injection would want to empty.
func databaseCensus(t *testing.T) map[string]int {
	t.Helper()
	out := map[string]int{}
	// The statements are constants, one per table: the census must not itself
	// be a place where a name is interpolated into SQL.
	out["accounts"] = countRows(t, `SELECT count(*) FROM accounts`)
	out["instruments"] = countRows(t, `SELECT count(*) FROM instruments`)
	out["users"] = countRows(t, `SELECT count(*) FROM users`)
	out["trade_intents"] = countRows(t, `SELECT count(*) FROM trade_intents`)
	out["journal_transactions"] = countRows(t, `SELECT count(*) FROM journal_transactions`)
	out["sessions"] = countRows(t, `SELECT count(*) FROM sessions`)
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
