package metering

import (
	"context"
	"log/slog"
	"time"
)

// Default worker tuning. BatchSize bounds how many events accumulate before a
// flush is triggered regardless of the timer; FlushInterval is the periodic
// flush trigger so events are not held arbitrarily long under light load.
const (
	defaultBatchSize     = 100
	defaultFlushInterval = 5 * time.Second
	// flushTimeout bounds a single Flush call so a hung Postgres can never wedge
	// the worker (and thereby the shutdown drain) indefinitely.
	defaultFlushTimeout = 5 * time.Second
	// flushRetries is the number of additional attempts after the first failure
	// before the batch is dropped-with-log (AC-037 bounded retry).
	defaultFlushRetries = 2
	// retryBackoff is the pause between flush attempts.
	defaultRetryBackoff = 50 * time.Millisecond
)

// Worker is COMP-017, the Metering Worker: a single background goroutine that
// batch-reads UsageEvents from the buffer and flushes them to Postgres via the
// MeteringRepository. All persistence (and all retry/error handling) happens in
// THIS goroutine — never on the request hot path (INV-003 / CON-006).
//
// Two flush triggers (FR-014): the batch reaching batchSize, OR a periodic
// timer (flushInterval). On Close the worker drains the buffer and flushes the
// remainder before returning, so no buffered event is lost on graceful shutdown
// (AC-032 / FR-012).
type Worker struct {
	events <-chan UsageEvent
	repo   MeteringRepository
	logger *slog.Logger

	batchSize     int
	flushInterval time.Duration
	flushTimeout  time.Duration
	flushRetries  int
	retryBackoff  time.Duration

	// done is closed when the worker goroutine has fully stopped, so Close can
	// wait for the final flush to complete before returning.
	done chan struct{}
	// stop signals the run loop to drain-and-exit.
	stop chan struct{}
}

// WorkerOption configures a Worker (functional options, CON-001).
type WorkerOption func(*Worker)

// WithBatchSize overrides the batch-size flush trigger (default 100). A
// non-positive value is ignored.
func WithBatchSize(n int) WorkerOption {
	return func(w *Worker) {
		if n > 0 {
			w.batchSize = n
		}
	}
}

// WithFlushInterval overrides the periodic flush trigger (default 5s). A
// non-positive value is ignored.
func WithFlushInterval(d time.Duration) WorkerOption {
	return func(w *Worker) {
		if d > 0 {
			w.flushInterval = d
		}
	}
}

// WithFlushTimeout overrides the per-flush deadline (default 5s).
func WithFlushTimeout(d time.Duration) WorkerOption {
	return func(w *Worker) {
		if d > 0 {
			w.flushTimeout = d
		}
	}
}

// WithFlushRetries overrides the number of additional flush attempts after the
// first failure before drop-with-log (default 2). A negative value is ignored;
// zero disables retrying.
func WithFlushRetries(n int) WorkerOption {
	return func(w *Worker) {
		if n >= 0 {
			w.flushRetries = n
		}
	}
}

// WithRetryBackoff overrides the pause between flush attempts (default 50ms).
func WithRetryBackoff(d time.Duration) WorkerOption {
	return func(w *Worker) {
		if d >= 0 {
			w.retryBackoff = d
		}
	}
}

// NewWorker constructs the Metering Worker reading from buf and flushing through
// repo. The worker does not start until Start is called.
func NewWorker(buf *Buffer, repo MeteringRepository, logger *slog.Logger, opts ...WorkerOption) *Worker {
	w := &Worker{
		events:        buf.Events(),
		repo:          repo,
		logger:        logger,
		batchSize:     defaultBatchSize,
		flushInterval: defaultFlushInterval,
		flushTimeout:  defaultFlushTimeout,
		flushRetries:  defaultFlushRetries,
		retryBackoff:  defaultRetryBackoff,
		done:          make(chan struct{}),
		stop:          make(chan struct{}),
	}
	for _, opt := range opts {
		opt(w)
	}
	return w
}

// Start launches the worker goroutine. It returns immediately. Call Close to
// stop the worker (drain + final flush) and wait for it to exit.
func (w *Worker) Start() {
	go w.run()
}

