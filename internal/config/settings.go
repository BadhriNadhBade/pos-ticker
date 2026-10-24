package config

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/badhrinadhbade/pos-ticker/internal/domain"
)

// settingDef lists every runtime setting: its env var and fallback default.
// Mirrors _DEFAULTS in the legacy printer_server.py.
var settingDefs = []struct{ key, env, def string }{
	{"prints_per_hour", "PRINTS_PER_HOUR", "10"},
	{"max_per_ip_hour", "MAX_PER_IP_HOUR", "3"},
	{"line_width", "LINE_WIDTH", "32"},
	{"allowed_origin", "ALLOWED_ORIGIN", "https://your-domain.com"},
	{"printer_mode", "PRINTER_MODE", "auto"},
	{"printer_host", "PRINTER_HOST", ""},
	{"printer_port", "PRINTER_PORT", "9100"},
	{"email_notifications", "EMAIL_NOTIFICATIONS", "true"},
	{"receipt_font", "RECEIPT_FONT", "receipt"},
	{"receipt_font_size", "RECEIPT_FONT_SIZE", "22"},
	{"receipt_show_timestamp", "RECEIPT_SHOW_TIMESTAMP", "true"},
	{"receipt_show_email", "RECEIPT_SHOW_EMAIL", "true"},
	{"receipt_show_id", "RECEIPT_SHOW_ID", "true"},
	{"receipt_title", "RECEIPT_TITLE", ""},
	{"receipt_footer", "RECEIPT_FOOTER", ""},
}

// intSettings mirrors _INT_SETTINGS in the legacy server.
var intSettings = map[string]bool{
	"prints_per_hour":   true,
	"max_per_ip_hour":   true,
	"line_width":        true,
	"printer_port":      true,
	"receipt_font_size": true,
}

var settingEnv = func() map[string]string {
	m := make(map[string]string, len(settingDefs))
	for _, d := range settingDefs {
		m[d.key] = d.env
	}
	return m
}()

// DefaultSettings returns the setting defaults, overridden by env vars
// (PRINTS_PER_HOUR, ...). An unset or empty variable keeps the built-in default.
func DefaultSettings(getenv func(string) string) map[string]string {
	out := make(map[string]string, len(settingDefs))
	for _, d := range settingDefs {
		v := d.def
		if e, ok := lookup(getenv, d.env); ok {
			v = e
		}
		out[d.key] = v
	}
	return out
}

// lookup treats an empty environment value as unset.
func lookup(getenv func(string) string, key string) (string, bool) {
	v := getenv(key)
	if v == "" {
		return "", false
	}
	return v, true
}

// SettingKeys returns the valid runtime setting keys in a stable order.
func SettingKeys() []string {
	keys := make([]string, len(settingDefs))
	for i, d := range settingDefs {
		keys[i] = d.key
	}
	return keys
}

// IsIntSetting reports whether key must hold an integer.
func IsIntSetting(key string) bool { return intSettings[key] }

func parseInt(s string) (int, error) { return strconv.Atoi(strings.TrimSpace(s)) }

// ValidationError is returned by Patch for a bad request (map to HTTP 400).
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

// Settings is the runtime-editable configuration: env defaults overridden by
// values persisted in SQLite. Safe for concurrent use. Implements domain.Settings.
type Settings struct {
	store    domain.Store
	defaults map[string]string

	mu  sync.RWMutex
	cur map[string]string
}

var _ domain.Settings = (*Settings)(nil)

// NewSettings creates Settings over store. The keys of defaults define the set
// of valid settings (use Config.SettingDefaults or DefaultSettings). Call Load
// to pull the persisted overrides.
func NewSettings(store domain.Store, defaults map[string]string) *Settings {
	d := make(map[string]string, len(defaults))
	for k, v := range defaults {
		d[k] = v
	}
	s := &Settings{store: store, defaults: d, cur: make(map[string]string, len(d))}
	for k, v := range d {
		s.cur[k] = v
	}
	return s
}

// Load (re)reads persisted overrides from the store: defaults, then DB rows on top.
func (s *Settings) Load(ctx context.Context) error {
	rows, err := s.store.LoadSettings(ctx)
	if err != nil {
		return err
	}
	merged := make(map[string]string, len(s.defaults)+len(rows))
	for k, v := range s.defaults {
		merged[k] = v
	}
	for k, v := range rows {
		merged[k] = v
	}
	s.mu.Lock()
	s.cur = merged
	s.mu.Unlock()
	return nil
}

// Get returns the current value of key ("" if unknown).
func (s *Settings) Get(key string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if v, ok := s.cur[key]; ok {
		return v
	}
	return s.defaults[key]
}

// Int returns key as an integer; an empty or unparsable value yields def.
func (s *Settings) Int(key string, def int) int {
	v := strings.TrimSpace(s.Get(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// All returns a copy of every setting.
func (s *Settings) All() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]string, len(s.cur))
	for k, v := range s.cur {
		out[k] = v
	}
	return out
}

// Patch validates and persists kv, then returns the full settings map.
// Unknown keys and non-integer values for int keys are rejected with a
// *ValidationError and nothing is saved.
func (s *Settings) Patch(ctx context.Context, kv map[string]string) (map[string]string, error) {
	var unknown []string
	for k := range kv {
		if _, ok := s.defaults[k]; !ok {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, &ValidationError{Msg: fmt.Sprintf("Unknown settings: %v", unknown)}
	}
	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if intSettings[k] {
			if _, err := parseInt(kv[k]); err != nil {
				return nil, &ValidationError{Msg: fmt.Sprintf("Setting '%s' must be an integer", k)}
			}
		}
	}
	if len(kv) > 0 {
		if err := s.store.SaveSettings(ctx, kv); err != nil {
			return nil, err
		}
		s.mu.Lock()
		for k, v := range kv {
			s.cur[k] = v
		}
		s.mu.Unlock()
	}
	return s.All(), nil
}
