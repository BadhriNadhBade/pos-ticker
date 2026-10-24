package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/badhrinadhbade/pos-ticker/internal/domain"
	"github.com/badhrinadhbade/pos-ticker/internal/metrics"
)

// cors adds CORS headers for the public contact endpoints only. The allowed
// origin is the runtime setting `allowed_origin`; it is never "*".
func (s *server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p := r.URL.Path; p == "/contact" || p == "/contact/count" {
			h := w.Header()
			if origin := strings.TrimSpace(s.d.Settings.Get("allowed_origin")); origin != "" && origin != "*" {
				h.Set("Access-Control-Allow-Origin", origin)
				h.Add("Vary", "Origin")
			}
			h.Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
			h.Set("Access-Control-Allow-Headers", "Content-Type")
			h.Set("Access-Control-Max-Age", "600")
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) handlePreflight(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleContactCount(w http.ResponseWriter, r *http.Request) {
	// Cached for a few seconds: this is public and unauthenticated, so
	// without the cache every hit would be a COUNT(*). (Cache-Control stays
	// no-store: a shared cache keyed without Origin could replay a CORS header
	// to the wrong site, and the server-side cache already removes the cost.)
	n, err := s.countCached.get(s.now(), func() (int, error) {
		return s.d.Store.PrintedTotal(r.Context())
	})
	if err != nil {
		s.log.Error("contact count", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"printed": n})
}

// contactBody is the only shape /contact decodes into. Unknown keys are
// skipped by the JSON decoder without being materialised, so a body full of
// junk keys/arrays cannot balloon memory (never decode into map[string]any).
// A field of the wrong JSON type is left empty (like the Python original).
type contactBody struct {
	Name        string `json:"name"`
	Email       string `json:"email"`
	Message     string `json:"message"`
	BrowserTime string `json:"browser_time"`
	ImageB64    string `json:"image_b64"`
}

// decodeB64Image mirrors base64.b64decode(s, validate=True): strict standard
// alphabet with padding, no embedded whitespace/newlines (Go's decoder would
// silently skip \r and \n).
func decodeB64Image(s string, maxBytes int) ([]byte, string) {
	if len(s) > (maxBytes+2)/3*4 { // exact base64 length of maxBytes; cheap pre-check
		return nil, "image too large"
	}
	if strings.ContainsAny(s, "\r\n") {
		return nil, "invalid image data"
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, "invalid image data"
	}
	return raw, ""
}

// originAllowed enforces the `allowed_origin` setting: when the browser sent
// an Origin and a (non-wildcard) origin is configured, they must match.
// Requests without Origin (curl, server-side) are not browser-driven CSRF and
// pass; the per-IP limits cover them.
func (s *server) originAllowed(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	want := strings.TrimSpace(s.d.Settings.Get("allowed_origin"))
	if want == "" || want == "*" {
		return true
	}
	return strings.EqualFold(origin, want)
}

// isJSONContentType reports whether the request declares application/json.
// Requiring it makes every cross-origin browser POST non-"simple", so it must
// pass a CORS preflight first.
func isJSONContentType(r *http.Request) bool {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && mt == "application/json"
}

// prepareContactImage decodes and validates an untrusted image under the
// global concurrency cap. It returns (raw, 0, "") on success, otherwise the
// HTTP status and message to send.
func (s *server) prepareContactImage(b64 string) ([]byte, int, string) {
	select {
	case s.imageSem <- struct{}{}:
		defer func() { <-s.imageSem }()
	default:
		return nil, http.StatusServiceUnavailable, "busy, try again"
	}
	raw, msg := decodeB64Image(b64, s.cfg.MaxImageBytes)
	if msg != "" {
		return nil, http.StatusBadRequest, msg
	}
	if _, err := s.d.Renderer.PrepareImage(raw); err != nil {
		return nil, http.StatusBadRequest, err.Error()
	}
	return raw, 0, ""
}

func (s *server) handleContact(w http.ResponseWriter, r *http.Request) {
	if !s.originAllowed(r) {
		writeErr(w, http.StatusForbidden, "origin not allowed")
		return
	}
	if !isJSONContentType(r) {
		writeErr(w, http.StatusUnsupportedMediaType, "content type must be application/json")
		return
	}

	// Cheap, read-only per-IP check BEFORE reading or decoding anything. The
	// counter is in-memory; Store.InsertMessage stays the authoritative check.
	ip := s.clientIP(r)
	now := s.now()
	if s.ipHits.count(ip, now) >= s.d.Settings.Int("max_per_ip_hour", 3) {
		s.m.Message(metrics.MsgRateLimitedIP)
		writeErr(w, http.StatusTooManyRequests, "too many requests")
		return
	}

	var body contactBody
	if err := decodeJSON(w, r, s.cfg.bodyLimit(), &body); err != nil {
		if isTooLarge(err) {
			writeErr(w, http.StatusRequestEntityTooLarge, "request too large")
			return
		}
		var te *json.UnmarshalTypeError
		if !errors.As(err, &te) {
			body = contactBody{} // Python: unparsable body => {} => "missing fields"
		} // else: wrongly-typed fields stay empty, the rest was decoded
	}

	in := contactInput{
		Name:        cleanField(body.Name, 64),
		Email:       cleanField(body.Email, 64),
		Message:     cleanField(body.Message, 1000),
		BrowserTime: cleanBrowserTime(body.BrowserTime),
		ImageB64:    body.ImageB64,
	}
	if msg := validateContact(in, s.cfg.MaxMessageChars); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}

	// Optional image: validated by the full prepare pipeline, but the ORIGINAL
	// bytes are what get stored (raster is regenerated at print time).
	var image []byte
	if in.ImageB64 != "" {
		if s.imgTries.count(ip, now) >= imageAttemptsMin {
			s.m.Message(metrics.MsgRateLimitedIP)
			writeErr(w, http.StatusTooManyRequests, "too many requests")
			return
		}
		s.imgTries.add(ip, now)
		if len(in.ImageB64) > s.cfg.maxBase64Len() {
			writeErr(w, http.StatusBadRequest, "image too large")
			return
		}
		raw, status, msg := s.prepareContactImage(in.ImageB64)
		if status != 0 {
			if status == http.StatusServiceUnavailable {
				w.Header().Set("Retry-After", "2")
			}
			writeErr(w, status, msg)
			return
		}
		image = raw
	}

	ua := truncRunes(strings.ToValidUTF8(r.Header.Get("User-Agent"), "�"), 120)
	if ua == "" {
		ua = "unknown"
	}
	nm := domain.NewMessage{
		Name: in.Name, Email: in.Email, Body: in.Message,
		IP: ip, UserAgent: ua, HeadersJSON: headersJSON(r),
		BrowserTime: in.BrowserTime, Image: image,
	}

	// The DB work must finish even if the visitor disconnects mid-request.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 20*time.Second)
	defer cancel()

	res, err := s.d.Store.InsertMessage(ctx, nm,
		s.d.Settings.Int("max_per_ip_hour", 3), s.d.Settings.Int("prints_per_hour", 10))
	if err != nil && !errors.Is(err, domain.ErrRateLimit) {
		s.log.Error("insert message", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err != nil || res.RateLimitedIP {
		s.m.Message(metrics.MsgRateLimitedIP)
		writeErr(w, http.StatusTooManyRequests, "too many requests")
		return
	}
	s.ipHits.add(ip, now)

	now = now.UTC()
	s.notifyMail(in, ip, now)

	summary := domain.MessageSummary{
		ID: res.ID, Name: in.Name, Email: in.Email, Message: in.Message, IP: ip,
		ReceivedAt: now.Format(domain.TimeLayout), HasImage: len(image) > 0,
	}
	if in.BrowserTime != "" {
		bt := in.BrowserTime
		summary.BrowserTime = &bt
	}
	if hj := nm.HeadersJSON; hj != "" {
		summary.Headers = &hj
	}

	// "queued" mirrors the Python API: true => stored but left pending (global
	// hourly cap), false => handed to the printer queue.
	queued := true
	if res.Claimed == nil {
		s.m.Message(metrics.MsgRateLimitedGlobal)
		s.log.Info("hourly print limit hit, message left pending", "id", res.ID)
	} else {
		s.m.Message(metrics.MsgAccepted)
		summary.Claimed = 1
		summary.ReceivedAt = res.Claimed.ReceivedAt
		if err := s.d.Queue.Submit(domain.Job{Kind: domain.JobMessage, MsgID: res.ID, Msg: res.Claimed}); err != nil {
			s.log.Error("queue submit failed; releasing claim", "id", res.ID, "err", err)
			if rerr := s.d.Store.ReleaseClaim(ctx, res.ID); rerr != nil {
				s.log.Error("release claim", "id", res.ID, "err", rerr)
			}
			summary.Claimed = 0
		} else {
			queued = false
		}
	}
	if summary.ReceivedAt == "" {
		summary.ReceivedAt = now.Format(domain.TimeLayout)
	}
	s.hub.Publish(domain.Event{Type: "message", Data: summary})
	s.publishStatus(ctx)

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true, "queued": queued})
}

// notifyMail sends the e-mail notification asynchronously and never lets a
// failing or panicking mailer affect the request.
func (s *server) notifyMail(in contactInput, ip string, now time.Time) {
	m := s.d.Mailer
	if m == nil || !strings.EqualFold(s.d.Settings.Get("email_notifications"), "true") {
		return
	}
	ts := in.BrowserTime
	if ts == "" {
		ts = formatDisplayTime(now, s.cfg.DisplayTZ)
	}
	name, email, msg := in.Name, in.Email, in.Message
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("mailer panic", "panic", rec)
			}
		}()
		m.Notify(name, email, msg, ip, ts)
	}()
}

// formatDisplayTime renders "Mon  May 12  2:41 PM" like _utc_to_pacific.
func formatDisplayTime(t time.Time, loc *time.Location) string {
	return t.In(loc).Format("Mon  Jan 2  3:04 PM")
}

var redactedHeaders = map[string]bool{"cookie": true, "authorization": true, "x-admin-key": true}

// headersJSON serialises request headers (lower-case names, like Starlette)
// with credentials redacted before they can reach the database.
func headersJSON(r *http.Request) string {
	m := make(map[string]string, len(r.Header)+1)
	if r.Host != "" {
		m["host"] = r.Host
	}
	for k, vs := range r.Header {
		lk := strings.ToLower(k)
		if redactedHeaders[lk] {
			m[lk] = "[redacted]"
			continue
		}
		m[lk] = strings.ToValidUTF8(strings.Join(vs, ", "), "�")
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "{}"
	}
	return string(b)
}
