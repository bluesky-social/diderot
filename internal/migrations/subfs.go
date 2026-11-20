package migrations

import (
	"embed"
	"io/fs"
	"strings"
)

// stripFS wraps an embed.FS and lies about the on-disk prefix,
// making sub-path files appear at the root. atlas.MigrateApply
// expects migrations at the root of the FS it reads from; our
// embed.FS naturally puts them under "sql/". This adapter bridges.
//
// Equivalent to `fs.Sub(root, prefix)` in stdlib — we hand-roll it
// to keep atlasexec integration obvious and avoid a shadow Sub that
// wraps fs.Sub's own quirks. Only Open is required; ReadDir and
// ReadFile are added so atlas can list/read without adapter dances.
type stripFS struct {
	root   embed.FS
	prefix string // "sql/"
}

func (s *stripFS) Open(name string) (fs.File, error) {
	if name == "." {
		name = strings.TrimSuffix(s.prefix, "/")
		return s.root.Open(name)
	}
	return s.root.Open(s.prefix + name)
}

func (s *stripFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if name == "." {
		name = strings.TrimSuffix(s.prefix, "/")
	} else {
		name = s.prefix + name
	}
	return s.root.ReadDir(name)
}

func (s *stripFS) ReadFile(name string) ([]byte, error) {
	return s.root.ReadFile(s.prefix + name)
}
