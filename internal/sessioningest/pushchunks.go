package sessioningest

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
)

// Content-defined chunking for pushed session bytes. Cut points depend only
// on nearby content, so an appended transcript reuses every chunk of its
// previous revision except the last, and storage across an append-only
// session's revisions stays close to its final size.
const (
	cdcMinChunk = 2 << 10
	cdcMaxChunk = 64 << 10
	// cdcMaskBits sets the average chunk size past the minimum (2^13 = 8 KiB).
	cdcMaskBits = 13
)

// cdcMask tests the top bits of the gear hash, which depend on the last 64
// bytes rather than only the last few.
const cdcMask = uint64(1<<cdcMaskBits-1) << (64 - cdcMaskBits)

// gearTable is fixed forever: changing it changes every cut point, which
// only costs deduplication, but there is no reason to.
var gearTable = func() [256]uint64 {
	var t [256]uint64
	state := uint64(0x6469646572_6f74) // "diderot"
	for i := range t {
		// splitmix64
		state += 0x9e3779b97f4a7c15
		z := state
		z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
		z = (z ^ (z >> 27)) * 0x94d049bb133111eb
		t[i] = z ^ (z >> 31)
	}
	return t
}()

// cdcChunks splits data into content-defined chunks. The chunks alias data.
func cdcChunks(data []byte) [][]byte {
	var out [][]byte
	for len(data) > 0 {
		n := cdcCut(data)
		out = append(out, data[:n])
		data = data[n:]
	}
	return out
}

func cdcCut(data []byte) int {
	if len(data) <= cdcMinChunk {
		return len(data)
	}
	limit := min(len(data), cdcMaxChunk)
	var h uint64
	for i := cdcMinChunk; i < limit; i++ {
		h = (h << 1) + gearTable[data[i]]
		if h&cdcMask == 0 {
			return i + 1
		}
	}
	return limit
}

// revisionToken is the push revision: a keyed hash over the owner, session,
// format, and received bytes. Keying and scoping it means a token cannot be
// used to confirm a guess about another session's contents.
func revisionToken(key []byte, owner, sessionID, format string, body []byte) string {
	mac := hmac.New(sha256.New, key)
	var n [8]byte
	for _, part := range []string{"diderot-session-revision-v1", owner, sessionID, format} {
		binary.BigEndian.PutUint64(n[:], uint64(len(part)))
		mac.Write(n[:])
		mac.Write([]byte(part))
	}
	mac.Write(body)
	return "hmac-sha256:" + hex.EncodeToString(mac.Sum(nil))
}
