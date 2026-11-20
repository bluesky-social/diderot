package files

import (
	"crypto/sha256"
	"encoding/hex"
)

// sha256Hex returns the lowercase hex encoding of sha256(data).
// Stored on slack_files.bytes_hash so a re-fetch can detect that
// the file is bit-identical to a prior mirror (skip caption regen)
// and so a future orphan-reaper / dedup pass has a content key.
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
