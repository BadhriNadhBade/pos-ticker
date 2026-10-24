package printer

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/badhrinadhbade/pos-ticker/internal/domain"
	"github.com/badhrinadhbade/pos-ticker/internal/escpos"
)

func bitmapOnes(w, h int) *domain.Bitmap {
	bm := &domain.Bitmap{W: w, H: h, Pix: make([]bool, w*h)}
	for i := range bm.Pix {
		bm.Pix[i] = true
	}
	return bm
}

func msgJob(id int64) domain.Job {
	return domain.Job{Kind: domain.JobMessage, MsgID: id, Msg: &domain.Message{ID: id}}
}

type recorder struct {
	mu     sync.Mutex
	banded []bool
	kinds  []domain.JobKind
}

func (r *recorder) build(doc escpos.Doc, err error) BuildFunc {
	return func(j domain.Job, banded bool) (escpos.Doc, error) {
		r.mu.Lock()
		r.banded = append(r.banded, banded)
		r.kinds = append(r.kinds, j.Kind)
		r.mu.Unlock()
		return doc, err
	}
}

func newTestWorker(t *testing.T, cfg Config, s SettingsGetter, b BuildFunc, st *fakeStore) (*Worker, *events) {
	t.Helper()
	ev := &events{}
	cfg.AllowLoopback = true // tests dial the 127.0.0.1 fake printer
	if cfg.DialTimeout == 0 {
		cfg.DialTimeout = time.Second
	}
	w := NewWorker(cfg, s, b, st, ev.add)
	return w, ev
}

func TestWorkerSuccessNetworkBanded(t *testing.T) {
	fp := newFakePrinter(t)
	st := &fakeStore{}
	rec := &recorder{}
	doc := escpos.Doc{{Data: []byte("RECEIPT"), PauseAfter: 10 * time.Millisecond}}
	w, ev := newTestWorker(t, Config{}, fp.settings("auto"), rec.build(doc, nil), st)
	w.Start(context.Background())
	defer w.Stop()

	if st0 := w.Status(); st0.Up || st0.Mode != "unknown" {
		t.Fatalf("initial status %+v", st0)
	}
	if err := w.Submit(msgJob(7)); err != nil {
		t.Fatal(err)
	}
	if got := fp.waitConn(t); string(got) != "RECEIPT" {
		t.Fatalf("printer got %q", got)
	}
	waitFor(t, "print event", func() bool { return len(ev.printEvents()) == 1 })

	printed, released := st.snapshot()
	if !reflect.DeepEqual(printed, []int64{7}) || len(released) != 0 {
		t.Fatalf("printed=%v released=%v", printed, released)
	}
	if rec.banded[0] != true {
		t.Fatal("network transport must request banded=true")
	}
	pe := ev.printEvents()[0]
	if pe.ID != 7 || !pe.OK || pe.Error != "" {
		t.Fatalf("print event %+v", pe)
	}
	s := w.Status()
	if !s.Up || s.Mode != "network:127.0.0.1:"+fp.port() || s.LastError != "" || s.LastOKAt.IsZero() {
		t.Fatalf("status %+v", s)
	}
	waitFor(t, "depth 0", func() bool { return w.Depth() == 0 })
	if st := w.Stats(); st.PrintedOK != 1 || st.PrintedFailed != 0 || st.QueueDepth != 0 {
		t.Fatalf("stats %+v", st)
	}
	// a "status" event follows each print event
	var types []string
	for _, e := range ev.all() {
		types = append(types, e.Type)
	}
	if !reflect.DeepEqual(types, []string{"print", "status"}) {
		t.Fatalf("event order %v", types)
	}
}

func TestWorkerUSBIsNotBanded(t *testing.T) {
	fs := newFakeSys(t)
	fs.addLP(t, "lp0", "0525", "a700")
	st := &fakeStore{}
	rec := &recorder{}
	cfg := Config{SysfsRoot: fs.sys, DevDir: fs.dev}
	w, ev := newTestWorker(t, cfg, fakeSettings{"printer_mode": "usb"}, rec.build(escpos.Doc{{Data: []byte("x")}}, nil), st)
	w.Start(context.Background())
	defer w.Stop()
	_ = w.Submit(domain.Job{Kind: domain.JobImage, Raster: bitmapOnes(8, 8)})
	waitFor(t, "print event", func() bool { return len(ev.printEvents()) == 1 })
	if rec.banded[0] {
		t.Fatal("usb must be banded=false")
	}
	if pe := ev.printEvents()[0]; !pe.OK || pe.ID != 0 {
		t.Fatalf("event %+v", pe)
	}
	printed, released := st.snapshot()
	if len(printed)+len(released) != 0 {
		t.Fatal("non-message jobs must not touch the store")
	}
	if got := w.Status().Mode; got[:4] != "usb:" {
		t.Fatalf("mode %q", got)
	}
}

