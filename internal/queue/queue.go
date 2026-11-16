// Package queue implements a bounded job queue with a worker pool and
// in-flight de-duplication by document ID.
package queue

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
)

var (
	ErrFull      = errors.New("queue full")
	ErrDuplicate = errors.New("document already queued")
	ErrClosed    = errors.New("queue closed")
)

// Handler processes one document. It must honor ctx cancellation.
type Handler func(ctx context.Context, docID int)

// Queue is a bounded queue with workers.
type Queue struct {
	size, workers int
	h             Handler
	log           *slog.Logger

	mu       sync.Mutex
	cond     *sync.Cond
	waiting  []int
	inflight map[int]struct{}
	closed   bool
	started  bool
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

// New creates a queue. size and workers < 1 are treated as 1.
func New(size, workers int, h Handler, logger *slog.Logger) *Queue {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	q := &Queue{
		size:     max(size, 1),
		workers:  max(workers, 1),
		h:        h,
		log:      logger,
		inflight: make(map[int]struct{}),
	}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// Start launches the workers; subsequent calls are no-ops.
func (q *Queue) Start(ctx context.Context) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.started || q.closed {
		return
	}
	q.started = true
	ctx, q.cancel = context.WithCancel(ctx)
	for range q.workers {
		q.wg.Add(1)
		go q.worker(ctx)
	}
	// Wake workers when the parent context ends.
	go func() {
		<-ctx.Done()
		q.mu.Lock()
		// Parent context ended or Shutdown ran: stop accepting and drop waiting
		// jobs so their IDs are not stuck in the in-flight set.
		q.closed = true
		for _, id := range q.waiting {
			delete(q.inflight, id)
		}
		q.waiting = nil
		q.cond.Broadcast()
		q.mu.Unlock()
	}()
}

func (q *Queue) worker(ctx context.Context) {
	defer q.wg.Done()
	for {
		q.mu.Lock()
		for len(q.waiting) == 0 && !q.closed && ctx.Err() == nil {
			q.cond.Wait()
		}
		if q.closed || ctx.Err() != nil {
			q.mu.Unlock()
			return
		}
		id := q.waiting[0]
		q.waiting = q.waiting[1:]
		q.mu.Unlock()

		q.run(ctx, id)

		q.mu.Lock()
		delete(q.inflight, id)
		q.mu.Unlock()
	}
}

func (q *Queue) run(ctx context.Context, id int) {
	defer func() {
		if r := recover(); r != nil {
			q.log.Error("handler panic", "doc_id", id, "panic", r)
		}
	}()
	q.h(ctx, id)
}

// Enqueue adds docID. It never blocks.
func (q *Queue) Enqueue(docID int) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return ErrClosed
	}
	if _, ok := q.inflight[docID]; ok {
		return ErrDuplicate
	}
	if len(q.waiting) >= q.size {
		return ErrFull
	}
	q.inflight[docID] = struct{}{}
	q.waiting = append(q.waiting, docID)
	q.cond.Signal()
	return nil
}

// Len returns the number of waiting jobs.
func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.waiting)
}

// InFlight returns the number of waiting plus running jobs.
func (q *Queue) InFlight() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.inflight)
}

// Shutdown stops accepting jobs, discards waiting jobs and waits for running
// handlers. If ctx expires first the handlers' context is canceled and
// ctx.Err() is returned.
func (q *Queue) Shutdown(ctx context.Context) error {
	q.mu.Lock()
	if !q.closed {
		q.closed = true
		if n := len(q.waiting); n > 0 {
			q.log.Warn("discarding waiting jobs", "count", n)
			for _, id := range q.waiting {
				delete(q.inflight, id)
			}
			q.waiting = nil
		}
		q.cond.Broadcast()
	}
	cancel := q.cancel
	q.mu.Unlock()

	done := make(chan struct{})
	go func() {
		q.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		if cancel != nil {
			cancel()
		}
		return nil
	case <-ctx.Done():
		if cancel != nil {
			cancel()
		}
		return ctx.Err()
	}
}
