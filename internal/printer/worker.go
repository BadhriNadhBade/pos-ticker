package printer

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/badhrinadhbade/pos-ticker/internal/domain"
	"github.com/badhrinadhbade/pos-ticker/internal/escpos"
)

var (
	// ErrQueueFull is returned by Submit when the bounded queue is full.
	ErrQueueFull = errors.New("printer: queue full")
	// ErrClosed is returned by Submit after Stop/Shutdown.
	ErrClosed = errors.New("printer: worker stopped")
)

// BuildFunc turns a job into an ESC/POS document. banded is true when the
// chosen transport is a network printer (paced raster bands). It is injected
// by main (wrapping render.BuildJob) so this package never imports render.
type BuildFunc func(j domain.Job, banded bool) (escpos.Doc, error)

// PrintEvent is the Data payload of the "print" domain.Event.
type PrintEvent struct {
	ID    int64  `json:"id"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// Stats is a point-in-time snapshot of worker counters, for the metrics package.
type Stats struct {
	PrintedOK     uint64 // jobs printed successfully
	PrintedFailed uint64 // jobs that failed (connect, build or write)
	QueueDepth    int    // jobs queued or in flight
}

// Worker is a single-goroutine print queue. It implements domain.Queue.
type Worker struct {
	cfg    Config
	s      SettingsGetter
	build  BuildFunc
	store  domain.Store
	notify func(domain.Event)

	jobs chan domain.Job

	mu      sync.RWMutex // guards closed, started, cancel
	closed  bool
	started bool
	cancel  context.CancelFunc
	done    chan struct{}

	statusMu sync.Mutex
	status   domain.PrinterStatus

	pending atomic.Int64
	okN     atomic.Uint64
	failN   atomic.Uint64
}

var _ domain.Queue = (*Worker)(nil)

// NewWorker creates a Worker. notify may be nil. Call Start to begin printing.
func NewWorker(cfg Config, s SettingsGetter, build BuildFunc, store domain.Store, notify func(domain.Event)) *Worker {
	cfg = cfg.withDefaults()
	return &Worker{
		cfg:    cfg,
		s:      s,
		build:  build,
		store:  store,
		notify: notify,
		jobs:   make(chan domain.Job, cfg.QueueSize),
		done:   make(chan struct{}),
		status: domain.PrinterStatus{Mode: "unknown"},
	}
}

// Submit enqueues a job without blocking. It returns ErrQueueFull when the
// bounded queue is full and ErrClosed after Stop.
func (w *Worker) Submit(j domain.Job) error {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.closed {
		return ErrClosed
	}
	w.pending.Add(1)
	select {
	case w.jobs <- j:
		return nil
	default:
		w.pending.Add(-1)
		return ErrQueueFull
	}
}

// Depth is the number of jobs queued or currently printing.
func (w *Worker) Depth() int { return int(w.pending.Load()) }

// Status returns the last known printer status.
func (w *Worker) Status() domain.PrinterStatus {
	w.statusMu.Lock()
	defer w.statusMu.Unlock()
	return w.status
}

// Stats returns the counters snapshot.
func (w *Worker) Stats() Stats {
	return Stats{PrintedOK: w.okN.Load(), PrintedFailed: w.failN.Load(), QueueDepth: w.Depth()}
}

// Start launches the worker goroutine. Cancelling ctx aborts the in-flight job
// and releases the claims of everything still queued. Calling Start twice is a no-op.
func (w *Worker) Start(ctx context.Context) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.started {
		return
	}
	w.started = true
	rctx, cancel := context.WithCancel(ctx)
	w.cancel = cancel
	go w.loop(rctx)
}

// Stop stops accepting jobs and drains the queue, waiting at most
// Config.DrainTimeout before aborting the remainder.
func (w *Worker) Stop() {
	ctx, cancel := context.WithTimeout(context.Background(), w.cfg.DrainTimeout)
	defer cancel()
	_ = w.Shutdown(ctx)
}

// Shutdown stops accepting jobs and lets the worker finish everything already
// queued. If ctx expires first, the in-flight job is aborted and the remaining
// queued jobs are abandoned (their claims released); ctx.Err() is returned.
func (w *Worker) Shutdown(ctx context.Context) error {
	w.closeIntake()

	w.mu.RLock()
	started, cancel := w.started, w.cancel
	w.mu.RUnlock()

	if !started {
		w.drainAbandon("worker stopped before start")
		return nil
	}
	select {
	case <-w.done:
		cancel()
		return nil
	case <-ctx.Done():
		cancel()
		<-w.done
		return ctx.Err()
	}
}

func (w *Worker) closeIntake() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.closed {
		w.closed = true
		close(w.jobs)
	}
}

func (w *Worker) loop(ctx context.Context) {
	defer close(w.done)
	for {
		select {
		case j, ok := <-w.jobs:
			if !ok {
				return
			}
			if ctx.Err() != nil {
				w.abandon(j, "shutdown")
				continue
			}
			w.run(ctx, j)
		case <-ctx.Done():
			w.closeIntake()
			w.drainAbandon("shutdown")
			return
		}
	}
}

func (w *Worker) drainAbandon(reason string) {
	for j := range w.jobs { // intake is closed, so this terminates
		w.abandon(j, reason)
	}
}

// abandon releases a job that will never be printed.
func (w *Worker) abandon(j domain.Job, reason string) {
	id := jobID(j)
	w.releaseClaim(j)
	w.pending.Add(-1)
	w.emit("print", PrintEvent{ID: id, OK: false, Error: reason})
}

func jobID(j domain.Job) int64 {
	if j.Kind != domain.JobMessage {
		return 0
	}
	if j.MsgID != 0 {
		return j.MsgID
	}
	if j.Msg != nil {
		return j.Msg.ID
	}
	return 0
}

func (w *Worker) releaseClaim(j domain.Job) {
	id := jobID(j)
	if id == 0 || w.store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := w.store.ReleaseClaim(ctx, id); err != nil {
		w.cfg.Logger.Error("release claim failed", "id", id, "err", err)
	}
}

func (w *Worker) run(ctx context.Context, j domain.Job) {
	jctx, cancel := context.WithTimeout(ctx, w.cfg.JobTimeout)
	err := w.print(jctx, j)
	cancel()

	id := jobID(j)
	if err == nil {
		if id != 0 && w.store != nil {
			sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
			if merr := w.store.MarkPrinted(sctx, id); merr != nil {
				w.cfg.Logger.Error("mark printed failed", "id", id, "err", merr)
			}
			scancel()
		}
		w.okN.Add(1)
		w.setStatus(func(s *domain.PrinterStatus) {
			s.Up, s.LastError, s.LastOKAt = true, "", time.Now().UTC()
		})
	} else {
		w.cfg.Logger.Error("print failed", "kind", string(j.Kind), "id", id, "err", err)
		w.releaseClaim(j)
		w.failN.Add(1)
		msg := SanitizeError(err)
		w.setStatus(func(s *domain.PrinterStatus) { s.Up, s.LastError = false, msg })
	}
	w.pending.Add(-1)

	ev := PrintEvent{ID: id, OK: err == nil}
	if err != nil {
		ev.Error = SanitizeError(err)
	}
	w.emit("print", ev)
	w.emit("status", w.Status())
}

// print connects, builds and writes one job. The connection is closed on return.
func (w *Worker) print(ctx context.Context, j domain.Job) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("print panic: %v", r)
		}
	}()
	if w.build == nil {
		return errors.New("printer: no BuildFunc configured")
	}
	t, err := Connect(ctx, w.cfg, w.s)
	if err != nil {
		return err
	}
	// Abort a blocked Write when the job context ends.
	stop := context.AfterFunc(ctx, func() { _ = t.Close() })
	defer func() {
		stop()
		_ = t.Close()
	}()
	label := t.Label()
	w.setStatus(func(s *domain.PrinterStatus) { s.Mode = label })

	// Queued message jobs carry no image bytes (bulk claims omit the BLOB);
	// fetch the original upload just-in-time and let it go out of scope when
	// this function returns.
	if j, err = w.withLazyImage(ctx, j); err != nil {
		return err
	}
	doc, err := w.build(j, t.Kind() == KindNetwork)
	if err != nil {
		return fmt.Errorf("build job: %w", err)
	}
	if err := WriteDoc(ctx, t, doc); err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return fmt.Errorf("print aborted: %w", cerr)
		}
		return fmt.Errorf("write to %s: %w", label, err)
	}
	return nil
}

// withLazyImage returns j with Msg.Image loaded from the store when the job is a
// message whose image was not carried in memory. The queued job's Message is
// copied, never mutated. domain.ErrNotFound means "no image".
func (w *Worker) withLazyImage(ctx context.Context, j domain.Job) (domain.Job, error) {
	if j.Kind != domain.JobMessage || j.Msg == nil || j.Msg.Image != nil || w.store == nil {
		return j, nil
	}
	id := jobID(j)
	if id == 0 {
		return j, nil
	}
	img, err := w.store.Image(ctx, id)
	if errors.Is(err, domain.ErrNotFound) {
		return j, nil
	}
	if err != nil {
		return j, fmt.Errorf("load image: %w", err)
	}
	m := *j.Msg
	m.Image = img
	j.Msg = &m
	return j, nil
}

func (w *Worker) setStatus(f func(*domain.PrinterStatus)) {
	w.statusMu.Lock()
	f(&w.status)
	w.statusMu.Unlock()
}

func (w *Worker) emit(typ string, data any) {
	if w.notify != nil {
		w.notify(domain.Event{Type: typ, Data: data})
	}
}
