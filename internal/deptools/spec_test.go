package deptools

import "testing"

// Every tool marks the answers that say it found nothing, so a lookup that
// came back empty-handed is not taken as confirming a fact.
func TestSpecs_MarkEmptyHandedAnswersInconclusive(t *testing.T) {
	specs, err := Build(AllNames, Options{RepoRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Spec{}
	for _, s := range specs {
		if !s.ExternalEvidence || !s.Untrusted || s.Inconclusive == nil {
			t.Errorf("%s: ExternalEvidence=%v Untrusted=%v Inconclusive set=%v", s.Name, s.ExternalEvidence, s.Untrusted, s.Inconclusive != nil)
		}
		byName[s.Name] = s
	}
	cases := []struct {
		tool, output string
		inconclusive bool
	}{
		{ToolPinnedVersion, `No pin for "left-pad" found in 3 lockfile or manifest file(s) of the checkout. That does not mean it is unused.`, true},
		{ToolPinnedVersion, "Pins for \"react\":\n- package.json: ^19.0.0 (declared)", false},
		{ToolReadDependencySource, `not installed: "left-pad" has no copy under node_modules.`, true},
		{ToolReadDependencySource, `no such path "x.d.ts" in the installed copy of "react"`, true},
		{ToolReadDependencySource, "export declare function f(): void;", false},
		{ToolRegistryLookup, "ecosystem: npm\npackage: nope\npackage exists: no (the registry has no such package)", true},
		{ToolRegistryLookup, "ecosystem: npm\npackage: react\npackage exists: yes\nlatest: 19.0.0", false},
		{ToolDocsLookup, "No documentation found for /a/b/v1. The library may not be indexed.", true},
		{ToolDocsLookup, "No documentation snippets returned for /a/b/v1 and that topic.", true},
		{ToolDocsLookup, "### Save state\nUse storageState.", false},
	}
	for _, c := range cases {
		if got := byName[c.tool].Inconclusive(c.output); got != c.inconclusive {
			t.Errorf("%s.Inconclusive(%.40q) = %v, want %v", c.tool, c.output, got, c.inconclusive)
		}
	}
}
