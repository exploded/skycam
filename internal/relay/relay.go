// Package relay is skycam-web, the small site on the Linode that lets the
// observatory be watched from anywhere without opening a port at home.
// The Pi pushes to it (frames, live frames, night reports) over HTTPS with
// a bearer token, and long-polls it to learn when someone wants the live
// view. Viewers log in with basic auth.
package relay

import (
	"bytes"
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/exploded/skycam/internal/fsutil"
	"github.com/exploded/skycam/internal/report"
	"github.com/exploded/skycam/internal/upload"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

type Config struct {
	Token    string // the Pi's bearer token
	User     string // viewer login
	Password string
	DataDir  string // latest frame, nights.json
	// ReportDir is deepspaceplace's reports/skycam folder; empty skips
	// publishing there.
	ReportDir string
	// ReportBaseURL is where those reports are read, for links.
	ReportBaseURL string
	Loc           *time.Location
	Log           *slog.Logger
}

const (
	maxFrame    = 8 << 20
	maxNight    = 32 << 20
	liveLinger  = 30 * time.Second // keep asking for live this long after the last viewer
	piStaleSecs = 600
)

type Server struct {
	cfg   Config
	pages map[string]*template.Template

	mu       sync.Mutex
	latest   []byte
	status   upload.StatusJSON
	seen     time.Time // last request from the Pi
	viewers  map[chan []byte]struct{}
	lastView time.Time
	wake     chan struct{} // closed when a viewer arrives
	liveAt   time.Time     // last live frame
	nights   map[string]upload.NightJSON
}

func New(cfg Config) (*Server, error) {
	s := &Server{cfg: cfg, viewers: map[chan []byte]struct{}{}, wake: make(chan struct{}), nights: map[string]upload.NightJSON{}}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, err
	}
	if b, err := os.ReadFile(filepath.Join(cfg.DataDir, "latest.jpg")); err == nil {
		s.latest = b
	}
	if b, err := os.ReadFile(filepath.Join(cfg.DataDir, "status.json")); err == nil {
		json.Unmarshal(b, &s.status)
	}
	if b, err := os.ReadFile(filepath.Join(cfg.DataDir, "nights.json")); err == nil {
		json.Unmarshal(b, &s.nights)
	}
	base, err := template.New("").ParseFS(templateFS, "templates/base.html", "templates/_*.html")
	if err != nil {
		return nil, err
	}
	s.pages = map[string]*template.Template{}
	for _, p := range []string{"home", "live", "nights"} {
		c, _ := base.Clone()
		if s.pages[p], err = c.ParseFS(templateFS, "templates/"+p+".html"); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /health/pi", s.healthPi)

	// The Pi.
	mux.Handle("POST /api/frame", s.pi(http.HandlerFunc(s.postFrame)))
	mux.Handle("POST /api/live", s.pi(http.HandlerFunc(s.postLive)))
	mux.Handle("GET /api/demand", s.pi(http.HandlerFunc(s.demand)))
	mux.Handle("POST /api/night", s.pi(http.HandlerFunc(s.postNight)))

	// Viewers.
	view := func(h http.HandlerFunc) http.Handler { return s.viewer(h) }
	mux.Handle("GET /{$}", view(s.home))
	mux.Handle("GET /partials/latest", view(s.latestPartial))
	mux.Handle("GET /image.jpg", view(s.image))
	mux.Handle("GET /live", view(s.livePage))
	mux.Handle("GET /live.mjpg", view(s.liveStream))
	mux.Handle("GET /nights", view(s.nightsPage))
	return mux
}

// pi checks the Pi's bearer token and records that it was heard from.
func (s *Server) pi(next http.Handler) http.Handler {
	want := []byte("Bearer " + s.cfg.Token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.Token == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		s.mu.Lock()
		s.seen = time.Now()
		s.mu.Unlock()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) viewer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if s.cfg.Password == "" || !ok ||
			subtle.ConstantTimeCompare([]byte(u), []byte(s.cfg.User)) != 1 ||
			subtle.ConstantTimeCompare([]byte(p), []byte(s.cfg.Password)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="skycam"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) healthPi(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	seen := s.seen
	s.mu.Unlock()
	age := time.Since(seen)
	if seen.IsZero() || age > piStaleSecs*time.Second {
		http.Error(w, fmt.Sprintf("pi silent for %s", age.Round(time.Second)), http.StatusServiceUnavailable)
		return
	}
	fmt.Fprintf(w, "ok, pi seen %s ago\n", age.Round(time.Second))
}

func readLimited(r *http.Request, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("body over %d bytes", limit)
	}
	return b, nil
}

func (s *Server) postFrame(w http.ResponseWriter, r *http.Request) {
	b, err := readLimited(r, maxFrame)
	if err != nil || len(b) < 4 || b[0] != 0xff || b[1] != 0xd8 {
		http.Error(w, "want a JPEG", http.StatusBadRequest)
		return
	}
	var st upload.StatusJSON
	json.Unmarshal([]byte(r.Header.Get("X-Skycam-Status")), &st)
	s.mu.Lock()
	s.latest, s.status = b, st
	s.mu.Unlock()
	if err := fsutil.WriteFileAtomic(filepath.Join(s.cfg.DataDir, "latest.jpg"), b); err != nil {
		s.cfg.Log.Warn("saving latest frame", "err", err)
	}
	js, _ := json.Marshal(st)
	fsutil.WriteFileAtomic(filepath.Join(s.cfg.DataDir, "status.json"), js)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) postLive(w http.ResponseWriter, r *http.Request) {
	b, err := readLimited(r, maxFrame)
	if err != nil || len(b) < 4 {
		http.Error(w, "want a JPEG", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.liveAt = time.Now()
	for ch := range s.viewers {
		select {
		case ch <- b:
		default:
		}
	}
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// wantLive reports whether a viewer is watching or left recently.
func (s *Server) wantLive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.viewers) > 0 || time.Since(s.lastView) < liveLinger
}

// demand answers the Pi's long poll: at once if someone wants live,
// otherwise when a viewer arrives or the wait runs out.
func (s *Server) demand(w http.ResponseWriter, r *http.Request) {
	wait, _ := strconv.Atoi(r.URL.Query().Get("wait"))
	wait = min(max(wait, 0), 30)
	if !s.wantLive() && wait > 0 {
		s.mu.Lock()
		wake := s.wake
		s.mu.Unlock()
		select {
		case <-wake:
		case <-time.After(time.Duration(wait) * time.Second):
		case <-r.Context().Done():
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"live": s.wantLive()})
}

var reNight = regexp.MustCompile(`^\d{8}$`)

func (s *Server) postNight(w http.ResponseWriter, r *http.Request) {
	b, err := readLimited(r, maxNight)
	if err != nil {
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	}
	var n upload.NightJSON
	if err := json.Unmarshal(b, &n); err != nil || !reNight.MatchString(n.Night) {
		http.Error(w, "bad night", http.StatusBadRequest)
		return
	}
	if s.cfg.ReportDir != "" && n.ReportHTML != "" {
		if err := fsutil.WriteFileAtomic(filepath.Join(s.cfg.ReportDir, reportName(n.Night)), []byte(n.ReportHTML)); err != nil {
			s.cfg.Log.Error("publishing report", "night", n.Night, "err", err)
			http.Error(w, "could not publish report", http.StatusInternalServerError)
			return
		}
	}
	n.ReportHTML = "" // published; no need to keep a second copy
	s.mu.Lock()
	s.nights[n.Night] = n
	js, _ := json.MarshalIndent(s.nights, "", " ")
	s.mu.Unlock()
	if err := fsutil.WriteFileAtomic(filepath.Join(s.cfg.DataDir, "nights.json"), js); err != nil {
		s.cfg.Log.Warn("saving nights", "err", err)
	}
	if err := s.writeIndex(); err != nil {
		s.cfg.Log.Warn("writing report index", "err", err)
	}
	s.cfg.Log.Info("night published", "night", n.Night)
	w.WriteHeader(http.StatusNoContent)
}

// reportName is skyq's naming (2026-10-23.html), so the two sets of
// reports sort the same way.
func reportName(night string) string {
	return night[:4] + "-" + night[4:6] + "-" + night[6:] + ".html"
}

func (s *Server) sortedNights() []upload.NightJSON {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]upload.NightJSON, 0, len(s.nights))
	for _, n := range s.nights {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Night > out[j].Night })
	return out
}

