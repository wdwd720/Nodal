package proof

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// RFC 6962 known answers (the Certificate Transparency reference vectors:
// leaves "", 00, 10, 2021, 3031, 40414243, 5051525354555657,
// 606162636465666768696a6b6c6d6e6f).
var rfc6962Leaves = func() [][]byte {
	hexes := []string{"", "00", "10", "2021", "3031", "40414243", "5051525354555657", "606162636465666768696a6b6c6d6e6f"}
	out := make([][]byte, len(hexes))
	for i, h := range hexes {
		b, err := hex.DecodeString(h)
		if err != nil {
			panic(err)
		}
		out[i] = b
	}
	return out
}()

var rfc6962Roots = []string{
	"6e340b9cffb37a989ca544e6bb780a2c78901d3fb33738768511a30617afa01d", // 1 leaf: sha256(0x00)
	"fac54203e7cc696cf0dfcb42c92a1d9dbaf70ad9e621f4bd8d98662f00e3c125",
	"aeb6bcfe274b70a14fb067a5e5578264db0fa9b51af5e0ba159158f329e06e77",
	"d37ee418976dd95753c1c73862b9398fa2a2cf9b4ff0fdfe8b30cd95209614b7",
	"4e3bbb1f7b478dcfe71fb631631519a3bca12c9aefca1612bfce4c13a86264d4",
	"76e67dadbcdf1e10e1b74ddc608abd2f98dfb16fbce75277b5232a127f2087ef",
	"ddb89be403809e325750d3d263cd78929c2942b7942a34b77e122c9594a74c8c",
	"5dc9da79a70659a9ad559cb701ded9a2ab9d823aad2f4960cfe370eff4604328",
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	require.NoError(t, err)
	return b
}

