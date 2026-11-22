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
//   - @mentions are put in code spans, so the bot can't be made to notify
//     people or teams, or to address other bots ("@dependabot merge").
//   - Lines that start with a slash command ("/approve", "/merge") are
//     escaped, so chat-ops bots that trust the bot's account don't act on them.
//   - The text "prbot:" is broken up everywhere, even inside code, so the
//     bot's marker parsers never match anything the model wrote.
//
// Fenced code blocks and inline code are left alone apart from that last step:
// GitHub shows their content as text, so it's already inert, and suggested
// fixes keep their code exactly.

const (
	maxModelBody  = 12000 // a finding body, summary or reply
	maxModelTitle = 200
)

var (
	markerRE  = regexp.MustCompile(`(?i)prbot:`)
	fenceRE   = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})")
	htmlTagRE = regexp.MustCompile(`<([A-Za-z/!?])`)
	// GitHub mentions: @user or @org/team, not preceded by a word character
	// (so e-mail addresses are left alone).
	atMentionRE = regexp.MustCompile(`(^|[^A-Za-z0-9_\-./\\` + "`" + `])@([A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?(?:/[A-Za-z0-9_.-]+)?)`)
	slashCmdRE  = regexp.MustCompile(`^( {0,3})/([A-Za-z])`)
)

// sanitizeModelText makes model-written markdown safe to post (see above).
func sanitizeModelText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = markerRE.ReplaceAllStringFunc(s, func(m string) string { return m[:len(m)-1] + "​:" })
	s = textutil.Truncate(strings.TrimSpace(s), maxModelBody)

	lines := strings.Split(s, "\n")
	var (
		out       []string
		fenceChar byte
		fenceLen  int
		para      []string // plain lines waiting to be cleaned together (code spans can span lines)
	)
	flush := func() {
		if len(para) > 0 {
			out = append(out, strings.Split(cleanProse(strings.Join(para, "\n")), "\n")...)
			para = para[:0]
		}
	}
	for _, l := range lines {
		if fenceLen > 0 { // inside a fenced code block
			out = append(out, l)
			if m := fenceRE.FindStringSubmatch(l); m != nil && m[1][0] == fenceChar && len(m[1]) >= fenceLen &&
				strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(l), string(fenceChar))) == "" {
				fenceLen = 0
			}
			continue
		}
		if m := fenceRE.FindStringSubmatch(l); m != nil {
			flush()
			fenceChar, fenceLen = m[1][0], len(m[1])
			out = append(out, l)
			continue
		}
		para = append(para, l)
	}
	flush()
	return strings.Join(out, "\n")
}

// sanitizeModelLine is sanitizeModelText for one-line text such as a title.
func sanitizeModelLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	return sanitizeModelText(textutil.Truncate(s, maxModelTitle))
}

// cleanProse neutralizes markdown outside fenced code, leaving code spans as they are.
func cleanProse(s string) string {
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

// nextCodeSpan finds the first inline code span: a run of backticks closed by
// a run of the same length. It returns -1 when there is none.
func nextCodeSpan(s string) (start, end int) {
	for i := 0; i < len(s); {
		if s[i] != '`' {
			i++
			continue
		}
		n := runLen(s, i)
		for j := i + n; j < len(s); {
			if s[j] != '`' {
				j++
				continue
			}
			m := runLen(s, j)
			if m == n {
				return i, j + m
			}
			j += m
		}
		i += n // unmatched: these backticks are literal text
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

// cleanText neutralizes plain markdown text (no code spans in it).
func cleanText(s string) string {
	s = htmlTagRE.ReplaceAllString(s, "&lt;$1")
	s = strings.ReplaceAll(s, "![", "[")
	s = atMentionRE.ReplaceAllString(s, "$1`@$2`")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = slashCmdRE.ReplaceAllString(l, `$1\/$2`)
	}
	return strings.Join(lines, "\n")
}
