// Package notify sends the optional "new message" e-mail notification.
//
// It is a port of _send_email from printer_server.py with extra hardening:
// header-injection safe, TLS 1.2+ with certificate verification, PLAIN auth
// only over TLS, bounded field sizes and a hard total timeout.
package notify

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	defaultHost = "smtp.gmail.com"
	defaultPort = 465

	maxName    = 64
	maxEmail   = 254
	maxMessage = 4000
	maxMeta    = 64 // ip, timestamp

	sendTimeout = 10 * time.Second

	// defaultMaxPerHour bounds e-mails per rolling hour so a flood of
	// submissions cannot turn into a flood of notifications (or a locked-out
	// SMTP account).
	defaultMaxPerHour = 20
	rateWindow        = time.Hour
)

// Config holds the SMTP settings (SMTP_HOST/SMTP_PORT/SMTP_USER/SMTP_PASS/NOTIFY_EMAIL).
type Config struct {
	SMTPHost string // default smtp.gmail.com
	SMTPPort int    // default 465 (implicit TLS); anything else uses STARTTLS
	User     string
	Pass     string
	To       string

	// MaxPerHour caps notifications per rolling hour (0 = default of 20,
	// negative = unlimited). Excess notifications are dropped.
	MaxPerHour int
}

// Mailer sends notification e-mails. The zero value is not usable; use New.
type Mailer struct {
	cfg     Config
	enabled func() bool
	log     *slog.Logger

	// Test hooks (unexported on purpose).
	rootCAs  *x509.CertPool // extra trust anchors for the server certificate
	timeout  time.Duration  // total send timeout, default 10s
	implicit *bool          // override the port-based TLS mode decision
	now      func() time.Time

	// Rolling-hour limiter state. sent holds at most limit timestamps.
	mu       sync.Mutex
	sent     []time.Time
	dropped  int
	lastWarn time.Time
}

// New builds a Mailer. enabled is consulted on every Notify call (it reads the
// live "email_notifications" setting). A nil log uses slog.Default().
func New(cfg Config, enabled func() bool, log *slog.Logger) *Mailer {
	if cfg.SMTPHost == "" {
		cfg.SMTPHost = defaultHost
	}
	if cfg.SMTPPort == 0 {
		cfg.SMTPPort = defaultPort
	}
	if log == nil {
		log = slog.Default()
	}
	if cfg.MaxPerHour == 0 {
		cfg.MaxPerHour = defaultMaxPerHour
	}
	return &Mailer{cfg: cfg, enabled: enabled, log: log, timeout: sendTimeout, now: time.Now}
}

// allow reports whether another e-mail may be sent now under the rolling-hour
// cap, and records it if so. It is safe for concurrent use and keeps at most
// MaxPerHour timestamps. When a notification is dropped, one warning is logged
// per hour (with the number dropped since the last warning).
func (m *Mailer) allow() bool {
	limit := m.cfg.MaxPerHour
	if limit < 0 {
		return true
	}
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	cut := now.Add(-rateWindow)
	i := 0
	for i < len(m.sent) && !m.sent[i].After(cut) {
		i++
	}
	m.sent = m.sent[i:]
	if len(m.sent) < limit {
		m.sent = append(m.sent, now)
		return true
	}
	m.dropped++
	if m.lastWarn.IsZero() || now.Sub(m.lastWarn) >= rateWindow {
		m.log.Warn("email notification rate limit reached; dropping notifications",
			"max_per_hour", limit, "dropped", m.dropped)
		m.lastWarn = now
		m.dropped = 0
	}
	return false
}

// Notify sends a notification. It is a silent no-op unless notifications are
// enabled and user, password and recipient are configured. It blocks until the
// mail is sent or the 10 s budget is spent; callers run it in a goroutine.
// Failures are logged, never returned or panicked.
func (m *Mailer) Notify(name, email, message, ip, timestamp string) {
	if m == nil || m.enabled == nil || !m.enabled() {
		return
	}
	if m.cfg.User == "" || m.cfg.Pass == "" || m.cfg.To == "" {
		return
	}
	if !m.allow() {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			m.log.Error("email notification panicked")
		}
	}()
	if err := m.send(name, email, message, ip, timestamp); err != nil {
		m.log.Error("email notification failed", "err", m.redact(err.Error()))
	}
}

func (m *Mailer) redact(s string) string {
	if m.cfg.Pass != "" {
		s = strings.ReplaceAll(s, m.cfg.Pass, "***")
	}
	return s
}

func (m *Mailer) useImplicitTLS() bool {
	if m.implicit != nil {
		return *m.implicit
	}
	return m.cfg.SMTPPort == 465
}

