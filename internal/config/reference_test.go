package config

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	envVarLiteral = regexp.MustCompile(`^DIDEROT_[A-Z0-9_]*[A-Z0-9]$`)
	// referenceRow matches a table row that documents a variable: its first
	// cell is the backquoted name.
	referenceRow = regexp.MustCompile("(?m)^\\| `(DIDEROT_[A-Z0-9_]*[A-Z0-9])` \\|")
)

// TestConfigurationReferenceMatchesCode keeps docs/configuration.md in step
// with the code. Configuration is read imperatively, so the reference is
// written by hand. This test checks only names: every DIDEROT_* variable
// that non-test Go source spells as a whole string literal (os.Getenv
// arguments, constants, name lists) needs a row, and every row must name a
// variable that some Go source, tests included, still spells.
func TestConfigurationReferenceMatchesCode(t *testing.T) {
	root := filepath.Join("..", "..")
	doc, err := os.ReadFile(filepath.Join(root, "docs", "configuration.md"))
	require.NoError(t, err)
	documented := map[string]bool{}
	for _, match := range referenceRow.FindAllStringSubmatch(string(doc), -1) {
		documented[match[1]] = true
	}

	read, spelled := map[string]bool{}, map[string]bool{}
	for _, dir := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if entry.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") {
				return nil
			}
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			isTest := strings.HasSuffix(path, "_test.go")
			ast.Inspect(file, func(node ast.Node) bool {
				literal, ok := node.(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					return true
				}
				value, err := strconv.Unquote(literal.Value)
				if err != nil || !envVarLiteral.MatchString(value) {
					return true
				}
				spelled[value] = true
				if !isTest {
					read[value] = true
				}
				return true
			})
			return nil
		})
		require.NoError(t, err)
	}
	require.NotEmpty(t, read, "found no DIDEROT_* literals; did the source layout move?")

	var undocumented, stale []string
	for _, name := range slices.Sorted(maps.Keys(read)) {
		if !documented[name] {
			undocumented = append(undocumented, name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(documented)) {
		if !spelled[name] {
			stale = append(stale, name)
		}
	}
	assert.Empty(t, undocumented, "variables read by the code but missing a row in docs/configuration.md")
	assert.Empty(t, stale, "rows in docs/configuration.md for variables no Go source names")
}
