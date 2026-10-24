// Package printer owns printer connectivity (network / USB) and the single
// print worker. It must not import internal/render; main injects BuildFunc.
package printer

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/badhrinadhbade/pos-ticker/internal/escpos"
)

// Transport kinds returned by Transport.Kind.
const (
	KindNetwork = "network"
	KindUSB     = "usb"
)

// Transport is a single-use connection to a printer.
type Transport interface {
	// Open establishes the connection.
	Open(ctx context.Context) error
	// Write sends bytes, honouring the transport's write deadline.
	Write(p []byte) error
	// Close releases the connection. Safe to call after a failed Open.
	Close() error
	// Kind is KindNetwork or KindUSB.
	Kind() string
	// Label is the human readable mode, e.g. "network:10.0.0.5:9100" or "usb:/dev/usb/lp0".
	Label() string
}

// WriteDoc writes doc chunk by chunk, sleeping PauseAfter after each chunk
// (also after the last one). The pause is aborted early if ctx is cancelled.
func WriteDoc(ctx context.Context, t Transport, doc escpos.Doc) error {
	for _, c := range doc {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(c.Data) > 0 {
			if err := t.Write(c.Data); err != nil {
				return err
			}
		}
		if c.PauseAfter > 0 {
			if err := sleep(ctx, c.PauseAfter); err != nil {
				return err
			}
		}
	}
	return nil
}

func sleep(ctx context.Context, d time.Duration) error {
	tm := time.NewTimer(d)
	defer tm.Stop()
	select {
	case <-tm.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ---- network ---------------------------------------------------------------

// Network is a raw TCP (port 9100 style) printer.
type Network struct {
	Host         string
	Port         int
	DialTimeout  time.Duration
	WriteTimeout time.Duration

	// AllowLoopback disables the SSRF guard (loopback / link-local / metadata
	// destinations). Only for tests and local development.
	AllowLoopback bool

	conn net.Conn
}

// errBlockedAddr is returned when the printer host resolves to an address the
// SSRF guard refuses.
var errBlockedAddr = errors.New("address not allowed")

// Extra non-obvious cloud metadata endpoints (169.254.169.254 is covered by the
// link-local check).
var metadataAddrs = []netip.Addr{
	netip.MustParseAddr("fd00:ec2::254"),   // AWS IMDS over IPv6
	netip.MustParseAddr("100.100.100.200"), // Alibaba Cloud metadata
}

// blockedAddr reports whether ip is a destination the printer must never be
// pointed at: loopback, link-local (incl. 169.254.0.0/16 metadata), multicast,
// unspecified (0.0.0.0, ::, 0.0.0.0/8) and broadcast. RFC1918 and other private
// ranges stay allowed because real printers live there.
func blockedAddr(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	if ip.Is4() {
		if ip.As4()[0] == 0 || ip == netip.AddrFrom4([4]byte{255, 255, 255, 255}) {
			return true
		}
	}
	for _, m := range metadataAddrs {
		if ip == m {
			return true
		}
	}
	return false
}

// guardControl is a net.Dialer.Control hook. It runs against the concrete
// address of every connection attempt, after DNS resolution, so it also defeats
// DNS-rebinding and hostnames that resolve to forbidden IPs.
func guardControl(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return errBlockedAddr
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return errBlockedAddr
	}
	if blockedAddr(ip.WithZone("")) {
		return errBlockedAddr
	}
	return nil
}

func (n *Network) addr() string { return net.JoinHostPort(n.Host, strconv.Itoa(n.Port)) }

func (n *Network) Kind() string  { return KindNetwork }
func (n *Network) Label() string { return "network:" + n.addr() }

func (n *Network) Open(ctx context.Context) error {
	d := net.Dialer{Timeout: n.DialTimeout}
	if !n.AllowLoopback {
		d.Control = guardControl
	}
	conn, err := d.DialContext(ctx, "tcp", n.addr())
	if err != nil {
		return err
	}
	n.conn = conn
	return nil
}

func (n *Network) Write(p []byte) error {
	if n.conn == nil {
		return errors.New("printer: network transport not open")
	}
	if n.WriteTimeout > 0 {
		if err := n.conn.SetWriteDeadline(time.Now().Add(n.WriteTimeout)); err != nil {
			return err
		}
	}
	_, err := n.conn.Write(p) // net.Conn.Write loops until all bytes are written or errors
	return err
}

func (n *Network) Close() error {
	if n.conn == nil {
		return nil
	}
	if tc, ok := n.conn.(*net.TCPConn); ok {
		_ = tc.CloseWrite() // graceful FIN so buffered data is delivered first
	}
	// conn is deliberately not reset: Close may race with a Write that the
	// worker is aborting, and net.Conn is safe for that.
	return n.conn.Close()
}

// ---- usb (kernel usblp char device) -----------------------------------------

// USB writes to a usblp character device such as /dev/usb/lp0.
type USB struct {
	Path         string
	WriteTimeout time.Duration

	f *os.File
}

func (u *USB) Kind() string  { return KindUSB }
func (u *USB) Label() string { return "usb:" + u.Path }

func (u *USB) Open(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := os.OpenFile(u.Path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	u.f = f
	return nil
}

func (u *USB) Write(p []byte) error {
	if u.f == nil {
		return errors.New("printer: usb transport not open")
	}
	if u.WriteTimeout > 0 {
		// Best effort: char devices are often not pollable, in which case
		// SetWriteDeadline returns an error and writes simply block.
		_ = u.f.SetWriteDeadline(time.Now().Add(u.WriteTimeout))
	}
	_, err := u.f.Write(p)
	return err
}

func (u *USB) Close() error {
	if u.f == nil {
		return nil
	}
	return u.f.Close() // *os.File is safe for Close racing a Write
}

// FindUSBDevice locates the usblp device node for vendor:product by scanning
// <sysfsRoot>/class/usbmisc/lp*/device/.. (up to a few parents) for matching
// idVendor / idProduct, and returns <devDir>/usb/lpN.
func FindUSBDevice(sysfsRoot, devDir string, vendor, product uint16) (string, error) {
	base := filepath.Join(sysfsRoot, "class", "usbmisc")
	entries, err := os.ReadDir(base)
	if err != nil {
		return "", fmt.Errorf("usb printer %04x:%04x not found: %w", vendor, product, err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "lp") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		// String concatenation on purpose: "device/.." must be resolved by the
		// kernel (device is a symlink), not lexically cleaned.
		dir := filepath.Join(base, name) + string(filepath.Separator) + "device"
		for level := 0; level < 4; level++ {
			dir += string(filepath.Separator) + ".."
			v, okv := readHexID(dir + string(filepath.Separator) + "idVendor")
			p, okp := readHexID(dir + string(filepath.Separator) + "idProduct")
			if !okv || !okp {
				continue
			}
			if v == vendor && p == product {
				return filepath.Join(devDir, "usb", name), nil
			}
			break // found a USB device dir that does not match
		}
	}
	return "", fmt.Errorf("usb printer %04x:%04x not found under %s", vendor, product, base)
}

func readHexID(path string) (uint16, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(b)), 16, 16)
	if err != nil {
		return 0, false
	}
	return uint16(v), true
}

