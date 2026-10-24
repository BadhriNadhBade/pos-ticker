package notify

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"io"
	"log/slog"
	"math/big"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"strings"
	"sync"
	"testing"
	"time"
)

const testPass = "s3cr3t-app-password"

type fakeSMTP struct {
	ln       net.Listener
	cert     tls.Certificate
	pool     *x509.CertPool
	starttls bool   // plain greeting + STARTTLS instead of implicit TLS
	hang     bool   // accept, then never speak
	wantPass string // expected PLAIN password
	mu       sync.Mutex
	data     string // raw DATA payload (dot-unstuffed)
	from, to string
	authed   bool
	sawPlain bool // AUTH attempted before TLS
	sessions int
}

func newFake(t *testing.T, starttls, hang bool) *fakeSMTP {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{ln: ln, pool: pool, starttls: starttls, hang: hang, wantPass: testPass,
		cert: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}}
	t.Cleanup(func() { ln.Close() })
	go f.serve()
	return f
}

func (f *fakeSMTP) port() int { return f.ln.Addr().(*net.TCPAddr).Port }

func (f *fakeSMTP) serve() {
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.handle(c)
	}
}

func (f *fakeSMTP) handle(c net.Conn) {
	defer c.Close()
	f.mu.Lock()
	f.sessions++
	f.mu.Unlock()
	cfg := &tls.Config{Certificates: []tls.Certificate{f.cert}, MinVersion: tls.VersionTLS12}
	secure := false
	if !f.starttls {
		tc := tls.Server(c, cfg)
		if err := tc.Handshake(); err != nil {
			return
		}
		c = tc
		secure = true
	}
	if f.hang {
		time.Sleep(30 * time.Second)
		return
	}
	r := bufio.NewReader(c)
	say := func(s string) { io.WriteString(c, s+"\r\n") }
	say("220 fake ESMTP")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		up := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(up, "EHLO"):
			io.WriteString(c, "250-fake\r\n")
			if !secure {
				io.WriteString(c, "250-STARTTLS\r\n")
			}
			io.WriteString(c, "250 AUTH PLAIN\r\n")
		case up == "STARTTLS":
			say("220 go ahead")
			tc := tls.Server(c, cfg)
			if err := tc.Handshake(); err != nil {
				return
			}
			c = tc
			r = bufio.NewReader(c)
			secure = true
		case strings.HasPrefix(up, "AUTH PLAIN"):
			if !secure {
				f.mu.Lock()
				f.sawPlain = true
				f.mu.Unlock()
			}
			raw, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(line[len("AUTH PLAIN"):]))
			parts := bytes.Split(raw, []byte{0})
			if len(parts) == 3 && string(parts[2]) == f.wantPass {
				f.mu.Lock()
				f.authed = true
				f.mu.Unlock()
				say("235 ok")
			} else {
				// A hostile server that echoes what it received.
				say("535 5.7.8 bad credentials for " + string(raw))
			}
		case strings.HasPrefix(up, "MAIL FROM:"):
			f.mu.Lock()
			f.from = line[len("MAIL FROM:"):]
			f.mu.Unlock()
			say("250 ok")
		case strings.HasPrefix(up, "RCPT TO:"):
			f.mu.Lock()
			f.to = line[len("RCPT TO:"):]
			f.mu.Unlock()
			say("250 ok")
		case up == "DATA":
			say("354 go")
			var sb strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				sb.WriteString(strings.TrimPrefix(l, "."))
			}
			f.mu.Lock()
			f.data = sb.String()
			f.mu.Unlock()
			say("250 queued")
		case up == "QUIT":
			say("221 bye")
			return
		default:
			say("250 ok")
		}
	}
}

func (f *fakeSMTP) snapshot() (data, from, to string, authed bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.data, f.from, f.to, f.authed
}

func (f *fakeSMTP) mailer(t *testing.T, enabled bool, logBuf *bytes.Buffer) *Mailer {
	t.Helper()
	log := slog.New(slog.NewTextHandler(logBuf, nil))
	m := New(Config{SMTPHost: "127.0.0.1", SMTPPort: f.port(), User: "bot@example.com", Pass: testPass, To: "owner@example.com"},
		func() bool { return enabled }, log)
	m.rootCAs = f.pool
	imp := !f.starttls
	m.implicit = &imp
	return m
}

func parse(t *testing.T, raw string) (*mail.Message, string) {
	t.Helper()
	msg, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("parse: %v\n%s", err, raw)
	}
	if got := msg.Header.Get("Content-Transfer-Encoding"); got != "quoted-printable" {
		t.Fatalf("cte = %q", got)
	}
	body, err := io.ReadAll(quotedprintable.NewReader(msg.Body))
	if err != nil {
		t.Fatal(err)
	}
	return msg, strings.ReplaceAll(string(body), "\r\n", "\n")
}

