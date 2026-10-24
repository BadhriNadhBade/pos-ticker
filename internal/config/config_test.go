package config

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/badhrinadhbade/pos-ticker/internal/domain"
	"github.com/badhrinadhbade/pos-ticker/internal/store"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

const goodKey = "0123456789abcdef"

func TestLoadDefaults(t *testing.T) {
	c, err := Load(env(map[string]string{"ADMIN_KEY": goodKey}))
	if err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		name      string
		got, want any
	}{
		{"PublicAddr", c.PublicAddr, "127.0.0.1:8080"},
		{"AdminAddr", c.AdminAddr, "127.0.0.1:8000"},
		{"AdminAllowedHosts", len(c.AdminAllowedHosts), 0},
		{"MaxSubmissionsPerHour", c.MaxSubmissionsPerHour, 200},
		{"MaxPendingRows", c.MaxPendingRows, 2000},
		{"DBPath", c.DBPath, "/app/data/messages.db"},
		{"TrustProxy", c.TrustProxy, false},
		{"DisplayTZ", c.DisplayTZ, "America/Los_Angeles"},
		{"USBVendor", c.PrinterUSBVendor, uint16(0x0525)},
		{"USBProduct", c.PrinterUSBProduct, uint16(0xa700)},
		{"Width", c.PrinterWidthPx, 576},
		{"MaxImageBytes", c.MaxImageBytes, 10 * 1024 * 1024},
		{"MaxImageHeight", c.MaxImageHeight, 1200},
		{"MaxImagePixels", c.MaxImagePixels, 24_000_000},
		{"BandPx", c.ImageBandPx, 128},
		{"BandPause", c.ImageBandPause, 200 * time.Millisecond},
		{"ImageImpl", c.ImageImpl, "bitImageColumn"},
		{"ASCIIColumns", c.ASCIIColumns, 62}, // (576-12)/9
		{"ASCIILineSpacing", c.ASCIILineSpacing, 18},
		{"MaxMessageChars", c.MaxMessageChars, 500},
		{"SMTPHost", c.SMTPHost, "smtp.gmail.com"},
		{"SMTPPort", c.SMTPPort, 465},
		{"PrinterUSBDevice", c.PrinterUSBDevice, ""},
	}
	for _, ch := range checks {
		if !reflect.DeepEqual(ch.got, ch.want) {
			t.Errorf("%s = %v, want %v", ch.name, ch.got, ch.want)
		}
	}
	if c.DisplayLocation == nil || c.DisplayLocation.String() != "America/Los_Angeles" {
		t.Errorf("DisplayLocation = %v", c.DisplayLocation)
	}
	if c.SettingDefaults["prints_per_hour"] != "10" || c.SettingDefaults["line_width"] != "32" {
		t.Errorf("SettingDefaults = %v", c.SettingDefaults)
	}
}

