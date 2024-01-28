// Copyright (c) 2024 Karl Gaissmaier
// SPDX-License-Identifier: MIT

package bart

import (
	"cmp"
	"fmt"
	"io"
	"net/netip"
	"slices"
	"strings"
)

// container for the direct kids, needed for hierarchical tree print.
type kidT[V any] struct {
	// for traversing
	n    *node[V]
	path []byte
	idx  uint
	// for printing
	cidr netip.Prefix
	val  V
}

// String returns a hierarchical tree diagram of the ordered CIDRs
// as string, just a wrapper for [Table.Fprint].
//
// The order from top to bottom is in ascending order of the prefix address
// and the subtree structure is determined by the CIDRs coverage.
//
//	▼
//	├─ 10.0.0.0/8 (9.9.9.9)
//	│  ├─ 10.0.0.0/24 (8.8.8.8)
//	│  └─ 10.0.1.0/24 (10.0.0.0)
//	├─ 127.0.0.0/8 (127.0.0.1)
//	│  └─ 127.0.0.1/32 (127.0.0.1)
//	├─ 169.254.0.0/16 (10.0.0.0)
//	├─ 172.16.0.0/12 (8.8.8.8)
//	└─ 192.168.0.0/16 (9.9.9.9)
//	   └─ 192.168.1.0/24 (127.0.0.1)
//	▼
//	└─ ::/0 (2001:db8::1)
//	   ├─ ::1/128 (::1%eth0)
//	   ├─ 2000::/3 (2001:db8::1)
//	   │  └─ 2001:db8::/32 (2001:db8::1)
//	   └─ fe80::/10 (::1%lo)
func (t *Table[V]) String() string {
	t.init()
	w := new(strings.Builder)
	_ = t.Fprint(w)
	return w.String()
}

// Fprint writes a hierarchical tree diagram of the ordered CIDRs to w.
// If w is nil, Fprint panics.
func (t *Table[V]) Fprint(w io.Writer) error {
	is4 := true
	root4 := t.rootNodeVersion(is4)
	if root4.hasType() == nullNode {
		return nil
	}
	if _, err := fmt.Fprint(w, "▼\n"); err != nil {
		return err
	}
	if err := root4.printRec(w, 0, nil, is4, ""); err != nil {
		return err
	}

	is4 = false
	root6 := t.rootNodeVersion(is4)
	if root6.hasType() == nullNode {
		return nil
	}
	if _, err := fmt.Fprint(w, "▼\n"); err != nil {
		return err
	}
	if err := root6.printRec(w, 0, nil, is4, ""); err != nil {
		return err
	}
	return nil
}

// printRec, the output is a hierarchical CIDR tree.
func (n *node[V]) printRec(w io.Writer, parentIdx uint, path []byte, is4 bool, pad string) error {
	// get direct childs for this parentIdx ...
	kids := n.getKidsRec(parentIdx, path, is4)

	// sort them by netip.Prefix, not by baseIndex
	slices.SortFunc(kids, sortPrefix[V])

	// symbols used in tree
	glyphe := "├─ "
	spacer := "│  "

	// for all kids under this node ...
	for i, kid := range kids {
		// ... treat last kid special
		if i == len(kids)-1 {
			glyphe = "└─ "
			spacer = "   "
		}

		// print prefix and val, padded with glyphe
		if _, err := fmt.Fprintf(w, "%s%s (%v)\n", pad+glyphe, kid.cidr, kid.val); err != nil {
			return err
		}

		// rec-descent with this prefix as parentIdx.
		// hierarchical nested tree view, two rec-descent functions
		// work together to spoil the reader.
		if err := kid.n.printRec(w, kid.idx, kid.path, is4, pad+spacer); err != nil {
			return err
		}
	}

	return nil
}

// getKidsRec, returns the dierct kids below path and parentIdx.
// It's a recursive monster together with printRec,
// you have to know the data structure by heart to understand this function!
//
// See the  artlookup.pdf paper in the doc folder,
// the baseIndex function is the key.
func (n *node[V]) getKidsRec(parentIdx uint, path []byte, is4 bool) []kidT[V] {
	directKids := []kidT[V]{}

	// the node may have prefixes,
	// for all prefixes in this node do ...
	for _, idx := range n.prefixes.allIndexes() {
		// parent or self, handled alreday in an upper stack frame.
		if idx <= parentIdx {
			continue
		}

		// check if lpmIdx for this idx is equal to parentIdx
		if lpmIdx, _, _ := n.prefixes.lpmByIndex(idx >> 1); lpmIdx == parentIdx {
			val := n.prefixes.getVal(idx)
			path := append([]byte{}, path...)
			cidr := cidrFromPath(path, idx, is4)
			directKids = append(directKids, kidT[V]{n, path, idx, cidr, *val})
		}
	}

	// the node may have childs, the rec-descent monster starts
	for _, addr := range n.children.allAddrs() {
		// do a longest-prefix-match
		if lpmIdx, _, _ := n.prefixes.lpmByAddr(addr); lpmIdx == parentIdx {
			// child is directKid, but we need the prefix for this child
			path := append([]byte{}, path...)

			// get next child node
			c := n.children.get(addr)

			// traverse, rec-descent call with next child node
			directKids = append(directKids, c.getKidsRec(0, append(path, byte(addr)), is4)...)
		}
	}

	return directKids
}

// cidrFromPath, make prefix from byte path, next addr (byte, stride) and pfxLen.
func cidrFromPath(path []byte, idx uint, is4 bool) netip.Prefix {
	addr, pfxLen := baseIndexToPrefix(idx)

	// append last (partially) masked byte to path and
	// calc bits with pathLen and pfxLen
	bs := append(path, byte(addr))
	bits := len(path)*stride + pfxLen

	var ip netip.Addr
	if is4 {
		b4 := [4]byte{}
		copy(b4[:], bs)
		ip = netip.AddrFrom4(b4)
	} else {
		b16 := [16]byte{}
		copy(b16[:], bs)
		ip = netip.AddrFrom16(b16)
	}

	// make a normalized prefix from ip/bits
	return netip.PrefixFrom(ip, bits).Masked()
}

// sortPrefix, sort the kids by addr and pfxLen.
func sortPrefix[V any](a, b kidT[V]) int {
	if cmp := a.cidr.Addr().Compare(b.cidr.Addr()); cmp != 0 {
		return cmp
	}
	return cmp.Compare(a.cidr.Bits(), b.cidr.Bits())
}