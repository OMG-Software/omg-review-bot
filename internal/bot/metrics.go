package bot

import (
	"context"
	"time"

	"github.com/OMG-Software/omg-review-bot/internal/llm"
	"github.com/OMG-Software/omg-review-bot/internal/store"
)

// Every call a job makes to a configured model is recorded (store.ModelCall)
// so the metrics pages can say which models reviews went to. The job says
// what the calls are for by tagging its context; tuned.Chat, which every
// job's call goes through, does the recording.

type callTagKey struct{}

// callTag is what a job's model calls are recorded under.
type callTag struct {
	kind      string // store.CallReview, store.CallReply
	accountID int64
	reviewID  int64
}

// withCallTag tags ctx's model calls.
func withCallTag(ctx context.Context, t callTag) context.Context {
	return context.WithValue(ctx, callTagKey{}, t)
}

// meterCall records one model call. A call cut short because its job ended
// (a shutdown, say) isn't the model failing, so it isn't recorded.
func (b *Bot) meterCall(ctx context.Context, u modelUse, resp *llm.Response, err error, took time.Duration) {
	if err != nil && ctx.Err() != nil {
		return
	}
	t, _ := ctx.Value(callTagKey{}).(callTag)
	c := store.ModelCall{
		AccountID: t.accountID, ReviewID: t.reviewID, Kind: t.kind,
		ModelSet: u.set, ConfigID: u.cfg.ID, Label: u.cfg.Label(), Provider: u.cfg.Provider, Model: u.cfg.Model,
		OK: err == nil, DurationMS: took.Milliseconds(),
	}
	if resp != nil {
		c.PromptTokens, c.CompletionTokens = resp.PromptTokens, resp.CompletionTokens
	}
	if rerr := b.store.RecordModelCall(c); rerr != nil {
		b.log.Warn("couldn't record a model call", "err", rerr)
	}
}
