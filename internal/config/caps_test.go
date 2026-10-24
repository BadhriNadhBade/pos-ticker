package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestLoadStorageCapsAndAllowedHosts(t *testing.T) {
	c, err := Load(env(map[string]string{
		"ADMIN_KEY":                goodKey,
		"MAX_SUBMISSIONS_PER_HOUR": "50",
		"MAX_PENDING_ROWS":         "300",
		"ADMIN_ALLOWED_HOSTS":      " Pos.Local:8000 , ,192.168.1.10:8000,",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxSubmissionsPerHour != 50 || c.MaxPendingRows != 300 {
		t.Fatalf("caps = %d %d", c.MaxSubmissionsPerHour, c.MaxPendingRows)
	}
	want := []string{"pos.local:8000", "192.168.1.10:8000"}
	if !reflect.DeepEqual(c.AdminAllowedHosts, want) {
		t.Fatalf("AdminAllowedHosts = %q, want %q", c.AdminAllowedHosts, want)
	}
}

func TestLoadStorageCapsInvalid(t *testing.T) {
	for _, tc := range []struct{ k, v string }{
		{"MAX_SUBMISSIONS_PER_HOUR", "0"},
		{"MAX_SUBMISSIONS_PER_HOUR", "lots"},
		{"MAX_PENDING_ROWS", "-5"},
	} {
		_, err := Load(env(map[string]string{"ADMIN_KEY": goodKey, tc.k: tc.v}))
		if err == nil || !strings.Contains(err.Error(), tc.k) {
			t.Errorf("%s=%s: want error naming the variable, got %v", tc.k, tc.v, err)
		}
	}
}

func TestAdminAddrDefaultsToLoopback(t *testing.T) {
	c, err := Load(env(map[string]string{"ADMIN_KEY": goodKey}))
	if err != nil || c.AdminAddr != "127.0.0.1:8000" {
		t.Fatalf("AdminAddr = %q, %v", c.AdminAddr, err)
	}
	c, err = Load(env(map[string]string{"ADMIN_KEY": goodKey, "ADMIN_ADDR": "0.0.0.0:8000"}))
	if err != nil || c.AdminAddr != "0.0.0.0:8000" {
		t.Fatalf("explicit ADMIN_ADDR must win, got %q, %v", c.AdminAddr, err)
	}
}