func TestSuccessImplicitTLS(t *testing.T) {
	f := newFake(t, false, false)
	var logs bytes.Buffer
	m := f.mailer(t, true, &logs)
	m.Notify("Ada", "ada@example.com", "hello\nworld", "1.2.3.4", "Mon May 12 2:41 PM")

	data, from, to, authed := f.snapshot()
	if !authed {
		t.Fatalf("not authenticated; logs: %s", logs.String())
	}
	if !strings.Contains(from, "bot@example.com") || !strings.Contains(to, "owner@example.com") {
		t.Fatalf("envelope from=%q to=%q", from, to)
	}
	msg, body := parse(t, data)
	if got := msg.Header.Get("Subject"); got != "New message from Ada" {
		t.Fatalf("subject %q", got)
	}
	if msg.Header.Get("From") != "bot@example.com" || msg.Header.Get("To") != "owner@example.com" {
		t.Fatalf("headers: %v", msg.Header)
	}
	if msg.Header.Get("MIME-Version") != "1.0" || msg.Header.Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Fatalf("mime headers: %v", msg.Header)
	}
	want := "From:  Ada <ada@example.com>\nTime:  Mon May 12 2:41 PM\nIP:    1.2.3.4\n\nhello\nworld"
	if strings.TrimRight(body, "\n") != want {
		t.Fatalf("body mismatch:\n%q\nwant\n%q", body, want)
	}
	if logs.Len() != 0 {
		t.Fatalf("unexpected logs: %s", logs.String())
	}
}

func TestSuccessSTARTTLS(t *testing.T) {
	f := newFake(t, true, false)
	var logs bytes.Buffer
	m := f.mailer(t, true, &logs)
	m.Notify("Bob", "", "hi", "::1", "t")
	data, _, _, authed := f.snapshot()
	if !authed {
		t.Fatalf("not authenticated; logs: %s", logs.String())
	}
	if f.sawPlain {
		t.Fatal("AUTH sent before TLS")
	}
	_, body := parse(t, data)
	if !strings.HasPrefix(body, "From:  Bob <>\n") {
		t.Fatalf("body %q", body)
	}
}

func TestNonASCIISubjectAndBody(t *testing.T) {
	f := newFake(t, false, false)
	var logs bytes.Buffer
	m := f.mailer(t, true, &logs)
	m.Notify("Zoë 日本", "z@example.com", "héllo 🎉 "+strings.Repeat("x", 200), "1.1.1.1", "t")
	data, _, _, _ := f.snapshot()
	msg, body := parse(t, data)
	raw := msg.Header.Get("Subject")
	if !strings.Contains(raw, "=?utf-8?q?") {
		t.Fatalf("subject not RFC 2047 encoded: %q", raw)
	}
	dec, err := new(mime.WordDecoder).DecodeHeader(raw)
	if err != nil || dec != "New message from Zoë 日本" {
		t.Fatalf("decoded subject %q err %v", dec, err)
	}
	if !strings.Contains(body, "héllo 🎉") {
		t.Fatalf("body %q", body)
	}
	for _, l := range strings.Split(data, "\r\n") {
		if len(l) > 998 {
			t.Fatal("overlong line")
		}
	}
}

func TestHeaderInjectionNeutralised(t *testing.T) {
	f := newFake(t, false, false)
	var logs bytes.Buffer
	m := f.mailer(t, true, &logs)
	evil := "Eve\r\nBcc: attacker@evil.com\r\nSubject: pwned X-Y: z\x00"
	m.Notify(evil, "e@x.com>\r\nBcc: a@evil.com", "msg", "1.1.1.1\r\nX: y", "t\r\nX: y")
	data, _, _, authed := f.snapshot()
	if !authed {
		t.Fatalf("not sent: %s", logs.String())
	}
	msg, _ := parse(t, data)
	if len(msg.Header["Bcc"]) != 0 || len(msg.Header["X-Y"]) != 0 || len(msg.Header["X"]) != 0 {
		t.Fatalf("injected header present: %v", msg.Header)
	}
	if len(msg.Header["Subject"]) != 1 {
		t.Fatalf("subject count %d: %v", len(msg.Header["Subject"]), msg.Header["Subject"])
	}
	dec, _ := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
	if strings.ContainsAny(dec, "\r\n\x00 ") || !strings.HasPrefix(dec, "New message from Eve") {
		t.Fatalf("subject %q", dec)
	}
	// Header block (before first blank line) must hold only the known headers.
	hdr, _, _ := strings.Cut(data, "\r\n\r\n")
	for _, l := range strings.Split(hdr, "\r\n") {
		k, _, _ := strings.Cut(l, ":")
		switch k {
		case "From", "To", "Subject", "Date", "MIME-Version", "Content-Type", "Content-Transfer-Encoding":
		default:
			if !strings.HasPrefix(l, " ") { // folded continuation of Subject
				t.Fatalf("unexpected header line %q", l)
			}
		}
	}
}

