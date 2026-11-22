package bot

import (
	"regexp"
	"strings"

	"github.com/OMG-Software/omg-review-bot/internal/textutil"
)

// Everything the model writes is derived from untrusted input (the PR's code,
// description and comments can carry prompt injections), so it's made inert
// before the bot posts it under its own name:
//
//   - Raw HTML is escaped. GitHub renders <img>, <picture>, <video> and the
//     like, and fetches them as soon as anyone views the PR, so a URL with
//     data in it would leak that data without anyone clicking. Escaping also
//     stops hidden HTML comments, including fake copies of the bot's own
//     <!-- prbot:… --> markers, which the bot reads back to make decisions.
//   - Markdown images become plain links, so nothing loads unless clicked.
//   - @mentions get a zero-width space after the "@", so the bot can't be
//     made to notify people or teams, or to address other bots
//     ("@dependabot merge"). It renders the same.
//   - Lines that start with a slash command ("/approve", "/merge") are
//     escaped, so chat-ops bots that read the raw comment and trust the bot's
//     account don't act on them. Inside code blocks too, for a line that is
//     just a command (paths such as /usr/bin are left alone).
//   - The text "prbot:" is broken up everywhere, even inside code, so the
//     bot's marker parsers never match anything the model wrote.
//
// Fenced code blocks and inline code are otherwise left alone: GitHub shows
// their content as text, so it's already inert, and suggested fixes keep their
// code exactly. That makes recognizing code the security-critical part: text
// taken for code that GitHub renders as markdown would skip the escaping. So
// the rules here only ever err towards prose (where the cost is a stray
// "&lt;" shown inside code):
//
//   - A fence opens only in column 0. An indented one may belong to a list
//     item or quote that ends before the fence closes, and the lines after
//     that end render as markdown.
//   - A backtick fence's info string can't contain a backtick (CommonMark),
//     or the line is a paragraph, not a fence.
//   - Code spans are found one line at a time. Markdown finds them within a
//     block, and a span that crossed a blank line, list item or heading would
//     not be one to GitHub.
//   - A backtick after a backslash is literal and doesn't open a span.
//   - A span with an unescaped "|" in it isn't trusted: in a table, GitHub
//     splits the cells on it before it looks for code.

const (
	maxModelBody  = 12000 // a finding body, summary or reply
	maxModelTitle = 200
	zwsp          = "​"
)

var (
	markerRE    = regexp.MustCompile(`(?i)prbot:`)
	fenceOpenRE = regexp.MustCompile("^(`{3,}|~{3,})(.*)$")
	fenceEndRE  = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})[ \t]*$")
	htmlTagRE   = regexp.MustCompile(`<([A-Za-z/!?])`)
	// GitHub links "@name" (a user, or "@org/team") after any non-word
	// character; after a word character it's an e-mail address.
	atMentionRE = regexp.MustCompile(`(^|[^A-Za-z0-9_])@([A-Za-z0-9])`)
	slashCmdRE  = regexp.MustCompile(`^( {0,3})/([A-Za-z])`)
	// A code line that is a bare command: "/approve", "/assign @x". A path
	// ("/usr/bin") or comment ("// x") isn't one.
	codeCmdRE = regexp.MustCompile(`^(\s*)/([A-Za-z][\w-]*)(\s|$)`)
)

// sanitizeModelText makes model-written markdown safe to post (see above).
func sanitizeModelText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = markerRE.ReplaceAllStringFunc(s, func(m string) string { return m[:len(m)-1] + zwsp + ":" })
	s = textutil.Truncate(strings.TrimSpace(s), maxModelBody)

	lines := strings.Split(s, "\n")
	var fenceChar byte
	fenceLen := 0 // > 0 inside a fenced code block
	for i, l := range lines {
		if fenceLen > 0 {
			if m := fenceEndRE.FindStringSubmatch(l); m != nil && m[1][0] == fenceChar && len(m[1]) >= fenceLen {
				fenceLen = 0
				continue
			}
			lines[i] = codeCmdRE.ReplaceAllString(l, "${1}"+zwsp+"/${2}${3}")
			continue
		}
		if m := fenceOpenRE.FindStringSubmatch(l); m != nil && !(m[1][0] == '`' && strings.Contains(m[2], "`")) {
			fenceChar, fenceLen = m[1][0], len(m[1])
			continue
		}
		lines[i] = cleanLine(l)
	}
	return strings.Join(lines, "\n")
}

// sanitizeModelLine is sanitizeModelText for one-line text such as a title,
// which is posted after other text on the same line. There it can't open a
// fence, so it gets no fence exemption: "**[P1]** ~~~ ![x](…)" isn't code.
func sanitizeModelLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	s = markerRE.ReplaceAllStringFunc(s, func(m string) string { return m[:len(m)-1] + zwsp + ":" })
	return cleanLine(textutil.Truncate(s, maxModelTitle))
}

// afterPrefix joins sanitized model text to the bot's own prefix. Text that
// opens with a fence keeps it on a line of its own: after the prefix it would
// no longer be a fence, and the lines it was exempting would render as
// markdown.
func afterPrefix(prefix, text string) string {
	first, _, _ := strings.Cut(text, "\n")
	if m := fenceOpenRE.FindStringSubmatch(first); m != nil && !(m[1][0] == '`' && strings.Contains(m[2], "`")) {
		return prefix + "\n\n" + text
	}
	return prefix + " " + text
}

// cleanLine neutralizes one line of markdown outside fenced code, leaving
// its code spans as they are.
func cleanLine(s string) string {
	s = slashCmdRE.ReplaceAllString(s, `$1\/$2`)
	var sb strings.Builder
	for len(s) > 0 {
		start, end := nextCodeSpan(s)
		if start < 0 {
			sb.WriteString(cleanText(s))
			break
		}
		sb.WriteString(cleanText(s[:start]))
		sb.WriteString(s[start:end])
		s = s[end:]
	}
	return sb.String()
}

// nextCodeSpan finds the first code span in a line that GitHub is sure to
// render as code: a run of backticks closed by a run of the same length, not
// opened by an escaped backtick, with no unescaped "|" inside. It returns -1
// when there is none.
func nextCodeSpan(s string) (start, end int) {
	for i := 0; i < len(s); {
		if s[i] != '`' {
			i++
			continue
		}
		open, n := i, runLen(s, i)
		if backslashesBefore(s, i)%2 == 1 { // "\`": the first backtick is literal
			open, n = open+1, n-1
		}
		next := open + n
		if n > 0 {
			for j := next; j < len(s); {
				if s[j] != '`' {
					j++
					continue
				}
				m := runLen(s, j)
				if m == n {
					if !hasUnescapedPipe(s[next:j]) {
						return open, j + m
					}
					break
				}
				j += m
			}
		}
		i = max(next, i+1) // unmatched or untrusted: these backticks are literal text
	}
	return -1, -1
}

func runLen(s string, i int) int {
	n := 0
	for i+n < len(s) && s[i+n] == '`' {
		n++
	}
	return n
}

func backslashesBefore(s string, i int) int {
	n := 0
	for i-n-1 >= 0 && s[i-n-1] == '\\' {
		n++
	}
	return n
}

func hasUnescapedPipe(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '|' && backslashesBefore(s, i)%2 == 0 {
			return true
		}
	}
	return false
}

// cleanText neutralizes plain markdown text (no code spans in it).
func cleanText(s string) string {
	s = htmlTagRE.ReplaceAllString(s, "&lt;$1")
	s = strings.ReplaceAll(s, "![", "[")
	return atMentionRE.ReplaceAllString(s, "$1@"+zwsp+"$2")
}