func (m *Mailer) send(name, email, message, ip, timestamp string) error {
	from, err := parseAddr(m.cfg.User)
	if err != nil {
		return fmt.Errorf("invalid SMTP_USER address: %w", err)
	}
	to, err := parseAddr(m.cfg.To)
	if err != nil {
		return fmt.Errorf("invalid NOTIFY_EMAIL address: %w", err)
	}
	msg := buildMessage(from, to, name, email, message, ip, timestamp, time.Now())

	timeout := m.timeout
	if timeout <= 0 {
		timeout = sendTimeout
	}
	deadline := time.Now().Add(timeout)

	host := m.cfg.SMTPHost
	addr := net.JoinHostPort(host, strconv.Itoa(m.cfg.SMTPPort))
	tlsCfg := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12, RootCAs: m.rootCAs}

	conn, err := (&net.Dialer{Deadline: deadline}).Dial("tcp", addr)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	// Single deadline for the entire exchange (handshake, auth, data).
	if err := conn.SetDeadline(deadline); err != nil {
		return err
	}

	if m.useImplicitTLS() {
		tc := tls.Client(conn, tlsCfg)
		if err := tc.Handshake(); err != nil {
			return fmt.Errorf("tls handshake: %w", err)
		}
		conn = tc
	}

	c, err := smtp.NewClient(conn, host)
	if err != nil {
		return fmt.Errorf("smtp greeting: %w", err)
	}
	defer c.Close()

	if !m.useImplicitTLS() {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return errors.New("server does not offer STARTTLS; refusing to send credentials in clear")
		}
		if err := c.StartTLS(tlsCfg); err != nil {
			return fmt.Errorf("starttls: %w", err)
		}
	}

	// net/smtp's PlainAuth additionally refuses to run on a non-TLS connection
	// (except localhost); by here the connection is always TLS.
	if err := c.Auth(smtp.PlainAuth("", m.cfg.User, m.cfg.Pass, host)); err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	if err := c.Mail(from); err != nil {
		return fmt.Errorf("mail from: %w", err)
	}
	if err := c.Rcpt(to); err != nil {
		return fmt.Errorf("rcpt to: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("data: %w", err)
	}
	if _, err := io.WriteString(w, msg); err != nil {
		return fmt.Errorf("write body: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("end data: %w", err)
	}
	_ = c.Quit()
	return nil
}

// parseAddr validates a configured address and returns the bare addr-spec.
func parseAddr(s string) (string, error) {
	a, err := mail.ParseAddress(cleanLine(s, maxEmail))
	if err != nil {
		return "", err
	}
	return a.Address, nil
}

// buildMessage renders the full RFC 5322 message with CRLF line endings. All
// attacker-influenced values are stripped of control characters before they
// reach a header; the body is quoted-printable so it cannot smuggle anything.
func buildMessage(from, to, name, email, message, ip, timestamp string, now time.Time) string {
	name = cleanLine(name, maxName)
	email = cleanLine(email, maxEmail)
	ip = cleanLine(ip, maxMeta)
	timestamp = cleanLine(timestamp, maxMeta)
	message = cleanText(message, maxMessage)

	body := fmt.Sprintf("From:  %s <%s>\nTime:  %s\nIP:    %s\n\n%s", name, email, timestamp, ip, message)

	var b strings.Builder
	b.WriteString("From: " + cleanLine(from, maxEmail) + "\r\n")
	b.WriteString("To: " + cleanLine(to, maxEmail) + "\r\n")
	b.WriteString("Subject: " + mime.QEncoding.Encode("utf-8", "New message from "+name) + "\r\n")
	b.WriteString("Date: " + now.Format(time.RFC1123Z) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("Content-Transfer-Encoding: quoted-printable\r\n")
	b.WriteString("\r\n")
	qw := quotedprintable.NewWriter(&b)
	_, _ = qw.Write([]byte(body))
	_ = qw.Close()
	b.WriteString("\r\n")
	return b.String()
}

// cleanLine removes every control character (CR, LF, NUL, C0, DEL, C1, U+2028/9)
// and truncates to max runes. Result is always a single line of valid UTF-8.
func cleanLine(s string, max int) string {
	return clean(s, max, false)
}

// cleanText is like cleanLine but keeps newlines (CRLF/CR normalised to LF) and tabs.
func cleanText(s string, max int) string {
	return clean(s, max, true)
}

func clean(s string, max int, keepNewlines bool) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
	}
	if keepNewlines {
		s = strings.ReplaceAll(s, "\r\n", "\n")
		s = strings.ReplaceAll(s, "\r", "\n")
	}
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n >= max {
			break
		}
		switch {
		case keepNewlines && (r == '\n' || r == '\t'):
		case unicode.IsControl(r), r == ' ', r == ' ':
			continue
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}
