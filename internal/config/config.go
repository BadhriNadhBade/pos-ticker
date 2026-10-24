// Package config holds the static, environment-derived configuration
// (Config / Load) and the runtime-editable, SQLite-persisted Settings.
package config

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // embed the tz database so DISPLAY_TZ works in a scratch image
)

// MinAdminKeyLen is the minimum accepted ADMIN_KEY length.
const MinAdminKeyLen = 16

// Config is the static (env-only) configuration, read once at startup.
type Config struct {
	PublicAddr string // PUBLIC_ADDR, default 127.0.0.1:8080 (cloudflared target)
	AdminAddr  string // ADMIN_ADDR, default 127.0.0.1:8000 (container images/compose set 0.0.0.0:8000 explicitly)
	AdminKey   string // ADMIN_KEY, required, >= 16 chars
	DBPath     string // DB_PATH, default /app/data/messages.db

	TrustProxy bool // TRUST_PROXY

	// AdminAllowedHosts is the optional ADMIN_ALLOWED_HOSTS comma list of Host
	// header values the admin listener accepts (DNS-rebinding defence). Entries
	// are trimmed and lower-cased; empty means "not configured".
	AdminAllowedHosts []string

	// Global storage caps enforced atomically inside store.InsertMessage.
	MaxSubmissionsPerHour int // MAX_SUBMISSIONS_PER_HOUR, default 200
	MaxPendingRows        int // MAX_PENDING_ROWS, default 2000

	DisplayTZ       string         // DISPLAY_TZ, default America/Los_Angeles
	DisplayLocation *time.Location // resolved DisplayTZ

	// Printer.
	PrinterUSBDevice  string // PRINTER_USB_DEVICE: explicit /dev/usb/lp* path (optional)
	PrinterUSBVendor  uint16 // PRINTER_USB_VENDOR (hex), default 0x0525
	PrinterUSBProduct uint16 // PRINTER_USB_PRODUCT (hex), default 0xa700
	PrinterWidthPx    int    // PRINTER_WIDTH_PX, default 576

	// Image / ASCII pipeline.
	MaxImageBytes    int           // MAX_IMAGE_BYTES, default 10 MiB
	MaxImageHeight   int           // MAX_IMAGE_HEIGHT, default 1200
	MaxImagePixels   int           // MAX_IMAGE_PIXELS, default 24M
	ImageBandPx      int           // IMAGE_BAND_PX, default 128
	ImageBandPause   time.Duration // IMAGE_BAND_PAUSE (seconds, float), default 0.2s
	ImageImpl        string        // IMAGE_IMPL, default bitImageColumn
	ASCIIColumns     int           // ASCII_COLUMNS, default max(16,(width-12)/9)
	ASCIILineSpacing int           // ASCII_LINE_SPACING, default 18
	MaxMessageChars  int           // MAX_MESSAGE_CHARS, default 500

	// Email notifications.
	SMTPHost    string // SMTP_HOST, default smtp.gmail.com
	SMTPPort    int    // SMTP_PORT, default 465 (implicit TLS)
	SMTPUser    string // SMTP_USER
	SMTPPass    string // SMTP_PASS
	NotifyEmail string // NOTIFY_EMAIL, defaults to SMTP_USER

	// SettingDefaults are the env-derived defaults of the runtime settings
	// (PRINTS_PER_HOUR, LINE_WIDTH, ...); DB overrides win. Pass to NewSettings.
	SettingDefaults map[string]string
}

