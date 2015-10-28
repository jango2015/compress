// Copyright 2015, Joe Tsai. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE.md file.

package brotli

import "math"

// TODO(dsnet): Almost all of this logic is identical to compress/flate.
// Centralize common logic to compress/internal/prefix.

// The algorithm used to decode variable length codes is based on the lookup
// method in zlib. If the code is less-than-or-equal to prefixMaxChunkBits,
// then the symbol can be decoded using a single lookup into the chunks table.
// Otherwise, the links table will be used for a second level lookup.
//
// The chunks slice is keyed by the contents of the bit buffer ANDed with
// the chunkMask to avoid a out-of-bounds lookup. The value of chunks is a tuple
// that is decoded as follow:
//
//	var length = chunks[bitBuffer&chunkMask] & prefixCountMask
//	var symbol = chunks[bitBuffer&chunkMask] >> prefixCountBits
//
// If the decoded length is larger than chunkBits, then an overflow link table
// must be used for further decoding. In this case, the symbol is actually the
// index into the links tables. The second-level links table returned is
// processed in the same way as the chunks table.
//
//	if length > chunkBits {
//		var index = symbol // Previous symbol is index into links tables
//		length = links[index][bitBuffer>>chunkBits & linkMask] & prefixCountMask
//		symbol = links[index][bitBuffer>>chunkBits & linkMask] >> prefixCountBits
//	}
//
// See the following:
//	http://www.gzip.org/algorithm.txt

const (
	// These values add up to the width of a uint16 integer.
	prefixCountBits  = 4  // Number of bits to store the bit-width of the code
	prefixSymbolBits = 12 // Number of bits to store the symbol value

	prefixCountMask    = (1 << prefixCountBits) - 1
	prefixMaxChunkBits = 9 // This can be tuned for better performance
)

type prefixDecoder struct {
	chunks    []uint16   // First-level lookup map
	links     [][]uint16 // Second-level lookup map
	chunkMask uint16     // Mask the width of the chunks table
	linkMask  uint16     // Mask the width of the link table
	numSyms   uint16     // Number of symbols
	chunkBits uint8      // Bit-width of the chunks table
	minBits   uint8      // The minimum number of bits to safely make progress
}

