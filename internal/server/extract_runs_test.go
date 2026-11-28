package server

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// gate is a piece of work a test holds open until it says otherwise.
type gate struct {
	started chan struct{}
	finish  chan struct{}
	report  *extractReport
	err     error
}

func newGate() *gate {
	return &gate{started: make(chan struct{}), finish: make(chan struct{})}
}

func (g *gate) work(ctx context.Context, onProgress func(extractProgress)) (*extractReport, error) {
	close(g.started)
	select {
	case <-g.finish:
		return g.report, g.err
	case <-ctx.Done():
		return g.report, ctx.Err()
	}
}

// releases counts how many times each run let go of what it read from.
type releases struct {
	mu sync.Mutex
	n  map[string]int
}

func (r *releases) of(name string) func() {
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.n == nil {
			r.n = map[string]int{}
		}
		r.n[name]++
	}
}

func (r *releases) count(name string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n[name]
}

// settled waits for the queue to have nothing left to do.
func settled(t *testing.T, q *extractRuns) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		q.mu.Lock()
		idle := !q.draining
		q.mu.Unlock()
		if idle {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the queue never settled")
}

func runNamed(t *testing.T, q *extractRuns, name string) extractRun {
	t.Helper()
	for _, run := range q.all() {
		if run.Name == name {
			return run
		}
	}
	t.Fatalf("no run named %s", name)
	return extractRun{}
}

