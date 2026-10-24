package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/badhrinadhbade/pos-ticker/internal/domain"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "sub", "messages.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func nm(ip, body string) domain.NewMessage {
	return domain.NewMessage{Name: "Alice", Email: "a@example.com", Body: body, IP: ip, UserAgent: "ua"}
}

func mustInsert(t *testing.T, s *Store, m domain.NewMessage, maxIP, perHour int) domain.InsertResult {
	t.Helper()
	r, err := s.InsertMessage(context.Background(), m, maxIP, perHour)
	if err != nil {
		t.Fatalf("InsertMessage: %v", err)
	}
	return r
}

func TestInsertRateLimitPerIP(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		r := mustInsert(t, s, nm("1.1.1.1", fmt.Sprintf("m%d", i)), 3, 100)
		if r.RateLimitedIP || r.ID == 0 {
			t.Fatalf("insert %d unexpectedly limited: %+v", i, r)
		}
	}
	r := mustInsert(t, s, nm("1.1.1.1", "over"), 3, 100)
	if !r.RateLimitedIP || r.ID != 0 || r.Claimed != nil {
		t.Fatalf("want rate limited with nothing inserted, got %+v", r)
	}
	// Other IPs are unaffected.
	if r := mustInsert(t, s, nm("2.2.2.2", "x"), 3, 100); r.RateLimitedIP {
		t.Fatalf("other IP limited")
	}
	c, _ := s.Counts(ctx)
	if c.Total != 4 {
		t.Fatalf("total=%d want 4", c.Total)
	}
}

func TestInsertRateLimitWindow(t *testing.T) {
	s := openTest(t)
	// Old submissions (>1h) must not count.
	for i := 0; i < 5; i++ {
		_, err := s.db.Exec(`INSERT INTO messages (name,email,message,ip,received_at)
			VALUES ('o','','old','9.9.9.9', datetime('now','-2 hours'))`)
		if err != nil {
			t.Fatal(err)
		}
	}
	if r := mustInsert(t, s, nm("9.9.9.9", "fresh"), 3, 100); r.RateLimitedIP {
		t.Fatal("old rows counted against the per-IP window")
	}
}