// Init initializes prefixDecoder according to the codes provided.
// The symbols provided must be unique and in ascending order.
//
// If assignCodes is true, then generate a canonical prefix tree using the
// prefixCode.len field and assign the generated value to prefixCode.val.
//
// If assignCodes is false, then initialize using the information inside the
// codes themselves. The input codes must form a valid prefix tree.
func (pd *prefixDecoder) Init(codes []prefixCode, assignCodes bool) {
	// Handle special case trees.
	if len(codes) <= 1 {
		switch {
		case len(codes) == 0: // Empty tree (should panic if used later)
			*pd = prefixDecoder{chunks: pd.chunks[:0], links: pd.links[:0], numSyms: 0}
		case len(codes) == 1: // Single code tree (bit-width of zero)
			*pd = prefixDecoder{
				chunks:  append(pd.chunks[:0], codes[0].sym<<prefixCountBits|0),
				links:   pd.links[:0],
				numSyms: 1,
			}
		}
		return
	}

	// Compute basic statistics on the symbols.
	var bitCnts [maxPrefixBits + 1]uint
	var minBits, maxBits uint8 = math.MaxUint8, 0
	symLast := -1
	for _, c := range codes {
		if int(c.sym) <= symLast {
			panic(ErrCorrupt) // Non-unique or non-monotonically increasing
		}
		if minBits > c.len {
			minBits = c.len
		}
		if maxBits < c.len {
			maxBits = c.len
		}
		bitCnts[c.len]++     // Histogram of bit counts
		symLast = int(c.sym) // Keep track of last symbol
	}
	if maxBits >= 1<<prefixCountBits || minBits == 0 {
		panic(ErrCorrupt) // Bit-width is too long or too short
	}
	if symLast >= 1<<prefixSymbolBits {
		panic(ErrCorrupt) // Alphabet cardinality too large
	}

	// Compute the next code for a symbol of a given bit length.
	var nextCodes [maxPrefixBits + 1]uint
	var code uint
	for i := minBits; i <= maxBits; i++ {
		code <<= 1
		nextCodes[i] = code
		code += bitCnts[i]
	}
	if code != 1<<maxBits {
		panic(ErrCorrupt) // Tree is under or over subscribed
	}

	// Allocate chunks table if necessary.
	pd.numSyms = uint16(len(codes))
	pd.minBits = minBits
	pd.chunkBits = maxBits
	if pd.chunkBits > prefixMaxChunkBits {
		pd.chunkBits = prefixMaxChunkBits
	}
	numChunks := 1 << pd.chunkBits
	pd.chunks = extendUint16s(pd.chunks, numChunks)
	pd.chunkMask = uint16(numChunks - 1)

	// Allocate links tables if necessary.
	pd.links = pd.links[:0]
	pd.linkMask = 0
	if pd.chunkBits < maxBits {
		numLinks := 1 << (maxBits - pd.chunkBits)
		pd.linkMask = uint16(numLinks - 1)

		if assignCodes {
			baseCode := nextCodes[pd.chunkBits+1] >> 1
			pd.links = extendSliceUints16s(pd.links, numChunks-int(baseCode))
			for linkIdx := range pd.links {
				code := reverseBits(uint16(baseCode)+uint16(linkIdx), uint(pd.chunkBits))
				pd.links[linkIdx] = extendUint16s(pd.links[linkIdx], numLinks)
				pd.chunks[code] = uint16(linkIdx<<prefixCountBits) | uint16(pd.chunkBits+1)
			}
		} else {
			for i := range pd.chunks {
				pd.chunks[i] = 0 // Logic below relies zero value as uninitialized
			}
			for _, c := range codes {
				if c.len <= pd.chunkBits {
					continue // Ignore symbols that don't require links
				}
				code := c.val & pd.chunkMask
				if pd.chunks[code] > 0 {
					continue // Link table already initialized
				}
				linkIdx := len(pd.links)
				pd.links = extendSliceUints16s(pd.links, len(pd.links)+1)
				pd.links[linkIdx] = extendUint16s(pd.links[linkIdx], numLinks)
				pd.chunks[code] = uint16(linkIdx<<prefixCountBits) | uint16(pd.chunkBits+1)
			}
		}
	}

	// Fill out chunks and links tables with values.
	for i, c := range codes {
		chunk := c.sym<<prefixCountBits | uint16(c.len)
		if assignCodes {
			codes[i].val = reverseBits(uint16(nextCodes[c.len]), uint(c.len))
			nextCodes[c.len]++
			c = codes[i]
		}

		if c.len <= pd.chunkBits {
			skip := 1 << uint(c.len)
			for j := int(c.val); j < len(pd.chunks); j += skip {
				pd.chunks[j] = chunk
			}
		} else {
			linkIdx := pd.chunks[c.val&pd.chunkMask] >> prefixCountBits
			links := pd.links[linkIdx]
			skip := 1 << uint(c.len-pd.chunkBits)
			for j := int(c.val >> pd.chunkBits); j < len(links); j += skip {
				links[j] = chunk
			}
		}
	}

	const sanity = false
	if sanity && !checkPrefixes(codes) {
		panic(ErrCorrupt) // The codes do not form a valid prefix tree.
	}
}

// checkPrefixes reports whether any codes have overlapping prefixes.
// This check is expensive and runs in O(n^2) time!
func checkPrefixes(codes []prefixCode) bool {
	for i, c1 := range codes {
		for j, c2 := range codes {
			mask := uint16(1)<<c1.len - 1
			if i != j && c1.len <= c2.len && c1.val&mask == c2.val&mask {
				return false
			}
		}
	}
	return true
}

func extendUint16s(s []uint16, n int) []uint16 {
	if cap(s) >= n {
		return s[:n]
	}
	return append(s[:cap(s)], make([]uint16, n-cap(s))...)
}

func extendSliceUints16s(s [][]uint16, n int) [][]uint16 {
	if cap(s) >= n {
		return s[:n]
	}
	return append(s[:cap(s)], make([][]uint16, n-cap(s))...)
}