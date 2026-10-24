// Package store implements domain.Store on top of pure-Go SQLite
// (modernc.org/sqlite). The schema is compatible with the legacy Python
// database, so an existing messages.db keeps working after the rewrite.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver

	"github.com/badhrinadhbade/pos-ticker/internal/domain"
)

// Default global storage caps applied by Open.
const (
	DefaultMaxSubmissionsPerHour = 200
	DefaultMaxPendingRows        = 2000
)

// Store is a SQLite-backed domain.Store.
type Store struct {
	db *sql.DB

	// MaxSubmissionsPerHour caps accepted submissions (all IPs) in a rolling
	// hour; MaxPendingRows caps unprinted, unskipped rows (pending + in flight).
	// When either is reached InsertMessage reports RateLimitedIP (caller -> 429).
	// A value <= 0 disables that cap. Set both right after Open, before the
	// store is shared between goroutines.
	MaxSubmissionsPerHour int
	MaxPendingRows        int
}

var _ domain.Store = (*Store)(nil)

const schemaMessages = `
CREATE TABLE IF NOT EXISTS messages (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	name         TEXT NOT NULL,
	email        TEXT NOT NULL,
	message      TEXT NOT NULL,
	ip           TEXT,
	user_agent   TEXT,
	received_at  TEXT NOT NULL,
	browser_time TEXT,
	printed_at   TEXT,
	claimed      INTEGER NOT NULL DEFAULT 0
)`

const schemaSettings = `
CREATE TABLE IF NOT EXISTS settings (
	key        TEXT PRIMARY KEY,
	value      TEXT NOT NULL,
	updated_at TEXT NOT NULL
)`

// Columns added by later legacy versions; added with ALTER on old databases.
var legacyColumns = []string{
	"ALTER TABLE messages ADD COLUMN browser_time TEXT",
	"ALTER TABLE messages ADD COLUMN headers TEXT",
	"ALTER TABLE messages ADD COLUMN image BLOB",
}

// Open opens (creating if needed) the SQLite database at path and applies the
// schema and legacy migrations. The parent directory is created when missing.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("store: empty database path")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("store: create db dir: %w", err)
		}
	}
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	// WAL allows concurrent readers alongside one writer; writers serialize on
	// the SQLite lock (BEGIN IMMEDIATE + busy_timeout), so a small pool is plenty.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(0)

	s := &Store{db: db, MaxSubmissionsPerHour: DefaultMaxSubmissionsPerHour, MaxPendingRows: DefaultMaxPendingRows}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// dsn builds a modernc DSN. Pragmas apply to every pooled connection;
// _txlock=immediate makes every transaction take the write lock up front so
// the read-check-write sequences below cannot interleave across connections.
func dsn(path string) string {
	p := filepath.ToSlash(path)
	p = strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(p)
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Set("_txlock", "immediate")
	return "file:" + p + "?" + q.Encode()
}

func (s *Store) migrate(ctx context.Context) error {
	for _, stmt := range []string{schemaMessages, schemaSettings} {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("store: create schema: %w", err)
		}
	}
	for _, stmt := range legacyColumns {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
				continue
			}
			return fmt.Errorf("store: migrate (%s): %w", stmt, err)
		}
	}
	if _, err := s.db.ExecContext(ctx,
		"CREATE INDEX IF NOT EXISTS idx_messages_ip_received ON messages(ip, received_at)"); err != nil {
		return fmt.Errorf("store: create index: %w", err)
	}
	return nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Ping verifies the database is reachable.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func nowUTC() string { return time.Now().UTC().Format(domain.TimeLayout) }

const messageCols = `id, name, email, message, COALESCE(ip,''), COALESCE(user_agent,''),
	received_at, browser_time, printed_at, claimed, COALESCE(headers,''), image`

type scanner interface{ Scan(dest ...any) error }

func scanMessage(r scanner) (domain.Message, error) {
	var (
		m  domain.Message
		bt sql.NullString
		pa sql.NullString
	)
	if err := r.Scan(&m.ID, &m.Name, &m.Email, &m.Body, &m.IP, &m.UserAgent,
		&m.ReceivedAt, &bt, &pa, &m.Claimed, &m.HeadersJSON, &m.Image); err != nil {
		return m, err
	}
	if bt.Valid {
		v := bt.String
		m.BrowserTime = &v
	}
	if pa.Valid {
		v := pa.String
		m.PrintedAt = &v
	}
	return m, nil
}