func TestInvalidConfiguredAddressRefused(t *testing.T) {
	f := newFake(t, false, false)
	var logs bytes.Buffer
	m := f.mailer(t, true, &logs)
	m.cfg.To = "owner@example.com\r\nBcc: a@evil.com"
	m.Notify("a", "b", "c", "d", "e")
	if data, _, _, _ := f.snapshot(); data != "" {
		t.Fatalf("sent anyway: %s", data)
	}
	// CR/LF stripped leaves "owner@example.comBcc: a@evil.com", which is not a valid address.
	if !strings.Contains(logs.String(), "email notification failed") {
		t.Fatalf("expected error log, got %q", logs.String())
	}
}

func TestSizeCaps(t *testing.T) {
	f := newFake(t, false, false)
	var logs bytes.Buffer
	m := f.mailer(t, true, &logs)
	m.Notify(strings.Repeat("n", 500), "e@x.com", strings.Repeat("m", 10000), "ip", "ts")
	data, _, _, _ := f.snapshot()
	_, body := parse(t, data)
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	if got := len(lines[0]); got != len("From:  ")+64+len(" <e@x.com>") {
		t.Fatalf("name not capped: %d", got)
	}
	if got := len(strings.TrimRight(lines[len(lines)-1], "\n")); got != 4000 {
		t.Fatalf("message not capped: %d", got)
	}
}

func TestNoOps(t *testing.T) {
	f := newFake(t, false, false)
	var logs bytes.Buffer
	cases := map[string]*Mailer{}
	cases["disabled"] = f.mailer(t, false, &logs)
	for _, k := range []string{"user", "pass", "to"} {
		m := f.mailer(t, true, &logs)
		switch k {
		case "user":
			m.cfg.User = ""
		case "pass":
			m.cfg.Pass = ""
		case "to":
			m.cfg.To = ""
		}
		cases["missing-"+k] = m
	}
	nilEnabled := f.mailer(t, true, &logs)
	nilEnabled.enabled = nil
	cases["nil-enabled"] = nilEnabled
	for name, m := range cases {
		m.Notify("a", "b", "c", "d", "e")
		f.mu.Lock()
		s := f.sessions
		f.mu.Unlock()
		if s != 0 {
			t.Fatalf("%s: connected to server", name)
		}
	}
	if logs.Len() != 0 {
		t.Fatalf("no-op logged: %s", logs.String())
	}
	var nilM *Mailer
	nilM.Notify("a", "b", "c", "d", "e") // must not panic
}

func TestDefaults(t *testing.T) {
	m := New(Config{}, nil, nil)
	if m.cfg.SMTPHost != "smtp.gmail.com" || m.cfg.SMTPPort != 465 || !m.useImplicitTLS() {
		t.Fatalf("defaults: %+v", m.cfg)
	}
	m = New(Config{SMTPPort: 587}, nil, nil)
	if m.useImplicitTLS() {
		t.Fatal("587 must use STARTTLS")
	}
}

func TestTimeout(t *testing.T) {
	f := newFake(t, false, true)
	var logs bytes.Buffer
	m := f.mailer(t, true, &logs)
	m.timeout = 300 * time.Millisecond
	start := time.Now()
	m.Notify("a", "b", "c", "d", "e")
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("took %v", el)
	}
	if !strings.Contains(logs.String(), "email notification failed") {
		t.Fatalf("expected failure log: %q", logs.String())
	}
}

func TestUntrustedCertRejected(t *testing.T) {
	f := newFake(t, false, false)
	var logs bytes.Buffer
	m := f.mailer(t, true, &logs)
	m.rootCAs = x509.NewCertPool() // does not trust the fake's cert
	m.Notify("a", "b", "c", "d", "e")
	if _, _, _, authed := f.snapshot(); authed {
		t.Fatal("authenticated despite untrusted cert")
	}
	if !strings.Contains(logs.String(), "tls handshake") {
		t.Fatalf("logs: %q", logs.String())
	}
}

func TestBadPasswordLogsWithoutLeaking(t *testing.T) {
	f := newFake(t, false, false)
	f.wantPass = "different"
	var logs bytes.Buffer
	m := f.mailer(t, true, &logs)
	m.Notify("a", "b", "c", "d", "e")
	out := logs.String()
	if !strings.Contains(out, "email notification failed") || !strings.Contains(out, "535") {
		t.Fatalf("expected auth failure log, got %q", out)
	}
	if strings.Contains(out, testPass) {
		t.Fatalf("password leaked in log: %q", out)
	}
	if data, _, _, _ := f.snapshot(); data != "" {
		t.Fatal("message sent after failed auth")
	}
}
