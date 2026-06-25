package metering

import (
	"context"
	"log/slog"
	"sync/atomic"
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
	buf    *Buffer
	events <-chan UsageEvent
	repo   MeteringRepository
	logger *slog.Logger

	// bufRecorder publishes the current buffer occupancy as the
	// metering_buffer_size gauge. It is updated each loop tick (and after every
	// enqueue/flush) from buf.Len(). The worker depends only on this narrow port,
	// so the metering package never imports Prometheus (ADR-0008).
	bufRecorder BufferSizeRecorder

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

	// shutdownFlushed counts the usage events successfully persisted during the
	// graceful-shutdown drain (the stop path). It is read by the lifecycle log so
	// the operator sees "flushed M usage events" alongside "drained N requests"
	// (AC-015c). Written only in the worker goroutine, read after w.done closes,
	// but kept atomic for safety.
	shutdownFlushed int64
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

// WithBufferSizeRecorder injects the recorder the worker uses to publish the
// metering_buffer_size gauge. A nil recorder is ignored (the worker keeps its
// no-op default), so the metering package never imports Prometheus (ADR-0008).
func WithBufferSizeRecorder(rec BufferSizeRecorder) WorkerOption {
	return func(w *Worker) {
		if rec != nil {
			w.bufRecorder = rec
		}
	}
}

// NewWorker constructs the Metering Worker reading from buf and flushing through
// repo. The worker does not start until Start is called.
func NewWorker(buf *Buffer, repo MeteringRepository, logger *slog.Logger, opts ...WorkerOption) *Worker {
	w := &Worker{
		buf:           buf,
		events:        buf.Events(),
		repo:          repo,
		logger:        logger,
		bufRecorder:   NopBufferSizeRecorder{},
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
		w.publishBufferSize()
	}

	// Publish the initial (empty) occupancy so the gauge exists from the start.
	w.publishBufferSize()

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
			// One event left the buffer for the in-memory batch; reflect the new
			// occupancy in the gauge.
			w.publishBufferSize()
			if len(batch) >= w.batchSize {
				flush()
			}
		case <-ticker.C:
			w.publishBufferSize()
			flush()
		case <-w.stop:
			// Graceful shutdown: flush the current batch, then drain whatever is
			// still buffered (events enqueued before Close) and flush it too, so
			// nothing buffered is lost (AC-032). The hot path is already closed
			// (HTTP drained) before Close is called, so no new events arrive. Count
			// the events persisted so the lifecycle can log "flushed M usage events"
			// alongside "drained N requests" (AC-015c).
			flushed := w.flush(batch)
			flushed += w.drain()
			atomic.StoreInt64(&w.shutdownFlushed, int64(flushed))
			w.publishBufferSize()
			return
		}
	}
}

// publishBufferSize reports the current Usage-buffer occupancy to the injected
// recorder (metering_buffer_size). It is called each loop tick and after every
// dequeue/flush so the gauge tracks how full the buffer is at any moment. The
// read is cheap (len of a channel) and never blocks the worker.
func (w *Worker) publishBufferSize() {
	w.bufRecorder.SetMeteringBufferSize(w.buf.Len())
}

// drain empties the buffer channel and flushes the remaining events in
// batch-sized chunks. It is called once, on stop, after the in-flight batch has
// been flushed. The hot path is already drained by the time Close runs, so this
// is a non-racy "read everything currently buffered" pass. It returns the number
// of events successfully persisted so the shutdown path can report the total.
func (w *Worker) drain() int {
	flushed := 0
	batch := make([]UsageEvent, 0, w.batchSize)
	for {
		select {
		case e, ok := <-w.events:
			if !ok {
				if len(batch) > 0 {
					flushed += w.flush(batch)
				}
				return flushed
			}
			batch = append(batch, e)
			if len(batch) >= w.batchSize {
				flushed += w.flush(batch)
				batch = batch[:0]
			}
		default:
			if len(batch) > 0 {
				flushed += w.flush(batch)
			}
			return flushed
		}
	}
}

// flush persists one batch with a bounded retry (AC-037): on a repository error
// it logs and retries up to flushRetries times, then logs a final drop. The
// batch is NEVER silently lost — every failure path emits a log line. This all
// runs in the worker goroutine, so the hot path is never blocked even when
// Postgres is down. It returns the number of events persisted (len(batch) on
// success, 0 when the batch was dropped after exhausting retries).
func (w *Worker) flush(batch []UsageEvent) int {
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
			return len(events)
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
	return 0
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

// FlushedOnShutdown returns the number of usage events successfully persisted
// during the graceful-shutdown drain. It is meaningful only after Close has
// returned (the worker has stopped); the lifecycle logs it alongside "drained N
// requests" so the operator sees how many buffered events survived shutdown
// (AC-015c).
func (w *Worker) FlushedOnShutdown() int {
	return int(atomic.LoadInt64(&w.shutdownFlushed))
}
