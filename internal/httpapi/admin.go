package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/badhrinadhbade/pos-ticker/internal/domain"
)

const (
	notFoundOrClaimed = "message not found or already printed/claimed"
	maxListLimit      = 500
	searchScanCap     = 5000 // newest messages scanned by /messages?q=
)

// statusData is the payload of the "status" SSE event (superset of /queue).
type statusData struct {
	Pending         int                  `json:"pending"`
	Total           int                  `json:"total"`
	PrintedThisHour int                  `json:"printed_this_hour"`
	InFlight        int                  `json:"in_flight"`
	LimitPerHour    int                  `json:"limit_per_hour"`
	Printer         domain.PrinterStatus `json:"printer"`
}

func (s *server) status(ctx context.Context) (statusData, error) {
	c, err := s.d.Store.Counts(ctx)
	if err != nil {
		return statusData{}, err
	}
	return statusData{
		Pending: c.Pending, Total: c.Total, PrintedThisHour: c.PrintedThisHour,
		InFlight:     s.d.Queue.Depth(),
		LimitPerHour: s.d.Settings.Int("prints_per_hour", 10),
		Printer:      s.d.Queue.Status(),
	}, nil
}

// publishStatus pushes a fresh "status" event to SSE clients (best effort).
func (s *server) publishStatus(ctx context.Context) {
	if st, err := s.status(ctx); err == nil {
		s.hub.Publish(domain.Event{Type: "status", Data: st})
	}
}

func (s *server) internalError(w http.ResponseWriter, what string, err error) {
	s.log.Error(what, "err", err)
	writeDetail(w, http.StatusInternalServerError, "internal error")
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	dbOK := true
	if err := s.d.Store.Ping(ctx); err != nil {
		s.log.Error("health check db error", "err", err)
		dbOK = false
	}
	db := "ok"
	if !dbOK {
		db = "failed"
	}
	// /health is unauthenticated: expose only coarse printer state, never the
	// printer address (status.Mode is "network:host:port" / "usb:/dev/...").
	st := s.d.Queue.Status()
	kind := printerKind(st.Mode)
	printer := "unknown"
	if kind != "unknown" {
		printer = "down"
		if st.Up {
			printer = "up"
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           dbOK,
		"queue_depth":  s.d.Queue.Depth(),
		"printer":      printer,
		"printer_mode": kind,
		"db":           db,
	})
}

// printerKind reduces a PrinterStatus.Mode label to "network", "usb" or
// "unknown", dropping any host/port/device path.
func printerKind(mode string) string {
	switch k, _, _ := strings.Cut(mode, ":"); k {
	case "network", "usb":
		return k
	}
	return "unknown"
}