func (s *Server) writeIndex() error {
	if s.cfg.ReportDir == "" {
		return nil
	}
	var entries []report.IndexEntry
	for _, n := range s.sortedNights() {
		entries = append(entries, report.IndexEntry{Night: n.Night, Href: reportName(n.Night), Summary: n.Summary})
	}
	html, err := report.RenderIndex(entries, s.cfg.Loc)
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(filepath.Join(s.cfg.ReportDir, "index.html"), html)
}

// Viewer pages.

type latestView struct {
	Has    bool
	St     upload.StatusJSON
	Age    string
	Stale  bool
	Stamp  int64
	PiSeen string
}

func (s *Server) latestView() latestView {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := latestView{Has: s.latest != nil, St: s.status}
	if !s.status.Time.IsZero() {
		age := time.Since(s.status.Time)
		v.Age = ago(age)
		v.Stale = age > 5*time.Minute
		v.Stamp = s.status.Time.UnixMilli()
		v.St.Time = s.status.Time.In(s.cfg.Loc)
	}
	if !s.seen.IsZero() {
		v.PiSeen = ago(time.Since(s.seen))
	}
	return v
}

func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm %02ds", int(d.Minutes()), int(d.Seconds())%60)
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
}

func (s *Server) render(w http.ResponseWriter, page, name string, data any) {
	var buf bytes.Buffer
	if err := s.pages[page].ExecuteTemplate(&buf, name, data); err != nil {
		s.cfg.Log.Error("template", "page", page, "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	buf.WriteTo(w)
}

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	s.render(w, "home", "base", map[string]any{"Title": "Observatory", "L": s.latestView()})
}

func (s *Server) latestPartial(w http.ResponseWriter, r *http.Request) {
	s.render(w, "home", "_latest", s.latestView())
}

func (s *Server) image(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	b := s.latest
	s.mu.Unlock()
	if b == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Write(b)
}

func (s *Server) livePage(w http.ResponseWriter, r *http.Request) {
	s.render(w, "live", "base", map[string]any{"Title": "Live"})
}

// liveStream sends MJPEG. The latest still goes first; live frames follow
// once the Pi has switched over, which takes up to one poll and a camera
// mode change.
func (s *Server) liveStream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ch := make(chan []byte, 2)
	s.mu.Lock()
	s.viewers[ch] = struct{}{}
	s.lastView = time.Now()
	close(s.wake)
	s.wake = make(chan struct{})
	first := s.latest
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.viewers, ch)
		s.lastView = time.Now()
		s.mu.Unlock()
	}()

	const boundary = "skycamframe"
	w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary="+boundary)
	w.Header().Set("X-Accel-Buffering", "no")
	// Send the headers now: a client waits on them, and the first frame
	// may be seconds away.
	w.WriteHeader(http.StatusOK)
	fl.Flush()
	send := func(j []byte) error {
		if _, err := fmt.Fprintf(w, "--%s\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n", boundary, len(j)); err != nil {
			return err
		}
		if _, err := w.Write(j); err != nil {
			return err
		}
		w.Write([]byte("\r\n"))
		fl.Flush()
		return nil
	}
	if first != nil && send(first) != nil {
		return
	}
	// Cap a session so a forgotten tab doesn't hold the Pi in live mode
	// all night; the Pi applies its own night limit as well.
	limit := time.NewTimer(30 * time.Minute)
	defer limit.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-limit.C:
			return
		case j := <-ch:
			if send(j) != nil {
				return
			}
		}
	}
}

func (s *Server) nightsPage(w http.ResponseWriter, r *http.Request) {
	type row struct {
		upload.NightJSON
		Date, Report string
	}
	var rows []row
	for _, n := range s.sortedNights() {
		rr := row{NightJSON: n}
		if t, err := time.ParseInLocation("20060102", n.Night, s.cfg.Loc); err == nil {
			rr.Date = t.Format("Mon 2 Jan 2006")
		}
		if s.cfg.ReportBaseURL != "" {
			rr.Report = s.cfg.ReportBaseURL + reportName(n.Night)
		}
		rows = append(rows, rr)
	}
	s.render(w, "nights", "base", map[string]any{"Title": "Nights", "Nights": rows})
}

// Serve runs the server until ctx ends.
func Serve(ctx context.Context, addr string, h http.Handler, log *slog.Logger) error {
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second}
	// Tie requests to ctx so open live streams end on shutdown.
	srv.BaseContext = func(net.Listener) context.Context { return ctx }
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("skycam-web listening", "addr", addr)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(sctx)
	}
}
