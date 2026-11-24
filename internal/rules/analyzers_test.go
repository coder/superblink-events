package rules_test

import (
	"go/parser"
	"go/token"
	"testing"

	"github.com/stretchr/testify/require"
)

func parseSource(t *testing.T, path, src string) sourceFile {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, parser.ParseComments|parser.SkipObjectResolution)
	require.NoError(t, err)
	return sourceFile{path: path, fset: fset, file: file}
}

func TestSleepsOutsideBubbles(t *testing.T) {
	t.Parallel()
	f := parseSource(t, "x_test.go", `package x

import (
	"testing"
	"testing/synctest"
	clock "time"
)

func helper() { clock.Sleep(1) }

func TestX(t *testing.T) {
	clock.Sleep(1)
	synctest.Test(t, func(t *testing.T) {
		clock.Sleep(1)
		go func() { clock.Sleep(1) }()
	})
	go func() { clock.Sleep(1) }()
}
`)
	require.Equal(t, []int{9, 12, 17}, sleepsOutsideBubbles(f), "a sleep counts unless a literal passed to synctest.Test encloses it, under any import name")
	require.Empty(t, sleepsOutsideBubbles(parseSource(t, "y_test.go", "package y\n\nfunc Sleep() {}\n\nfunc f() { Sleep() }\n")), "a file that does not import time has no real sleep")
}

func TestOutsideIDs(t *testing.T) {
	t.Parallel()
	src := `package x

// The token has two holders (R14).
// RFC 6749 section 5.2, and HTTP/2 and the D-Bus API.
/* Vercel finding 5 and
D4 */
var s = "R6 in a string is no comment"
`
	require.Equal(t, []string{"x.go:3: cites R14", "x.go:5: cites finding 5", "x.go:6: cites D4"}, outsideIDs([]sourceFile{parseSource(t, "x.go", src)}))
	require.Empty(t, outsideIDs([]sourceFile{parseSource(t, "faucet/compartments_test.go", "package x\n\n// R7 forbids that.\n")}), "a file that defines its own IDs may cite them")
}
