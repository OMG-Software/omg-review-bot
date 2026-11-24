package bot

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/OMG-Software/omg-review-bot/internal/diff"
	"github.com/OMG-Software/omg-review-bot/internal/github"
	"github.com/OMG-Software/omg-review-bot/internal/store"
)

// reviewScope is what a re-review may raise new findings on: the code changed
// since the bot's last review. The rest was reviewed then; new findings there
// are what kept a pull request going round after round, each push answered
// with findings on code it never touched.
type reviewScope struct {
	since string                   // the commit last reviewed
	files map[string]*changedLines // path at head -> what changed in it since
}

// changedLines is what changed in one file since the last review: the
// new-file lines added, and the line where lines were removed. Context lines
// around them don't count.
type changedLines struct {
	whole bool  // GitHub doesn't show the change (binary, or too large): the whole file counts
	lines []int // in order
}

// scopeSlack is how far from a changed line a finding may be and still count
// as on the change: the model may name the line a problem shows on rather
// than the one changed.
const scopeSlack = 3

// maxScopeRanges is how many ranges of one file the prompt lists.
const maxScopeRanges = 40

// changedSince returns the scope of a re-review of head after since, or nil
// (the whole pull request) when the comparison can't be read. A comparison
// after a rebase starts where the two commits' histories meet, so it covers
// more, never less.
func (b *Bot) changedSince(ctx context.Context, cl *github.Client, pr *store.PR, since, head string) *reviewScope {
	files, err := cl.CompareFiles(ctx, pr.Repo, since, head)
	if err != nil {
		b.logf("info", pr.Repo, pr.Number, "comparing %s...%s failed (%v), so this re-review covers the whole pull request", short(since), short(head), err)
		return nil
	}
	s := &reviewScope{since: since, files: make(map[string]*changedLines, len(files))}
	for _, f := range files {
		if f.Status != "removed" {
			s.files[f.Filename] = changesIn(f.Patch)
		}
	}
	return s
}

// changesIn reads the changed lines from a patch: each added line, and for
// removed lines, the line after them (or, at a hunk's end, the one before).
func changesIn(patch string) *changedLines {
	set := map[int]bool{}
	for _, h := range diff.Parse("", patch).Hunks {
		removed, last := false, 0
		for _, l := range h.Lines {
			switch l.Kind {
			case '-':
				removed = true
				continue
			case '+':
				set[l.New] = true
			default:
				if removed {
					set[l.New] = true
				}
			}
			removed, last = false, l.New
		}
		if removed && last > 0 {
			set[last] = true
		}
	}
	c := &changedLines{whole: len(set) == 0}
	for n := range set {
		c.lines = append(c.lines, n)
	}
	sort.Ints(c.lines)
	return c
}

// near reports whether a changed line is within slack of lo..hi.
func (c *changedLines) near(lo, hi, slack int) bool {
	i := sort.SearchInts(c.lines, lo-slack)
	return i < len(c.lines) && c.lines[i] <= hi+slack
}

// covers reports whether a finding is on code changed since the last review,
// within scopeSlack lines of a changed line. A file whose change GitHub
// doesn't show counts whole, as does a finding on no line in particular. A
// nil scope covers everything.
func (s *reviewScope) covers(f finding) bool {
	if s == nil {
		return true
	}
	c, ok := s.files[f.Path]
	switch {
	case !ok:
		return false
	case c.whole || f.Line <= 0:
		return true
	}
	return c.near(f.Line, f.Line, scopeSlack)
}

// touches reports whether any of a part's diff is near code changed since the
// last review. A large file's hunks can be split over several parts, so each
// part is checked by the lines it shows, not just by its files.
func (s *reviewScope) touches(bt batch) bool {
	if s == nil {
		return true
	}
	for _, p := range bt.paths {
		c, ok := s.files[p]
		if !ok {
			continue
		}
		spans := bt.spans[p]
		if c.whole || len(spans) == 0 {
			return true
		}
		for _, sp := range spans {
			if c.near(sp.lo, sp.hi, scopeSlack) {
				return true
			}
		}
	}
	return false
}

// touchesPlan reports whether any part of p changed since the last review.
func (s *reviewScope) touchesPlan(p plan) bool {
	for _, bt := range p.batches {
		if s.touches(bt) {
			return true
		}
	}
	return false
}

// describe lists, for the prompt, the lines of a part's files changed since
// the last review, as far as the part shows them ("" for a nil scope).
func (s *reviewScope) describe(bt batch) string {
	if s == nil {
		return ""
	}
	var sb strings.Builder
	for _, p := range bt.paths {
		c, ok := s.files[p]
		if !ok {
			continue
		}
		if c.whole {
			fmt.Fprintf(&sb, "- %s: the whole file\n", p)
			continue
		}
		var ranges []span
		for _, n := range c.lines {
			if !bt.shows(p, n) {
				continue
			}
			if k := len(ranges) - 1; k >= 0 && ranges[k].hi == n-1 {
				ranges[k].hi = n
				continue
			}
			ranges = append(ranges, span{n, n})
		}
		if len(ranges) == 0 {
			continue
		}
		text := make([]string, 0, min(len(ranges), maxScopeRanges))
		for _, r := range ranges[:min(len(ranges), maxScopeRanges)] {
			if r.lo == r.hi {
				text = append(text, fmt.Sprint(r.lo))
			} else {
				text = append(text, fmt.Sprintf("%d-%d", r.lo, r.hi))
			}
		}
		if more := len(ranges) - maxScopeRanges; more > 0 {
			// Not listed, but in scope all the same: the prompt mustn't rule them out.
			fmt.Fprintf(&sb, "- %s: lines %s, and %d more changed ranges after line %d (lines added or changed in the diff there count too)\n",
				p, strings.Join(text, ", "), more, ranges[maxScopeRanges-1].hi)
			continue
		}
		fmt.Fprintf(&sb, "- %s: lines %s\n", p, strings.Join(text, ", "))
	}
	return sb.String()
}

// span is a range of new-file lines, inclusive.
type span struct{ lo, hi int }

// hunkSpan is the new-file lines a hunk shows (false for a hunk of removed
// lines only, which shows none).
func hunkSpan(h diff.Hunk) (span, bool) {
	var sp span
	for _, l := range h.Lines {
		if l.New > 0 {
			if sp.lo == 0 {
				sp.lo = l.New
			}
			sp.hi = l.New
		}
	}
	return sp, sp.lo > 0
}

// shows reports whether the part shows line n of path, give or take
// scopeSlack: a part with no spans recorded for the file shows all of it.
func (bt batch) shows(path string, n int) bool {
	spans, ok := bt.spans[path]
	if !ok {
		return true
	}
	for _, sp := range spans {
		if n >= sp.lo-scopeSlack && n <= sp.hi+scopeSlack {
			return true
		}
	}
	return false
}