func TestWorkerBuildFailureReleasesClaim(t *testing.T) {
	fp := newFakePrinter(t)
	st := &fakeStore{}
	rec := &recorder{}
	w, ev := newTestWorker(t, Config{}, fp.settings("network"), rec.build(nil, errors.New("boom")), st)
	w.Start(context.Background())
	defer w.Stop()
	_ = w.Submit(msgJob(3))
	waitFor(t, "print event", func() bool { return len(ev.printEvents()) == 1 })
	printed, released := st.snapshot()
	if len(printed) != 0 || !reflect.DeepEqual(released, []int64{3}) {
		t.Fatalf("printed=%v released=%v", printed, released)
	}
	pe := ev.printEvents()[0]
	if pe.OK || pe.ID != 3 || pe.Error == "" {
		t.Fatalf("event %+v", pe)
	}
	s := w.Status()
	if s.Up || s.LastError == "" {
		t.Fatalf("status %+v", s)
	}
	waitFor(t, "stats", func() bool { return w.Stats().PrintedFailed == 1 && w.Depth() == 0 })
}

func TestWorkerConnectFailureThenRecovery(t *testing.T) {
	h, p := closedPort(t)
	st := &fakeStore{}
	rec := &recorder{}
	s := fakeSettings{"printer_mode": "network", "printer_host": h, "printer_port": p}
	w, ev := newTestWorker(t, Config{}, s, rec.build(escpos.Doc{{Data: []byte("ok")}}, nil), st)
	w.Start(context.Background())
	defer w.Stop()

	_ = w.Submit(msgJob(1))
	waitFor(t, "first failure", func() bool { return len(ev.printEvents()) == 1 })
	if len(rec.banded) != 0 {
		t.Fatal("build must not run when connect fails")
	}
	if s := w.Status(); s.Up || s.LastError == "" {
		t.Fatalf("status %+v", s)
	}
	if _, released := st.snapshot(); !reflect.DeepEqual(released, []int64{1}) {
		t.Fatalf("released %v", released)
	}

	// printer comes online; settings are re-read for every job
	fp := newFakePrinter(t)
	s["printer_host"], s["printer_port"] = fp.host(), fp.port() // read-only after this point by worker only
	_ = w.Submit(msgJob(2))
	fp.waitConn(t)
	waitFor(t, "second event", func() bool { return len(ev.printEvents()) == 2 })
	if st := w.Status(); !st.Up || st.LastError != "" {
		t.Fatalf("status after recovery %+v", st)
	}
	printed, _ := st.snapshot()
	if !reflect.DeepEqual(printed, []int64{2}) {
		t.Fatalf("printed %v", printed)
	}
}

func TestWorkerMarkPrintedErrorStillSuccess(t *testing.T) {
	fp := newFakePrinter(t)
	st := &fakeStore{markErr: errors.New("db down")}
	w, ev := newTestWorker(t, Config{}, fp.settings("network"), (&recorder{}).build(escpos.Doc{{Data: []byte("x")}}, nil), st)
	w.Start(context.Background())
	defer w.Stop()
	_ = w.Submit(msgJob(5))
	waitFor(t, "event", func() bool { return len(ev.printEvents()) == 1 })
	if !ev.printEvents()[0].OK {
		t.Fatal("paper was printed; event must be ok")
	}
	if _, released := st.snapshot(); len(released) != 0 {
		t.Fatal("must not release claim of a printed message")
	}
}

func TestWorkerPanicInBuildIsContained(t *testing.T) {
	fp := newFakePrinter(t)
	st := &fakeStore{}
	b := func(domain.Job, bool) (escpos.Doc, error) { panic("kaboom") }
	w, ev := newTestWorker(t, Config{}, fp.settings("network"), b, st)
	w.Start(context.Background())
	defer w.Stop()
	_ = w.Submit(msgJob(1))
	waitFor(t, "event", func() bool { return len(ev.printEvents()) == 1 })
	if ev.printEvents()[0].OK {
		t.Fatal("want failure")
	}
	// worker still alive
	_ = w.Submit(msgJob(2))
	waitFor(t, "second", func() bool { return len(ev.printEvents()) == 2 })
}

