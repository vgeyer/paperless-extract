package queue_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vgeyer/paperless-extract/internal/queue"
)

const waitTimeout = 5 * time.Second

func recv[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(waitTimeout):
		t.Fatalf("timeout waiting for %s", what)
		var zero T
		return zero
	}
}

// eventually polls cond (used only for observing state transitions that have
// no other hook, e.g. InFlight dropping after a handler returns).
func eventually(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

type blockingHandler struct {
	started chan int
	release chan struct{}
	done    chan int
	calls   atomic.Int32
}

func newBlocking() *blockingHandler {
	return &blockingHandler{
		started: make(chan int, 100),
		release: make(chan struct{}),
		done:    make(chan int, 100),
	}
}

func (b *blockingHandler) handle(ctx context.Context, id int) {
	b.calls.Add(1)
	b.started <- id
	<-b.release
	b.done <- id
}

func TestFullAndDuplicateBeforeStart(t *testing.T) {
	var calls atomic.Int32
	q := queue.New(2, 1, func(ctx context.Context, id int) { calls.Add(1) }, nil)
	t.Cleanup(func() { _ = q.Shutdown(context.Background()) })

	if err := q.Enqueue(1); err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue(1); !errors.Is(err, queue.ErrDuplicate) {
		t.Fatalf("dup waiting: %v", err)
	}
	if err := q.Enqueue(2); err != nil {
		t.Fatal(err)
	}
	if q.Len() != 2 || q.InFlight() != 2 {
		t.Fatalf("Len=%d InFlight=%d", q.Len(), q.InFlight())
	}
	if err := q.Enqueue(3); !errors.Is(err, queue.ErrFull) {
		t.Fatalf("full: %v", err)
	}
	// duplicate check precedes full check
	if err := q.Enqueue(2); !errors.Is(err, queue.ErrDuplicate) {
		t.Fatalf("dup while full: %v", err)
	}
	// rejected ID 3 must not be marked in flight
	if q.InFlight() != 2 {
		t.Fatalf("InFlight=%d after ErrFull", q.InFlight())
	}
	if calls.Load() != 0 {
		t.Fatalf("handler ran before Start")
	}
}

func TestSizeAndWorkersBelowOneTreatedAsOne(t *testing.T) {
	q := queue.New(0, 0, func(ctx context.Context, id int) {}, nil)
	t.Cleanup(func() { _ = q.Shutdown(context.Background()) })
	if err := q.Enqueue(1); err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue(2); !errors.Is(err, queue.ErrFull) {
		t.Fatalf("want ErrFull, got %v", err)
	}
	b := newBlocking()
	q2 := queue.New(-5, -1, b.handle, nil)
	q2.Start(context.Background())
	if err := q2.Enqueue(7); err != nil {
		t.Fatal(err)
	}
	if id := recv(t, b.started, "start"); id != 7 {
		t.Fatalf("id=%d", id)
	}
	close(b.release)
	recv(t, b.done, "done")
	if err := q2.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestProcessesJobsAndDedupeWhileRunning(t *testing.T) {
	b := newBlocking()
	q := queue.New(5, 1, b.handle, nil)
	q.Start(context.Background())
	q.Start(context.Background()) // no-op

	if err := q.Enqueue(10); err != nil {
		t.Fatal(err)
	}
	if id := recv(t, b.started, "start 10"); id != 10 {
		t.Fatalf("started %d", id)
	}
	// running now: not waiting, but in flight
	if err := q.Enqueue(10); !errors.Is(err, queue.ErrDuplicate) {
		t.Fatalf("dup running: %v", err)
	}
	if q.Len() != 0 || q.InFlight() != 1 {
		t.Fatalf("Len=%d InFlight=%d", q.Len(), q.InFlight())
	}
	// running job does not occupy capacity
	for i := 1; i <= 5; i++ {
		if err := q.Enqueue(100 + i); err != nil {
			t.Fatalf("enqueue %d: %v", 100+i, err)
		}
	}
	if err := q.Enqueue(200); !errors.Is(err, queue.ErrFull) {
		t.Fatalf("want full, got %v", err)
	}
	if q.Len() != 5 || q.InFlight() != 6 {
		t.Fatalf("Len=%d InFlight=%d", q.Len(), q.InFlight())
	}
	close(b.release)
	got := map[int]bool{}
	for i := 0; i < 6; i++ {
		got[recv(t, b.done, "done")] = true
	}
	if len(got) != 6 {
		t.Fatalf("done = %v", got)
	}
	eventually(t, func() bool { return q.InFlight() == 0 }, "InFlight==0")
	// ID reusable after handler returned
	if err := q.Enqueue(10); err != nil {
		t.Fatalf("re-enqueue after done: %v", err)
	}
	recv(t, b.started, "restart 10")
	recv(t, b.done, "redone 10")
	if err := q.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b.calls.Load() != 7 {
		t.Fatalf("calls=%d", b.calls.Load())
	}
}

func TestMultipleWorkersRunConcurrently(t *testing.T) {
	b := newBlocking()
	q := queue.New(10, 3, b.handle, nil)
	q.Start(context.Background())
	for i := 1; i <= 3; i++ {
		if err := q.Enqueue(i); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		recv(t, b.started, "concurrent start")
	}
	if q.InFlight() != 3 || q.Len() != 0 {
		t.Fatalf("InFlight=%d Len=%d", q.InFlight(), q.Len())
	}
	close(b.release)
	for i := 0; i < 3; i++ {
		recv(t, b.done, "done")
	}
	if err := q.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestEnqueueAfterShutdown(t *testing.T) {
	q := queue.New(5, 1, func(ctx context.Context, id int) {}, nil)
	q.Start(context.Background())
	if err := q.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue(1); !errors.Is(err, queue.ErrClosed) {
		t.Fatalf("want ErrClosed, got %v", err)
	}
	// second Shutdown safe
	if err := q.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
}

func TestShutdownBeforeStart(t *testing.T) {
	var calls atomic.Int32
	q := queue.New(5, 1, func(ctx context.Context, id int) { calls.Add(1) }, nil)
	_ = q.Enqueue(1)
	if err := q.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown before Start: %v", err)
	}
	if err := q.Enqueue(2); !errors.Is(err, queue.ErrClosed) {
		t.Fatalf("want ErrClosed, got %v", err)
	}
	// ErrClosed takes precedence over duplicate
	if err := q.Enqueue(1); !errors.Is(err, queue.ErrClosed) {
		t.Fatalf("want ErrClosed for previously queued id, got %v", err)
	}
	q.Start(context.Background())
	// Give a (wrongly) started worker a chance; nothing must run.
	time.Sleep(20 * time.Millisecond)
	if calls.Load() != 0 {
		t.Fatalf("handler ran after Shutdown")
	}
}

func TestShutdownWaitsForRunningAndDiscardsWaiting(t *testing.T) {
	b := newBlocking()
	q := queue.New(5, 1, b.handle, nil)
	q.Start(context.Background())
	if err := q.Enqueue(1); err != nil {
		t.Fatal(err)
	}
	recv(t, b.started, "start 1")
	for _, id := range []int{2, 3} {
		if err := q.Enqueue(id); err != nil {
			t.Fatal(err)
		}
	}
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- q.Shutdown(context.Background()) }()

	// Shutdown must not return while handler 1 is running.
	select {
	case err := <-shutdownDone:
		t.Fatalf("Shutdown returned early: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	eventually(t, func() bool { return errors.Is(q.Enqueue(99), queue.ErrClosed) }, "closed")
	close(b.release)
	if id := recv(t, b.done, "done 1"); id != 1 {
		t.Fatalf("done %d", id)
	}
	if err := recv(t, shutdownDone, "shutdown"); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if n := b.calls.Load(); n != 1 {
		t.Fatalf("handler calls = %d, want 1 (waiting jobs must be discarded)", n)
	}
	select {
	case id := <-b.started:
		t.Fatalf("job %d started after Shutdown", id)
	default:
	}
}

func TestShutdownContextExpiryCancelsHandler(t *testing.T) {
	ctxSeen := make(chan error, 1)
	started := make(chan struct{})
	h := func(ctx context.Context, id int) {
		close(started)
		<-ctx.Done()
		ctxSeen <- ctx.Err()
	}
	q := queue.New(1, 1, h, nil)
	q.Start(context.Background())
	if err := q.Enqueue(1); err != nil {
		t.Fatal(err)
	}
	recv(t, started, "start")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := q.Shutdown(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown err = %v, want DeadlineExceeded", err)
	}
	if e := recv(t, ctxSeen, "handler ctx canceled"); e == nil {
		t.Fatalf("handler ctx not canceled")
	}
}

func TestHandlerContextDerivedFromStartCtx(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ctxSeen := make(chan error, 1)
	started := make(chan struct{})
	q := queue.New(1, 1, func(hctx context.Context, id int) {
		close(started)
		<-hctx.Done()
		ctxSeen <- hctx.Err()
	}, nil)
	q.Start(ctx)
	if err := q.Enqueue(1); err != nil {
		t.Fatal(err)
	}
	recv(t, started, "start")
	cancel()
	if e := recv(t, ctxSeen, "handler ctx"); !errors.Is(e, context.Canceled) {
		t.Fatalf("handler ctx err = %v", e)
	}
	sctx, scancel := context.WithTimeout(context.Background(), waitTimeout)
	defer scancel()
	_ = q.Shutdown(sctx)
}

func TestPanicRecovery(t *testing.T) {
	var mu sync.Mutex
	var seen []int
	done := make(chan struct{}, 10)
	q := queue.New(5, 1, func(ctx context.Context, id int) {
		defer func() { done <- struct{}{} }()
		mu.Lock()
		seen = append(seen, id)
		mu.Unlock()
		if id == 1 {
			panic("boom")
		}
	}, nil)
	q.Start(context.Background())
	if err := q.Enqueue(1); err != nil {
		t.Fatal(err)
	}
	recv(t, done, "panic job")
	eventually(t, func() bool { return q.InFlight() == 0 }, "in-flight cleared after panic")
	// ID reusable after a panic, worker still alive
	if err := q.Enqueue(1); err != nil {
		t.Fatalf("re-enqueue after panic: %v", err)
	}
	recv(t, done, "panic job again")
	if err := q.Enqueue(2); err != nil {
		t.Fatal(err)
	}
	recv(t, done, "job 2")
	if err := q.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 3 || seen[2] != 2 {
		t.Fatalf("seen = %v", seen)
	}
}

func TestConcurrentEnqueueRace(t *testing.T) {
	var processed sync.Map
	q := queue.New(1000, 4, func(ctx context.Context, id int) { processed.Store(id, true) }, nil)
	q.Start(context.Background())
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				err := q.Enqueue(i) // same IDs from all goroutines
				if err != nil && !errors.Is(err, queue.ErrDuplicate) {
					t.Errorf("enqueue: %v", err)
				}
				_ = q.Len()
				_ = q.InFlight()
			}
		}(g)
	}
	wg.Wait()
	eventually(t, func() bool { return q.InFlight() == 0 }, "drain")
	if err := q.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		if _, ok := processed.Load(i); !ok {
			t.Errorf("id %d never processed", i)
		}
	}
}