func (s *server) handleQueue(w http.ResponseWriter, r *http.Request) {
	st, err := s.status(r.Context())
	if err != nil {
		s.internalError(w, "queue status", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{
		"pending":           st.Pending,
		"total":             st.Total,
		"printed_this_hour": st.PrintedThisHour,
		"in_flight":         st.InFlight,
		"limit_per_hour":    st.LimitPerHour,
	})
}

func (s *server) handleDrain(w http.ResponseWriter, r *http.Request) {
	msgs, err := s.d.Store.ClaimAllPending(r.Context()) // oldest first
	if err != nil {
		s.internalError(w, "drain", err)
		return
	}
	queued := 0
	for i := range msgs {
		m := msgs[i]
		if err := s.d.Queue.Submit(domain.Job{Kind: domain.JobMessage, MsgID: m.ID, Msg: &m}); err != nil {
			s.log.Error("drain submit", "id", m.ID, "err", err)
			_ = s.d.Store.ReleaseClaim(r.Context(), m.ID)
			continue
		}
		queued++
	}
	s.publishStatus(r.Context())
	writeJSON(w, http.StatusOK, map[string]int{"queued_to_print": queued})
}

type imagePrintReq struct {
	ImageB64 string `json:"image_b64"`
	Mode     string `json:"mode"`
}

func (s *server) handlePrintImage(w http.ResponseWriter, r *http.Request) {
	req := imagePrintReq{Mode: "bitmap"}
	if err := decodeJSON(w, r, s.cfg.bodyLimit(), &req); err != nil {
		if isTooLarge(err) {
			writeDetail(w, http.StatusRequestEntityTooLarge, "request too large")
			return
		}
		writeDetail(w, http.StatusUnprocessableEntity, "invalid JSON body")
		return
	}
	if req.Mode != "bitmap" && req.Mode != "ascii" {
		writeDetail(w, http.StatusBadRequest, "mode must be 'bitmap' or 'ascii'")
		return
	}
	raw, msg := decodeB64Image(req.ImageB64, s.cfg.MaxImageBytes)
	if msg != "" {
		if msg == "invalid image data" {
			msg = "invalid base64"
		}
		writeDetail(w, http.StatusBadRequest, msg)
		return
	}
	bm, err := s.d.Renderer.PrepareImage(raw)
	if err != nil {
		writeDetail(w, http.StatusBadRequest, err.Error())
		return
	}
	job := domain.Job{Kind: domain.JobImage, Raster: bm}
	if req.Mode == "ascii" {
		job = domain.Job{Kind: domain.JobASCII, Text: s.d.Renderer.ASCII(bm, s.cfg.ASCIIColumns)}
	}
	if err := s.d.Queue.Submit(job); err != nil {
		s.log.Error("print image submit", "err", err)
		writeDetail(w, http.StatusServiceUnavailable, "print queue unavailable")
		return
	}
	s.publishStatus(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"queued": true, "mode": req.Mode})
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeDetail(w, http.StatusUnprocessableEntity, "id must be an integer")
		return 0, false
	}
	return id, true
}

func (s *server) handlePrintMessage(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	m, err := s.d.Store.ClaimPending(r.Context(), id)
	if errors.Is(err, domain.ErrNotFound) {
		writeDetail(w, http.StatusNotFound, notFoundOrClaimed)
		return
	}
	if err != nil {
		s.internalError(w, "claim pending", err)
		return
	}
	if err := s.d.Queue.Submit(domain.Job{Kind: domain.JobMessage, MsgID: id, Msg: m}); err != nil {
		s.log.Error("print submit", "id", id, "err", err)
		_ = s.d.Store.ReleaseClaim(r.Context(), id)
		writeDetail(w, http.StatusServiceUnavailable, "print queue unavailable")
		return
	}
	s.publishStatus(r.Context())
	writeJSON(w, http.StatusOK, map[string]bool{"queued": true})
}

func (s *server) handleSkip(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	err := s.d.Store.Skip(r.Context(), id)
	if errors.Is(err, domain.ErrNotFound) {
		writeDetail(w, http.StatusNotFound, notFoundOrClaimed)
		return
	}
	if err != nil {
		s.internalError(w, "skip", err)
		return
	}
	s.publishStatus(r.Context())
	writeJSON(w, http.StatusOK, map[string]bool{"skipped": true})
}

