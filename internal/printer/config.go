package printer

import (
	"log/slog"
	"strconv"
	"strings"
	"time"
)

// Defaults matching printer_server.py.
const (
	DefaultUSBVendor  uint16 = 0x0525
	DefaultUSBProduct uint16 = 0xa700
)

// Config holds process-level printer settings (the runtime-editable ones —
// mode, host, port — come from SettingsGetter at connect time).
type Config struct {
	USBVendor  uint16 // PRINTER_USB_VENDOR; 0 with USBProduct 0 = defaults 0525:a700
	USBProduct uint16 // PRINTER_USB_PRODUCT
	USBDevice  string // PRINTER_USB_DEVICE: explicit device path, skips sysfs lookup

	// AllowLoopback (PRINTER_ALLOW_LOOPBACK=true) lets the network printer be a
	// loopback / link-local address. Off by default: printer_host is
	// admin-editable, so it is an SSRF vector. Tests and local dev turn it on.
	AllowLoopback bool

	SysfsRoot string // default "/sys" (injectable for tests)
	DevDir    string // default "/dev" (injectable for tests)

	DialTimeout  time.Duration // default 5s
	WriteTimeout time.Duration // per Write call, default 30s
	QueueSize    int           // default 128
	DrainTimeout time.Duration // Stop() drain budget, default 30s
	JobTimeout   time.Duration // hard cap for one job incl. pauses, default 5m

	Logger *slog.Logger // default slog.Default()
}

func (c Config) withDefaults() Config {
	if c.USBVendor == 0 && c.USBProduct == 0 {
		c.USBVendor, c.USBProduct = DefaultUSBVendor, DefaultUSBProduct
	}
	if c.SysfsRoot == "" {
		c.SysfsRoot = "/sys"
	}
	if c.DevDir == "" {
		c.DevDir = "/dev"
	}
	if c.DialTimeout <= 0 {
		c.DialTimeout = 5 * time.Second
	}
	if c.WriteTimeout <= 0 {
		c.WriteTimeout = 30 * time.Second
	}
	if c.QueueSize <= 0 {
		c.QueueSize = 128
	}
	if c.DrainTimeout <= 0 {
		c.DrainTimeout = 30 * time.Second
	}
	if c.JobTimeout <= 0 {
		c.JobTimeout = 5 * time.Minute
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	return c
}

// ConfigFromEnv reads PRINTER_USB_VENDOR / PRINTER_USB_PRODUCT (hex, optional
// 0x prefix), PRINTER_USB_DEVICE and PRINTER_ALLOW_LOOPBACK. Unparseable IDs
// keep the defaults.
func ConfigFromEnv(getenv func(string) string) Config {
	c := Config{
		USBDevice:     strings.TrimSpace(getenv("PRINTER_USB_DEVICE")),
		AllowLoopback: strings.EqualFold(strings.TrimSpace(getenv("PRINTER_ALLOW_LOOPBACK")), "true"),
	}
	c.USBVendor, c.USBProduct = DefaultUSBVendor, DefaultUSBProduct
	if v, ok := parseHexID(getenv("PRINTER_USB_VENDOR")); ok {
		c.USBVendor = v
	}
	if v, ok := parseHexID(getenv("PRINTER_USB_PRODUCT")); ok {
		c.USBProduct = v
	}
	return c
}

func parseHexID(s string) (uint16, bool) {
	s = strings.TrimSpace(strings.ToLower(s))
	s = strings.TrimPrefix(s, "0x")
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseUint(s, 16, 16)
	return uint16(v), err == nil
}
