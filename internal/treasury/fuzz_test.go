package treasury

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/money"
)

// A stored risk policy or paper-fill disclosure is external input to the
// next house command and to the auditor. Whatever bytes a row holds, the
// decoders never panic, and anything they accept is byte-for-byte the
// canonical form of the value they return, so the hash recorded with the
// bytes names that value.

func FuzzDecodePolicy(f *testing.F) {
	house := econ.NewHouseAccountID()
	doc, err := samplePolicy().Document(house)
	require.NoError(f, err)
	bare := samplePolicy()
	bare.MaxInventory, bare.MaxInstrumentExposure = nil, nil
	plain, err := bare.Document(house)
	require.NoError(f, err)
	for _, valid := range [][]byte{doc, plain} {
		_, _, err := decodePolicy(valid)
		require.NoError(f, err, "the valid seeds exercise the accepting path")
	}
	for _, seed := range [][]byte{doc, plain, nil, []byte(`{}`), []byte(`null`), append(append([]byte(nil), doc...), []byte(`{}`)...),
		[]byte(`{"kind":"treasury.policy.v1","max_mark_age_ms":9223372036854775807}`)} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		p, h, err := decodePolicy(raw)
		if err != nil {
			return
		}
		require.NoError(t, p.Validate())
		again, err := p.Document(h)
		require.NoError(t, err)
		require.Equal(t, string(raw), string(again), "an accepted policy is exactly its canonical form")
	})
}

func FuzzDecodeDisclosure(f *testing.F) {
	quoteAsset := assets.NewAssetID()
	s := fees.ZeroSchedule(quoteAsset, 6)
	s.Version = "treasury-paper/v1/fuzz"
	s.SpreadBPS, s.VenuePassThroughBPS = 25, 5
	at := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	ref, err := money.PriceFromDecimalString("100.00", quoteAsset.String(), "fuzz", at)
	require.NoError(f, err)
	for _, side := range []fees.Side{fees.SideBuy, fees.SideSell} {
		quote, err := s.Quote(fees.QuoteInput{Side: side, BaseQty: q(2_000_000), BaseDecimals: 6, QuoteDecimals: 6, ReferencePrice: ref})
		require.NoError(f, err)
		b, err := quote.Disclosure().Canonical()
		require.NoError(f, err)
		_, err = decodeDisclosure(b, hashHex(b))
		require.NoError(f, err, "the valid seeds exercise the accepting path")
		f.Add(b)
	}
	f.Add([]byte(`{}`))
	f.Add([]byte(`null`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		d, err := decodeDisclosure(raw, hashHex(raw))
		if err != nil {
			return
		}
		again, err := d.Canonical()
		require.NoError(t, err)
		require.Equal(t, string(raw), string(again), "an accepted disclosure is exactly its canonical form")
		_, err = decodeDisclosure(raw, hashHex(append(append([]byte(nil), raw...), ' ')))
		require.Error(t, err, "a disclosure is accepted only under its own hash")
	})
}
