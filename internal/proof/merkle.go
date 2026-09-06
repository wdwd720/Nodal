package proof

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
)

// HashSize is the size of every leaf, node and root hash.
const HashSize = sha256.Size

// Domain-separation prefixes (RFC 6962 §2.1).
const (
	leafPrefix = 0x00
	nodePrefix = 0x01
)

// Merkle errors.
var (
	ErrLeafIndex    = errors.New("proof: leaf index out of range")
	ErrProofShape   = errors.New("proof: membership proof does not match the tree size")
	ErrProofInvalid = errors.New("proof: membership proof does not verify")
)

// LeafHash returns sha256(0x00 || leaf).
func LeafHash(leaf []byte) []byte {
	h := sha256.New()
	h.Write([]byte{leafPrefix})
	h.Write(leaf)
	return h.Sum(nil)
}

// NodeHash returns sha256(0x01 || left || right).
func NodeHash(left, right []byte) []byte {
	h := sha256.New()
	h.Write([]byte{nodePrefix})
	h.Write(left)
	h.Write(right)
	return h.Sum(nil)
}

// EmptyRoot is MTH({}) = sha256("") (RFC 6962 §2.1). Checkpoints never cover
// zero leaves; it exists so the tree is total.
func EmptyRoot() []byte {
	sum := sha256.Sum256(nil)
	return sum[:]
}

// Tree is an immutable RFC 6962 Merkle tree over a list of leaves. Leaves
// are copied on construction, so the caller's slice can be reused.
type Tree struct {
	leaves [][]byte
}

// NewTree builds a tree over leaves in the given order.
func NewTree(leaves [][]byte) *Tree {
	cp := make([][]byte, len(leaves))
	for i, l := range leaves {
		cp[i] = bytes.Clone(l)
	}
	return &Tree{leaves: cp}
}

// Len returns the number of leaves.
func (t *Tree) Len() int { return len(t.leaves) }

// Leaf returns a copy of leaf i.
func (t *Tree) Leaf(i int) ([]byte, error) {
	if i < 0 || i >= len(t.leaves) {
		return nil, fmt.Errorf("%w: %d of %d", ErrLeafIndex, i, len(t.leaves))
	}
	return bytes.Clone(t.leaves[i]), nil
}

// Root returns MTH over all leaves.
func (t *Tree) Root() []byte { return t.mth(0, len(t.leaves)) }

// mth computes MTH(D[lo:hi]) per RFC 6962 §2.1.
func (t *Tree) mth(lo, hi int) []byte {
	n := hi - lo
	switch n {
	case 0:
		return EmptyRoot()
	case 1:
		return LeafHash(t.leaves[lo])
	default:
		k := splitPoint(n)
		return NodeHash(t.mth(lo, lo+k), t.mth(lo+k, hi))
	}
}

// splitPoint returns the largest power of two strictly less than n (n >= 2).
func splitPoint(n int) int {
	k := 1
	for k*2 < n {
		k *= 2
	}
	return k
}

// Sibling is one node of an audit path. Left reports whether the sibling
// sits to the left of the path node (so the parent is NodeHash(sibling,
// node)); the flag is redundant with the index-based verification and is
// cross-checked by VerifyMembership so a proof cannot lie about its shape.
type Sibling struct {
	Hash []byte `json:"hash"`
	Left bool   `json:"left"`
}

// Proof is an RFC 6962 inclusion proof for one leaf, ordered from the
// sibling of the leaf up to the child of the root.
type Proof struct {
	LeafIndex int       `json:"leaf_index"`
	LeafCount int       `json:"leaf_count"`
	Siblings  []Sibling `json:"siblings"`
}

// Prove returns the audit path of leaf index (RFC 6962 §2.1.1).
func (t *Tree) Prove(index int) (Proof, error) {
	if index < 0 || index >= len(t.leaves) {
		return Proof{}, fmt.Errorf("%w: %d of %d", ErrLeafIndex, index, len(t.leaves))
	}
	return Proof{LeafIndex: index, LeafCount: len(t.leaves), Siblings: t.path(index, 0, len(t.leaves))}, nil
}

// path computes PATH(m, D[lo:hi]) with m relative to lo, leaf to root.
func (t *Tree) path(m, lo, hi int) []Sibling {
	n := hi - lo
	if n <= 1 {
		return nil
	}
	k := splitPoint(n)
	if m < k {
		return append(t.path(m, lo, lo+k), Sibling{Hash: t.mth(lo+k, hi), Left: false})
	}
	return append(t.path(m-k, lo+k, hi), Sibling{Hash: t.mth(lo, lo+k), Left: true})
}

// VerifyMembership checks that leaf sits at p.LeafIndex in a tree of
// p.LeafCount leaves whose root is root (RFC 9162 §2.1.3.2). It returns nil
// only when the recomputed root equals root, the path length is exactly
// right for the position, and every sibling's Left flag matches the
// position-derived direction.
func VerifyMembership(root, leaf []byte, p Proof) error {
	if p.LeafCount < 1 || p.LeafIndex < 0 || p.LeafIndex >= p.LeafCount {
		return fmt.Errorf("%w: index %d, count %d", ErrProofShape, p.LeafIndex, p.LeafCount)
	}
	fn, sn := p.LeafIndex, p.LeafCount-1
	r := LeafHash(leaf)
	for i, s := range p.Siblings {
		if sn == 0 {
			return fmt.Errorf("%w: path longer than the tree height", ErrProofShape)
		}
		if len(s.Hash) != HashSize {
			return fmt.Errorf("%w: sibling %d is not a sha256 hash", ErrProofShape, i)
		}
		if fn&1 == 1 || fn == sn {
			if !s.Left {
				return fmt.Errorf("%w: sibling %d direction flag disagrees with the leaf position", ErrProofShape, i)
			}
			r = NodeHash(s.Hash, r)
			for fn&1 == 0 && fn != 0 {
				fn >>= 1
				sn >>= 1
			}
		} else {
			if s.Left {
				return fmt.Errorf("%w: sibling %d direction flag disagrees with the leaf position", ErrProofShape, i)
			}
			r = NodeHash(r, s.Hash)
		}
		fn >>= 1
		sn >>= 1
	}
	if sn != 0 {
		return fmt.Errorf("%w: path shorter than the tree height", ErrProofShape)
	}
	if !bytes.Equal(r, root) {
		return ErrProofInvalid
	}
	return nil
}