func TestLoadOverrides(t *testing.T) {
	c, err := Load(env(map[string]string{
		"ADMIN_KEY":           goodKey,
		"PUBLIC_ADDR":         "0.0.0.0:9090",
		"ADMIN_ADDR":          "127.0.0.1:9000",
		"DB_PATH":             "/tmp/x.db",
		"TRUST_PROXY":         "TRUE",
		"DISPLAY_TZ":          "Europe/Berlin",
		"PRINTER_USB_DEVICE":  "/dev/usb/lp1",
		"PRINTER_USB_VENDOR":  "0x04b8",
		"PRINTER_USB_PRODUCT": "0E15",
		"PRINTER_WIDTH_PX":    "384",
		"MAX_IMAGE_BYTES":     "1000",
		"IMAGE_BAND_PX":       "240",
		"IMAGE_BAND_PAUSE":    "0.5",
		"IMAGE_IMPL":          "graphics",
		"ASCII_LINE_SPACING":  "20",
		"SMTP_USER":           "me@example.com",
		"SMTP_PASS":           "pw",
		"SMTP_PORT":           "587",
		"PRINTS_PER_HOUR":     "25",
		"ALLOWED_ORIGIN":      "https://example.org",
		"RECEIPT_TITLE":       "HELLO",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.PublicAddr != "0.0.0.0:9090" || c.AdminAddr != "127.0.0.1:9000" || c.DBPath != "/tmp/x.db" ||
		!c.TrustProxy || c.DisplayTZ != "Europe/Berlin" || c.PrinterUSBDevice != "/dev/usb/lp1" ||
		c.PrinterUSBVendor != 0x04b8 || c.PrinterUSBProduct != 0x0e15 || c.PrinterWidthPx != 384 ||
		c.MaxImageBytes != 1000 || c.ImageBandPx != 240 || c.ImageBandPause != 500*time.Millisecond ||
		c.ImageImpl != "graphics" || c.ASCIILineSpacing != 20 || c.SMTPUser != "me@example.com" ||
		c.SMTPPass != "pw" || c.SMTPPort != 587 {
		t.Fatalf("overrides not applied: %+v", c)
	}
	if c.ASCIIColumns != 41 { // (384-12)/9
		t.Errorf("ASCIIColumns = %d, want 41", c.ASCIIColumns)
	}
	if c.NotifyEmail != "me@example.com" {
		t.Errorf("NotifyEmail should default to SMTP_USER, got %q", c.NotifyEmail)
	}
	if c.SettingDefaults["prints_per_hour"] != "25" || c.SettingDefaults["allowed_origin"] != "https://example.org" ||
		c.SettingDefaults["receipt_title"] != "HELLO" {
		t.Errorf("SettingDefaults = %v", c.SettingDefaults)
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantSub []string
	}{
		{"missing admin key", map[string]string{}, []string{"ADMIN_KEY"}},
		{"admin key too short", map[string]string{"ADMIN_KEY": "short-key-15chr"}, []string{"ADMIN_KEY", "at least 16"}},
		{"admin key 15 chars", map[string]string{"ADMIN_KEY": strings.Repeat("x", 15)}, []string{"ADMIN_KEY"}},
		{"bad int", map[string]string{"ADMIN_KEY": goodKey, "PRINTER_WIDTH_PX": "wide"}, []string{"PRINTER_WIDTH_PX"}},
		{"bad hex", map[string]string{"ADMIN_KEY": goodKey, "PRINTER_USB_VENDOR": "0xZZ"}, []string{"PRINTER_USB_VENDOR"}},
		{"hex overflow", map[string]string{"ADMIN_KEY": goodKey, "PRINTER_USB_PRODUCT": "0x10000"}, []string{"PRINTER_USB_PRODUCT"}},
		{"bad tz", map[string]string{"ADMIN_KEY": goodKey, "DISPLAY_TZ": "Mars/Olympus"}, []string{"DISPLAY_TZ"}},
		{"bad impl", map[string]string{"ADMIN_KEY": goodKey, "IMAGE_IMPL": "magic"}, []string{"IMAGE_IMPL"}},
		{"bad pause", map[string]string{"ADMIN_KEY": goodKey, "IMAGE_BAND_PAUSE": "-1"}, []string{"IMAGE_BAND_PAUSE"}},
		{"bad int setting default", map[string]string{"ADMIN_KEY": goodKey, "PRINTS_PER_HOUR": "lots"}, []string{"PRINTS_PER_HOUR"}},
		{"zero max bytes", map[string]string{"ADMIN_KEY": goodKey, "MAX_IMAGE_BYTES": "0"}, []string{"MAX_IMAGE_BYTES"}},
		{"multiple reported", map[string]string{"ADMIN_KEY": "x", "DISPLAY_TZ": "Nope/Nope"}, []string{"ADMIN_KEY", "DISPLAY_TZ"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(env(tt.env))
			if err == nil {
				t.Fatal("expected error")
			}
			for _, s := range tt.wantSub {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("error %q lacks %q", err, s)
				}
			}
		})
	}
}

func TestTrustProxyParsing(t *testing.T) {
	for v, want := range map[string]bool{"": false, "false": false, "true": true, "True": true, " true ": true, "yes": false, "1": false} {
		c, err := Load(env(map[string]string{"ADMIN_KEY": goodKey, "TRUST_PROXY": v}))
		if err != nil || c.TrustProxy != want {
			t.Errorf("TRUST_PROXY=%q -> %v (err %v), want %v", v, c.TrustProxy, err, want)
		}
	}
}

// fakeStore implements only the settings half of domain.Store.
type fakeStore struct {
	domain.Store
	mu      sync.Mutex
	rows    map[string]string
	saveErr error
	saves   int
}

func (f *fakeStore) LoadSettings(context.Context) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]string{}
	for k, v := range f.rows {
		out[k] = v
	}
	return out, nil
}

func (f *fakeStore) SaveSettings(_ context.Context, kv map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.saveErr != nil {
		return f.saveErr
	}
	f.saves++
	if f.rows == nil {
		f.rows = map[string]string{}
	}
	for k, v := range kv {
		f.rows[k] = v
	}
	return nil
}

