package bot

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/OMG-Software/omg-review-bot/internal/settings"
	"github.com/OMG-Software/omg-review-bot/internal/store"
)

// TestMeterCall: a call whose job was cancelled isn't recorded, but one
// that ran out of time is, as a failure: a model too slow for its job.
func TestMeterCall(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "bot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	b := New(st, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	u := modelUse{cfg: settings.LLM{ID: "m1", Model: "slow-model"}}
	tag := withCallTag(context.Background(), callTag{kind: store.CallReview, accountID: 500, reviewID: 1})

	cancelled, cancel := context.WithCancel(tag)
	cancel()
	b.meterCall(cancelled, u, nil, cancelled.Err(), time.Second)

	expired, cancel2 := context.WithDeadline(tag, time.Now().Add(-time.Second))
	defer cancel2()
	b.meterCall(expired, u, nil, expired.Err(), time.Second)

	b.meterCall(tag, u, nil, errors.New("connection refused"), time.Second) // the client's own timeout or an error
	b.meterCall(tag, u, nil, nil, time.Second)

	ms, err := st.ModelUsageSince(500, 0)
	if err != nil || len(ms) != 1 {
		t.Fatalf("model use = %+v (%v)", ms, err)
	}
	if m := ms[0]; m.Calls != 3 || m.Failed != 2 {
		t.Errorf("calls = %d, failed = %d; want 3 recorded (not the cancelled one), 2 failed", m.Calls, m.Failed)
	}
}
