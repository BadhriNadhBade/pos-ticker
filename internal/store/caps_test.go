package store

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

func TestClaimAllPendingOmitsImageBlob(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	m := nm("1.1.1.1", "with image")
	m.Image = make([]byte, 64*1024)
	r := mustInsert(t, s, m, 10, 0) // perHour 0: left pending, not claimed
	if r.ID == 0 || r.Claimed != nil {
		t.Fatalf("setup: %+v", r)
	}
	got, err := s.ClaimAllPending(ctx)
	if err != nil || len(got) != 1 {
		t.Fatalf("ClaimAllPending: %v %v", got, err)
	}
	if got[0].Image != nil {
		t.Fatalf("ClaimAllPending must not load the image BLOB, got %d bytes", len(got[0].Image))
	}
	if got[0].Body != "with image" || got[0].Claimed != 1 {
		t.Fatalf("other fields lost: %+v", got[0])
	}
	// The image is still retrievable lazily.
	if b, err := s.Image(ctx, r.ID); err != nil || len(b) != 64*1024 {
		t.Fatalf("Image: %d %v", len(b), err)
	}
}

func TestGlobalSubmissionsPerHourCap(t *testing.T) {
	s := openTest(t)
	s.MaxSubmissionsPerHour = 3
	for i := 0; i < 3; i++ {
		// distinct IPs: the per-IP limit must not be what stops us
		if r := mustInsert(t, s, nm(fmt.Sprintf("10.0.0.%d", i), "x"), 5, 100); r.RateLimitedIP {
			t.Fatalf("insert %d limited early", i)
		}
	}
	r := mustInsert(t, s, nm("10.0.0.99", "over"), 5, 100)
	if !r.RateLimitedIP || r.ID != 0 || r.Claimed != nil {
		t.Fatalf("want global cap hit with nothing inserted, got %+v", r)
	}
	if c, _ := s.Counts(context.Background()); c.Total != 3 {
		t.Fatalf("total=%d want 3", c.Total)
	}
	// Rows older than an hour fall out of the window.
	if _, err := s.db.Exec("UPDATE messages SET received_at=datetime('now','-2 hours')"); err != nil {
		t.Fatal(err)
	}
	if r := mustInsert(t, s, nm("10.0.0.99", "again"), 5, 100); r.RateLimitedIP {
		t.Fatalf("window should have slid: %+v", r)
	}
}

func TestMaxPendingRowsCap(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	s.MaxPendingRows = 2
	a := mustInsert(t, s, nm("1.0.0.1", "a"), 5, 100)
	b := mustInsert(t, s, nm("1.0.0.2", "b"), 5, 100) // claimed rows (in flight) count too
	if a.RateLimitedIP || b.RateLimitedIP {
		t.Fatal("first two must be accepted")
	}
	if r := mustInsert(t, s, nm("1.0.0.3", "c"), 5, 100); !r.RateLimitedIP || r.ID != 0 {
		t.Fatalf("want pending cap hit, got %+v", r)
	}
	// Printing a row frees a slot.
	if err := s.MarkPrinted(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if r := mustInsert(t, s, nm("1.0.0.3", "c"), 5, 100); r.RateLimitedIP {
		t.Fatalf("slot should be free: %+v", r)
	}
}

func TestCapsDefaultsAndDisabledWhenZero(t *testing.T) {
	s := openTest(t)
	if s.MaxSubmissionsPerHour != 200 || s.MaxPendingRows != 2000 {
		t.Fatalf("defaults: %d %d", s.MaxSubmissionsPerHour, s.MaxPendingRows)
	}
	s.MaxSubmissionsPerHour, s.MaxPendingRows = 0, 0
	for i := 0; i < 5; i++ {
		if r := mustInsert(t, s, nm("9.9.9.9", "x"), 100, 100); r.RateLimitedIP {
			t.Fatal("caps should be disabled")
		}
	}
}

func TestGlobalCapAtomic(t *testing.T) {
	s := openTest(t)
	s.MaxSubmissionsPerHour = 5
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := s.InsertMessage(context.Background(), nm(fmt.Sprintf("2.0.0.%d", i), "x"), 5, 100)
			if err != nil {
				t.Error(err)
				return
			}
			if !r.RateLimitedIP {
				mu.Lock()
				accepted++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if accepted != 5 {
		t.Fatalf("accepted=%d want exactly 5", accepted)
	}
}