// withTx runs fn in a transaction (BEGIN IMMEDIATE) and commits on success.
func (s *Store) withTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// InsertMessage atomically applies the per-IP limit, inserts the row and
// claims it when a print slot is free (mirrors the Python contact() flow).
func (s *Store) InsertMessage(ctx context.Context, m domain.NewMessage, maxPerIP, perHour int) (domain.InsertResult, error) {
	var res domain.InsertResult
	headers := RedactHeadersJSON(m.HeadersJSON)
	var browserTime any
	if m.BrowserTime != "" {
		browserTime = m.BrowserTime
	}
	var image any
	if len(m.Image) > 0 {
		image = m.Image
	}
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var ipCount int
		if err := tx.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM messages WHERE ip=? AND received_at >= datetime('now','-1 hour')",
			m.IP).Scan(&ipCount); err != nil {
			return err
		}
		if ipCount >= maxPerIP {
			res.RateLimitedIP = true
			return nil
		}
		// Global storage caps (same transaction as the insert, so they are
		// race-free): accepted-per-hour across all IPs, and unprinted backlog.
		if s.MaxSubmissionsPerHour > 0 {
			var n int
			if err := tx.QueryRowContext(ctx,
				"SELECT COUNT(*) FROM messages WHERE received_at >= datetime('now','-1 hour')").Scan(&n); err != nil {
				return err
			}
			if n >= s.MaxSubmissionsPerHour {
				res.RateLimitedIP = true
				return nil
			}
		}
		if s.MaxPendingRows > 0 {
			var n int
			if err := tx.QueryRowContext(ctx,
				"SELECT COUNT(*) FROM messages WHERE printed_at IS NULL AND claimed IN (0,1)").Scan(&n); err != nil {
				return err
			}
			if n >= s.MaxPendingRows {
				res.RateLimitedIP = true
				return nil
			}
		}
		r, err := tx.ExecContext(ctx,
			`INSERT INTO messages (name,email,message,ip,user_agent,received_at,browser_time,headers,image)
			 VALUES (?,?,?,?,?,?,?,?,?)`,
			m.Name, m.Email, m.Body, m.IP, m.UserAgent, nowUTC(), browserTime, headers, image)
		if err != nil {
			return err
		}
		id, err := r.LastInsertId()
		if err != nil {
			return err
		}
		res.ID = id

		var slots int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM messages
			 WHERE printed_at >= datetime('now','-1 hour')
			    OR (printed_at IS NULL AND claimed=1)`).Scan(&slots); err != nil {
			return err
		}
		if slots >= perHour {
			return nil // stored, left pending
		}
		if _, err := tx.ExecContext(ctx, "UPDATE messages SET claimed=1 WHERE id=?", id); err != nil {
			return err
		}
		msg, err := scanMessage(tx.QueryRowContext(ctx,
			"SELECT "+messageCols+" FROM messages WHERE id=?", id))
		if err != nil {
			return err
		}
		res.Claimed = &msg
		return nil
	})
	if err != nil {
		return domain.InsertResult{}, err
	}
	return res, nil
}

// ClaimPending claims one pending message (claimed 0 -> 1).
func (s *Store) ClaimPending(ctx context.Context, id int64) (*domain.Message, error) {
	var out *domain.Message
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		r, err := tx.ExecContext(ctx,
			"UPDATE messages SET claimed=1 WHERE id=? AND printed_at IS NULL AND claimed=0", id)
		if err != nil {
			return err
		}
		if n, _ := r.RowsAffected(); n == 0 {
			return domain.ErrNotFound
		}
		m, err := scanMessage(tx.QueryRowContext(ctx,
			"SELECT "+messageCols+" FROM messages WHERE id=?", id))
		if err != nil {
			return err
		}
		out = &m
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// messageColsNoImage is messageCols with the image BLOB replaced by NULL, so
// bulk reads never pull stored uploads into memory. Callers fetch the image
// lazily with Image().
const messageColsNoImage = `id, name, email, message, COALESCE(ip,''), COALESCE(user_agent,''),
	received_at, browser_time, printed_at, claimed, COALESCE(headers,''), NULL`

// ClaimAllPending claims every pending message and returns exactly the rows it
// claimed, oldest first. (Rows already in flight are not returned again, so a
// drain can never double-print them.) Message.Image is NOT populated (it would
// load up to MAX_IMAGE_BYTES per row); use Image(ctx, id) to fetch it.
func (s *Store) ClaimAllPending(ctx context.Context) ([]domain.Message, error) {
	var out []domain.Message
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			"SELECT "+messageColsNoImage+" FROM messages WHERE printed_at IS NULL AND claimed=0 ORDER BY received_at ASC, id ASC")
		if err != nil {
			return err
		}
		for rows.Next() {
			m, err := scanMessage(rows)
			if err != nil {
				rows.Close()
				return err
			}
			out = append(out, m)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		for i := range out {
			if _, err := tx.ExecContext(ctx,
				"UPDATE messages SET claimed=1 WHERE id=? AND claimed=0", out[i].ID); err != nil {
				return err
			}
			out[i].Claimed = 1
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Skip marks a pending message as skipped (claimed=2).
func (s *Store) Skip(ctx context.Context, id int64) error {
	r, err := s.db.ExecContext(ctx,
		"UPDATE messages SET claimed=2 WHERE id=? AND printed_at IS NULL AND claimed=0", id)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// MarkPrinted records a successful print.
func (s *Store) MarkPrinted(ctx context.Context, id int64) error {
	r, err := s.db.ExecContext(ctx,
		"UPDATE messages SET printed_at=?, claimed=0 WHERE id=?", nowUTC(), id)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// ReleaseClaim returns an in-flight (claimed=1, unprinted) message to pending
// after a failed print. It is a no-op for any other state.
func (s *Store) ReleaseClaim(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE messages SET claimed=0 WHERE id=? AND printed_at IS NULL AND claimed=1", id)
	return err
}

// ReleaseStaleClaims releases every in-flight claim (startup recovery).
func (s *Store) ReleaseStaleClaims(ctx context.Context) (int, error) {
	r, err := s.db.ExecContext(ctx,
		"UPDATE messages SET claimed=0 WHERE printed_at IS NULL AND claimed=1")
	if err != nil {
		return 0, err
	}
	n, _ := r.RowsAffected()
	return int(n), nil
}

const summaryCols = `id, name, email, message, COALESCE(ip,''), received_at, browser_time,
	printed_at, headers, claimed, (image IS NOT NULL) AS has_image`

func scanSummary(r scanner) (domain.MessageSummary, error) {
	var (
		m        domain.MessageSummary
		bt, pa   sql.NullString
		hd       sql.NullString
		hasImage int
	)
	if err := r.Scan(&m.ID, &m.Name, &m.Email, &m.Message, &m.IP, &m.ReceivedAt,
		&bt, &pa, &hd, &m.Claimed, &hasImage); err != nil {
		return m, err
	}
	if bt.Valid {
		m.BrowserTime = &bt.String
	}
	if pa.Valid {
		m.PrintedAt = &pa.String
	}
	if hd.Valid {
		m.Headers = &hd.String
	}
	m.HasImage = hasImage != 0
	return m, nil
}

func clampPage(limit, offset int) (int, int) {
	if limit < 0 {
		limit = 0
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

// List returns messages newest first plus the total row count.
func (s *Store) List(ctx context.Context, limit, offset int) ([]domain.MessageSummary, int, error) {
	return s.list(ctx, "", nil, limit, offset)
}

// Search is List filtered by a case-insensitive substring match on name,
// email or message. LIKE wildcards in q are matched literally. The returned
// total is the number of matching rows. An empty q behaves like List.
func (s *Store) Search(ctx context.Context, q string, limit, offset int) ([]domain.MessageSummary, int, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return s.List(ctx, limit, offset)
	}
	pat := "%" + escapeLike(q) + "%"
	where := ` WHERE name LIKE ? ESCAPE '\' OR email LIKE ? ESCAPE '\' OR message LIKE ? ESCAPE '\'`
	return s.list(ctx, where, []any{pat, pat, pat}, limit, offset)
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func (s *Store) list(ctx context.Context, where string, args []any, limit, offset int) ([]domain.MessageSummary, int, error) {
	limit, offset = clampPage(limit, offset)
	var total int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM messages"+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	qargs := append(append([]any{}, args...), limit, offset)
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+summaryCols+" FROM messages"+where+" ORDER BY received_at DESC, id DESC LIMIT ? OFFSET ?", qargs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []domain.MessageSummary{}
	for rows.Next() {
		m, err := scanSummary(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, m)
	}
	return out, total, rows.Err()
}