func intParam(r *http.Request, key string, def int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func matchesQuery(m domain.MessageSummary, q string) bool {
	for _, f := range [...]string{m.Name, m.Email, m.Message, m.IP} {
		if strings.Contains(strings.ToLower(f), q) {
			return true
		}
	}
	return false
}

func (s *server) handleMessages(w http.ResponseWriter, r *http.Request) {
	limit := intParam(r, "limit", 50)
	if limit < 1 {
		limit = 50
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}
	offset := intParam(r, "offset", 0)
	if offset < 0 {
		offset = 0
	}
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))

	var (
		msgs  []domain.MessageSummary
		total int
		err   error
	)
	if q == "" {
		msgs, total, err = s.d.Store.List(r.Context(), limit, offset)
	} else if sr, ok := s.d.Store.(interface {
		Search(ctx context.Context, q string, limit, offset int) ([]domain.MessageSummary, int, error)
	}); ok {
		msgs, total, err = sr.Search(r.Context(), q, limit, offset)
	} else {
		// domain.Store.List has no search parameter: scan the newest
		// searchScanCap rows and filter here.
		var all []domain.MessageSummary
		all, _, err = s.d.Store.List(r.Context(), searchScanCap, 0)
		if err == nil {
			matched := all[:0:0]
			for _, m := range all {
				if matchesQuery(m, q) {
					matched = append(matched, m)
				}
			}
			total = len(matched)
			if offset > len(matched) {
				offset = len(matched)
			}
			end := offset + limit
			if end > len(matched) {
				end = len(matched)
			}
			msgs = matched[offset:end]
		}
	}
	if err != nil {
		s.internalError(w, "list messages", err)
		return
	}
	if msgs == nil {
		msgs = []domain.MessageSummary{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": total, "messages": msgs})
}

func (s *server) handleMessageImage(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	blob, err := s.d.Store.Image(r.Context(), id)
	if errors.Is(err, domain.ErrNotFound) || (err == nil && len(blob) == 0) {
		writeDetail(w, http.StatusNotFound, "no image")
		return
	}
	if err != nil {
		s.internalError(w, "message image", err)
		return
	}
	ct := http.DetectContentType(blob)
	if !strings.HasPrefix(ct, "image/") || strings.Contains(ct, "svg") {
		ct = "application/octet-stream"
	}
	h := w.Header()
	h.Set("Content-Type", ct)
	h.Set("Cache-Control", "private, max-age=300")
	h.Set("Content-Disposition", "inline")
	_, _ = w.Write(blob)
}

func (s *server) handleGetSettings(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.d.Settings.All())
}

func (s *server) handlePatchSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Settings map[string]any `json:"settings"`
	}
	if err := decodeJSON(w, r, s.cfg.JSONBodyLimit, &body); err != nil {
		if isTooLarge(err) {
			writeDetail(w, http.StatusRequestEntityTooLarge, "request too large")
			return
		}
		writeDetail(w, http.StatusUnprocessableEntity, "invalid JSON body")
		return
	}
	if body.Settings == nil {
		writeDetail(w, http.StatusUnprocessableEntity, "settings object required")
		return
	}
	kv := make(map[string]string, len(body.Settings))
	for k, v := range body.Settings {
		switch t := v.(type) {
		case string:
			kv[k] = t
		case float64, bool:
			kv[k] = fmt.Sprint(t)
		default:
			writeDetail(w, http.StatusBadRequest, fmt.Sprintf("Setting '%s' must be a string", k))
			return
		}
	}
	all, err := s.d.Settings.Patch(r.Context(), kv)
	if err != nil {
		// Patch validates keys/ints; its error text is user-facing.
		writeDetail(w, http.StatusBadRequest, err.Error())
		return
	}
	s.hub.Publish(domain.Event{Type: "settings", Data: all})
	writeJSON(w, http.StatusOK, all)
}

type previewReq struct {
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	Email    string `json:"email"`
	Message  string `json:"message"`
	ImageB64 string `json:"image_b64"`
	Mode     string `json:"mode"`
}

