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
//   - Raw HTML is broken. GitHub renders <img>, <picture>, <video> and the
//     like, and fetches them as soon as anyone views the PR, so a URL with
//     data in it would leak that data without anyone clicking. This also
//     stops hidden HTML comments, including fake copies of the bot's own
//     <!-- prbot:… --> markers, which the bot reads back to make decisions.
//   - Markdown images become plain links, so nothing loads unless clicked.
//   - @mentions are broken, so the bot can't be made to notify people or
//     teams, or to address other bots ("@dependabot merge").
//   - Lines that start with a slash command ("/approve", "/merge") are
//     broken, so chat-ops bots that read the raw comment and trust the bot's
//     account don't act on them.
//   - The text "prbot:" is broken up everywhere, so the bot's marker parsers
//     never match anything the model wrote.
//
// Each is broken with a zero-width space ("<​img", "!​[",
// "@​name", "​/approve"), which renders the same in prose and in
// inline code alike. So inline code needs no exemption, and none is made:
// deciding exactly which backticks GitHub pairs into code spans means parsing
// markdown blocks as GitHub does, and any mismatch would leave live markdown
// unescaped. The cost is a hidden character in inline code that someone copies.
//
// Fenced code blocks are left alone apart from "prbot:" and bare command
// lines, which are rare in real code. A suggested change ("```suggestion")
// that needed either is posted as plain code, so a reviewer can't commit the
// hidden character with "Apply suggestion". A fence is only trusted where
// GitHub is sure to see one:
//
//   - It opens in column 0. An indented one may belong to a list item or quote
//     that ends before the fence closes, and the lines after that end render
//     as markdown.
//   - A backtick fence's info string can't contain a backtick (CommonMark),
//     or the line is a paragraph, not a fence.
//
// Every other run of three or more backticks or tildes is broken, so GitHub
// sees no fence the sanitizer doesn't. Otherwise an indented "  ```", which
// GitHub opens a fence with, would make it close at the next "```", while the
// sanitizer took that line for an opener and left the markdown after it alone.

const (
	maxModelBody  = 12000 // a finding body, summary or reply
	maxModelTitle = 200
	zwsp          = "​"
)

var (
	markerRE    = regexp.MustCompile(`(?i)prbot:`)
	fenceOpenRE = regexp.MustCompile("^(`{3,}|~{3,})(.*)$")
	fenceEndRE  = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})[ \t]*$")
	// Where raw HTML starts: a tag, closing tag, comment, declaration or
	// processing instruction (and autolinks, which are harmless but alike).
	htmlTagRE = regexp.MustCompile(`<([A-Za-z/!?])`)
	// GitHub links "@name" (a user, or "@org/team") after any non-word
	// character; after a word character it's an e-mail address.
	atMentionRE = regexp.MustCompile(`(^|[^A-Za-z0-9_])@([A-Za-z0-9])`)
	slashCmdRE  = regexp.MustCompile(`^(\s*)/([A-Za-z])`)
	// A code line that is a bare command: "/approve", "/assign @x". A path
	// ("/usr/bin") or comment ("// x") isn't one.
	codeCmdRE = regexp.MustCompile(`^(\s*)/([A-Za-z][\w-]*)(\s|$)`)
	// A run that could open or close a fence somewhere the sanitizer doesn't
	// trust one (see cleanLine).
	fenceRunRE = regexp.MustCompile("(`{3,}|~{3,})")
)

// sanitizeModelText makes model-written markdown safe to post (see above).
func sanitizeModelText(s string) string {
	// Every line ending CommonMark knows, so these lines are GitHub's lines:
	// a lone "\r" ends one too.
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
	lines := strings.Split(textutil.Truncate(strings.TrimSpace(s), maxModelBody), "\n")

	out := make([]string, 0, len(lines)+1)
	for i := 0; i < len(lines); i++ {
		if !opensFence(lines[i]) {
			out = append(out, cleanLine(breakMarkers(lines[i])))
			continue
		}
		open := fenceOpenRE.FindStringSubmatch(lines[i])
		end := i + 1
		for end < len(lines) && !closesFence(lines[end], open[1]) {
			end++
		}
		code, changed := make([]string, 0, end-i-1), false
		for _, l := range lines[i+1 : end] {
			c := codeCmdRE.ReplaceAllString(breakMarkers(l), "${1}"+zwsp+"/${2}${3}")
			code, changed = append(code, c), changed || c != l
		}
		opener := breakMarkers(lines[i])
		// A suggestion is applied to the code as it's posted, hidden
		// characters and all, so one that had to change, or that the length
		// cap cut short, is shown as plain code instead.
		if isSuggestion(open[2]) && (changed || end == len(lines)) {
			opener = open[1]
		}
		out = append(append(out, opener), code...)
		if end < len(lines) {
			out = append(out, lines[end])
		} else {
			// Left open (the model forgot, or the cap cut it off): close it, or
			// everything the bot appends, its markers included, renders as code.
			out = append(out, open[1])
		}
		i = end
	}
	return strings.Join(out, "\n")
}

// closesFence reports whether a line closes the fence opened by fence ("```", "~~~~"…).
func closesFence(l, fence string) bool {
	m := fenceEndRE.FindStringSubmatch(l)
	return m != nil && m[1][0] == fence[0] && len(m[1]) >= len(fence)
}

// isSuggestion reports whether a fence's info string makes it a GitHub
// suggested change, which a reviewer can commit with one click.
func isSuggestion(info string) bool {
	f := strings.Fields(info)
	return len(f) > 0 && strings.EqualFold(f[0], "suggestion")
}

// sanitizeModelLine is sanitizeModelText for one-line text such as a title,
// which is posted after other text on the same line. There it can't open a
// fence, so it gets no fence exemption: "**[P1]** ~~~ ![x](…)" isn't code.
func sanitizeModelLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	return cleanLine(breakMarkers(textutil.Truncate(s, maxModelTitle)))
}

// afterPrefix joins sanitized model text to the bot's own prefix. Text that
// opens with a fence keeps it on a line of its own: after the prefix it would
// no longer be a fence, and the lines it was exempting would render as
// markdown.
func afterPrefix(prefix, text string) string {
	if first, _, _ := strings.Cut(text, "\n"); opensFence(first) {
		return prefix + "\n\n" + text
	}
	return prefix + " " + text
}

// opensFence reports whether a line opens a fenced code block GitHub is sure to see.
func opensFence(l string) bool {
	m := fenceOpenRE.FindStringSubmatch(l)
	return m != nil && !(m[1][0] == '`' && strings.Contains(m[2], "`"))
}

func breakMarkers(s string) string {
	return markerRE.ReplaceAllStringFunc(s, func(m string) string { return m[:len(m)-1] + zwsp + ":" })
}

// cleanLine neutralizes one line of markdown outside fenced code.
func cleanLine(s string) string {
	s = fenceRunRE.ReplaceAllString(s, zwsp+"$1")
	s = slashCmdRE.ReplaceAllString(s, "${1}"+zwsp+"/${2}")
	s = htmlTagRE.ReplaceAllString(s, "<"+zwsp+"$1")
	s = strings.ReplaceAll(s, "![", "!"+zwsp+"[")
	return atMentionRE.ReplaceAllString(s, "${1}@"+zwsp+"${2}")
}
