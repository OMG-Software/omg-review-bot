package bot

import (
	"context"
	"fmt"
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
	stop    context.CancelFunc       // stops the workers
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
	e.stop = cancel
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
		e.t.Fatalf("%s started while every worker should be busy or waiting", j.key())
	case <-time.After(100 * time.Millisecond):
	}
}

func replies(repo string, n int) job { return job{kind: jobReplies, repo: repo, number: n} }

// Four jobs for different PRs run at the same time, and the dashboard shows
// each in its own slot; a fifth waits until one of them is done.
func TestFourJobsRunAtOnce(t *testing.T) {
	if workers != 4 {
		t.Fatalf("workers = %d, want 4", workers)
	}
	e := newWorkerEnv(t)
	queued := []job{replies("acme/web", 1), replies("acme/web", 2), replies("acme/api", 3), replies("acme/api", 4)}
	last := replies("acme/docs", 5)
	for _, j := range append(queued, last) {
		if !e.b.enqueue(j) {
			t.Fatalf("%s not queued", j.key())
		}
	}
	got := map[string]bool{}
	var started []job
	for range queued {
		j := e.next()
		got[j.key()] = true
		started = append(started, j)
	}
	for _, j := range queued {
		if !got[j.key()] {
			t.Fatalf("started %v, want the first four queued", got)
		}
	}
	e.quiet()
	s := e.b.Status()
	if len(s.Working) != 4 || s.QueueLen != 1 || len(s.Slots) != 4 {
		t.Fatalf("status while four run = %+v, want four working in four slots and one queued", s)
	}
	inSlots := map[string]bool{}
	for i, a := range s.Slots {
		if a.What == "" || a.Repo == "" || a.Number == 0 {
			t.Fatalf("slot %d = %+v, want its running PR", i+1, a)
		}
		inSlots[fmt.Sprintf("%s#%d", a.Repo, a.Number)] = true
	}
	if len(inSlots) != 4 {
		t.Fatalf("slots = %+v, want a different PR in each", s.Slots)
	}
	e.finish(started[0])
	if j := e.next(); j.key() != last.key() {
		t.Fatalf("started %s, want %s", j.key(), last.key())
	}
	for _, j := range append(started[1:], last) {
		e.finish(j)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(e.b.Status().Working) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("still working after every job finished: %+v", e.b.Status().Working)
		}
		time.Sleep(10 * time.Millisecond)
	}
	for i, a := range e.b.Status().Slots {
		if a.What != "" {
			t.Errorf("slot %d = %+v after every job finished, want idle", i+1, a)
		}
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

// Running jobs are listed by when each job started: a job's next step
// restarts its own timer, as before, but doesn't move it down the list.
func TestWorkingOrderedByJobStart(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	b := New(st, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	slot := func(i int) context.Context { return context.WithValue(context.Background(), workerSlot{}, i) }
	pr := &store.PR{Repo: "acme/web", Number: 1}

	b.jobStarted(1)
	b.setCurrent(slot(1), pr, "first, part 1")
	time.Sleep(10 * time.Millisecond)
	b.jobStarted(0)
	b.setCurrent(slot(0), pr, "second")
	time.Sleep(10 * time.Millisecond)
	before := b.Status().Working[0].Since
	b.setCurrent(slot(1), pr, "first, part 2")

	s := b.Status()
	w := s.Working
	if len(w) != 2 || w[0].What != "first, part 2" || w[1].What != "second" {
		t.Fatalf("working = %+v, want the first job still listed first", w)
	}
	// Slots stay in worker order, whichever job started first.
	if len(s.Slots) != workers || s.Slots[0].What != "second" || s.Slots[1].What != "first, part 2" || s.Slots[2].What != "" {
		t.Fatalf("slots = %+v, want each job in its own worker's slot and the rest idle", s.Slots)
	}
	if !w[0].Since.After(before) {
		t.Errorf("a job's new step kept the old step's time: %v, then %v", before, w[0].Since)
	}
}

// Progress reported from outside a worker isn't shown as a worker's.
func TestProgressOutsideWorkerIgnored(t *testing.T) {
	e := newWorkerEnv(t)
	e.b.setCurrent(context.Background(), &store.PR{Repo: "acme/web", Number: 1}, "not a worker's")
	if w := e.b.Status().Working; len(w) != 0 {
		t.Fatalf("working = %+v, want nothing shown", w)
	}
}

// A worker that stops gives its PR up: the jobs waiting for that PR go back
// on the queue, still counted, rather than being lost with the PR claimed.
func TestStoppingWorkerReleasesPR(t *testing.T) {
	e := newWorkerEnv(t)
	first, waiting := job{kind: jobReview, repo: "acme/web", number: 1}, replies("acme/web", 1)
	for _, j := range []job{first, waiting} {
		if !e.b.enqueue(j) {
			t.Fatalf("%s not queued", j.key())
		}
	}
	e.next()
	deadline := time.Now().Add(5 * time.Second)
	for e.waitingFor(first) != 1 { // the other worker has handed it over
		if time.Now().After(deadline) {
			t.Fatal("the second job never waited for the first")
		}
		time.Sleep(time.Millisecond)
	}
	e.stop()
	e.finish(first)
	for {
		e.b.mu.Lock()
		claimed, back := len(e.b.active), len(e.b.jobs)
		e.b.mu.Unlock()
		if claimed == 0 && back == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after stopping: %d PRs claimed, %d jobs queued; want 0 and 1", claimed, back)
		}
		time.Sleep(time.Millisecond)
	}
	if j := <-e.b.jobs; j.key() != waiting.key() {
		t.Errorf("queued %s, want %s", j.key(), waiting.key())
	}
	if q := e.b.Status().QueueLen; q != 1 {
		t.Errorf("queue length = %d, want the waiting job still counted", q)
	}
	select {
	case j := <-e.started:
		t.Errorf("%s ran after the workers stopped", j.key())
	default:
	}
}

func (e *workerEnv) waitingFor(j job) int {
	e.b.mu.Lock()
	defer e.b.mu.Unlock()
	return len(e.b.active[prKey(j)])
}

// Stopping reviews lets the jobs under way finish but starts no more until
// work resumes, so the bot can be restarted without cutting a review short.
func TestStopReviewsLetsRunningJobsFinish(t *testing.T) {
	e := newWorkerEnv(t)
	first := replies("acme/web", 1)
	e.b.enqueue(first)
	if j := e.next(); j.key() != first.key() {
		t.Fatalf("started %s", j.key())
	}
	e.b.StopReviews()
	later := []job{replies("acme/web", 2), replies("acme/api", 3)}
	for _, j := range later {
		e.b.enqueue(j)
	}
	e.quiet()
	if s := e.b.Status(); !s.Stopped || len(s.Working) != 1 {
		t.Fatalf("status while stopping = %+v, want stopped with one job still running", s)
	}
	e.finish(first)
	e.quiet()
	deadline := time.Now().Add(5 * time.Second)
	for len(e.b.Status().Working) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the running job never finished")
		}
		time.Sleep(10 * time.Millisecond)
	}

	e.b.ResumeReviews()
	got := map[string]bool{}
	for range later {
		j := e.next()
		got[j.key()] = true
		e.finish(j)
	}
	for _, j := range later {
		if !got[j.key()] {
			t.Fatalf("after resuming, started %v; want %s too", got, j.key())
		}
	}
	if e.b.Status().Stopped {
		t.Error("still stopped after resuming")
	}
}
