package bot

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/OMG-Software/omg-review-bot/internal/github"
	"github.com/OMG-Software/omg-review-bot/internal/store"
)

// reviewCommandRE matches a line asking the bot for a review: just
// "@<slug> review" (any case, an optional "[bot]" and full stop or
// exclamation mark), indented less than an indented code block's 4 spaces.
func reviewCommandRE(slug string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)^ {0,3}@` + regexp.QuoteMeta(slug) + `(\[bot\])?[ \t]+review[ \t]*[.!]?[ \t]*$`)
}

// asksForReview reports whether a PR conversation comment asks for a review:
// whether a line of its own matches cmd (see reviewCommandRE), outside fenced
// code blocks and code spans. A line of its own, so a question that happens to
// start with the word ("@bot reviewing this, why …?") is answered as a
// mention; and not quoted ("> @bot review") or in code, so quoting the command
// or showing how it's used doesn't queue a review. A line typed straight under
// a quote does count, though GitHub shows it in the quote: GitHub's quote reply
// marks every line it quotes, so that line is the commenter's own.
func asksForReview(cmd *regexp.Regexp, body string) bool {
	fence := ""       // the open fence ("```", "~~~~" …); "" outside one
	var para []string // the lines of the paragraph so far, outside fences
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if run, rest := fenceRun(line); run != "" {
			if fence == "" {
				if commandIn(cmd, para) {
					return true
				}
				fence, para = run, nil
				continue
			}
			if run[0] == fence[0] && len(run) >= len(fence) && strings.TrimSpace(rest) == "" {
				fence = ""
				continue
			}
		}
		switch {
		case fence != "":
		case strings.TrimSpace(line) == "": // a paragraph ends, and code spans with it
			if commandIn(cmd, para) {
				return true
			}
			para = nil
		default:
			para = append(para, line)
		}
	}
	return commandIn(cmd, para)
}

// commandIn reports whether a line of paragraph para matches cmd outside a
// code span. A span can run across lines: "`", "@bot review", "`" is code.
func commandIn(cmd *regexp.Regexp, para []string) bool {
	spans := codeSpans(strings.Join(para, "\n"))
	at := 0 // where the line starts in the joined paragraph
	for _, line := range para {
		if cmd.MatchString(line) && !slices.ContainsFunc(spans, func(s [2]int) bool { return s[0] <= at && at < s[1] }) {
			return true
		}
		at += len(line) + 1
	}
	return false
}

// codeSpans returns where text's code spans are (from after the opening
// backticks to the closing ones), paired as CommonMark does: a run of
// backticks opens a span that the next run of the same length closes, and a
// run nothing closes is plain text.
func codeSpans(text string) [][2]int {
	type run struct{ at, n int }
	var runs []run
	for i := 0; i < len(text); {
		if text[i] != '`' {
			i++
			continue
		}
		j := i
		for j < len(text) && text[j] == '`' {
			j++
		}
		runs = append(runs, run{i, j - i})
		i = j
	}
	var spans [][2]int
	for i := 0; i < len(runs); i++ {
		for j := i + 1; j < len(runs); j++ {
			if runs[j].n == runs[i].n {
				spans = append(spans, [2]int{runs[i].at + runs[i].n, runs[j].at})
				i = j
				break
			}
		}
	}
	return spans
}

// fenceRun returns the run of 3 or more backticks or tildes that would open or
// close a fenced code block on line (indented at most 3 spaces), and the rest
// of the line after it; "" if there's none.
func fenceRun(line string) (run, rest string) {
	t := strings.TrimLeft(line, " ")
	if len(line)-len(t) > 3 || t == "" || (t[0] != '`' && t[0] != '~') {
		return "", ""
	}
	n := len(t) - len(strings.TrimLeft(t, t[:1]))
	if n < 3 {
		return "", ""
	}
	return t[:n], t[n:]
}

// canAskForReview reports whether a repository permission (see
// github.Client.Permission) lets its holder ask for a review in a comment:
// write access, as pushing a commit (which gets a review anyway) needs. That's
// a maintainer too, whom GitHub reports as "write". A review asked for this way
// passes the per-PR review limit, so on a public repository it can't be left
// to anyone who can comment.
func canAskForReview(permission string) bool {
	return permission == "admin" || permission == "write"
}

// reviewCommand acts on comment c, which asks for a review of pr: it queues a
// manual review, as Review now in the web UI does. A review it queued is
// answered by the review's own "Queued for review" comment; a request that
// isn't taken gets a reply saying why. A failure (reading the person's access,
// queuing, posting the reply) leaves the comment unhandled, so it's tried again.
func (b *Bot) reviewCommand(ctx context.Context, cl *github.Client, pr *store.PR, c github.IssueComment) error {
	who := c.User.Login
	perm, err := cl.Permission(ctx, pr.Repo, who)
	if err != nil {
		return fmt.Errorf("reading @%s's access to %s: %w", who, pr.Repo, err)
	}
	var reply string
	var refused refusal
	if !canAskForReview(perm) {
		reply = "only people with write access to this repository can ask for a review."
		b.logf("info", pr.Repo, pr.Number, "refused a review request from @%s (%s access)", who, perm)
	} else if err := b.ReviewNow(pr.Repo, pr.Number, pr.AccountID); errors.As(err, &refused) {
		reply = "I couldn't queue a review: " + refused.msg + "."
		b.logf("info", pr.Repo, pr.Number, "review request from @%s not queued: %v", who, err)
	} else if err != nil {
		// Not a reason to give the person (the database's, say): nothing
		// was queued, so the comment is tried again.
		return fmt.Errorf("queuing the review @%s asked for: %w", who, err)
	} else {
		b.logf("info", pr.Repo, pr.Number, "@%s asked for a review in a comment", who)
	}
	if reply == "" {
		// Marked at once: the review is queued, and trying the comment
		// again would queue another.
		b.markCommand(pr, c.ID)
		return nil
	}
	posted, err := cl.CreateIssueComment(ctx, pr.Repo, pr.Number, afterPrefix("@"+who, sanitizeModelText(reply)))
	if err != nil {
		return err
	}
	if err := b.store.MarkHandled(c.ID, "review-command", pr.Repo, pr.Number, posted.ID); err != nil {
		b.logf("warn", pr.Repo, pr.Number, "could not record @%s's review request as handled; it may be answered again: %v", who, err)
	}
	return nil
}

// markCommand records review-request comment id, whose review is queued, as
// handled. If the write fails, the comment is kept as taken in memory, and
// commandTaken records it on a later scan instead of letting it queue a
// second review (which would also use up another of the day's reviews).
func (b *Bot) markCommand(pr *store.PR, id int64) {
	err := b.store.MarkHandled(id, "review-command", pr.Repo, pr.Number, 0)
	b.mu.Lock()
	first := !b.commandsTaken[id]
	if err == nil {
		delete(b.commandsTaken, id)
	} else {
		b.commandsTaken[id] = true
	}
	b.mu.Unlock()
	if err != nil && first {
		b.logf("warn", pr.Repo, pr.Number, "could not record review request %d as handled; trying again on the next check: %v", id, err)
	}
}

// commandTaken reports whether review-request comment id has had its review
// queued already, though it isn't recorded as handled yet; if so, it tries
// recording it again.
func (b *Bot) commandTaken(pr *store.PR, id int64) bool {
	b.mu.Lock()
	taken := b.commandsTaken[id]
	b.mu.Unlock()
	if taken {
		b.markCommand(pr, id)
	}
	return taken
}
