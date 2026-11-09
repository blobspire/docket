// Package selfcheck spots sentences where an agent asks the human for
// something ("needs your approval", "you'll need to log in", …). The Stop
// hook uses it to nudge an agent, at most once per turn, that said such a
// thing without filing it in docket. It is a cheap regex pass: no model.
package selfcheck

import (
	"regexp"
	"strings"
)

type Pattern struct {
	ID string
	Re *regexp.Regexp
}

func p(id, re string) Pattern { return Pattern{ID: id, Re: regexp.MustCompile(`(?i)` + re)} }

// Patterns are the default ask-like phrasings.
var Patterns = []Pattern{
	p("needs-your", `\b(need|needs|require|requires|waiting (on|for)|pending)\s+(your|cole'?s|the user'?s|human)\s+(approval|ok|okay|go-ahead|sign-?off|decision|input|confirmation|review|permission|action)\b`),
	p("waiting-on-you", `\b(waiting|blocked)\s+on\s+(you|cole|the user)\b`),
	p("youll-need-to", `\byou('ll| will| would| may| might)?\s+(need|have)\s+to\b`),
	p("once-you", `\b(once|when|after|until)\s+you('ve| have)?\s+(approve|confirm|log ?in|sign ?in|authenticate|re-?auth|install|plug|insert|connect|enable|grant|add|provide|decide|reply|respond|review)`),
	p("can-you", `\b(can|could|would)\s+you\s+(please\s+)?(approve|confirm|log ?in|sign ?in|provide|send|add|enable|grant|decide|choose|review|run|install|plug|insert|check)\b`),
	p("please-do", `\bplease\s+(approve|confirm|log ?in|sign ?in|re-?authenticate|run|install|plug|insert|enable|grant|provide|decide|review)\b`),
	p("holding", `\b(i'?m|i am|i'?ll|i will)\s+(hold|holding|wait|waiting|pause|pausing)\b.{0,60}\b(until|for)\s+(you|your|cole)\b`),
	p("manual-step", `\b(requires?|needs?)\s+(a\s+)?(human|manual|physical)\s+(step|action|intervention|approval)\b`),
	p("only-you", `\bonly\s+you\s+can\b`),
	p("should-i", `\b(should i|shall i|do you want me to|would you like me to|want me to)\b[^?]{0,200}\?`),
}

// Match returns the first ask-like sentence in text and the pattern that
// matched. Code blocks, quoted lines and inline code are ignored.
func Match(text string) (sentence, patternID string, ok bool) {
	for _, s := range Sentences(text) {
		for _, pat := range Patterns {
			if pat.Re.MatchString(s) {
				return s, pat.ID, true
			}
		}
	}
	return "", "", false
}

var (
	fence      = regexp.MustCompile("(?s)```.*?(```|$)")
	inlineCode = regexp.MustCompile("`[^`\n]*`")
	splitter   = regexp.MustCompile(`([.!?])\s+|\n{2,}|\n\s*[-*•]\s+|\n\s*\d+[.)]\s+`)
)

// Sentences splits prose into sentences, dropping code and quoted lines.
func Sentences(text string) []string {
	text = fence.ReplaceAllString(text, " ")
	var kept []string
	for _, line := range strings.Split(text, "\n") {
		if t := strings.TrimSpace(line); strings.HasPrefix(t, ">") || strings.HasPrefix(t, "|") {
			continue // quotes and table rows
		}
		kept = append(kept, line)
	}
	text = inlineCode.ReplaceAllString(strings.Join(kept, "\n"), "CODE")
	text = splitter.ReplaceAllString(text, "$1\x00")
	var out []string
	for _, s := range strings.Split(text, "\x00") {
		s = strings.Join(strings.Fields(s), " ")
		if len(s) >= 12 && len(s) <= 600 {
			out = append(out, s)
		}
	}
	return out
}