func (s *server) handlePreview(w http.ResponseWriter, r *http.Request) {
	var req previewReq
	if err := decodeJSON(w, r, s.cfg.bodyLimit(), &req); err != nil {
		if isTooLarge(err) {
			writeDetail(w, http.StatusRequestEntityTooLarge, "request too large")
			return
		}
		writeDetail(w, http.StatusUnprocessableEntity, "invalid JSON body")
		return
	}
	var (
		png []byte
		err error
	)
	switch req.Kind {
	case "message":
		now := s.now().UTC().Format(domain.TimeLayout)
		m := domain.Message{
			Name:       cleanField(req.Name, 64),
			Email:      cleanField(req.Email, 64),
			Body:       truncRunes(strings.ToValidUTF8(req.Message, "�"), 1000),
			ReceivedAt: now,
		}
		png, err = s.d.Renderer.PreviewReceipt(m, s.d.Settings)
		if err != nil {
			s.internalError(w, "preview receipt", err)
			return
		}
	case "image":
		if req.Mode != "" && req.Mode != "bitmap" && req.Mode != "ascii" {
			writeDetail(w, http.StatusBadRequest, "mode must be 'bitmap' or 'ascii'")
			return
		}
		raw, msg := decodeB64Image(req.ImageB64, s.cfg.MaxImageBytes)
		if msg != "" {
			if msg == "invalid image data" {
				msg = "invalid base64"
			}
			writeDetail(w, http.StatusBadRequest, msg)
			return
		}
		bm, perr := s.d.Renderer.PrepareImage(raw)
		if perr != nil {
			writeDetail(w, http.StatusBadRequest, perr.Error())
			return
		}
		// Both modes preview the 1-bit raster that would be printed.
		png, err = s.d.Renderer.PreviewBitmap(bm)
		if err != nil {
			s.internalError(w, "preview bitmap", err)
			return
		}
	default:
		writeDetail(w, http.StatusBadRequest, "kind must be 'message' or 'image'")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "image/png")
	h.Set("Cache-Control", "no-store")
	_, _ = w.Write(png)
}

func (s *server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	s.m.QueueDepth.Set(float64(s.d.Queue.Depth()))
	if s.d.Queue.Status().Up {
		s.m.PrinterUp.Set(1)
	} else {
		s.m.PrinterUp.Set(0)
	}
	if s.d.Stats != nil {
		ok, failed := s.d.Stats.PrintTotals()
		s.m.Prints.Set(float64(ok), "success")
		s.m.Prints.Set(float64(failed), "failed")
	}
	s.m.Handler().ServeHTTP(w, r)
}

// ----------------------------------------------------------------------- SSE

func writeSSE(w http.ResponseWriter, ev domain.Event) error {
	name := strings.NewReplacer("\r", "", "\n", "").Replace(ev.Type)
	data, err := json.Marshal(ev.Data)
	if err != nil {
		return nil // skip unserialisable event, keep the stream alive
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
	return err
}

func (s *server) handleEvents(w http.ResponseWriter, r *http.Request) {
	sub := s.hub.Subscribe()
	if sub == nil {
		writeDetail(w, http.StatusServiceUnavailable, "too many event listeners")
		return
	}
	defer sub.Close()

	// Cookie-authenticated streams die with their session (logout, revocation,
	// expiry). Streams authenticated by X-Admin-Key have no session and just
	// continue. Every stream is also capped in lifetime; EventSource reconnects
	// and is re-authenticated from scratch.
	var sessionDone <-chan struct{}
	var cookieVal string
	if r.Header.Get("X-Admin-Key") == "" {
		c, err := r.Cookie(cookieName)
		if err != nil {
			return
		}
		cookieVal = c.Value
		var ok bool
		if sessionDone, ok = s.auth.sessionDone(cookieVal); !ok {
			return
		}
	}
	life := time.NewTimer(s.cfg.SSEMaxLifetime)
	defer life.Stop()

	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{}) // SSE is exempt from WriteTimeout

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if _, err := fmt.Fprint(w, "retry: 3000\n\n"); err != nil {
		return
	}
	send := func(ev domain.Event) bool {
		if writeSSE(w, ev) != nil {
			return false
		}
		return rc.Flush() == nil
	}
	if st, err := s.status(r.Context()); err == nil {
		if !send(domain.Event{Type: "status", Data: st}) {
			return
		}
	} else if rc.Flush() != nil {
		return
	}

	tick := time.NewTicker(s.cfg.StatusInterval)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-sessionDone: // nil channel (key auth) never fires
			return
		case <-life.C:
			return
		case ev, ok := <-sub.C:
			if !ok { // dropped: too slow
				return
			}
			if !send(ev) {
				return
			}
		case <-tick.C:
			if sessionDone != nil && !s.auth.validSession(cookieVal) {
				return // expired or revoked
			}
			// Periodic status doubles as the keep-alive.
			if st, err := s.status(r.Context()); err == nil {
				if !send(domain.Event{Type: "status", Data: st}) {
					return
				}
			}
		}
	}
}
