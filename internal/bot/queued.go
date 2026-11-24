package bot

import (
	"context"
	"time"

	"github.com/OMG-Software/omg-review-bot/internal/github"
	"github.com/OMG-Software/omg-review-bot/internal/store"
)

// A review job gets a PR comment as soon as it is queued ("Queued for
// review"). The worker turns that same comment into the review-started notice
// when it gets to the job, and every way the job can end edits it once more,
// so it never goes on saying a review is coming that won't.
//
// The comment is posted in the background: the queue is filled by the poller,
// webhooks and the web UI, none of which should wait on GitHub. The worker
// waits for the post to finish before it starts, so the comment can't be
// posted after the review has started, or twice.

// queuedNotice is one queued review's comment.
type queuedNotice struct {
	done chan struct{} // closed once the post has finished, posted or not
	// Set before done is closed:
	id     int64 // the comment; 0 if none could be posted
	cl     *github.Client
	repo   string
	number int
	head   string // the commit the comment names
	// claimed: reviewPR has taken the comment over and keeps it current.
	// Only the worker uses it, after done.
	claimed bool
}

// noticePostTimeout bounds the background post, so the worker's wait is bounded too.
const noticePostTimeout = 2 * time.Minute

// queueNotice starts posting the queued comment for review job j. The caller holds b.mu.
func (b *Bot) queueNotice(j job) {
	qn := &queuedNotice{done: make(chan struct{})}
	b.notices[j.key()] = qn
	go b.postQueued(qn, j)
}

func (b *Bot) postQueued(qn *queuedNotice, j job) {
	defer close(qn.done)
	ctx, cancel := context.WithTimeout(context.Background(), noticePostTimeout)
	defer cancel()
	pr, err := b.store.GetPR(j.repo, j.number)
	if err != nil || pr == nil || pr.HeadSHA == "" {
		return
	}
	app, err := b.getApp(ctx, b.Settings())
	if err != nil {
		b.logf("warn", pr.Repo, pr.Number, "could not post the review-queued comment: %v", err)
		return
	}
	qn.cl, qn.repo, qn.number, qn.head = app.Installation(pr.InstallationID), pr.Repo, pr.Number, pr.HeadSHA
	qn.id = b.announceNotice(ctx, qn.cl, pr, queuedBody(pr.HeadSHA, time.Now()))
}

// takeQueued returns review job j's queued comment once its post has
// finished, and forgets it (nil if the job has none).
func (b *Bot) takeQueued(ctx context.Context, j job) *queuedNotice {
	b.mu.Lock()
	qn := b.notices[j.key()]
	delete(b.notices, j.key())
	b.mu.Unlock()
	if qn == nil {
		return nil
	}
	select {
	case <-qn.done:
	case <-ctx.Done():
		return nil
	}
	if qn.id == 0 {
		return nil
	}
	return qn
}

// closeQueued edits a queued comment whose review never started to say why.
// It needs nothing but the notice, so it works when the PR's row can't be read.
func (b *Bot) closeQueued(qn *queuedNotice, reason string) {
	if qn == nil {
		return
	}
	b.updateNotice(qn.cl, &store.PR{Repo: qn.repo, Number: qn.number}, qn.id, notStartedBody(qn.head, reason))
}
