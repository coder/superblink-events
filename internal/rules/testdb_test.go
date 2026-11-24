package rules_test

import (
	"fmt"
	"go/ast"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

const testDatabaseVariable = "FAUCET_TEST_DATABASE_URL"

// testDatabaseOwners are the files that may name the test database's
// variable, with the reason.
var testDatabaseOwners = map[string]string{
	"internal/pgtest/pgtest.go":      "the one reader: every test takes the URL from pgtest.URL or pgtest.NewDB",
	"internal/docgen/env.go":         "the registry of the variables that packages other than cmd/faucet read",
	"internal/docgen/docgen_test.go": "a fixture of the registry's test",
	"internal/rules/testdb_test.go":  "this check",
}

// Every test that needs Postgres takes the server from internal/pgtest, so it
// fails without one, as in CI, and never skips on its own. Eleven copies of
// that skip once drifted apart: one of them would have skipped in CI too.
func TestOnlyPgtestReadsTheTestDatabaseVariable(t *testing.T) {
	t.Parallel()
	if problems := testDatabaseReads(goFiles(t, moduleRoot(t))); len(problems) > 0 {
		t.Fatal(strings.Join(problems, "\n"))
	}
}

func testDatabaseReads(files []sourceFile) []string {
	var problems []string
	for _, f := range files {
		if _, ok := testDatabaseOwners[f.path]; ok {
			continue
		}
		ast.Inspect(f.file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if v, err := strconv.Unquote(lit.Value); err == nil && v == testDatabaseVariable {
				problems = append(problems, fmt.Sprintf("%s:%d: names %s: take the server from pgtest.URL(t) or a database from pgtest.NewDB(t, migrate) in internal/pgtest", f.path, f.line(lit.Pos()), testDatabaseVariable))
			}
			return true
		})
	}
	return problems
}
