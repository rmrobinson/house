package configutil

import "testing"

// Deliberately failing test. Exists only on a throwaway branch to confirm the
// `main` ruleset blocks merging when CI fails. Never merge.
func TestCIRulesetVerification(t *testing.T) {
	t.Fatal("deliberate failure: verifying that required checks block the merge")
}