// splitList parses a comma-separated list: trimmed, lower-cased, empties dropped.
func splitList(raw string) []string {
	var out []string
	for _, p := range strings.Split(raw, ",") {
		if p = strings.ToLower(strings.TrimSpace(p)); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Load builds a Config from getenv (os.Getenv in production). All problems
// are reported together.
func Load(getenv func(string) string) (Config, error) {
	var errs []error
	fail := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	str := func(key, def string) string {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			return v
		}
		return def
	}
	integer := func(key string, def, min int) int {
		raw := strings.TrimSpace(getenv(key))
		if raw == "" {
			return def
		}
		n, err := strconv.Atoi(raw)
		if err != nil {
			fail("%s: %q is not an integer", key, raw)
			return def
		}
		if n < min {
			fail("%s: %d is below the minimum %d", key, n, min)
			return def
		}
		return n
	}
	hex16 := func(key string, def uint16) uint16 {
		raw := strings.TrimSpace(getenv(key))
		if raw == "" {
			return def
		}
		s := strings.TrimPrefix(strings.TrimPrefix(raw, "0x"), "0X")
		n, err := strconv.ParseUint(s, 16, 16)
		if err != nil {
			fail("%s: %q is not a 16-bit hex number", key, raw)
			return def
		}
		return uint16(n)
	}

	c := Config{
		PublicAddr:        str("PUBLIC_ADDR", "127.0.0.1:8080"),
		AdminAddr:         str("ADMIN_ADDR", "127.0.0.1:8000"),
		AdminKey:          getenv("ADMIN_KEY"),
		DBPath:            str("DB_PATH", "/app/data/messages.db"),
		TrustProxy:        strings.EqualFold(strings.TrimSpace(getenv("TRUST_PROXY")), "true"),
		DisplayTZ:         str("DISPLAY_TZ", "America/Los_Angeles"),
		PrinterUSBDevice:  str("PRINTER_USB_DEVICE", ""),
		PrinterUSBVendor:  hex16("PRINTER_USB_VENDOR", 0x0525),
		PrinterUSBProduct: hex16("PRINTER_USB_PRODUCT", 0xa700),
		PrinterWidthPx:    integer("PRINTER_WIDTH_PX", 576, 8),
		MaxImageBytes:     integer("MAX_IMAGE_BYTES", 10*1024*1024, 1),
		MaxImageHeight:    integer("MAX_IMAGE_HEIGHT", 1200, 1),
		MaxImagePixels:    integer("MAX_IMAGE_PIXELS", 24_000_000, 1),
		ImageBandPx:       integer("IMAGE_BAND_PX", 128, 1),
		ImageImpl:         str("IMAGE_IMPL", "bitImageColumn"),
		ASCIILineSpacing:  integer("ASCII_LINE_SPACING", 18, 0),
		MaxMessageChars:   integer("MAX_MESSAGE_CHARS", 500, 1),
		AdminAllowedHosts: splitList(getenv("ADMIN_ALLOWED_HOSTS")),

		MaxSubmissionsPerHour: integer("MAX_SUBMISSIONS_PER_HOUR", 200, 1),
		MaxPendingRows:        integer("MAX_PENDING_ROWS", 2000, 1),

		SMTPHost: str("SMTP_HOST", "smtp.gmail.com"),
		SMTPPort: integer("SMTP_PORT", 465, 1),
		SMTPUser: strings.TrimSpace(getenv("SMTP_USER")),
		SMTPPass: getenv("SMTP_PASS"),
	}
	c.NotifyEmail = str("NOTIFY_EMAIL", c.SMTPUser)
	c.ASCIIColumns = integer("ASCII_COLUMNS", max(16, (c.PrinterWidthPx-12)/9), 1)

	switch c.ImageImpl {
	case "bitImageColumn", "bitImageRaster", "graphics":
	default:
		fail("IMAGE_IMPL: %q must be bitImageColumn, bitImageRaster or graphics", c.ImageImpl)
	}

	if raw := strings.TrimSpace(getenv("IMAGE_BAND_PAUSE")); raw == "" {
		c.ImageBandPause = 200 * time.Millisecond
	} else if f, err := strconv.ParseFloat(raw, 64); err != nil || f < 0 || f > 60 {
		fail("IMAGE_BAND_PAUSE: %q must be seconds between 0 and 60", raw)
	} else {
		c.ImageBandPause = time.Duration(f * float64(time.Second))
	}

	if len(c.AdminKey) < MinAdminKeyLen {
		fail("ADMIN_KEY: required and must be at least %d characters (got %d)", MinAdminKeyLen, len(c.AdminKey))
	}

	if loc, err := time.LoadLocation(c.DisplayTZ); err != nil {
		fail("DISPLAY_TZ: unknown time zone %q", c.DisplayTZ)
	} else {
		c.DisplayLocation = loc
	}

	c.SettingDefaults = DefaultSettings(getenv)
	for k := range intSettings {
		if _, err := parseInt(c.SettingDefaults[k]); err != nil {
			fail("%s: %q is not an integer", settingEnv[k], c.SettingDefaults[k])
		}
	}

	if len(errs) > 0 {
		return Config{}, fmt.Errorf("invalid configuration: %w", errors.Join(errs...))
	}
	return c, nil
}
