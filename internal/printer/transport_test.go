package printer

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/badhrinadhbade/pos-ticker/internal/escpos"
)

func testCfg(fs *fakeSys) Config {
	c := Config{DialTimeout: time.Second, WriteTimeout: 2 * time.Second, AllowLoopback: true}
	if fs != nil {
		c.SysfsRoot, c.DevDir = fs.sys, fs.dev
	}
	return c
}

func TestFindUSBDevice(t *testing.T) {
	fs := newFakeSys(t)
	fs.addLP(t, "lp0", "04b8", "0005") // other printer
	want := fs.addLP(t, "lp1", "0525", "a700")

	got, err := FindUSBDevice(fs.sys, fs.dev, 0x0525, 0xa700)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("got %s want %s", got, want)
	}
	if _, err := FindUSBDevice(fs.sys, fs.dev, 0x1234, 0x5678); err == nil {
		t.Fatal("expected not-found error")
	}
	if _, err := FindUSBDevice(filepath.Join(fs.sys, "missing"), fs.dev, 0x0525, 0xa700); err == nil {
		t.Fatal("expected error for missing sysfs")
	}
}

func TestConnectNetwork(t *testing.T) {
	fp := newFakePrinter(t)
	for _, mode := range []string{"network", "auto"} {
		tr, err := Connect(context.Background(), testCfg(nil), fp.settings(mode))
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		if tr.Kind() != KindNetwork || tr.Label() != "network:127.0.0.1:"+fp.port() {
			t.Fatalf("%s: kind=%s label=%s", mode, tr.Kind(), tr.Label())
		}
		if err := tr.Write([]byte("hello")); err != nil {
			t.Fatal(err)
		}
		tr.Close()
		if got := fp.waitConn(t); string(got) != "hello" {
			t.Fatalf("printer got %q", got)
		}
	}
}

func TestConnectNetworkWithoutHost(t *testing.T) {
	_, err := Connect(context.Background(), testCfg(nil), fakeSettings{"printer_mode": "network"})
	if err == nil || !strings.Contains(err.Error(), "printer_host") {
		t.Fatalf("want printer_host error, got %v", err)
	}
}

func TestConnectNetworkForcedDoesNotFallBack(t *testing.T) {
	fs := newFakeSys(t)
	fs.addLP(t, "lp0", "0525", "a700")
	h, p := closedPort(t)
	_, err := Connect(context.Background(), testCfg(fs), fakeSettings{"printer_mode": "network", "printer_host": h, "printer_port": p})
	if err == nil || strings.Contains(err.Error(), "usb") {
		t.Fatalf("network mode must fail without trying USB, got %v", err)
	}
}

func TestConnectAutoFallsBackToUSB(t *testing.T) {
	fs := newFakeSys(t)
	node := fs.addLP(t, "lp0", "0525", "a700")
	h, p := closedPort(t)
	tr, err := Connect(context.Background(), testCfg(fs), fakeSettings{"printer_mode": "auto", "printer_host": h, "printer_port": p})
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	if tr.Kind() != KindUSB || tr.Label() != "usb:"+node {
		t.Fatalf("kind=%s label=%s", tr.Kind(), tr.Label())
	}
	if err := tr.Write([]byte("usb-data")); err != nil {
		t.Fatal(err)
	}
	tr.Close()
	if b, _ := os.ReadFile(node); string(b) != "usb-data" {
		t.Fatalf("device got %q", b)
	}
}

func TestConnectAutoNoHostUsesUSB(t *testing.T) {
	fs := newFakeSys(t)
	fs.addLP(t, "lp0", "0525", "a700")
	tr, err := Connect(context.Background(), testCfg(fs), fakeSettings{"printer_mode": "auto"})
	if err != nil {
		t.Fatal(err)
	}
	tr.Close()
	if tr.Kind() != KindUSB {
		t.Fatal("want usb")
	}
}

func TestConnectAutoBothFailJoinsErrors(t *testing.T) {
	fs := newFakeSys(t) // no printers
	h, p := closedPort(t)
	_, err := Connect(context.Background(), testCfg(fs), fakeSettings{"printer_mode": "auto", "printer_host": h, "printer_port": p})
	if err == nil || !strings.Contains(err.Error(), "network printer") || !strings.Contains(err.Error(), "usb printer") {
		t.Fatalf("want both errors, got %v", err)
	}
}

