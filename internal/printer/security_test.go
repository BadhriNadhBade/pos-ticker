package printer

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/badhrinadhbade/pos-ticker/internal/domain"
	"github.com/badhrinadhbade/pos-ticker/internal/escpos"
)

func TestBlockedAddr(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.9.9.9", "::1", "0.0.0.0", "::", "0.1.2.3",
		"169.254.169.254", "169.254.0.1", "fe80::1", "fd00:ec2::254", "100.100.100.200",
		"224.0.0.1", "239.255.255.250", "ff02::1", "255.255.255.255",
		"::ffff:127.0.0.1", "::ffff:169.254.169.254", // IPv4-mapped bypass attempts
	}
	for _, s := range blocked {
		if !blockedAddr(netip.MustParseAddr(s)) {
			t.Errorf("%s should be blocked", s)
		}
	}
	allowed := []string{"10.0.0.5", "192.168.1.50", "172.16.3.4", "8.8.8.8", "fd12:3456::1", "::ffff:192.168.1.5"}
	for _, s := range allowed {
		if blockedAddr(netip.MustParseAddr(s)) {
			t.Errorf("%s should be allowed", s)
		}
	}
}

func TestConnectRefusesLoopbackByDefault(t *testing.T) {
	fp := newFakePrinter(t) // listening on 127.0.0.1
	cfg := testCfg(nil)
	cfg.AllowLoopback = false
	_, err := Connect(context.Background(), cfg, fp.settings("network"))
	if err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("want 'not allowed' error, got %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if n := fp.connCount(); n != 0 {
		t.Fatalf("loopback printer was dialed %d times despite the guard", n)
	}
}

func TestConnectRefusesMetadataAndLocalhostName(t *testing.T) {
	for _, host := range []string{"169.254.169.254", "0.0.0.0", "::1", "localhost", "[::ffff:127.0.0.1]"} {
		host = strings.Trim(host, "[]")
		cfg := testCfg(nil)
		cfg.AllowLoopback = false
		_, err := Connect(context.Background(), cfg, fakeSettings{"printer_mode": "network", "printer_host": host, "printer_port": "80"})
		if err == nil || !strings.Contains(err.Error(), "not allowed") {
			t.Errorf("%s: want 'not allowed', got %v", host, err)
		}
	}
}

func TestConnectAllowLoopbackOverride(t *testing.T) {
	fp := newFakePrinter(t)
	tr, err := Connect(context.Background(), testCfg(nil), fp.settings("network")) // AllowLoopback: true
	if err != nil {
		t.Fatal(err)
	}
	tr.Close()
}

func TestConfigFromEnvAllowLoopback(t *testing.T) {
	get := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if ConfigFromEnv(get(nil)).AllowLoopback {
		t.Fatal("default must be false")
	}
	if !ConfigFromEnv(get(map[string]string{"PRINTER_ALLOW_LOOPBACK": " TRUE "})).AllowLoopback {
		t.Fatal("PRINTER_ALLOW_LOOPBACK=true not honoured")
	}
	if ConfigFromEnv(get(map[string]string{"PRINTER_ALLOW_LOOPBACK": "1"})).AllowLoopback {
		t.Fatal("only 'true' enables it")
	}
}

func TestSanitizeError(t *testing.T) {
	if SanitizeError(nil) != "" {
		t.Fatal("nil")
	}
	long := errors.New("boom\r\n\x00\x1b[31m" + strings.Repeat("é", 500))
	got := SanitizeError(long)
	if len(got) > 200 {
		t.Fatalf("len %d > 200", len(got))
	}
	for _, r := range got {
		if r < 0x20 || r == 0x7f {
			t.Fatalf("control char %q survived in %q", r, got)
		}
	}
	if !strings.HasPrefix(got, "boom") {
		t.Fatalf("got %q", got)
	}
}

func TestDialErrorDoesNotEchoResolverText(t *testing.T) {
	secret := "SECRET-FROM-RESOLVER-" + strings.Repeat("x", 400)
	err := dialError("evil.example:9100", errors.New(secret))
	if strings.Contains(err.Error(), "SECRET") || len(err.Error()) > 200 {
		t.Fatalf("leaked/oversized: %q", err)
	}
}