func TestExtractRunsOneAtATimeInTheOrderAsked(t *testing.T) {
	q := newExtractRuns()
	var rel releases
	first, second := newGate(), newGate()
	first.report = &extractReport{Extracted: 3}

	a, err := q.add(extractRun{Name: "a.zip", Dir: "/a"}, first.work, rel.of("a"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := q.add(extractRun{Name: "b.zip", Dir: "/b"}, second.work, rel.of("b"))
	if a.ID == "" || a.ID == b.ID {
		t.Fatalf("ids = %q, %q", a.ID, b.ID)
	}

	<-first.started
	if run := runNamed(t, q, "a.zip"); run.Waiting || run.Done {
		t.Errorf("a = %+v, want it running", run)
	}
	if run := runNamed(t, q, "b.zip"); !run.Waiting {
		t.Errorf("b = %+v, want it waiting behind a", run)
	}
	select {
	case <-second.started:
		t.Fatal("b started while a was still running")
	case <-time.After(20 * time.Millisecond):
	}
	if q.running() != 2 {
		t.Errorf("running = %d, want both counted", q.running())
	}

	close(first.finish)
	<-second.started
	if run := runNamed(t, q, "a.zip"); !run.Done || run.Report == nil || run.Report.Extracted != 3 || run.Error != "" {
		t.Errorf("a = %+v, want it done with its report", run)
	}
	close(second.finish)
	settled(t, q)

	if q.running() != 0 {
		t.Errorf("running = %d after both ended", q.running())
	}
	if rel.count("a") != 1 || rel.count("b") != 1 {
		t.Errorf("released a %d times and b %d, want once each", rel.count("a"), rel.count("b"))
	}
}

func TestExtractRunReportsProgressAndItsError(t *testing.T) {
	q := newExtractRuns()
	reported := make(chan struct{})
	finish := make(chan struct{})
	q.add(extractRun{Name: "a.zip"}, func(ctx context.Context, onProgress func(extractProgress)) (*extractReport, error) {
		onProgress(extractProgress{Files: 2, Done: 50, Total: 100, Current: "x/y.txt"})
		close(reported)
		<-finish
		return &extractReport{Extracted: 2}, errors.New("cut short")
	}, func() {})

	<-reported
	if at := runNamed(t, q, "a.zip").At; at.Files != 2 || at.Done != 50 || at.Current != "x/y.txt" {
		t.Errorf("progress = %+v", at)
	}
	close(finish)
	settled(t, q)

	run := runNamed(t, q, "a.zip")
	if run.Error != "cut short" || run.Cancelled || run.Report.Extracted != 2 {
		t.Errorf("run = %+v, want the error beside what it managed", run)
	}
	if run.At.Current != "" {
		t.Errorf("a finished run still names %q as in hand", run.At.Current)
	}
}

func TestStoppingARunningExtractEndsItAsCancelled(t *testing.T) {
	q := newExtractRuns()
	var rel releases
	g := newGate()
	g.report = &extractReport{Extracted: 1}
	run, _ := q.add(extractRun{Name: "a.zip"}, g.work, rel.of("a"))

	<-g.started
	if !q.stop(run.ID) {
		t.Fatal("stop found no run")
	}
	settled(t, q)

	got := runNamed(t, q, "a.zip")
	if !got.Done || !got.Cancelled || got.Error != "" || got.Report.Extracted != 1 {
		t.Errorf("run = %+v, want it cancelled, without an error, with what it had stored", got)
	}

	// The same gesture on a finished run dismisses it.
	if !q.stop(run.ID) || len(q.all()) != 0 {
		t.Errorf("a finished run was not dismissed: %+v", q.all())
	}
	if q.stop(run.ID) {
		t.Error("stop answered for a run that is gone")
	}
	if rel.count("a") != 1 {
		t.Errorf("released %d times", rel.count("a"))
	}
}

func TestStoppingAWaitingExtractNeverRunsIt(t *testing.T) {
	q := newExtractRuns()
	var rel releases
	first := newGate()
	q.add(extractRun{Name: "a.zip"}, first.work, rel.of("a"))
	ran := false
	waiting, _ := q.add(extractRun{Name: "b.zip"}, func(context.Context, func(extractProgress)) (*extractReport, error) {
		ran = true
		return nil, nil
	}, rel.of("b"))

	<-first.started
	q.stop(waiting.ID)
	// Over at once, not when its turn would have come.
	if run := runNamed(t, q, "b.zip"); !run.Done || !run.Cancelled || run.Waiting {
		t.Errorf("b = %+v, want it over the moment it was stopped", run)
	}
	if rel.count("b") != 1 {
		t.Errorf("b released %d times, want once, at the stop", rel.count("b"))
	}

	close(first.finish)
	settled(t, q)
	if ran {
		t.Error("a run stopped while waiting was carried out")
	}
	if rel.count("b") != 1 {
		t.Errorf("b released %d times after the queue drained", rel.count("b"))
	}
}

func TestStopAllDropsEverythingAndReleasesIt(t *testing.T) {
	q := newExtractRuns()
	var rel releases
	first := newGate()
	q.add(extractRun{Name: "a.zip"}, first.work, rel.of("a"))
	ran := false
	q.add(extractRun{Name: "b.zip"}, func(context.Context, func(extractProgress)) (*extractReport, error) {
		ran = true
		return nil, nil
	}, rel.of("b"))
	<-first.started

	q.stopAll()
	if len(q.all()) != 0 || q.running() != 0 {
		t.Errorf("runs = %+v — a locked vault remembers none of them", q.all())
	}
	settled(t, q)
	if ran {
		t.Error("a waiting run was carried out after everything was stopped")
	}
	if rel.count("a") != 1 || rel.count("b") != 1 {
		t.Errorf("released a %d times and b %d, want once each", rel.count("a"), rel.count("b"))
	}
	if len(q.all()) != 0 {
		t.Errorf("a dropped run came back as finished: %+v", q.all())
	}

	// And the queue still works afterwards.
	again := newGate()
	q.add(extractRun{Name: "c.zip"}, again.work, rel.of("c"))
	<-again.started
	close(again.finish)
	settled(t, q)
	if run := runNamed(t, q, "c.zip"); !run.Done {
		t.Errorf("c = %+v", run)
	}
}

func TestExtractRunsRefusePastTheCeilingAndReleaseWhatWasOffered(t *testing.T) {
	q := newExtractRuns()
	var rel releases
	first := newGate()
	q.add(extractRun{Name: "running.zip"}, first.work, func() {})
	<-first.started
	for i := 0; i < maxWaitingExtracts; i++ {
		if _, err := q.add(extractRun{Name: "w.zip"}, newGate().work, func() {}); err != nil {
			t.Fatalf("waiting run %d refused: %v", i, err)
		}
	}
	if _, err := q.add(extractRun{Name: "over.zip"}, newGate().work, rel.of("over")); !errors.Is(err, errTooManyExtracts) {
		t.Errorf("err = %v, want the ceiling", err)
	}
	if rel.count("over") != 1 {
		t.Error("a refused run kept hold of its archive")
	}
	q.stopAll()
	settled(t, q)
}

func TestExtractRunsForgetOldResults(t *testing.T) {
	q := newExtractRuns()
	now := time.Now()
	q.now = func() time.Time { return now }

	q.add(extractRun{Name: "a.zip"}, func(context.Context, func(extractProgress)) (*extractReport, error) {
		return &extractReport{}, nil
	}, func() {})
	settled(t, q)
	if len(q.all()) != 1 {
		t.Fatal("the result was not kept")
	}
	now = now.Add(finishedTransferTTL + time.Second)
	if len(q.all()) != 0 {
		t.Error("a result outlived the time it is kept for")
	}
}

func TestAnExtractRunTimesOutAsAnErrorNotAStop(t *testing.T) {
	q := newExtractRuns()
	q.timeout = 10 * time.Millisecond
	q.add(extractRun{Name: "slow.zip"}, func(ctx context.Context, _ func(extractProgress)) (*extractReport, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}, func() {})
	settled(t, q)
	if run := runNamed(t, q, "slow.zip"); run.Cancelled || run.Error == "" {
		t.Errorf("run = %+v, want an error: nobody stopped it", run)
	}
}

func TestStopVaultDropsOnlyThatSubVaultsRuns(t *testing.T) {
	q := newExtractRuns()
	var rel releases
	mine, theirs := newGate(), newGate()
	q.add(extractRun{Name: "main.zip"}, mine.work, rel.of("main"))
	q.add(extractRun{Name: "sub.zip", Vault: "sub-1"}, theirs.work, rel.of("sub"))
	<-mine.started

	q.stopVault("sub-1")
	if runs := q.all(); len(runs) != 1 || runs[0].Name != "main.zip" {
		t.Errorf("runs = %+v, want only the vault's own", runs)
	}
	close(mine.finish)
	settled(t, q)
	select {
	case <-theirs.started:
		t.Error("a run into a locked sub vault was carried out")
	default:
	}
	if rel.count("sub") != 1 {
		t.Errorf("sub released %d times", rel.count("sub"))
	}
}
