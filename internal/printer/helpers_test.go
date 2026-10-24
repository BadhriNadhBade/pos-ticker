package printer

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/badhrinadhbade/pos-ticker/internal/domain"
)

// ---- fake settings -----------------------------------------------------------

type fakeSettings map[string]string

func (f fakeSettings) Get(k string) string { return f[k] }
func (f fakeSettings) Int(k string, def int) int {
	if v, err := strconv.Atoi(f[k]); err == nil {
		return v
	}
	return def
}

// ---- fake TCP printer --------------------------------------------------------

type recvByte struct {
	b byte
	t time.Time
}

type fakePrinter struct {
	ln net.Listener

	mu    sync.Mutex
	conns int
	data  [][]byte // one entry per finished connection
	bytes []recvByte
	done  chan struct{} // receives one token per finished connection
}

func newFakePrinter(t *testing.T) *fakePrinter {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &fakePrinter{ln: ln, done: make(chan struct{}, 64)}
	go p.serve()
	t.Cleanup(func() { ln.Close() })
	return p
}

func (p *fakePrinter) serve() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		p.conns++
		p.mu.Unlock()
		go func() {
			defer c.Close()
			var all []byte
			buf := make([]byte, 4096)
			for {
				n, err := c.Read(buf)
				if n > 0 {
					now := time.Now()
					p.mu.Lock()
					for _, b := range buf[:n] {
						p.bytes = append(p.bytes, recvByte{b, now})
					}
					p.mu.Unlock()
					all = append(all, buf[:n]...)
				}
				if err != nil {
					if err != io.EOF {
						_ = err
					}
					break
				}
			}
			p.mu.Lock()
			p.data = append(p.data, all)
			p.mu.Unlock()
			p.done <- struct{}{}
		}()
	}
}

func (p *fakePrinter) host() string { h, _, _ := net.SplitHostPort(p.ln.Addr().String()); return h }
func (p *fakePrinter) port() string { _, pt, _ := net.SplitHostPort(p.ln.Addr().String()); return pt }

func (p *fakePrinter) settings(mode string) fakeSettings {
	return fakeSettings{"printer_mode": mode, "printer_host": p.host(), "printer_port": p.port()}
}

func (p *fakePrinter) waitConn(t *testing.T) []byte {
	t.Helper()
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for printer connection to finish")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.data[len(p.data)-1]
}

func (p *fakePrinter) connCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.conns
}

func (p *fakePrinter) firstArrival(b byte) (time.Time, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, rb := range p.bytes {
		if rb.b == b {
			return rb.t, true
		}
	}
	return time.Time{}, false
}

// closedPort returns an address nothing listens on.
func closedPort(t *testing.T) (host, port string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h, p, _ := net.SplitHostPort(ln.Addr().String())
	ln.Close()
	return h, p
}

// ---- fake sysfs --------------------------------------------------------------

type fakeSys struct{ sys, dev string }

// newFakeSys builds <root>/sys and <root>/dev. The "device" dir stands in for
// the usb interface; its parent (the lpN dir) stands in for the usb device dir
// that holds idVendor/idProduct, matching the kernel's device/.. relation.
func newFakeSys(t *testing.T) *fakeSys {
	t.Helper()
	root := t.TempDir()
	fs := &fakeSys{sys: filepath.Join(root, "sys"), dev: filepath.Join(root, "dev")}
	must(t, os.MkdirAll(filepath.Join(fs.sys, "class", "usbmisc"), 0o755))
	must(t, os.MkdirAll(filepath.Join(fs.dev, "usb"), 0o755))
	return fs
}

func (f *fakeSys) addLP(t *testing.T, name, vendor, product string) string {
	t.Helper()
	d := filepath.Join(f.sys, "class", "usbmisc", name)
	must(t, os.MkdirAll(filepath.Join(d, "device"), 0o755))
	must(t, os.WriteFile(filepath.Join(d, "idVendor"), []byte(vendor+"\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(d, "idProduct"), []byte(product+"\n"), 0o644))
	node := filepath.Join(f.dev, "usb", name)
	must(t, os.WriteFile(node, nil, 0o644))
	return node
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// ---- fake store ----------------------------------------------------------------

type fakeStore struct {
	domain.Store // nil: unused methods panic, which would flag unexpected calls

	mu       sync.Mutex
	printed  []int64
	released []int64
	markErr  error

	images     map[int64][]byte
	imageCalls []int64
}

func (s *fakeStore) MarkPrinted(_ context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.printed = append(s.printed, id)
	return s.markErr
}

func (s *fakeStore) ReleaseClaim(_ context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.released = append(s.released, id)
	return nil
}

func (s *fakeStore) Image(_ context.Context, id int64) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.imageCalls = append(s.imageCalls, id)
	if b, ok := s.images[id]; ok {
		return b, nil
	}
	return nil, domain.ErrNotFound
}

func (s *fakeStore) snapshot() (printed, released []int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int64(nil), s.printed...), append([]int64(nil), s.released...)
}

// ---- event collector -------------------------------------------------------------

type events struct {
	mu sync.Mutex
	ev []domain.Event
}

func (e *events) add(ev domain.Event) {
	e.mu.Lock()
	e.ev = append(e.ev, ev)
	e.mu.Unlock()
}

func (e *events) all() []domain.Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]domain.Event(nil), e.ev...)
}

func (e *events) printEvents() []PrintEvent {
	var out []PrintEvent
	for _, ev := range e.all() {
		if ev.Type == "print" {
			out = append(out, ev.Data.(PrintEvent))
		}
	}
	return out
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}
