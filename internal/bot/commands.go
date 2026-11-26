package bot

import (
	"context"
	"fmt"
	"regexp"

	"github.com/OMG-Software/omg-review-bot/internal/github"
	"github.com/OMG-Software/omg-review-bot/internal/store"
)

// reviewCommandRE matches a PR conversation comment asking the bot for a
// review: a line of its own that is just "@<slug> review" (any case, an
// optional "[bot]" and full stop or exclamation mark). Only a line of its own,
// so a question that happens to start with the word ("@bot reviewing this, why
// …?") is answered as a mention, and a quoted request ("> @bot review") isn't
// taken as a new one.
func reviewCommandRE(slug string) *regexp.Regexp {
	return regexp.MustCompile(`(?im)^[ \t]*@` + regexp.QuoteMeta(slug) + `(\[bot\])?[ \t]+review[ \t]*[.!]?[ \t\r]*$`)
}

// canAskForReview reports whether a repository permission (see
// github.Client.Permission) lets its holder ask for a review in a comment:
// write access, as pushing a commit (which gets a review anyway) needs. A
// review asked for this way passes the per-PR review limit, so on a public
// repository it can't be left to anyone who can comment.
func canAskForReview(permission string) bool {
	return permission == "admin" || permission == "write"
}

// reviewCommand acts on comment c, which asks for a review of pr: it queues a
// manual review, as Review now in the web UI does. A review it queued is
// answered by the review's own "Queued for review" comment; a request that
// isn't taken gets a reply saying why. A failure to read the person's access
// or to post the reply leaves the comment unhandled, so it's tried again.
func (b *Bot) reviewCommand(ctx context.Context, cl *github.Client, pr *store.PR, c github.IssueComment) error {
	who := c.User.Login
	perm, err := cl.Permission(ctx, pr.Repo, who)
	if err != nil {
		return fmt.Errorf("reading @%s's access to %s: %w", who, pr.Repo, err)
	}
	var reply string
	if !canAskForReview(perm) {
		reply = "only people with write access to this repository can ask for a review."
		b.logf("info", pr.Repo, pr.Number, "refused a review request from @%s (%s access)", who, perm)
	} else if err := b.ReviewNow(pr.Repo, pr.Number, pr.AccountID); err != nil {
		reply = "I couldn't queue a review: " + err.Error() + "."
		b.logf("info", pr.Repo, pr.Number, "review request from @%s not queued: %v", who, err)
	} else {
		b.logf("info", pr.Repo, pr.Number, "@%s asked for a review in a comment", who)
	}
	if reply == "" {
		// Marked at once: the review is queued, and trying the comment
		// again would queue another.
		_ = b.store.MarkHandled(c.ID, "review-command", pr.Repo, pr.Number, 0)
		return nil
	}
	posted, err := cl.CreateIssueComment(ctx, pr.Repo, pr.Number, "@"+who+" "+reply)
	if err != nil {
		return err
	}
	_ = b.store.MarkHandled(c.ID, "review-command", pr.Repo, pr.Number, posted.ID)
	return nil
}
