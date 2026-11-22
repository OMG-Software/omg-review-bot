package bot

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/OMG-Software/omg-review-bot/internal/store"
)

// workerEnv runs the bot's workers with a fake job runner. A job runs until
// the test releases it, so the test decides what overlaps.
type workerEnv struct {
	t       *testing.T
	b       *Bot
	started chan job // each job as it starts
	mu      sync.Mutex
	release map[string]chan struct{} // job key → closed to let it finish
	running map[string]bool          // PRs (prKey) with a job running now
	overlap []string                 // PRs that had two jobs running at once
}

func newWorkerEnv(t *testing.T) *workerEnv {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	e := &workerEnv{
		t:       t,
		b:       New(st, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil))),
		started: make(chan job, 16),
		release: map[string]chan struct{}{},
		running: map[string]bool{},
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	for i := range workers {
		go e.b.work(ctx, i, e.run)
	}
	return e
}

func (e *workerEnv) gate(j job) chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.release[j.key()] == nil {
		e.release[j.key()] = make(chan struct{})
	}
	return e.release[j.key()]
}

func (e *workerEnv) run(ctx context.Context, j job) {
	e.mu.Lock()
	if e.running[prKey(j)] {
		e.overlap = append(e.overlap, prKey(j))
	}
	e.running[prKey(j)] = true
	e.mu.Unlock()
	e.b.setCurrent(ctx, &store.PR{Repo: j.repo, Number: j.number}, "%s", j.key())
	e.started <- j
	<-e.gate(j)
	e.mu.Lock()
	delete(e.running, prKey(j))
	e.mu.Unlock()
}

func (e *workerEnv) finish(j job) { close(e.gate(j)) }

// next returns the next job to start, failing if none starts in time.
func (e *workerEnv) next() job {
	e.t.Helper()
	select {
	case j := <-e.started:
		return j
	case <-time.After(5 * time.Second):
		e.t.Fatal("no job started")
		return job{}
	}
}

// quiet fails if a job starts within a short while.
func (e *workerEnv) quiet() {
	e.t.Helper()
	select {
	case j := <-e.started:
		e.t.Fatalf("%s started while both workers should be busy or waiting", j.key())
	case <-time.After(100 * time.Millisecond):
	}
}

func replies(repo string, n int) job { return job{kind: jobReplies, repo: repo, number: n} }

// Two jobs for different PRs run at the same time, and the dashboard shows
// both; a third waits until one of them is done.
func TestTwoJobsRunAtOnce(t *testing.T) {
	e := newWorkerEnv(t)
	a, b, c := replies("acme/web", 1), replies("acme/web", 2), replies("acme/api", 3)
	for _, j := range []job{a, b, c} {
		if !e.b.enqueue(j) {
			t.Fatalf("%s not queued", j.key())
		}
	}
	first, second := e.next(), e.next()
	if got := map[string]bool{first.key(): true, second.key(): true}; !got[a.key()] || !got[b.key()] {
		t.Fatalf("started %s and %s, want the first two queued", first.key(), second.key())
	}
	e.quiet()
	s := e.b.Status()
	if len(s.Working) != 2 || s.QueueLen != 1 {
		t.Fatalf("status while two run = %+v, want two working and one queued", s)
	}
	e.finish(first)
	if j := e.next(); j.key() != c.key() {
		t.Fatalf("started %s, want %s", j.key(), c.key())
	}
	e.finish(second)
	e.finish(c)
	deadline := time.Now().Add(5 * time.Second)
	for len(e.b.Status().Working) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("still working after every job finished: %+v", e.b.Status().Working)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A PR's jobs never overlap: one taken off the queue while another worker
// is busy with the same PR runs after it, in order, and the free worker goes
// on to other PRs meanwhile.
func TestSamePRJobsDontOverlap(t *testing.T) {
	e := newWorkerEnv(t)
	rev := job{kind: jobReview, repo: "acme/web", number: 1}
	rep := replies("Acme/Web", 1) // the same PR, named in another case
	other := replies("acme/api", 2)
	for _, j := range []job{rev, rep, other} {
		if !e.b.enqueue(j) {
			t.Fatalf("%s not queued", j.key())
		}
	}
	// One worker takes the review; the other takes rep, finds its PR busy,
	// and moves on to the other PR. Those two start in either order.
	first, second := e.next(), e.next()
	if got := map[string]bool{first.key(): true, second.key(): true}; !got[rev.key()] || !got[other.key()] {
		t.Fatalf("started %s and %s, want %s and %s", first.key(), second.key(), rev.key(), other.key())
	}
	e.quiet()
	if q := e.b.Status().QueueLen; q != 1 {
		t.Errorf("queue length = %d, want the waiting job still counted as queued", q)
	}
	e.finish(other)
	e.quiet() // rep still waits for the review, even with a worker free
	e.finish(rev)
	if j := e.next(); j.key() != rep.key() {
		t.Fatalf("started %s after the review, want %s", j.key(), rep.key())
	}
	e.finish(rep)
	// The PR is free again: a new job for it starts at once.
	again := replies("acme/web", 1)
	if !e.b.enqueue(again) {
		t.Fatalf("%s not queued", again.key())
	}
	e.next()
	e.finish(again)
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.overlap) != 0 {
		t.Errorf("jobs for the same PR overlapped: %v", e.overlap)
	}
}