func TestSubmitQueueFull(t *testing.T) {
	st := &fakeStore{}
	w, _ := newTestWorker(t, Config{QueueSize: 2}, fakeSettings{}, (&recorder{}).build(nil, nil), st)
	// not started: jobs stay queued
	if err := w.Submit(msgJob(1)); err != nil {
		t.Fatal(err)
	}
	if err := w.Submit(msgJob(2)); err != nil {
		t.Fatal(err)
	}
	if err := w.Submit(msgJob(3)); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("want ErrQueueFull, got %v", err)
	}
	if w.Depth() != 2 || w.Stats().QueueDepth != 2 {
		t.Fatalf("depth %d", w.Depth())
	}
	// stopping a never-started worker releases the queued claims
	w.Stop()
	_, released := st.snapshot()
	if !reflect.DeepEqual(released, []int64{1, 2}) || w.Depth() != 0 {
		t.Fatalf("released=%v depth=%d", released, w.Depth())
	}
	if err := w.Submit(msgJob(4)); !errors.Is(err, ErrClosed) {
		t.Fatalf("want ErrClosed, got %v", err)
	}
}

func TestStopDrainsQueuedJobs(t *testing.T) {
	fp := newFakePrinter(t)
	st := &fakeStore{}
	doc := escpos.Doc{{Data: []byte("x"), PauseAfter: 30 * time.Millisecond}}
	w, ev := newTestWorker(t, Config{}, fp.settings("network"), (&recorder{}).build(doc, nil), st)
	w.Start(context.Background())
	for i := int64(1); i <= 4; i++ {
		if err := w.Submit(msgJob(i)); err != nil {
			t.Fatal(err)
		}
	}
	w.Stop() // must wait for all four
	printed, released := st.snapshot()
	if !reflect.DeepEqual(printed, []int64{1, 2, 3, 4}) || len(released) != 0 {
		t.Fatalf("printed=%v released=%v", printed, released)
	}
	if n := len(ev.printEvents()); n != 4 {
		t.Fatalf("%d print events", n)
	}
	if err := w.Submit(msgJob(9)); !errors.Is(err, ErrClosed) {
		t.Fatalf("want ErrClosed, got %v", err)
	}
	w.Stop() // idempotent
}

func TestShutdownDeadlineAbortsAndReleases(t *testing.T) {
	fp := newFakePrinter(t)
	st := &fakeStore{}
	doc := escpos.Doc{{Data: []byte("x"), PauseAfter: time.Minute}}
	w, ev := newTestWorker(t, Config{}, fp.settings("network"), (&recorder{}).build(doc, nil), st)
	w.Start(context.Background())
	for i := int64(1); i <= 3; i++ {
		_ = w.Submit(msgJob(i))
	}
	waitFor(t, "first job in flight", func() bool { _, ok := fp.firstArrival('x'); return ok })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := w.Shutdown(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("shutdown took %v", time.Since(start))
	}
	printed, released := st.snapshot()
	if len(printed) != 0 || len(released) != 3 {
		t.Fatalf("printed=%v released=%v", printed, released)
	}
	if w.Depth() != 0 {
		t.Fatalf("depth %d", w.Depth())
	}
	for _, pe := range ev.printEvents() {
		if pe.OK {
			t.Fatalf("unexpected success %+v", pe)
		}
	}
	if len(ev.printEvents()) != 3 {
		t.Fatalf("want 3 print events, got %d", len(ev.printEvents()))
	}
}

func TestParentContextCancelStopsWorker(t *testing.T) {
	st := &fakeStore{}
	w, _ := newTestWorker(t, Config{}, fakeSettings{}, (&recorder{}).build(nil, nil), st)
	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	cancel()
	select {
	case <-w.done:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not exit after parent ctx cancel")
	}
	if err := w.Submit(msgJob(1)); !errors.Is(err, ErrClosed) {
		t.Fatalf("want ErrClosed, got %v", err)
	}
}

func TestBandedJobEndToEndPacing(t *testing.T) {
	fp := newFakePrinter(t)
	st := &fakeStore{}
	pause := 50 * time.Millisecond
	build := func(j domain.Job, banded bool) (escpos.Doc, error) {
		return escpos.NewBuilder(escpos.Options{Banded: banded, BandPx: 24, BandPause: pause, ImageImpl: escpos.ImplBitImageRaster}).
			Init().Raster(bitmapOnes(8, 72)).Cut().Doc(), nil
	}
	w, ev := newTestWorker(t, Config{}, fp.settings("network"), build, st)
	w.Start(context.Background())
	defer w.Stop()
	start := time.Now()
	_ = w.Submit(msgJob(1))
	waitFor(t, "event", func() bool { return len(ev.printEvents()) == 1 })
	if d := time.Since(start); d < 3*pause {
		t.Fatalf("job took %v, want >= 3 band pauses (%v)", d, 3*pause)
	}
	if !ev.printEvents()[0].OK {
		t.Fatalf("%+v", ev.printEvents()[0])
	}
}