func TestConnectUSBForcedIgnoresHost(t *testing.T) {
	fp := newFakePrinter(t)
	fs := newFakeSys(t)
	fs.addLP(t, "lp0", "0525", "a700")
	s := fp.settings("usb")
	tr, err := Connect(context.Background(), testCfg(fs), s)
	if err != nil {
		t.Fatal(err)
	}
	tr.Close()
	if tr.Kind() != KindUSB || fp.connCount() != 0 {
		t.Fatalf("usb mode must not touch the network (conns=%d)", fp.connCount())
	}
}

func TestConnectExplicitUSBDevice(t *testing.T) {
	dev := filepath.Join(t.TempDir(), "lp9")
	must(t, os.WriteFile(dev, nil, 0o644))
	cfg := testCfg(newFakeSys(t)) // empty sysfs: lookup would fail
	cfg.USBDevice = dev
	tr, err := Connect(context.Background(), cfg, fakeSettings{"printer_mode": "usb"})
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	if tr.Label() != "usb:"+dev {
		t.Fatalf("label %s", tr.Label())
	}
}

func TestConfigFromEnv(t *testing.T) {
	env := map[string]string{"PRINTER_USB_VENDOR": "0x04B8", "PRINTER_USB_PRODUCT": "zz", "PRINTER_USB_DEVICE": " /dev/usb/lp3 "}
	c := ConfigFromEnv(func(k string) string { return env[k] })
	if c.USBVendor != 0x04b8 || c.USBProduct != DefaultUSBProduct || c.USBDevice != "/dev/usb/lp3" {
		t.Fatalf("%+v", c)
	}
}

func TestWriteDocHonoursPauses(t *testing.T) {
	fp := newFakePrinter(t)
	pause := 120 * time.Millisecond
	doc := escpos.Doc{
		{Data: []byte("A"), PauseAfter: pause},
		{Data: []byte("B"), PauseAfter: pause},
		{Data: []byte("C"), PauseAfter: pause}, // pause after the LAST chunk too
	}
	tr, err := Connect(context.Background(), testCfg(nil), fp.settings("network"))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := WriteDoc(context.Background(), tr, doc); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	tr.Close()
	if got := fp.waitConn(t); !bytes.Equal(got, []byte("ABC")) {
		t.Fatalf("got %q", got)
	}
	if elapsed < 3*pause {
		t.Fatalf("elapsed %v < 3 pauses (%v): last-band pause missing", elapsed, 3*pause)
	}
	ta, _ := fp.firstArrival('A')
	tb, _ := fp.firstArrival('B')
	tc, _ := fp.firstArrival('C')
	slack := 15 * time.Millisecond
	if tb.Sub(ta) < pause-slack || tc.Sub(tb) < pause-slack {
		t.Fatalf("gaps A->B %v, B->C %v, want >= %v", tb.Sub(ta), tc.Sub(tb), pause)
	}
}

func TestWriteDocBandedRasterPacing(t *testing.T) {
	// end to end with the escpos builder: 3 bands => 3 pauses
	fp := newFakePrinter(t)
	pause := 60 * time.Millisecond
	bm := bitmapOnes(8, 48*2+10)
	doc := escpos.NewBuilder(escpos.Options{Banded: true, BandPx: 48, BandPause: pause, ImageImpl: escpos.ImplBitImageRaster}).
		Raster(bm).Doc()
	if len(doc) != 3 {
		t.Fatalf("want 3 bands got %d", len(doc))
	}
	tr, err := Connect(context.Background(), testCfg(nil), fp.settings("network"))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := WriteDoc(context.Background(), tr, doc); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 3*pause {
		t.Fatalf("banded print finished in %v, want >= %v", time.Since(start), 3*pause)
	}
	tr.Close()
	if got := fp.waitConn(t); !bytes.Equal(got, doc.Bytes()) {
		t.Fatal("printer received different bytes than the doc")
	}
}

func TestWriteDocContextCancelAbortsPause(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	tr := &memTransport{}
	start := time.Now()
	err := WriteDoc(ctx, tr, escpos.Doc{{Data: []byte("x"), PauseAfter: 10 * time.Second}})
	if err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("want prompt ctx error, got %v after %v", err, time.Since(start))
	}
}

type memTransport struct{ buf bytes.Buffer }

func (m *memTransport) Open(context.Context) error { return nil }
func (m *memTransport) Write(p []byte) error       { m.buf.Write(p); return nil }
func (m *memTransport) Close() error               { return nil }
func (m *memTransport) Kind() string               { return KindNetwork }
func (m *memTransport) Label() string              { return "mem" }
