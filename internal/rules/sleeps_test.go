package rules_test

import (
	"fmt"
	"go/ast"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// realSleeps counts, for each test file that still has them, the time.Sleep
// calls outside a synctest bubble. A test that sleeps in real time to let
// something happen races the load of the machine and fails under -race on a
// busy host (#32, #33). The counts only go down: remove a sleep and lower its
// entry here.
var realSleeps = map[string]int{
	"cmd/faucet/internal_test.go":            1,
	"cmd/faucet/names_internal_test.go":      1,
	"faucet/deliver_storeerror_test.go":      2,
	"faucet/deliver_test.go":                 1,
	"faucet/deliverharness_test.go":          1,
	"faucet/envelope_test.go":                1,
	"faucet/fairness_test.go":                1,
	"faucet/oauth/accesscache_test.go":       1,
	"faucet/oauth/cimd_cache_test.go":        1,
	"faucet/oauth/grant_test.go":             1,
	"faucet/oauth/workload_internal_test.go": 1,
	"faucet/pgstore/lines_test.go":           3,
	"faucet/pgstore/pgstore_test.go":         1,
	"faucet/pgstore/rawevents_test.go":       2,
	"faucet/pins_test.go":                    1,
	"faucet/targetlines_test.go":             3,
}

// A test waits for what it needs, or runs in a synctest bubble, where
// time.Sleep moves a virtual clock once every goroutine waits. It never
// sleeps in real time.
func TestNoTestAddsASleepInRealTime(t *testing.T) {
	t.Parallel()
	found := map[string][]int{}
	for _, f := range goFiles(t, moduleRoot(t)) {
		if strings.HasSuffix(f.path, "_test.go") {
			if lines := sleepsOutsideBubbles(f); len(lines) > 0 {
				found[f.path] = lines
			}
		}
	}
	var problems []string
	for _, path := range slices.Sorted(maps.Keys(found)) {
		lines := found[path]
		if len(lines) > realSleeps[path] {
			for _, line := range lines {
				problems = append(problems, fmt.Sprintf("%s:%d: time.Sleep waits in real time outside a synctest bubble: wrap the test in synctest.Test (as TestDeliverDueReturnsTheFirstStoreFailureWhenWorkersFailInTurn does) or wait for the event itself", path, line))
			}
		}
	}
	for _, path := range slices.Sorted(maps.Keys(realSleeps)) {
		if n := len(found[path]); n < realSleeps[path] {
			problems = append(problems, fmt.Sprintf("%s has %d real-time sleeps: lower its entry in realSleeps from %d to %d, or delete it at 0", path, n, realSleeps[path], n))
		}
	}
	if len(problems) > 0 {
		t.Fatal(strings.Join(problems, "\n"))
	}
}

// sleepsOutsideBubbles returns the lines of the time.Sleep calls in f that no
// function literal passed to synctest.Test encloses.
func sleepsOutsideBubbles(f sourceFile) []int {
	timePkg, synctestPkg := importName(f.file, "time"), importName(f.file, "testing/synctest")
	if timePkg == "" {
		return nil
	}
	bubbles := map[*ast.FuncLit]bool{}
	var lines []int
	var stack []ast.Node
	ast.Inspect(f.file, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return false
		}
		stack = append(stack, n)
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if synctestPkg != "" && (isCall(call, synctestPkg, "Test") || isCall(call, synctestPkg, "Run")) {
			for _, arg := range call.Args {
				if lit, ok := arg.(*ast.FuncLit); ok {
					bubbles[lit] = true
				}
			}
		}
		if isCall(call, timePkg, "Sleep") && !slices.ContainsFunc(stack, func(n ast.Node) bool {
			lit, ok := n.(*ast.FuncLit)
			return ok && bubbles[lit]
		}) {
			lines = append(lines, f.line(call.Pos()))
		}
		return true
	})
	return lines
}

// importName is the name under which f imports path, or "" when it does not.
func importName(f *ast.File, path string) string {
	for _, imp := range f.Imports {
		if p, err := strconv.Unquote(imp.Path.Value); err == nil && p == path {
			if imp.Name != nil {
				return imp.Name.Name
			}
			return path[strings.LastIndex(path, "/")+1:]
		}
	}
	return ""
}

func isCall(call *ast.CallExpr, pkg, name string) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == pkg
}