// run is the worker loop. It accumulates events into a batch and flushes when
// either the batch fills (batchSize) or the periodic timer fires. On stop it
// performs a final drain-and-flush so buffered events are not lost (AC-032).
func (w *Worker) run() {
	defer close(w.done)

	ticker := time.NewTicker(w.flushInterval)
	defer ticker.Stop()

	batch := make([]UsageEvent, 0, w.batchSize)

	flush := func() {
		if len(batch) == 0 {
			return
		}
		w.flush(batch)
		batch = batch[:0]
	}

	for {
		select {
		case e, ok := <-w.events:
			if !ok {
				// Buffer channel closed (not used in normal operation; Close uses
				// the stop signal). Flush remaining and exit.
				flush()
				return
			}
			batch = append(batch, e)
			if len(batch) >= w.batchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-w.stop:
			// Graceful shutdown: flush the current batch, then drain whatever is
			// still buffered (events enqueued before Close) and flush it too, so
			// nothing buffered is lost (AC-032). The hot path is already closed
			// (HTTP drained) before Close is called, so no new events arrive.
			flush()
			w.drain()
			return
		}
	}
}

// drain empties the buffer channel and flushes the remaining events in
// batch-sized chunks. It is called once, on stop, after the in-flight batch has
// been flushed. The hot path is already drained by the time Close runs, so this
// is a non-racy "read everything currently buffered" pass.
func (w *Worker) drain() {
	batch := make([]UsageEvent, 0, w.batchSize)
	for {
		select {
		case e, ok := <-w.events:
			if !ok {
				if len(batch) > 0 {
					w.flush(batch)
				}
				return
			}
			batch = append(batch, e)
			if len(batch) >= w.batchSize {
				w.flush(batch)
				batch = batch[:0]
			}
		default:
			if len(batch) > 0 {
				w.flush(batch)
			}
			return
		}
	}
}

// flush persists one batch with a bounded retry (AC-037): on a repository error
// it logs and retries up to flushRetries times, then logs a final drop. The
// batch is NEVER silently lost — every failure path emits a log line. This all
// runs in the worker goroutine, so the hot path is never blocked even when
// Postgres is down.
func (w *Worker) flush(batch []UsageEvent) {
	// Copy the slice so the caller can safely reuse its backing array; the
	// repository may hold the slice for the duration of the call.
	events := make([]UsageEvent, len(batch))
	copy(events, batch)

	var lastErr error
	for attempt := 0; attempt <= w.flushRetries; attempt++ {
		if attempt > 0 && w.retryBackoff > 0 {
			time.Sleep(w.retryBackoff)
		}
		ctx, cancel := context.WithTimeout(context.Background(), w.flushTimeout)
		err := w.repo.Flush(ctx, events)
		cancel()
		if err == nil {
			return
		}
		lastErr = err
		w.logger.LogAttrs(context.Background(), slog.LevelError, "metering flush failed",
			slog.Int("attempt", attempt+1),
			slog.Int("max_attempts", w.flushRetries+1),
			slog.Int("batch_size", len(events)),
			slog.String("error", err.Error()),
		)
	}

	// Bounded retry exhausted: drop the batch but LOG it loudly so the loss is
	// observable (AC-037 — not silently lost). The hot path is unaffected.
	w.logger.LogAttrs(context.Background(), slog.LevelError, "metering batch dropped after retries",
		slog.Int("batch_size", len(events)),
		slog.String("error", lastErr.Error()),
	)
}

// Close stops the worker, draining and flushing all buffered events before
// returning (AC-032 / FR-012). It is wired into cmd/gateway's shutdown sequence
// AFTER the HTTP drain so that, by the time it runs, no new events are being
// enqueued. ctx bounds how long Close waits for the final flush; if it elapses
// Close returns ctx.Err() without leaking the worker goroutine (it keeps running
// to completion but Close stops waiting).
func (w *Worker) Close(ctx context.Context) error {
	select {
	case <-w.stop:
		// already closing/closed.
	default:
		close(w.stop)
	}

	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		w.logger.LogAttrs(context.Background(), slog.LevelWarn,
			"metering worker close timed out before final flush completed",
			slog.String("error", ctx.Err().Error()),
		)
		return ctx.Err()
	}
}