// ---- mode resolution ---------------------------------------------------------

// SettingsGetter is the slice of domain.Settings the printer needs.
type SettingsGetter interface {
	Get(key string) string
	Int(key string, def int) int
}

// Connect resolves the printer exactly like _get_printer in printer_server.py:
//
//	network: host required; network failure is returned
//	auto:    network first if a host is set; any failure falls back to USB
//	usb (or anything else): USB
//
// The returned transport is open.
func Connect(ctx context.Context, cfg Config, s SettingsGetter) (Transport, error) {
	cfg = cfg.withDefaults()
	mode := strings.ToLower(strings.TrimSpace(s.Get("printer_mode")))
	host := strings.TrimSpace(s.Get("printer_host"))
	port := s.Int("printer_port", 9100)

	if mode == "network" && host == "" {
		return nil, errors.New("printer_mode is 'network' but printer_host is not set")
	}

	var netErr error
	if (mode == "network" || mode == "auto") && host != "" {
		nt := &Network{Host: host, Port: port, DialTimeout: cfg.DialTimeout, WriteTimeout: cfg.WriteTimeout,
			AllowLoopback: cfg.AllowLoopback}
		if err := nt.Open(ctx); err == nil {
			return nt, nil
		} else {
			netErr = dialError(nt.addr(), err)
		}
		if mode == "network" {
			return nil, netErr
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}

	ut, err := openUSB(ctx, cfg)
	if err != nil {
		if netErr != nil {
			return nil, errors.Join(netErr, err)
		}
		return nil, err
	}
	return ut, nil
}

// maxErrLen bounds any error text kept in printer status / events.
const maxErrLen = 200

// dialError turns a dial failure into a short, fixed-vocabulary message. The
// printer host/port are admin-supplied, so resolver and socket error text
// (which can echo arbitrary data from an attacker-controlled DNS server or
// service) is never copied through.
func dialError(addr string, err error) error {
	var reason string
	var dnsErr *net.DNSError
	switch {
	case errors.Is(err, errBlockedAddr):
		reason = "address not allowed (loopback, link-local, multicast or metadata addresses are refused)"
	case errors.Is(err, context.DeadlineExceeded), isTimeout(err):
		reason = "connection timed out"
	case errors.Is(err, context.Canceled):
		reason = "canceled"
	case errors.As(err, &dnsErr):
		reason = "host name could not be resolved"
	case errors.Is(err, syscall.ECONNREFUSED):
		reason = "connection refused"
	default:
		reason = "connection failed"
	}
	return errors.New(truncate("network printer "+sanitizeText(addr)+" unreachable: "+reason, maxErrLen))
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// sanitizeText strips control characters and non-ASCII-printable runes.
func sanitizeText(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
}

// truncate shortens s to at most n bytes without splitting a rune.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// SanitizeError renders err as a single short line (<= 200 bytes, no control
// characters), suitable for status fields and SSE events.
func SanitizeError(err error) string {
	if err == nil {
		return ""
	}
	return truncate(strings.Join(strings.Fields(sanitizeText(err.Error())), " "), maxErrLen)
}

func openUSB(ctx context.Context, cfg Config) (Transport, error) {
	path := cfg.USBDevice
	if path == "" {
		var err error
		path, err = FindUSBDevice(cfg.SysfsRoot, cfg.DevDir, cfg.USBVendor, cfg.USBProduct)
		if err != nil {
			return nil, err
		}
	}
	ut := &USB{Path: path, WriteTimeout: cfg.WriteTimeout}
	if err := ut.Open(ctx); err != nil {
		return nil, fmt.Errorf("usb printer %s: %w", path, err)
	}
	return ut, nil
}
