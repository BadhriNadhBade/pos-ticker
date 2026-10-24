package notify

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func limiterMailer(max int, logBuf *bytes.Buffer, clock *time.Time) *Mailer {
	m := New(Config{MaxPerHour: max}, nil, slog.New(slog.NewTextHandler(logBuf, nil)))
	m.now = func() time.Time { return *clock }
	return m
}

func TestRateLimitDefaultIs20(t *testing.T) {
	if got := New(Config{}, nil, nil).cfg.MaxPerHour; got != 20 {
		t.Fatalf("default MaxPerHour = %d", got)
	}
}

func TestRateLimitRollingWindowAndSingleWarning(t *testing.T) {
	var logs bytes.Buffer
	clock := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	m := limiterMailer(3, &logs, &clock)

	for i := 0; i < 3; i++ {
		if !m.allow() {
			t.Fatalf("send %d should be allowed", i)
		}
		clock = clock.Add(time.Minute)
	}
	// Excess within the hour: dropped, exactly one warning.
	for i := 0; i < 10; i++ {
		if m.allow() {
			t.Fatal("over-limit send allowed")
		}
		clock = clock.Add(time.Minute)
	}
	if n := strings.Count(logs.String(), "rate limit"); n != 1 {
		t.Fatalf("want 1 warning, got %d: %s", n, logs.String())
	}
	if len(m.sent) > 3 {
		t.Fatalf("limiter state unbounded: %d", len(m.sent))
	}

	// An hour on, every old slot has aged out so sends fit again.
	clock = time.Date(2025, 1, 1, 13, 4, 0, 0, time.UTC)
	if !m.allow() {
		t.Fatal("window did not roll")
	}
	// Over the cap again; an hour has passed since the first warning, so one
	// more warning is allowed, but only one.
	for i := 0; i < 8; i++ {
		m.allow()
	}
	if n := strings.Count(logs.String(), "rate limit"); n != 2 {
		t.Fatalf("want 2 warnings after an hour, got %d", n)
	}
}

func TestRateLimitUnlimitedAndConcurrent(t *testing.T) {
	var logs bytes.Buffer
	clock := time.Now()
	m := limiterMailer(-1, &logs, &clock)
	for i := 0; i < 1000; i++ {
		if !m.allow() {
			t.Fatal("negative MaxPerHour must be unlimited")
		}
	}

	m = limiterMailer(20, &logs, &clock)
	var ok atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if m.allow() {
				ok.Add(1)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 20 {
		t.Fatalf("concurrent allowed %d, want exactly 20", ok.Load())
	}
}

func TestNotifyDropsExcess(t *testing.T) {
	f := newFake(t, false, false)
	var logs bytes.Buffer
	m := f.mailer(t, true, &logs)
	m.cfg.MaxPerHour = 2
	for i := 0; i < 5; i++ {
		m.Notify("Ada", "a@example.com", "hi", "1.2.3.4", "t")
	}
	f.mu.Lock()
	n := f.sessions
	f.mu.Unlock()
	if n != 2 {
		t.Fatalf("SMTP sessions = %d, want 2", n)
	}
	if c := strings.Count(logs.String(), "rate limit"); c != 1 {
		t.Fatalf("warnings = %d: %s", c, logs.String())
	}
}

func TestDisabledDoesNotConsumeBudget(t *testing.T) {
	var logs bytes.Buffer
	clock := time.Now()
	m := limiterMailer(1, &logs, &clock)
	for i := 0; i < 5; i++ {
		m.Notify("a", "b", "c", "d", "e") // enabled == nil: no-op
	}
	if len(m.sent) != 0 {
		t.Fatalf("no-op notifications consumed budget: %d", len(m.sent))
	}
}
