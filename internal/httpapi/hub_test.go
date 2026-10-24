package httpapi

import (
	"sync"
	"testing"
	"time"

	"github.com/badhrinadhbade/pos-ticker/internal/domain"
)

func TestHubFanOut(t *testing.T) {
	h := NewHub()
	a, b := h.Subscribe(), h.Subscribe()
	h.Publish(domain.Event{Type: "x", Data: 1})
	for _, s := range []*Subscription{a, b} {
		select {
		case ev := <-s.C:
			if ev.Type != "x" {
				t.Errorf("event %v", ev)
			}
		case <-time.After(time.Second):
			t.Fatal("no delivery")
		}
	}
	a.Close()
	a.Close() // idempotent
	if h.Len() != 1 {
		t.Errorf("len = %d", h.Len())
	}
	h.Publish(domain.Event{Type: "y"})
	if ev := <-b.C; ev.Type != "y" {
		t.Error("remaining subscriber starved")
	}
}

func TestHubDropsSlowClientWithoutBlocking(t *testing.T) {
	h := NewHubSize(2, 8)
	slow := h.Subscribe() // never read
	fast := h.Subscribe()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 10; i++ {
			h.Publish(domain.Event{Type: "e", Data: i})
			select { // fast client keeps up
			case <-fast.C:
			default:
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on a slow client")
	}
	if h.Dropped() != 1 {
		t.Errorf("dropped = %d, want 1", h.Dropped())
	}
	// slow channel was closed after its buffer drained
	n := 0
	for range slow.C {
		n++
	}
	if n != 2 {
		t.Errorf("slow client buffered %d events, want 2", n)
	}
	if h.Len() != 1 {
		t.Errorf("subscribers = %d, want only the fast one", h.Len())
	}
	slow.Close() // safe after being dropped
}

func TestHubSubscriberCap(t *testing.T) {
	h := NewHubSize(1, 2)
	if h.Subscribe() == nil || h.Subscribe() == nil {
		t.Fatal("subscribe under cap failed")
	}
	if h.Subscribe() != nil {
		t.Error("cap not enforced")
	}
}

func TestHubConcurrent(t *testing.T) {
	h := NewHub()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				h.Publish(domain.Event{Type: "e"})
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if s := h.Subscribe(); s != nil {
					select {
					case <-s.C:
					default:
					}
					s.Close()
				}
			}
		}()
	}
	wg.Wait()
}