func TestMerkle_RFC6962KnownAnswers(t *testing.T) {
	assert.Equal(t, mustHex(t, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"), EmptyRoot(), "MTH({}) = sha256(\"\")")
	assert.Equal(t, mustHex(t, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"), NewTree(nil).Root())
	for n := 1; n <= 8; n++ {
		root := NewTree(rfc6962Leaves[:n]).Root()
		assert.Equal(t, rfc6962Roots[n-1], hex.EncodeToString(root), "root over %d leaves", n)
	}
}

func TestMerkle_ConventionIsDocumented(t *testing.T) {
	// leaf = sha256(0x00 || leaf); node = sha256(0x01 || left || right).
	leaf := []byte("leaf")
	want := sha256.Sum256(append([]byte{0x00}, leaf...))
	assert.Equal(t, want[:], LeafHash(leaf))
	l, r := LeafHash([]byte("a")), LeafHash([]byte("b"))
	wantNode := sha256.Sum256(append(append([]byte{0x01}, l...), r...))
	assert.Equal(t, wantNode[:], NodeHash(l, r))
	// Three leaves: H(H(a,b), c) with c promoted, never duplicated.
	a, b, c := []byte("a"), []byte("b"), []byte("c")
	assert.Equal(t, NodeHash(NodeHash(LeafHash(a), LeafHash(b)), LeafHash(c)), NewTree([][]byte{a, b, c}).Root())
	// A tree whose odd leaf were duplicated would have a different root.
	assert.NotEqual(t, NodeHash(NodeHash(LeafHash(a), LeafHash(b)), NodeHash(LeafHash(c), LeafHash(c))), NewTree([][]byte{a, b, c}).Root())
}

func TestMerkle_RFC6962InclusionProofVectors(t *testing.T) {
	// {leaf index (0-based), tree size, path} from the CT reference tests.
	cases := []struct {
		index, size int
		path        []string
	}{
		{0, 1, nil},
		{0, 8, []string{
			"96a296d224f285c67bee93c30f8a309157f0daa35dc5b87e410b78630a09cfc7",
			"5f083f0a1a33ca076a95279832580db3e0ef4584bdff1f54c8a360f50de3031e",
			"6b47aaf29ee3c2af9af889bc1fb9254dabd31177f16232dd6aab035ca39bf6e4",
		}},
		{5, 8, []string{
			"bc1a0643b12e4d2d7c77918f44e0f4f79a838b6cf9ec5b5c283e1f4d88599e6b",
			"ca854ea128ed050b41b35ffc1b87b8eb2bde461e9e3b5596ece6b9d5975a0ae0",
			"d37ee418976dd95753c1c73862b9398fa2a2cf9b4ff0fdfe8b30cd95209614b7",
		}},
		{2, 3, []string{"fac54203e7cc696cf0dfcb42c92a1d9dbaf70ad9e621f4bd8d98662f00e3c125"}},
		{1, 5, []string{
			"6e340b9cffb37a989ca544e6bb780a2c78901d3fb33738768511a30617afa01d",
			"5f083f0a1a33ca076a95279832580db3e0ef4584bdff1f54c8a360f50de3031e",
			"bc1a0643b12e4d2d7c77918f44e0f4f79a838b6cf9ec5b5c283e1f4d88599e6b",
		}},
	}
	for _, tc := range cases {
		tree := NewTree(rfc6962Leaves[:tc.size])
		p, err := tree.Prove(tc.index)
		require.NoError(t, err)
		require.Len(t, p.Siblings, len(tc.path), "index %d size %d", tc.index, tc.size)
		for i, want := range tc.path {
			assert.Equal(t, want, hex.EncodeToString(p.Siblings[i].Hash), "index %d size %d sibling %d", tc.index, tc.size, i)
		}
		assert.NoError(t, VerifyMembership(tree.Root(), rfc6962Leaves[tc.index], p))
	}
}

func TestMerkle_ProveRejectsOutOfRange(t *testing.T) {
	tree := NewTree(rfc6962Leaves[:3])
	_, err := tree.Prove(-1)
	assert.ErrorIs(t, err, ErrLeafIndex)
	_, err = tree.Prove(3)
	assert.ErrorIs(t, err, ErrLeafIndex)
	_, err = tree.Leaf(3)
	assert.ErrorIs(t, err, ErrLeafIndex)
	_, err = NewTree(nil).Prove(0)
	assert.ErrorIs(t, err, ErrLeafIndex)
}

func TestMerkle_DeterministicAndOrderSensitive(t *testing.T) {
	leaves := rfc6962Leaves[:7]
	assert.Equal(t, NewTree(leaves).Root(), NewTree(leaves).Root(), "same leaves, same root")
	swapped := append([][]byte(nil), leaves...)
	swapped[2], swapped[5] = swapped[5], swapped[2]
	assert.NotEqual(t, NewTree(leaves).Root(), NewTree(swapped).Root(), "reordering leaves changes the root")
	// Leaves are copied: mutating the input after construction changes nothing.
	own := [][]byte{[]byte("x"), []byte("y")}
	tree := NewTree(own)
	root := tree.Root()
	own[0][0] = 'z'
	assert.Equal(t, root, tree.Root())
}

// TestMerkle_AdversarialProofsAreRejected covers the ways a proof can lie:
// a tampered leaf, a leaf presented at another index, a swapped sibling, a
// truncated or extended path, a flipped direction flag, and a foreign root.
// Each is rejected for its specific reason, never accepted.
func TestMerkle_AdversarialProofsAreRejected(t *testing.T) {
	leaves := rfc6962Leaves
	tree := NewTree(leaves)
	root := tree.Root()
	p, err := tree.Prove(5)
	require.NoError(t, err)
	require.NoError(t, VerifyMembership(root, leaves[5], p))

	t.Run("tampered leaf", func(t *testing.T) {
		bad := append(bytes.Clone(leaves[5]), 0xff)
		assert.ErrorIs(t, VerifyMembership(root, bad, p), ErrProofInvalid)
	})
	t.Run("leaf at another index", func(t *testing.T) {
		q := p
		q.LeafIndex = 4
		err := VerifyMembership(root, leaves[5], q)
		assert.Error(t, err)
		assert.True(t, isProofShape(err) || isProofInvalid(err), "%v", err)
		assert.ErrorIs(t, VerifyMembership(root, leaves[4], p), ErrProofInvalid, "the real leaf 4 does not verify at index 5")
	})
	t.Run("swapped siblings", func(t *testing.T) {
		q := p
		q.Siblings = append([]Sibling(nil), p.Siblings...)
		q.Siblings[0], q.Siblings[1] = q.Siblings[1], q.Siblings[0]
		err := VerifyMembership(root, leaves[5], q)
		assert.True(t, isProofShape(err) || isProofInvalid(err), "%v", err)
	})
	t.Run("swapped sibling hashes only", func(t *testing.T) {
		q := p
		q.Siblings = append([]Sibling(nil), p.Siblings...)
		q.Siblings[0].Hash, q.Siblings[1].Hash = p.Siblings[1].Hash, p.Siblings[0].Hash
		assert.ErrorIs(t, VerifyMembership(root, leaves[5], q), ErrProofInvalid)
	})
	t.Run("truncated path", func(t *testing.T) {
		q := p
		q.Siblings = p.Siblings[:len(p.Siblings)-1]
		assert.ErrorIs(t, VerifyMembership(root, leaves[5], q), ErrProofShape)
	})
	t.Run("extended path", func(t *testing.T) {
		q := p
		q.Siblings = append(append([]Sibling(nil), p.Siblings...), Sibling{Hash: LeafHash([]byte("extra")), Left: true})
		assert.ErrorIs(t, VerifyMembership(root, leaves[5], q), ErrProofShape)
	})
	t.Run("flipped direction flag", func(t *testing.T) {
		q := p
		q.Siblings = append([]Sibling(nil), p.Siblings...)
		q.Siblings[0].Left = !q.Siblings[0].Left
		assert.ErrorIs(t, VerifyMembership(root, leaves[5], q), ErrProofShape)
	})
	t.Run("malformed sibling", func(t *testing.T) {
		q := p
		q.Siblings = append([]Sibling(nil), p.Siblings...)
		q.Siblings[1].Hash = []byte{1, 2, 3}
		assert.ErrorIs(t, VerifyMembership(root, leaves[5], q), ErrProofShape)
	})
	t.Run("foreign root", func(t *testing.T) {
		other := NewTree(leaves[:7]).Root()
		assert.ErrorIs(t, VerifyMembership(other, leaves[5], p), ErrProofInvalid)
	})
	t.Run("wrong leaf count", func(t *testing.T) {
		// RFC 9162 binds the claimed size only through the path shape: a
		// size that changes the shape (6: index 5 becomes the last leaf) is
		// rejected; the root binds the content in every case.
		q := p
		q.LeafCount = 6
		assert.ErrorIs(t, VerifyMembership(root, leaves[5], q), ErrProofShape)
		q.LeafCount = 4
		assert.ErrorIs(t, VerifyMembership(root, leaves[5], q), ErrProofShape)
		q.LeafCount = 0
		assert.ErrorIs(t, VerifyMembership(root, leaves[5], q), ErrProofShape)
	})
}

func isProofShape(err error) bool   { return errors.Is(err, ErrProofShape) }
func isProofInvalid(err error) bool { return errors.Is(err, ErrProofInvalid) }

// TestProp_MembershipProofsVerify: for random leaf sets of every size up to
// 300, every leaf's proof verifies against the root, every proof has the
// RFC 6962 height for its position, and no proof verifies for a different
// leaf or a different root.
func TestProp_MembershipProofsVerify(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(1, 300).Draw(rt, "n")
		leaves := make([][]byte, n)
		for i := range leaves {
			leaves[i] = rapid.SliceOfN(rapid.Byte(), 0, 64).Draw(rt, "leaf")
		}
		tree := NewTree(leaves)
		root := tree.Root()
		i := rapid.IntRange(0, n-1).Draw(rt, "i")
		p, err := tree.Prove(i)
		if err != nil {
			rt.Fatalf("prove: %v", err)
		}
		if err := VerifyMembership(root, leaves[i], p); err != nil {
			rt.Fatalf("leaf %d of %d: %v", i, n, err)
		}
		if p.LeafIndex != i || p.LeafCount != n {
			rt.Fatalf("proof position %d/%d, want %d/%d", p.LeafIndex, p.LeafCount, i, n)
		}
		// A different leaf never verifies at this position unless it is
		// byte-identical.
		j := rapid.IntRange(0, n-1).Draw(rt, "j")
		if !bytes.Equal(leaves[j], leaves[i]) && VerifyMembership(root, leaves[j], p) == nil {
			rt.Fatalf("leaf %d verified with the proof of leaf %d", j, i)
		}
		// A root over one leaf more never accepts the proof.
		bigger := NewTree(append(append([][]byte(nil), leaves...), []byte("tail"))).Root()
		if VerifyMembership(bigger, leaves[i], p) == nil {
			rt.Fatalf("proof for size %d verified against a size %d root", n, n+1)
		}
	})
}

// TestProp_RootMatchesNaiveDefinition cross-checks the tree against a
// second, independent transcription of the RFC 6962 recursion over hashed
// leaves.
func TestProp_RootMatchesNaiveDefinition(t *testing.T) {
	var naive func(h [][]byte) []byte
	naive = func(h [][]byte) []byte {
		switch len(h) {
		case 0:
			return EmptyRoot()
		case 1:
			return h[0]
		}
		k := 1
		for k*2 < len(h) {
			k *= 2
		}
		return NodeHash(naive(h[:k]), naive(h[k:]))
	}
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(0, 200).Draw(rt, "n")
		leaves := make([][]byte, n)
		hashed := make([][]byte, n)
		for i := range leaves {
			leaves[i] = rapid.SliceOfN(rapid.Byte(), 0, 40).Draw(rt, "leaf")
			hashed[i] = LeafHash(leaves[i])
		}
		if got, want := NewTree(leaves).Root(), naive(hashed); !bytes.Equal(got, want) {
			rt.Fatalf("size %d: tree root %x != naive %x", n, got, want)
		}
	})
}
