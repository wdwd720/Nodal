package sandboxrt

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseOutput_AcceptsExactlyIntentAndLogRecords(t *testing.T) {
	out := `{"type":"intent","seq":1,"kind":"ACQUIRE_EXPOSURE","instrument_id":"` + insA + `","side":"BUY","quantity":"10","limit_price":"150.10"}` + "\n" +
		`{"type":"log","seq":2,"event":"signal","fields":{"a":"1"}}` + "\n" +
		`{"seq":3,"type":"log","event":"no_fields"}` + "\n"
	p := ParseOutput([]byte(out), 10)
	require.Empty(t, p.Violations)
	require.Len(t, p.Intents, 1)
	assert.Equal(t, IntentRecord{Seq: 1, Kind: "ACQUIRE_EXPOSURE", InstrumentID: insA, Side: "BUY", Quantity: "10", LimitPrice: "150.10"}, p.Intents[0])
	require.Len(t, p.Logs, 2)
	assert.Equal(t, map[string]string{"a": "1"}, p.Logs[0].Fields)
	assert.Empty(t, ParseOutput(nil, 10).Violations, "no output is a valid, empty run")
}

func TestParseOutput_RefusesEverythingOutsideTheProtocol(t *testing.T) {
	intent := func(extra string) string {
		return `{"type":"intent","seq":1,"kind":"ACQUIRE_EXPOSURE","instrument_id":"` + insA + `","side":"BUY","quantity":"10"` + extra + `}`
	}
	cases := map[string]struct {
		out  string
		code ViolationCode
	}{
		"plain text":               {"hello\n", ViolationOutputNotJSON},
		"json array":               {"[1,2]\n", ViolationOutputNotJSON},
		"two objects on one line":  {`{"type":"log","seq":1,"event":"a"}{"type":"log","seq":2,"event":"b"}` + "\n", ViolationOutputNotJSON},
		"invalid utf8":             {"{\"type\":\"log\",\"seq\":1,\"event\":\"\xff\"}\n", ViolationOutputNotJSON},
		"sign record":              {`{"type":"sign","seq":1,"transaction":"AQID"}` + "\n", ViolationOutputRecordType},
		"transfer record":          {`{"type":"transfer","seq":1,"to":"x"}` + "\n", ViolationOutputRecordType},
		"withdraw record":          {`{"type":"withdraw","seq":1}` + "\n", ViolationOutputRecordType},
		"type not a string":        {`{"type":1,"seq":1}` + "\n", ViolationOutputRecordMalformed},
		"destination smuggled":     {intent(`,"destination":"So111"`) + "\n", ViolationOutputRecordMalformed},
		"signature smuggled":       {intent(`,"signature":"00"`) + "\n", ViolationOutputRecordMalformed},
		"duplicate key":            {`{"type":"log","type":"intent","seq":1,"event":"a"}` + "\n", ViolationOutputRecordMalformed},
		"missing field":            {`{"type":"intent","seq":1,"kind":"ACQUIRE_EXPOSURE"}` + "\n", ViolationOutputRecordMalformed},
		"quantity as a number":     {strings.Replace(intent(""), `"quantity":"10"`, `"quantity":10`, 1) + "\n", ViolationOutputRecordMalformed},
		"seq out of order":         {`{"type":"log","seq":2,"event":"a"}` + "\n", ViolationOutputRecordMalformed},
		"seq as a float":           {`{"type":"log","seq":1.0,"event":"a"}` + "\n", ViolationOutputRecordMalformed},
		"seq as a string":          {`{"type":"log","seq":"1","event":"a"}` + "\n", ViolationOutputRecordMalformed},
		"log field not a string":   {`{"type":"log","seq":1,"event":"a","fields":{"x":1}}` + "\n", ViolationOutputRecordMalformed},
		"no trailing newline":      {`{"type":"log","seq":1,"event":"a"}`, ViolationOutputTruncated},
		"blank line":               {"\n", ViolationOutputNotJSON},
		"trailing garbage":         {`{"type":"log","seq":1,"event":"a"} x` + "\n", ViolationOutputNotJSON},
		"nested object as a field": {`{"type":"log","seq":1,"event":"a","fields":{"x":{"y":"z"}}}` + "\n", ViolationOutputRecordMalformed},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			p := ParseOutput([]byte(c.out), 10)
			require.NotEmpty(t, p.Violations)
			assert.Equal(t, c.code, p.Violations[0].Code, "%v", p.Violations)
			assert.True(t, p.Violations[0].Code.RunLevel())
		})
	}
	many := strings.Repeat(`{"type":"log","seq":1,"event":"a"}`+"\n", 3)
	p := ParseOutput([]byte(many), 2)
	assert.Contains(t, codesOf(p.Violations), ViolationOutputTooManyRecords)
}

func codesOf(vs []Violation) []ViolationCode {
	out := make([]ViolationCode, 0, len(vs))
	for _, v := range vs {
		out = append(out, v.Code)
	}
	return out
}

func TestInterpret_OnlyAnOKRunYieldsProposals(t *testing.T) {
	in := testInput()
	canon, err := in.Canonical()
	require.NoError(t, err)
	good := `{"type":"intent","seq":1,"kind":"ACQUIRE_EXPOSURE","instrument_id":"` + insA + `","side":"BUY","quantity":"10"}` + "\n"
	for _, term := range AllTerminations() {
		raw := RawRun{Termination: term, Stdout: []byte(good), Input: canon, ArtifactDigest: make([]byte, 32), InputDigest: make([]byte, 32)}
		res, err := Interpret(raw, testLimits())
		require.NoError(t, err)
		if term == TerminationOK {
			assert.Len(t, res.Proposals, 1)
			continue
		}
		assert.Empty(t, res.Proposals, "a %s run yields nothing", term)
		assert.Equal(t, term, res.Termination)
	}
	// One bad line poisons the whole output.
	raw := RawRun{Termination: TerminationOK, Stdout: []byte(good + "{\"type\":\"sign\",\"seq\":2}\n"), Input: canon,
		ArtifactDigest: make([]byte, 32), InputDigest: make([]byte, 32)}
	res, err := Interpret(raw, testLimits())
	require.NoError(t, err)
	assert.Equal(t, TerminationInvalidOutput, res.Termination)
	assert.Empty(t, res.Proposals)
}

// FuzzParseOutput: the parser never panics, every code is declared, and a
// clean parse accounts for every line.
func FuzzParseOutput(f *testing.F) {
	f.Add([]byte(`{"type":"intent","seq":1,"kind":"ACQUIRE_EXPOSURE","instrument_id":"x","side":"BUY","quantity":"1"}` + "\n"))
	f.Add([]byte(`{"type":"log","seq":1,"event":"a","fields":{"k":"v"}}` + "\n"))
	f.Add([]byte("{\"type\":\"sign\"}\n\xff"))
	f.Add([]byte(`{"type":"log","seq":1,"seq":1}`))
	f.Fuzz(func(t *testing.T, out []byte) {
		p := ParseOutput(out, 50)
		for _, v := range p.Violations {
			if !v.Code.Valid() || !v.Code.RunLevel() {
				t.Fatalf("parser produced %q", v.Code)
			}
		}
		if len(p.Violations) == 0 && len(out) > 0 {
			lines := strings.Count(string(out), "\n")
			if len(p.Intents)+len(p.Logs) != lines {
				t.Fatalf("%d records from %d lines", len(p.Intents)+len(p.Logs), lines)
			}
		}
	})
}