func TestInsertRateLimitAtomic(t *testing.T) {
	s := openTest(t)
	const workers, maxIP, perHour = 24, 3, 5
	var wg sync.WaitGroup
	var mu sync.Mutex
	var inserted, limited, claimed int
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			r, err := s.InsertMessage(context.Background(), nm("7.7.7.7", fmt.Sprintf("m%d", i)), maxIP, perHour)
			if err != nil {
				t.Errorf("insert: %v", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if r.RateLimitedIP {
				limited++
				return
			}
			inserted++
			if r.Claimed != nil {
				claimed++
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if inserted != maxIP || limited != workers-maxIP {
		t.Fatalf("inserted=%d limited=%d want %d/%d", inserted, limited, maxIP, workers-maxIP)
	}
	if claimed != maxIP { // all 3 fit under the 5/hour cap
		t.Fatalf("claimed=%d want %d", claimed, maxIP)
	}
}

func TestInsertGlobalSlotsAtomic(t *testing.T) {
	s := openTest(t)
	const workers, perHour = 20, 4
	var wg sync.WaitGroup
	var mu sync.Mutex
	claimed := 0
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// distinct IPs so only the global cap applies
			r, err := s.InsertMessage(context.Background(), nm(fmt.Sprintf("10.0.0.%d", i), "x"), 3, perHour)
			if err != nil {
				t.Errorf("insert: %v", err)
				return
			}
			if r.Claimed != nil {
				mu.Lock()
				claimed++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if claimed != perHour {
		t.Fatalf("claimed=%d want exactly %d", claimed, perHour)
	}
	c, _ := s.Counts(context.Background())
	if c.Total != workers || c.Pending != workers-perHour {
		t.Fatalf("counts=%+v", c)
	}
}

func TestClaimTransitions(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	// perHour=0 => everything stays pending.
	ids := make([]int64, 4)
	for i := range ids {
		r := mustInsert(t, s, nm(fmt.Sprintf("3.3.3.%d", i), fmt.Sprintf("m%d", i)), 3, 0)
		if r.Claimed != nil {
			t.Fatal("should be pending")
		}
		ids[i] = r.ID
	}

	// ClaimPending: 0 -> 1, second time ErrNotFound.
	m, err := s.ClaimPending(ctx, ids[0])
	if err != nil || m.Claimed != 1 || m.Body != "m0" {
		t.Fatalf("ClaimPending: %v %+v", err, m)
	}
	if _, err := s.ClaimPending(ctx, ids[0]); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("second claim err=%v", err)
	}
	if _, err := s.ClaimPending(ctx, 9999); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing claim err=%v", err)
	}

	// Skip: pending -> 2; not repeatable; claimed rows can't be skipped.
	if err := s.Skip(ctx, ids[1]); err != nil {
		t.Fatal(err)
	}
	if err := s.Skip(ctx, ids[1]); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("re-skip err=%v", err)
	}
	if err := s.Skip(ctx, ids[0]); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("skip of claimed err=%v", err)
	}
	if _, err := s.ClaimPending(ctx, ids[1]); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("claim of skipped err=%v", err)
	}

	// MarkPrinted: sets printed_at, claimed=0.
	if err := s.MarkPrinted(ctx, ids[0]); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkPrinted(ctx, 9999); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("MarkPrinted missing err=%v", err)
	}
	var printedAt sql.NullString
	var claimed int
	if err := s.db.QueryRow("SELECT printed_at, claimed FROM messages WHERE id=?", ids[0]).Scan(&printedAt, &claimed); err != nil {
		t.Fatal(err)
	}
	if !printedAt.Valid || claimed != 0 {
		t.Fatalf("printed row: %v claimed=%d", printedAt, claimed)
	}
	if n, _ := s.PrintedTotal(ctx); n != 1 {
		t.Fatalf("PrintedTotal=%d", n)
	}
	// A printed row can no longer be claimed or skipped.
	if _, err := s.ClaimPending(ctx, ids[0]); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("claim printed err=%v", err)
	}

	// ClaimAllPending returns only the remaining pending (ids[2], ids[3]) oldest first.
	all, err := s.ClaimAllPending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].ID != ids[2] || all[1].ID != ids[3] || all[0].Claimed != 1 {
		t.Fatalf("ClaimAllPending=%+v", all)
	}
	if again, _ := s.ClaimAllPending(ctx); len(again) != 0 {
		t.Fatalf("second drain returned %d", len(again))
	}

	// ReleaseClaim: back to pending; no-op on skipped.
	if err := s.ReleaseClaim(ctx, ids[2]); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseClaim(ctx, ids[1]); err != nil {
		t.Fatal(err)
	}
	c, _ := s.Counts(ctx)
	if c.Pending != 1 || c.Total != 4 || c.PrintedThisHour != 1 {
		t.Fatalf("counts=%+v", c)
	}
	var skipped int
	_ = s.db.QueryRow("SELECT claimed FROM messages WHERE id=?", ids[1]).Scan(&skipped)
	if skipped != 2 {
		t.Fatalf("skipped row claimed=%d, ReleaseClaim must not touch it", skipped)
	}
}

