package main

import (
	"compress/gzip"
	"crypto/subtle"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "time/tzdata"
)

//go:embed API.md
var apiDoc string

type Server struct {
	cfg      *Config
	st       *Store
	notify   *notifier
	enrollRL *rateLimiter
	ipc      *ipCache
	loc      *time.Location
}

func NewServer(cfg *Config, st *Store) *Server {
	loc, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		loc = time.UTC
	}
	return &Server{
		cfg:      cfg,
		st:       st,
		notify:   newNotifier(),
		enrollRL: newRateLimiter(10, time.Minute),
		ipc:      newIPCache(st, cfg.IPInfoURL),
		loc:      loc,
	}
}

type Device struct {
	ID             string
	TokenHash      string
	Name           string
	Status         string
	Profile        string
	Overrides      string
	Note           string
	EnrolledAt     int64
	LastSeen       int64
	LastIP         string
	Info           string
	AppliedVersion string
	ApplyResult    string
	LiveUntil      int64
	LogLevel       string
	LogLevelUntil  int64
	LastState      string
	LastLogTs      float64
}

const deviceCols = `id, token_hash, name, status, profile, overrides, note, enrolled_at, last_seen, last_ip, info,
 applied_version, apply_result, live_until, loglevel, loglevel_until, last_state, last_log_ts`

func scanDevice(r interface{ Scan(...any) error }) (*Device, error) {
	d := &Device{}
	err := r.Scan(&d.ID, &d.TokenHash, &d.Name, &d.Status, &d.Profile, &d.Overrides, &d.Note, &d.EnrolledAt,
		&d.LastSeen, &d.LastIP, &d.Info, &d.AppliedVersion, &d.ApplyResult, &d.LiveUntil, &d.LogLevel,
		&d.LogLevelUntil, &d.LastState, &d.LastLogTs)
	return d, err
}

func (s *Server) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })

	// device API
	m.HandleFunc("POST /v1/enroll", s.handleEnroll)
	m.HandleFunc("POST /v1/sync", s.device(s.handleSync))
	m.HandleFunc("POST /v1/logs", s.device(s.handleLogs))
	m.HandleFunc("GET /v1/poll", s.device(s.handlePoll))
	m.HandleFunc("POST /v1/results", s.device(s.handleResults))

	// admin API
	a := func(p string, h http.HandlerFunc) { m.HandleFunc(p, s.admin(h)) }
	a("GET /v1/admin/help", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		io.WriteString(w, apiDoc)
	})
	a("GET /v1/admin/devices", s.adminDevices)
	a("GET /v1/admin/devices/{id}", s.adminDevice)
	a("PATCH /v1/admin/devices/{id}", s.adminDevicePatch)
	a("DELETE /v1/admin/devices/{id}", s.adminDeviceDelete)
	a("GET /v1/admin/devices/{id}/config", s.adminDeviceConfig)
	a("GET /v1/admin/devices/{id}/overrides", s.adminOverridesGet)
	a("PUT /v1/admin/devices/{id}/overrides", s.adminOverridesPut)
	a("PATCH /v1/admin/devices/{id}/overrides", s.adminOverridesPatch)
	a("GET /v1/admin/devices/{id}/state", s.adminDeviceState)
	a("GET /v1/admin/devices/{id}/states", s.adminDeviceStates)
	a("POST /v1/admin/devices/{id}/commands", s.adminCommandCreate)
	a("POST /v1/admin/devices/{id}/live", s.adminLive)
	a("POST /v1/admin/devices/{id}/loglevel", s.adminLogLevel)
	a("GET /v1/admin/commands", s.adminCommands)
	a("GET /v1/admin/commands/{cid}", s.adminCommand)
	a("GET /v1/admin/profiles", s.adminProfiles)
	a("GET /v1/admin/profiles/{name}", s.adminProfileGet)
	a("PUT /v1/admin/profiles/{name}", s.adminProfilePut)
	a("PATCH /v1/admin/profiles/{name}", s.adminProfilePatch)
	a("DELETE /v1/admin/profiles/{name}", s.adminProfileDelete)
	a("GET /v1/admin/logs", s.adminLogs)
	a("GET /v1/admin/events", s.adminEvents)
	a("GET /v1/admin/measurements", s.adminMeasurements)
	a("GET /v1/admin/stats", s.adminStats)
	a("GET /v1/admin/dns", s.adminDNS)
	a("GET /v1/admin/timeline", s.adminTimeline)
	a("GET /v1/admin/audit", s.adminAudit)
	a("GET /v1/admin/ipinfo/{ip}", s.adminIPInfo)
	return m
}

// ---------- helpers ----------

type httpError struct {
	code int
	msg  string
}

func (e *httpError) Error() string { return e.msg }

func errf(code int, f string, a ...any) error { return &httpError{code, fmt.Sprintf(f, a...)} }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	var he *httpError
	if errors.As(err, &he) {
		writeJSON(w, he.code, map[string]any{"error": he.msg})
		return
	}
	log.Printf("internal error: %v", err)
	writeJSON(w, 500, map[string]any{"error": err.Error()})
}

