package quant

import (
	"fmt"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// battery computes a fixed set of results across every public function and
// renders them as one string per result. It reports failures as an error
// so it can run on goroutines other than the test's own.
func battery() ([]string, error) {
	var out []string
	add := func(label string, v Dec) { out = append(out, label+"="+v.String()) }
	for _, s := range []string{"-92.5", "-1.96", "-0.5", "0", "0.0000000000000000000000000000000000000001", "1", "13.99", "40", "99.75"} {
		x, err := ParseDec(s)
		if err != nil {
			return nil, err
		}
		e, err := Exp(x)
		if err != nil {
			return nil, err
		}
		add("exp("+s+")", e)
		add("Phi("+s+")", NormCDF(x))
		add("phi("+s+")", NormPDF(x))
		if x.Sign() > 0 {
			l, err := Ln(x)
			if err != nil {
				return nil, err
			}
			add("ln("+s+")", l)
			r, err := Sqrt(x)
			if err != nil {
				return nil, err
			}
			add("sqrt("+s+")", r)
		}
	}
	for _, g := range bsmGolden {
		for _, kind := range []OptionKind{Call, Put} {
			var in BSMInput
			in.Kind = kind
			for _, f := range []struct {
				dst *Dec
				src string
			}{{&in.Spot, g.s}, {&in.Strike, g.k}, {&in.TimeYears, g.t}, {&in.Rate, g.r}, {&in.DividendYield, g.q}, {&in.Vol, g.vol}} {
				v, err := ParseDec(f.src)
				if err != nil {
					return nil, err
				}
				*f.dst = v
			}
			res, err := BlackScholes(in)
			if err != nil {
				return nil, fmt.Errorf("%s %s: %w", g.name, kind, err)
			}
			for i, f := range resultFields(res) {
				add(g.name+" "+string(kind)+" "+greekNames[i], f)
			}
			iv, err := ImpliedVol(ivInput(in, res.Price))
			if err != nil {
				return nil, fmt.Errorf("%s %s iv: %w", g.name, kind, err)
			}
			add(g.name+" "+string(kind)+" iv", iv.Vol)
			out = append(out, "iterations="+strconv.Itoa(iv.Iterations))
		}
	}
	return out, nil
}

// TestDeterminism computes the battery twice in sequence and then in 16
// goroutines at once: every rendering must be identical, digit for digit.
func TestDeterminism(t *testing.T) {
	first, err := battery()
	require.NoError(t, err)
	second, err := battery()
	require.NoError(t, err)
	require.Equal(t, first, second)

	const workers = 16
	results := make([][]string, workers)
	errors := make([]error, workers)
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[w], errors[w] = battery()
		}()
	}
	wg.Wait()
	for w := range workers {
		require.NoError(t, errors[w])
		require.Equal(t, first, results[w], "goroutine %d diverged", w)
	}
	require.Len(t, first, 245, "the battery covers every function, golden case and solver run")
	require.Equal(t, "exp(-92.5)=0.0000000000000000000000000000000000000001", first[0]) // 6.7e-41 rounds to one ulp
}
