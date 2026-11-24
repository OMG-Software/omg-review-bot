package bot

import (
	"context"
	"fmt"
	"strconv"
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
	since string                // the commit last reviewed
	files map[string]*diff.File // path at head -> its changes since; no lines: the whole file
}

// maxScopeRanges is how many line ranges of one file the prompt lists.
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
	s := &reviewScope{since: since, files: make(map[string]*diff.File, len(files))}
	for _, f := range files {
		if f.Status != "removed" {
			s.files[f.Filename] = diff.Parse(f.Filename, f.Patch)
		}
	}
	return s
}

// covers reports whether a finding is on code changed since the last review:
// on a line of a changed hunk, or within 3 lines of one. A file whose changes
// GitHub doesn't show (binary, or too large) counts whole, as does a finding
// on no line in particular. A nil scope covers everything.
func (s *reviewScope) covers(f finding) bool {
	if s == nil {
		return true
	}
	df, ok := s.files[f.Path]
	switch {
	case !ok:
		return false
	case df.Empty() || f.Line <= 0 || df.Valid(f.Line):
		return true
	}
	_, near := df.Nearest(f.Line, 3)
	return near
}

// touches reports whether any of paths changed since the last review.
func (s *reviewScope) touches(paths []string) bool {
	if s == nil {
		return true
	}
	for _, p := range paths {
		if _, ok := s.files[p]; ok {
			return true
		}
	}
	return false
}

// touchesPlan reports whether any part of p changed since the last review.
func (s *reviewScope) touchesPlan(p plan) bool {
	for _, bt := range p.batches {
		if s.touches(bt.paths) {
			return true
		}
	}
	return false
}

// describe lists, for the prompt, the lines of paths changed since the last
// review ("" for a nil scope).
func (s *reviewScope) describe(paths []string) string {
	if s == nil {
		return ""
	}
	var sb strings.Builder
	for _, p := range paths {
		df, ok := s.files[p]
		if !ok {
			continue
		}
		var ranges []string
		for _, h := range df.Hunks {
			lo, hi := 0, 0
			for _, l := range h.Lines {
				if l.New > 0 {
					if lo == 0 {
						lo = l.New
					}
					hi = l.New
				}
			}
			switch {
			case lo == 0:
			case lo == hi:
				ranges = append(ranges, strconv.Itoa(lo))
			default:
				ranges = append(ranges, fmt.Sprintf("%d-%d", lo, hi))
			}
		}
		switch {
		case len(ranges) == 0:
			fmt.Fprintf(&sb, "- %s: the whole file\n", p)
		case len(ranges) > maxScopeRanges:
			fmt.Fprintf(&sb, "- %s: lines %s, and %d more ranges\n", p, strings.Join(ranges[:maxScopeRanges], ", "), len(ranges)-maxScopeRanges)
		default:
			fmt.Fprintf(&sb, "- %s: lines %s\n", p, strings.Join(ranges, ", "))
		}
	}
	return sb.String()
}