func readBody(r *http.Request, limit int64) ([]byte, error) {
	var rd io.Reader = http.MaxBytesReader(nil, r.Body, limit)
	if strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
		gz, err := gzip.NewReader(rd)
		if err != nil {
			return nil, errf(400, "bad gzip: %v", err)
		}
		defer gz.Close()
		rd = io.LimitReader(gz, limit*8)
	}
	b, err := io.ReadAll(rd)
	if err != nil {
		return nil, errf(413, "body: %v", err)
	}
	return b, nil
}

func readJSON(r *http.Request, limit int64, v any) error {
	b, err := readBody(r, limit)
	if err != nil {
		return err
	}
	if len(b) == 0 {
		return nil
	}
	if err := json.Unmarshal(b, v); err != nil {
		return errf(400, "bad json: %v", err)
	}
	return nil
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

func tokenMatches(tok string, hashes []string) bool {
	if tok == "" {
		return false
	}
	h := sha256Hex(tok)
	ok := false
	for _, x := range hashes {
		if subtle.ConstantTimeCompare([]byte(h), []byte(strings.ToLower(x))) == 1 {
			ok = true
		}
	}
	return ok
}

func (s *Server) admin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !tokenMatches(bearer(r), s.cfg.AdminTokenSHA256) {
			time.Sleep(300 * time.Millisecond)
			writeJSON(w, 401, map[string]any{"error": "unauthorized"})
			return
		}
		h(w, r)
	}
}

type deviceHandler func(w http.ResponseWriter, r *http.Request, d *Device)

func (s *Server) device(h deviceHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := bearer(r)
		if tok == "" {
			writeJSON(w, 401, map[string]any{"error": "unauthorized"})
			return
		}
		d, err := scanDevice(s.st.db.QueryRow(`SELECT `+deviceCols+` FROM devices WHERE token_hash=?`, sha256Hex(tok)))
		if err == sql.ErrNoRows {
			time.Sleep(300 * time.Millisecond)
			writeJSON(w, 401, map[string]any{"error": "unknown device token", "reenroll": true})
			return
		}
		if err != nil {
			writeErr(w, err)
			return
		}
		if d.Status == "disabled" {
			writeJSON(w, 403, map[string]any{"error": "device disabled", "status": "disabled"})
			return
		}
		h(w, r, d)
	}
}

// ---------- notifier: wakes long-polling devices ----------

type notifier struct {
	mu sync.Mutex
	ch map[string]chan struct{}
}

func newNotifier() *notifier { return &notifier{ch: map[string]chan struct{}{}} }

func (n *notifier) wait(id string) <-chan struct{} {
	n.mu.Lock()
	defer n.mu.Unlock()
	c, ok := n.ch[id]
	if !ok {
		c = make(chan struct{})
		n.ch[id] = c
	}
	return c
}

func (n *notifier) notify(id string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if c, ok := n.ch[id]; ok {
		close(c)
		delete(n.ch, id)
	}
}

func (n *notifier) notifyAll() {
	n.mu.Lock()
	defer n.mu.Unlock()
	for id, c := range n.ch {
		close(c)
		delete(n.ch, id)
	}
}

// ---------- rate limiter for enrollment ----------

type rateLimiter struct {
	mu     sync.Mutex
	n      int
	window time.Duration
	hits   map[string][]time.Time
}

func newRateLimiter(n int, window time.Duration) *rateLimiter {
	return &rateLimiter{n: n, window: window, hits: map[string][]time.Time{}}
}

func (l *rateLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	var kept []time.Time
	for _, t := range l.hits[key] {
		if now.Sub(t) < l.window {
			kept = append(kept, t)
		}
	}
	if len(kept) >= l.n {
		l.hits[key] = kept
		return false
	}
	l.hits[key] = append(kept, now)
	if len(l.hits) > 10000 {
		l.hits = map[string][]time.Time{}
	}
	return true
}

// ---------- time parsing ----------

var relRe = regexp.MustCompile(`^(\d+(?:\.\d+)?)(s|m|h|d|w)$`)

// parseWhen accepts "90m", "6h", "2d", unix seconds, RFC3339, or "2006-01-02 15:04" (Moscow time).
func (s *Server) parseWhen(v string, def float64) (float64, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return def, nil
	}
	if m := relRe.FindStringSubmatch(v); m != nil {
		n, _ := strconv.ParseFloat(m[1], 64)
		mul := map[string]float64{"s": 1, "m": 60, "h": 3600, "d": 86400, "w": 7 * 86400}[m[2]]
		return float64(time.Now().UnixNano())/1e9 - n*mul, nil
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil {
		return f, nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, v, s.loc); err == nil {
			return float64(t.UnixNano()) / 1e9, nil
		}
	}
	return 0, errf(400, "cannot parse time %q (use 6h, 2d, unix seconds, RFC3339 or '2006-01-02 15:04' Moscow time)", v)
}

func (s *Server) iso(ts float64) string {
	sec := int64(ts)
	ns := int64((ts - float64(sec)) * 1e9)
	return time.Unix(sec, ns).In(s.loc).Format("2006-01-02T15:04:05.000-07:00")
}

func intParam(r *http.Request, name string, def, max int) int {
	v, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil || v <= 0 {
		return def
	}
	if v > max {
		return max
	}
	return v
}

func rawOrEmpty(s string) json.RawMessage {
	if strings.TrimSpace(s) == "" {
		return json.RawMessage("null")
	}
	if !json.Valid([]byte(s)) {
		b, _ := json.Marshal(s)
		return b
	}
	return json.RawMessage(s)
}
