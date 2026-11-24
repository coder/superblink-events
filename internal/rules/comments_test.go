package rules_test

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// outsideIDRE matches the IDs that review findings, plans and design notes
// give their items: an R or a D and a number, or the word finding and a
// number. Those documents are not in the repository, so a comment that cites
// one tells the reader nothing.
var outsideIDRE = regexp.MustCompile(`\b[RD][0-9]{1,2}\b|\bfinding [0-9]+\b`)

// ownIDs are the files whose comments cite IDs that the same file defines.
var ownIDs = map[string]string{
	"faucet/compartments_test.go": "TestCompartments numbers its own rules R1 to R9 in its messages",
}

// A comment says the rule or the reason itself, never the ID of an item in a
// document outside the repository.
func TestNoCommentCitesAnIDFromOutsideTheRepository(t *testing.T) {
	t.Parallel()
	if problems := outsideIDs(goFiles(t, moduleRoot(t))); len(problems) > 0 {
		t.Fatalf("%d comments cite an ID that no file in the repository defines; say the rule or the reason itself:\n%s", len(problems), strings.Join(problems, "\n"))
	}
}

func outsideIDs(files []sourceFile) []string {
	var problems []string
	for _, f := range files {
		if _, ok := ownIDs[f.path]; ok {
			continue
		}
		for _, group := range f.file.Comments {
			for _, c := range group.List {
				for i, text := range strings.Split(c.Text, "\n") {
					for _, id := range outsideIDRE.FindAllString(text, -1) {
						problems = append(problems, fmt.Sprintf("%s:%d: cites %s", f.path, f.line(c.Pos())+i, id))
					}
				}
			}
		}
	}
	return problems
}