func newSettings(t *testing.T, fs *fakeStore) *Settings {
	t.Helper()
	s := NewSettings(fs, DefaultSettings(func(string) string { return "" }))
	if err := s.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSettingsDefaultsMatchLegacy(t *testing.T) {
	want := map[string]string{
		"prints_per_hour": "10", "max_per_ip_hour": "3", "line_width": "32",
		"allowed_origin": "https://your-domain.com", "printer_mode": "auto", "printer_host": "",
		"printer_port": "9100", "email_notifications": "true", "receipt_font": "receipt",
		"receipt_font_size": "22", "receipt_show_timestamp": "true", "receipt_show_email": "true",
		"receipt_show_id": "true", "receipt_title": "", "receipt_footer": "",
	}
	s := newSettings(t, &fakeStore{})
	if !reflect.DeepEqual(s.All(), want) {
		t.Fatalf("defaults = %v", s.All())
	}
}

func TestSettingsEnvDefaultsAndDBOverride(t *testing.T) {
	fs := &fakeStore{rows: map[string]string{"line_width": "48", "legacy_extra": "x"}}
	d := DefaultSettings(env(map[string]string{"LINE_WIDTH": "40", "PRINTS_PER_HOUR": "99"}))
	s := NewSettings(fs, d)
	if s.Get("line_width") != "40" {
		t.Fatal("env default before Load")
	}
	if err := s.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.Get("line_width") != "48" || s.Get("prints_per_hour") != "99" || s.Get("legacy_extra") != "x" {
		t.Fatalf("DB must override env default: %v", s.All())
	}
	if s.Get("nope") != "" {
		t.Fatal("unknown key should be empty")
	}
}

func TestSettingsInt(t *testing.T) {
	fs := &fakeStore{rows: map[string]string{"line_width": "abc", "printer_port": "", "max_per_ip_hour": " 7 "}}
	s := newSettings(t, fs)
	tests := []struct {
		key  string
		def  int
		want int
	}{
		{"line_width", 32, 32},       // invalid -> default
		{"printer_port", 9100, 9100}, // empty -> default
		{"max_per_ip_hour", 3, 7},    // whitespace tolerated
		{"prints_per_hour", 1, 10},   // plain default value
		{"missing", 5, 5},
	}
	for _, tt := range tests {
		if got := s.Int(tt.key, tt.def); got != tt.want {
			t.Errorf("Int(%s,%d)=%d want %d", tt.key, tt.def, got, tt.want)
		}
	}
}

func TestSettingsPatchValidation(t *testing.T) {
	tests := []struct {
		name    string
		patch   map[string]string
		wantErr string // "" = success
	}{
		{"valid mixed", map[string]string{"line_width": "48", "receipt_title": "Hi"}, ""},
		{"empty patch", map[string]string{}, ""},
		{"nil patch", nil, ""},
		{"unknown key", map[string]string{"bogus": "1"}, "Unknown settings: [bogus]"},
		{"unknown sorted", map[string]string{"zeta": "1", "alpha": "1", "line_width": "1"}, "Unknown settings: [alpha zeta]"},
		{"non-int", map[string]string{"line_width": "wide"}, "Setting 'line_width' must be an integer"},
		{"float for int", map[string]string{"printer_port": "9100.5"}, "Setting 'printer_port' must be an integer"},
		{"empty int", map[string]string{"prints_per_hour": ""}, "Setting 'prints_per_hour' must be an integer"},
		{"int key negative ok", map[string]string{"max_per_ip_hour": "-1"}, ""},
		{"text keys not validated", map[string]string{"printer_mode": "whatever", "allowed_origin": ""}, ""},
		{"valid + invalid saves nothing", map[string]string{"line_width": "40", "receipt_font_size": "x"}, "Setting 'receipt_font_size' must be an integer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := &fakeStore{}
			s := newSettings(t, fs)
			before := s.All()
			got, err := s.Patch(context.Background(), tt.patch)
			if tt.wantErr != "" {
				var ve *ValidationError
				if !errors.As(err, &ve) || ve.Msg != tt.wantErr {
					t.Fatalf("err = %v, want ValidationError %q", err, tt.wantErr)
				}
				if got != nil || fs.saves != 0 || !reflect.DeepEqual(s.All(), before) {
					t.Fatal("rejected patch must not change or persist anything")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(before) {
				t.Fatalf("Patch must return the full map: %d keys, want %d", len(got), len(before))
			}
			for k, v := range tt.patch {
				if got[k] != v || s.Get(k) != v {
					t.Errorf("%s = %q (get %q), want %q", k, got[k], s.Get(k), v)
				}
			}
		})
	}
}

func TestSettingsPatchStoreError(t *testing.T) {
	fs := &fakeStore{saveErr: errors.New("disk full")}
	s := newSettings(t, fs)
	if _, err := s.Patch(context.Background(), map[string]string{"line_width": "40"}); err == nil || errors.As(err, new(*ValidationError)) {
		t.Fatalf("want non-validation store error, got %v", err)
	}
	if s.Get("line_width") != "32" {
		t.Fatal("failed save must not change in-memory value")
	}
}

func TestSettingsConcurrent(t *testing.T) {
	s := newSettings(t, &fakeStore{})
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_, _ = s.Patch(context.Background(), map[string]string{"line_width": "40"})
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = s.Int("line_width", 1)
				_ = s.All()
			}
		}()
	}
	wg.Wait()
}

func TestSettingsPersistWithRealStore(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "m.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s := NewSettings(st, DefaultSettings(func(string) string { return "" }))
	if err := s.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Patch(ctx, map[string]string{"prints_per_hour": "42", "receipt_footer": "bye"}); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	st2, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	s2 := NewSettings(st2, DefaultSettings(func(string) string { return "" }))
	if err := s2.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if s2.Int("prints_per_hour", 0) != 42 || s2.Get("receipt_footer") != "bye" || s2.Get("line_width") != "32" {
		t.Fatalf("not persisted: %v", s2.All())
	}
}
