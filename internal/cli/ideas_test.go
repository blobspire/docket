package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestProposeFilesAPlanForTheIdea(t *testing.T) {
	getenv := env(t)
	var out bytes.Buffer
	if err := File("propose", []string{"Add swipe to stash", "--why", "plan"}, &out, getenv); err == nil {
		t.Fatal("propose outside an idea's session must say how to name the idea")
	}
	if err := File("propose", []string{"Plan", "--idea", "idea-abcdefgh", "--options", "a|b"}, &out, getenv); err == nil {
		t.Fatal("a proposal's options are fixed")
	}
	if err := File("propose", []string{"Plan", "--idea", "idea-abcdefgh", "--recommend", "ship it"}, &out, getenv); err == nil {
		t.Fatal("--recommend must be one of the two options")
	}
	out.Reset()
	if err := File("propose", []string{"Add swipe to stash", "--idea", "idea-abcdefgh", "--why", "**What I heard** …"}, &out, getenv); err != nil {
		t.Fatal(err)
	}
	it := last(t).Item
	if it.Kind != "ask" || it.Idea != "idea-abcdefgh" || it.Key != "idea-abcdefgh" || it.Recommend != proposeBuild ||
		strings.Join(it.Options, "|") != proposeBuild+"|"+proposeSave || it.Body != "**What I heard** …" {
		t.Fatalf("item = %+v", it)
	}
	if !strings.Contains(out.String(), "End your turn") {
		t.Fatalf("output = %q", out.String())
	}

	// DOCKET_IDEA (set when docket started the session) names it too.
	m := map[string]string{"DOCKET_SESSION": "s", "DOCKET_IDEA": "idea-zzzzzzzz"}
	if err := File("propose", []string{"Plan", "--recommend", proposeSave}, &out, func(k string) string { return m[k] }); err != nil {
		t.Fatal(err)
	}
	if it := last(t).Item; it.Idea != "idea-zzzzzzzz" || it.Recommend != proposeSave {
		t.Fatalf("item = %+v", it)
	}
}

func TestIdeaRecords(t *testing.T) {
	getenv := env(t)
	var out bytes.Buffer
	for _, bad := range [][]string{{}, {"ship", "idea-abcdefgh"}, {"done"}, {"done", "dk-12345678"}} {
		if err := Idea(bad, &out, getenv); err == nil {
			t.Fatalf("docket idea %v should fail", bad)
		}
	}
	if err := Idea([]string{"done", "idea-abcdefgh", "--why", "merged #12"}, &out, getenv); err != nil {
		t.Fatal(err)
	}
	r := last(t)
	var p map[string]string
	json.Unmarshal(r.Payload, &p)
	if r.Event != "cli.idea" || p["op"] != "done" || p["id"] != "idea-abcdefgh" || p["why"] != "merged #12" || r.SessionID != "sess-1" {
		t.Fatalf("record = %+v %v", r, p)
	}
}

func TestCleanRemote(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/acme/docket.git":            "https://github.com/acme/docket.git",
		"https://cole:ghp_secret@github.com/acme/x.git": "https://github.com/acme/x.git",
		"https://x-access-token:tok@github.com/a/b":     "https://github.com/a/b",
		"git@github.com:acme/docket.git":                "git@github.com:acme/docket.git",
		"deploy@github.com:acme/docket.git":             "git@github.com:acme/docket.git",
		"ssh://git@github.com/acme/docket":              "ssh://git@github.com/acme/docket",
		"ssh://someone@host/repo":                       "ssh://host/repo",
		"ext::sh -c 'touch /tmp/pwned'":                 "",
		"file:///etc/passwd":                            "",
		"/Users/cole/code/repo":                         "",
		"--upload-pack=evil":                            "",
	} {
		if got := CleanRemote(in); got != want {
			t.Errorf("CleanRemote(%q) = %q, want %q", in, got, want)
		}
	}
}