func TestReleaseStaleClaims(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	a := mustInsert(t, s, nm("4.4.4.1", "a"), 3, 10) // claimed
	b := mustInsert(t, s, nm("4.4.4.2", "b"), 3, 10) // claimed then printed
	c := mustInsert(t, s, nm("4.4.4.3", "c"), 3, 0)  // pending
	d := mustInsert(t, s, nm("4.4.4.4", "d"), 3, 0)  // skipped
	if a.Claimed == nil || b.Claimed == nil || c.Claimed != nil {
		t.Fatal("setup")
	}
	if err := s.MarkPrinted(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Skip(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	n, err := s.ReleaseStaleClaims(ctx)
	if err != nil || n != 1 {
		t.Fatalf("released=%d err=%v want 1", n, err)
	}
	counts, _ := s.Counts(ctx)
	if counts.Pending != 2 { // a (released) + c
		t.Fatalf("pending=%d want 2", counts.Pending)
	}
	if n, _ := s.ReleaseStaleClaims(ctx); n != 0 {
		t.Fatalf("second release=%d", n)
	}
}

func TestSlotsCountClaimedAndPrinted(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	// perHour=2: first claimed, second claimed, third pending.
	r1 := mustInsert(t, s, nm("5.5.5.1", "1"), 3, 2)
	r2 := mustInsert(t, s, nm("5.5.5.2", "2"), 3, 2)
	r3 := mustInsert(t, s, nm("5.5.5.3", "3"), 3, 2)
	if r1.Claimed == nil || r2.Claimed == nil || r3.Claimed != nil {
		t.Fatal("slot accounting wrong for claimed rows")
	}
	// Printing keeps the slot consumed (printed within the hour).
	_ = s.MarkPrinted(ctx, r1.ID)
	_ = s.MarkPrinted(ctx, r2.ID)
	if r := mustInsert(t, s, nm("5.5.5.4", "4"), 3, 2); r.Claimed != nil {
		t.Fatal("printed rows must still use slots")
	}
	// A failed print releases the slot.
	s2 := openTest(t)
	a := mustInsert(t, s2, nm("6.6.6.1", "a"), 3, 1)
	if r := mustInsert(t, s2, nm("6.6.6.2", "b"), 3, 1); r.Claimed != nil {
		t.Fatal("cap not enforced")
	}
	_ = s2.ReleaseClaim(ctx, a.ID)
	if r := mustInsert(t, s2, nm("6.6.6.3", "c"), 3, 1); r.Claimed == nil {
		t.Fatal("slot not freed after release")
	}
}

func TestInsertStoresFieldsAndRedactsHeaders(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	hdr := `{"Cookie":"sess=abc","authorization":"Bearer x","X-ADMIN-KEY":"k","User-Agent":"ua","Accept":["a","b"]}`
	img := []byte{0x89, 'P', 'N', 'G', 1, 2, 3}
	m := nm("8.8.8.8", "hello")
	m.HeadersJSON = hdr
	m.BrowserTime = "Mon  May 12  2:41 PM"
	m.Image = img
	r := mustInsert(t, s, m, 3, 10)
	if r.Claimed == nil {
		t.Fatal("expected claimed")
	}
	got := r.Claimed
	if got.Name != "Alice" || got.Body != "hello" || got.IP != "8.8.8.8" || got.UserAgent != "ua" ||
		got.BrowserTime == nil || *got.BrowserTime != "Mon  May 12  2:41 PM" || string(got.Image) != string(img) ||
		got.Claimed != 1 || got.PrintedAt != nil {
		t.Fatalf("claimed message wrong: %+v", got)
	}
	var h map[string]any
	if err := json.Unmarshal([]byte(got.HeadersJSON), &h); err != nil {
		t.Fatalf("headers not JSON: %v (%s)", err, got.HeadersJSON)
	}
	for _, k := range []string{"Cookie", "authorization", "X-ADMIN-KEY"} {
		if h[k] != RedactedValue {
			t.Errorf("%s not redacted: %v", k, h[k])
		}
	}
	if h["User-Agent"] != "ua" {
		t.Errorf("non-sensitive header altered: %v", h["User-Agent"])
	}
	if _, ok := h["Accept"].([]any); !ok {
		t.Errorf("array header value not preserved: %v", h["Accept"])
	}
	// Persisted form is redacted too.
	var stored string
	_ = s.db.QueryRow("SELECT headers FROM messages WHERE id=?", r.ID).Scan(&stored)
	if stored != got.HeadersJSON {
		t.Errorf("stored != returned")
	}

	b, err := s.Image(ctx, r.ID)
	if err != nil || string(b) != string(img) {
		t.Fatalf("Image: %v %v", b, err)
	}
	// No image / unknown id => ErrNotFound; empty browser_time is NULL.
	r2 := mustInsert(t, s, nm("8.8.8.9", "noimg"), 3, 10)
	if _, err := s.Image(ctx, r2.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("no-image err=%v", err)
	}
	if _, err := s.Image(ctx, 424242); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing err=%v", err)
	}
	if r2.Claimed.BrowserTime != nil {
		t.Fatal("empty browser_time should be NULL")
	}
}

func TestRedactHeadersJSON(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"empty", "", ""},
		{"untouched", `{"A":"b"}`, `{"A":"b"}`},
		{"cookie", `{"cookie":"x","A":"b"}`, `{"A":"b","cookie":"[redacted]"}`},
		{"mixed case", `{"AUTHORIZATION":"x"}`, `{"AUTHORIZATION":"[redacted]"}`},
		{"not json", `cookie=abc`, `{}`},
		{"array", `["Cookie","x"]`, `{}`},
		{"null", `null`, `{}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RedactHeadersJSON(tt.in); got != tt.want {
				t.Fatalf("got %q want %q", got, tt.want)
			}
		})
	}
}

func TestListAndSearch(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	seed := []struct{ name, email, body, ts string }{
		{"Alice", "alice@x.com", "hello world", "2024-01-01 10:00:00"},
		{"Bob", "bob@y.com", "100% sure", "2024-01-02 10:00:00"},
		{"Carol", "carol@z.com", "snake_case name", "2024-01-03 10:00:00"},
		{"Dave", "dave@x.com", `back\slash`, "2024-01-04 10:00:00"},
	}
	for i, r := range seed {
		var img any
		if i == 0 {
			img = []byte("img")
		}
		if _, err := s.db.Exec(`INSERT INTO messages (name,email,message,ip,received_at,headers,image) VALUES (?,?,?,?,?,?,?)`,
			r.name, r.email, r.body, "1.2.3.4", r.ts, "{}", img); err != nil {
			t.Fatal(err)
		}
	}

	all, total, err := s.List(ctx, 2, 0)
	if err != nil || total != 4 || len(all) != 2 || all[0].Name != "Dave" || all[1].Name != "Carol" {
		t.Fatalf("List: %v total=%d %+v", err, total, all)
	}
	page2, _, _ := s.List(ctx, 2, 2)
	if len(page2) != 2 || page2[0].Name != "Bob" || page2[1].Name != "Alice" || !page2[1].HasImage || page2[0].HasImage {
		t.Fatalf("page2=%+v", page2)
	}
	if page2[0].Headers == nil || *page2[0].Headers != "{}" || page2[0].BrowserTime != nil {
		t.Fatalf("nullable fields wrong: %+v", page2[0])
	}

	tests := []struct {
		q     string
		names []string
	}{
		{"alice", []string{"Alice"}},
		{"X.COM", []string{"Dave", "Alice"}}, // email, case-insensitive
		{"world", []string{"Alice"}},
		{"%", []string{"Bob"}},   // literal percent, not wildcard
		{"_", []string{"Carol"}}, // literal underscore
		{`\`, []string{"Dave"}},  // literal backslash
		{"a%e", nil},             // wildcard must not match "Alice"
		{"'; DROP TABLE messages;--", nil},
		{"", []string{"Dave", "Carol", "Bob", "Alice"}},
		{"  bob  ", []string{"Bob"}},
	}
	for _, tt := range tests {
		t.Run("q="+tt.q, func(t *testing.T) {
			got, total, err := s.Search(ctx, tt.q, 50, 0)
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, m := range got {
				names = append(names, m.Name)
			}
			if fmt.Sprint(names) != fmt.Sprint(tt.names) || total != len(tt.names) {
				t.Fatalf("names=%v total=%d want %v", names, total, tt.names)
			}
		})
	}
	// Search pagination keeps total of the full match set.
	got, total, _ := s.Search(ctx, "x.com", 1, 1)
	if total != 2 || len(got) != 1 || got[0].Name != "Alice" {
		t.Fatalf("search paging: %+v total=%d", got, total)
	}
	if _, _, err := s.List(ctx, -5, -1); err != nil {
		t.Fatalf("negative paging: %v", err)
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "m.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.LoadSettings(ctx); len(got) != 0 {
		t.Fatalf("fresh settings=%v", got)
	}
	if err := s.SaveSettings(ctx, map[string]string{"a": "1", "b": "2"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSettings(ctx, map[string]string{"a": "3"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSettings(ctx, nil); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	s, err = Open(path) // persisted across reopen
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, _ := s.LoadSettings(ctx)
	if len(got) != 2 || got["a"] != "3" || got["b"] != "2" {
		t.Fatalf("settings=%v", got)
	}
}

func TestLegacySchemaUpgrade(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	raw, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	// The oldest legacy schema: no browser_time / headers / image columns.
	stmts := []string{
		`CREATE TABLE messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL, email TEXT NOT NULL,
			message TEXT NOT NULL, ip TEXT, user_agent TEXT, received_at TEXT NOT NULL,
			printed_at TEXT, claimed INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL, updated_at TEXT NOT NULL)`,
		`INSERT INTO messages (name,email,message,ip,user_agent,received_at,printed_at,claimed)
			VALUES ('Old','o@x.com','legacy msg','1.1.1.1','ua','2023-05-01 12:00:00','2023-05-01 12:01:00',0)`,
		`INSERT INTO messages (name,email,message,ip,received_at,claimed)
			VALUES ('Stuck','','in flight',NULL,'2023-05-02 12:00:00',1)`,
		`INSERT INTO settings VALUES ('prints_per_hour','7','2023-05-01 00:00:00')`,
	}
	for _, q := range stmts {
		if _, err := raw.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	_ = raw.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open legacy: %v", err)
	}
	defer s.Close()

	// Columns now exist, legacy data intact, NULLs tolerated.
	list, total, err := s.List(ctx, 10, 0)
	if err != nil || total != 2 {
		t.Fatalf("List: %v total=%d", err, total)
	}
	if list[0].Name != "Stuck" || list[0].IP != "" || list[0].Headers != nil || list[0].HasImage || list[0].BrowserTime != nil {
		t.Fatalf("legacy NULL row: %+v", list[0])
	}
	if list[1].PrintedAt == nil || *list[1].PrintedAt != "2023-05-01 12:01:00" {
		t.Fatalf("legacy printed row: %+v", list[1])
	}
	if n, err := s.ReleaseStaleClaims(ctx); err != nil || n != 1 {
		t.Fatalf("stale: %d %v", n, err)
	}
	if set, _ := s.LoadSettings(ctx); set["prints_per_hour"] != "7" {
		t.Fatalf("legacy settings=%v", set)
	}
	// New writes using the added columns work.
	m := nm("3.3.3.3", "new")
	m.HeadersJSON = `{"A":"b"}`
	m.BrowserTime = "bt"
	m.Image = []byte("x")
	if r := mustInsert(t, s, m, 3, 10); r.Claimed == nil || r.Claimed.Image == nil {
		t.Fatalf("insert on upgraded db: %+v", r)
	}
	_ = s.Close()

	// Re-opening an already-upgraded DB tolerates "duplicate column".
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	_ = s2.Close()
}

func TestPingAndPathEdgeCases(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "we ird#name%41.db"))
	if err != nil {
		t.Fatalf("Open odd path: %v", err)
	}
	defer s.Close()
	if err := s.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(""); err == nil {
		t.Fatal("empty path should fail")
	}
}
