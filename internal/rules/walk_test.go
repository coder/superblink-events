package rules_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// sourceFile is one parsed Go file of the module, its path relative to the
// module root and in slash form.
type sourceFile struct {
	path string
	fset *token.FileSet
	file *ast.File
}

func (f sourceFile) line(pos token.Pos) int { return f.fset.Position(pos).Line }

// moduleRoot is the directory of the module's go.mod, two levels above this
// package.
func moduleRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(root, "go.mod"))
	require.NoError(t, err, "internal/rules must sit two levels below the module root")
	return root
}

// goFiles parses every Go file of the module under root. It skips testdata,
// hidden and _ directories and every directory that holds a module of its
// own (tools/, verify/node_modules).
func goFiles(t *testing.T, root string) []sourceFile {
	t.Helper()
	var files []sourceFile
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (name == "testdata" || name == "node_modules" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")) {
				return filepath.SkipDir
			}
			if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		files = append(files, sourceFile{path: filepath.ToSlash(rel), fset: fset, file: file})
		return nil
	})
	require.NoError(t, err)
	require.NotEmpty(t, files)
	return files
}