// Image returns the stored original upload, or domain.ErrNotFound.
func (s *Store) Image(ctx context.Context, id int64) ([]byte, error) {
	var b []byte
	err := s.db.QueryRowContext(ctx, "SELECT image FROM messages WHERE id=?", id).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && len(b) == 0) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return b, nil
}

// Counts returns queue counters (pending, total, printed in the last hour).
func (s *Store) Counts(ctx context.Context) (domain.QueueCounts, error) {
	var c domain.QueueCounts
	err := s.db.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(CASE WHEN printed_at IS NULL AND claimed=0 THEN 1 ELSE 0 END),0),
		COUNT(*),
		COALESCE(SUM(CASE WHEN printed_at >= datetime('now','-1 hour') THEN 1 ELSE 0 END),0)
		FROM messages`).Scan(&c.Pending, &c.Total, &c.PrintedThisHour)
	return c, err
}

// PrintedTotal returns the number of messages that were actually printed.
func (s *Store) PrintedTotal(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM messages WHERE printed_at IS NOT NULL").Scan(&n)
	return n, err
}

// LoadSettings returns all persisted setting overrides.
func (s *Store) LoadSettings(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT key, value FROM settings")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// SaveSettings upserts the given settings in one transaction.
func (s *Store) SaveSettings(ctx context.Context, kv map[string]string) error {
	if len(kv) == 0 {
		return nil
	}
	now := nowUTC()
	return s.withTx(ctx, func(tx *sql.Tx) error {
		for k, v := range kv {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO settings (key,value,updated_at) VALUES (?,?,?)
				 ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
				k, v, now); err != nil {
				return err
			}
		}
		return nil
	})
}