func TestWorkerStatusErrorIsShortWhenHostBlocked(t *testing.T) {
	st := &fakeStore{}
	ev := &events{}
	w := NewWorker(Config{DialTimeout: time.Second}, // AllowLoopback false
		fakeSettings{"printer_mode": "network", "printer_host": "169.254.169.254", "printer_port": "80"},
		(&recorder{}).build(escpos.Doc{{Data: []byte("x")}}, nil), st, ev.add)
	w.Start(context.Background())
	defer w.Stop()
	if err := w.Submit(msgJob(5)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "print event", func() bool { return len(ev.printEvents()) == 1 })
	s := w.Status()
	if s.Up || s.LastError == "" || len(s.LastError) > 200 || !strings.Contains(s.LastError, "not allowed") {
		t.Fatalf("status %+v", s)
	}
	if pe := ev.printEvents()[0]; pe.OK || len(pe.Error) > 200 {
		t.Fatalf("event %+v", pe)
	}
}

// ---- lazy image loading ----------------------------------------------------------

func TestWorkerLoadsImageLazily(t *testing.T) {
	fp := newFakePrinter(t)
	st := &fakeStore{images: map[int64][]byte{7: []byte("PNGDATA")}}
	var mu sync.Mutex
	got := map[int64][]byte{}
	build := func(j domain.Job, banded bool) (escpos.Doc, error) {
		mu.Lock()
		got[j.Msg.ID] = j.Msg.Image
		mu.Unlock()
		return escpos.Doc{{Data: []byte("x")}}, nil
	}
	w, ev := newTestWorker(t, Config{}, fp.settings("network"), build, st)
	w.Start(context.Background())
	defer w.Stop()

	queued := msgJob(7) // as ClaimAllPending delivers it: no image bytes
	_ = w.Submit(queued)
	_ = w.Submit(msgJob(8)) // has no stored image -> ErrNotFound -> printed without one
	waitFor(t, "2 prints", func() bool { return len(ev.printEvents()) == 2 })

	mu.Lock()
	defer mu.Unlock()
	if string(got[7]) != "PNGDATA" {
		t.Fatalf("job 7 image = %q, want lazily loaded", got[7])
	}
	if got[8] != nil {
		t.Fatalf("job 8 image = %q, want nil", got[8])
	}
	for _, pe := range ev.printEvents() {
		if !pe.OK {
			t.Fatalf("print failed: %+v", pe)
		}
	}
	if queued.Msg.Image != nil {
		t.Fatal("queued job's Message must not be mutated (would pin the BLOB)")
	}
}

func TestWorkerKeepsPreloadedImageAndSkipsStore(t *testing.T) {
	fp := newFakePrinter(t)
	st := &fakeStore{}
	var seen []byte
	build := func(j domain.Job, banded bool) (escpos.Doc, error) {
		seen = j.Msg.Image
		return escpos.Doc{{Data: []byte("x")}}, nil
	}
	w, ev := newTestWorker(t, Config{}, fp.settings("network"), build, st)
	w.Start(context.Background())
	defer w.Stop()
	_ = w.Submit(domain.Job{Kind: domain.JobMessage, MsgID: 3, Msg: &domain.Message{ID: 3, Image: []byte("inline")}})
	waitFor(t, "print", func() bool { return len(ev.printEvents()) == 1 })
	st.mu.Lock()
	defer st.mu.Unlock()
	if string(seen) != "inline" || len(st.imageCalls) != 0 {
		t.Fatalf("seen=%q calls=%v", seen, st.imageCalls)
	}
}

type failingImageStore struct{ fakeStore }

func (s *failingImageStore) Image(context.Context, int64) ([]byte, error) {
	return nil, errors.New("disk I/O error")
}

func TestWorkerImageLoadErrorFailsAndReleases(t *testing.T) {
	fp := newFakePrinter(t)
	st := &failingImageStore{}
	built := false
	build := func(domain.Job, bool) (escpos.Doc, error) { built = true; return nil, nil }
	ev := &events{}
	w := NewWorker(Config{DialTimeout: time.Second, AllowLoopback: true}, fp.settings("network"), build, st, ev.add)
	w.Start(context.Background())
	defer w.Stop()
	_ = w.Submit(msgJob(4))
	waitFor(t, "print event", func() bool { return len(ev.printEvents()) == 1 })
	if built || ev.printEvents()[0].OK {
		t.Fatal("must fail before building when the image cannot be loaded")
	}
	if _, rel := st.snapshot(); len(rel) != 1 || rel[0] != 4 {
		t.Fatalf("claim not released: %v", rel)
	}
}
