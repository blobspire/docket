package selfcheck

import "testing"

func TestCatchesAsks(t *testing.T) {
	for _, s := range []string{
		"I've staged the migration. It needs your approval before I run it against prod.",
		"Done with the refactor. You'll need to re-authenticate WARP in the GUI.",
		"The SSD is formatted; once you plug in the second drive I can finish the RAID.",
		"Could you please approve the ADR so I can merge?",
		"I'm holding the deploy until you confirm the new price.",
		"This step requires a physical action: power-cycle the arm controller.",
		"Should I go ahead and delete the old backups?",
		"Blocked on you: the API key in the vault expired.",
		"Please log in to the vendor portal and download the CSV.",
		"Only you can approve the spend.",
	} {
		if _, _, ok := Match(s); !ok {
			t.Errorf("missed: %q", s)
		}
	}
}

func TestIgnoresOrdinaryProse(t *testing.T) {
	for _, s := range []string{
		"I updated the parser and all 42 tests pass.",
		"Next I'll refactor the collector to use the new cursor type.",
		"The function returns an error when the cursor is past the end of the file.",
		"Here's a summary of what changed in the build.",
		"I ran the benchmark; latency dropped by 30%.",
	} {
		if sent, id, ok := Match(s); ok {
			t.Errorf("false positive (%s) on %q: %q", id, s, sent)
		}
	}
}

func TestIgnoresCodeAndQuotes(t *testing.T) {
	text := "Here is the help text:\n```\nYou'll need to run make install first.\n```\n> Could you approve this? (quoted from the issue)\nAll done; `you will need to` appears only in code."
	if sent, id, ok := Match(text); ok {
		t.Fatalf("matched code or quote (%s): %q", id, sent)
	}
}

func TestReturnsTheSentence(t *testing.T) {
	sent, id, ok := Match("Built and tested everything. Once you approve the PR I'll merge it. Thanks!")
	if !ok || id != "once-you" || sent != "Once you approve the PR I'll merge it." {
		t.Fatalf("got %q %q %v", sent, id, ok)
	}
}
