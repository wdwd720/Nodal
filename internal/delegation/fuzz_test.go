package delegation

import (
	"bytes"
	"testing"

	"github.com/nodal/controlplane/internal/devnet"
)

// FuzzInspectSwapTransaction: the inspector never panics on any input, and
// it accepts nothing but the planned transaction signed by the wallet and
// the venue.
func FuzzInspectSwapTransaction(f *testing.F) {
	e := unitSwapExpectation(f)
	m, err := e.Plan.Message(e.Blockhash)
	if err != nil {
		f.Fatal(err)
	}
	tx, err := devnet.Sign(m, devnet.NewKeypairFromSeed(useed("wallet")), devnet.NewKeypairFromSeed(useed("venue")))
	if err != nil {
		f.Fatal(err)
	}
	good, err := tx.Encode()
	if err != nil {
		f.Fatal(err)
	}
	f.Add(good)
	f.Add([]byte{})
	f.Add([]byte("ndvt"))
	f.Add(good[:len(good)/2])
	want, err := m.Bytes()
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		txID, err := InspectSwapTransaction(e, raw)
		if err != nil {
			return
		}
		decoded, derr := devnet.Decode(raw)
		if derr != nil {
			t.Fatalf("accepted bytes that do not decode")
		}
		got, berr := decoded.Message.Bytes()
		if berr != nil || !bytes.Equal(got, want) {
			t.Fatalf("accepted a transaction that is not the planned one")
		}
		if decoded.VerifySignatures() != nil || txID == "" {
			t.Fatalf("accepted a transaction without valid signatures")
		}
	})
}

// FuzzDecodeMessage: message decoding never panics and is canonical: what it
// accepts re-encodes to exactly the same bytes.
func FuzzDecodeMessage(f *testing.F) {
	e := unitSwapExpectation(f)
	m, err := e.Plan.Message(e.Blockhash)
	if err != nil {
		f.Fatal(err)
	}
	b, err := m.Bytes()
	if err != nil {
		f.Fatal(err)
	}
	f.Add(b)
	f.Add([]byte("ndvm"))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, raw []byte) {
		msg, err := decodeMessage(raw)
		if err != nil {
			return
		}
		again, err := msg.Bytes()
		if err != nil || !bytes.Equal(again, raw) {
			t.Fatalf("decoding is not canonical")
		}
	})
}
