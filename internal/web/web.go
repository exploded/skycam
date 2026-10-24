// Package web is the Pi's own web UI on port 80, where allsky's used to be:
// the latest frame with the observatory's state, the live view, the nights
// and their products, and allsky's /images/ layout for skyq.
package web

import (
	"bytes"
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/exploded/skycam/internal/capture"
	"github.com/exploded/skycam/internal/config"
	"github.com/exploded/skycam/internal/store/db"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

type Server struct {
	Cfg config.Config
	// Base is config.json without the settings page's changes; the page
	// shows it as each setting's default.
	Base config.Config
	// Restart, if set, makes skycam exit so systemd starts it with saved
	// settings.
	Restart func()
	// SettingsErr says why settings.json was ignored at start-up, if it was.
	SettingsErr string
	Loc         *time.Location
	Hub         *capture.Hub
	DB          *db.Queries // may be nil
	Log         *slog.Logger
	// Camera is the name shown on the pages.
	Camera string

	pages map[string]*template.Template
}

var funcs = template.FuncMap{
	"pct": func(v float64) string {
		if math.IsNaN(v) {
			return "—"
		}
		return fmt.Sprintf("%.0f%%", v*100)
	},
	"temp": func(v float64) string {
		if math.IsNaN(v) {
			return "—"
		}
		return fmt.Sprintf("%.1f °C", v)
	},
	"exposure": func(d time.Duration) string {
		if d >= time.Second {
			return fmt.Sprintf("%.1f s", d.Seconds())
		}
		return fmt.Sprintf("%.1f ms", float64(d.Microseconds())/1000)
	},
	"lower": strings.ToLower,
}

// Handler builds the routes.
func (s *Server) Handler() (http.Handler, error) {
	if err := s.parse(); err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	static, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", cacheForever(http.StripPrefix("/static/", http.FileServerFS(static))))

	mux.HandleFunc("GET /{$}", s.dashboard)
	mux.HandleFunc("GET /partials/latest", s.latestPartial)
	mux.HandleFunc("GET /image.jpg", s.latestJPEG)
	mux.HandleFunc("GET /current/image.jpg", s.latestJPEG) // allsky's path
	mux.HandleFunc("GET /live", s.livePage)
	mux.HandleFunc("GET /live.mjpg", s.liveStream)
	mux.HandleFunc("GET /nights", s.nights)
	mux.Handle("GET /products/", noCache(http.StripPrefix("/products/", http.FileServer(noDirs{http.Dir(s.Cfg.ProductsDir())}))))
	mux.HandleFunc("GET /api/status", s.status)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })

	images := http.HandlerFunc(s.images)
	mux.Handle("GET /images/", s.basicAuth(images))
	mux.Handle("GET /api/nights/{night}/sky", s.basicAuth(http.HandlerFunc(s.skyNight)))
	mux.Handle("GET /images", http.RedirectHandler("/images/", http.StatusMovedPermanently))

	mux.Handle("GET /settings", localOnly(http.HandlerFunc(s.settingsPage)))
	mux.Handle("POST /settings", localOnly(http.HandlerFunc(s.saveSettings)))
	mux.Handle("POST /settings/reset", localOnly(http.HandlerFunc(s.resetSettings)))
	mux.Handle("POST /settings/check", localOnly(http.HandlerFunc(s.checkSource)))
	return s.logRequests(mux), nil
}

func (s *Server) parse() error {
	base, err := template.New("").Funcs(funcs).ParseFS(templateFS, "templates/base.html", "templates/_*.html")
	if err != nil {
		return err
	}
	s.pages = map[string]*template.Template{}
	for _, p := range []string{"dashboard", "live", "nights", "settings"} {
		c, err := base.Clone()
		if err != nil {
			return err
		}
		if s.pages[p], err = c.ParseFS(templateFS, "templates/"+p+".html"); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) render(w http.ResponseWriter, page, name string, data any) {
	var buf bytes.Buffer
	if err := s.pages[page].ExecuteTemplate(&buf, name, data); err != nil {
		s.Log.Error("template", "page", page, "name", name, "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	buf.WriteTo(w)
}

// latestView is what the dashboard shows of the newest frame.
type latestView struct {
	Has      bool
	Rec      *capture.Record
	Age      string
	Stale    bool
	Stamp    int64
	Live     bool
	Viewers  int
	Camera   string
	Cloud    float64
	ImageURL string
}

func (s *Server) latest() latestView {
	v := latestView{Camera: s.Camera, Cloud: math.NaN()}
	v.Live, _ = s.Hub.LiveState()
	v.Viewers = s.Hub.LiveViewers()
	r := s.Hub.Latest()
	if r == nil {
		return v
	}
	v.Has, v.Rec, v.Cloud = true, r, r.Cloud
	age := time.Since(r.Time)
	v.Age = ago(age)
	// Three missed frames at the slowest cadence means capture has stalled.
	// Stills pause while live mode runs; that isn't a stall.
	v.Stale = !v.Live && age > 3*max(s.Cfg.Exposure.Day.Interval.D(), s.Cfg.Exposure.Night.Interval.D())+r.Exposure
	v.Stamp = r.Time.UnixMilli()
	v.ImageURL = fmt.Sprintf("/image.jpg?t=%d", v.Stamp)
	return v
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	s.render(w, "dashboard", "base", map[string]any{"Title": "Observatory", "L": s.latest()})
}

func (s *Server) latestPartial(w http.ResponseWriter, r *http.Request) {
	s.render(w, "dashboard", "_latest", s.latest())
}

func (s *Server) latestJPEG(w http.ResponseWriter, r *http.Request) {
	rec := s.Hub.Latest()
	w.Header().Set("Cache-Control", "no-cache")
	if rec == nil {
		// Before the first frame of this run, fall back to the file the
		// last run left behind.
		http.ServeFile(w, r, filepath.Join(s.Cfg.DataDir, "current", "image.jpg"))
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Last-Modified", rec.Time.UTC().Format(http.TimeFormat))
	w.Write(rec.JPEG)
}

func (s *Server) livePage(w http.ResponseWriter, r *http.Request) {
	s.render(w, "live", "base", map[string]any{
		"Title": "Live", "Camera": s.Camera, "NightLimit": s.Cfg.Live.NightLimit.D().String(),
	})
}

// liveStream serves MJPEG. Holding it open is what keeps live mode on;
// the latest still goes first so the page isn't blank while the camera
// switches over.
func (s *Server) liveStream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ch, stop := s.Hub.Watch()
	defer stop()
	const boundary = "skycamframe"
	w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary="+boundary)
	w.Header().Set("Cache-Control", "no-cache, no-store")
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
		if _, err := w.Write([]byte("\r\n")); err != nil {
			return err
		}
		fl.Flush()
		return nil
	}
	if rec := s.Hub.Latest(); rec != nil {
		if send(rec.JPEG) != nil {
			return
		}
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case j := <-ch:
			if send(j) != nil {
				return
			}
		}
	}
}

type nightRow struct {
	Night, Date                            string
	Frames                                 int64
	Timelapse, Keogram, Startrails, Report string
	Images                                 bool
}

func (s *Server) nights(w http.ResponseWriter, r *http.Request) {
	var rows []nightRow
	seen := map[string]bool{}
	if s.DB != nil {
		// Every night retention keeps, however long that is.
		ns, err := s.DB.ListNights(r.Context(), int64(max(400, s.Cfg.ProductDays+1)))
		if err != nil {
			s.Log.Warn("listing nights", "err", err)
		}
		for _, n := range ns {
			row := nightRow{Night: n.Night, Frames: n.Frames}
			link := func(f string) string {
				if f == "" {
					return ""
				}
				return "/products/" + n.Night + "/" + f
			}
			row.Timelapse, row.Keogram, row.Startrails, row.Report = link(n.Timelapse), link(n.Keogram), link(n.Startrails), link(n.Report)
			rows = append(rows, row)
			seen[n.Night] = true
		}
	}
	// Nights still being captured, or not yet processed.
	if entries, err := os.ReadDir(s.Cfg.ImagesDir()); err == nil {
		for _, e := range entries {
			if e.IsDir() && reNight.MatchString(e.Name()) && !seen[e.Name()] {
				rows = append(rows, nightRow{Night: e.Name()})
			}
		}
	}
	for i := range rows {
		if t, err := time.ParseInLocation("20060102", rows[i].Night, s.Loc); err == nil {
			rows[i].Date = t.Format("Mon 2 Jan 2006")
		}
		if _, err := os.Stat(filepath.Join(s.Cfg.ImagesDir(), rows[i].Night)); err == nil {
			rows[i].Images = true
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Night > rows[j].Night })
	s.render(w, "nights", "base", map[string]any{"Title": "Nights", "Nights": rows})
}

var (
	reNight = regexp.MustCompile(`^\d{8}$`)
	reImage = regexp.MustCompile(`^image-\d{14}\.jpg$`)
)

// images serves allsky's layout: /images/ lists night folders,
// /images/<night>/ lists stills, /images/<night>/<file> is the still. The
// listing is plain HTML with one link per file, which is all skyq's
// fetcher reads.
func (s *Server) images(w http.ResponseWriter, r *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/images"), "/")
	parts := strings.Split(rest, "/")
	switch {
	case rest == "":
		s.listing(w, "/images/", s.Cfg.ImagesDir(), func(e os.DirEntry) bool { return e.IsDir() && reNight.MatchString(e.Name()) }, true)
	case len(parts) == 1 && reNight.MatchString(parts[0]):
		if !strings.HasSuffix(r.URL.Path, "/") {
			http.Redirect(w, r, r.URL.Path+"/", http.StatusMovedPermanently)
			return
		}
		s.listing(w, "/images/"+parts[0]+"/", filepath.Join(s.Cfg.ImagesDir(), parts[0]),
			func(e os.DirEntry) bool { return !e.IsDir() && reImage.MatchString(e.Name()) }, false)
	case len(parts) == 2 && reNight.MatchString(parts[0]) && reImage.MatchString(parts[1]):
		w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
		http.ServeFile(w, r, filepath.Join(s.Cfg.ImagesDir(), parts[0], parts[1]))
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) listing(w http.ResponseWriter, title, dir string, keep func(os.DirEntry) bool, dirs bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		http.NotFound(w, nil)
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "<!doctype html><html><head><meta charset=\"utf-8\"><title>Index of %s</title></head><body><h1>Index of %s</h1><ul>\n",
		template.HTMLEscapeString(title), template.HTMLEscapeString(title))
	if title != "/images/" {
		b.WriteString("<li><a href=\"../\">../</a></li>\n")
	}
	for _, e := range entries {
		if !keep(e) {
			continue
		}
		name := e.Name()
		if dirs {
			name += "/"
		}
		fmt.Fprintf(&b, "<li><a href=\"%s\">%s</a></li>\n", name, name)
	}
	b.WriteString("</ul></body></html>\n")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write([]byte(b.String()))
}

// statusJSON is /api/status: the newest frame's numbers and the
// observatory, for scripts and other tools.
type statusJSON struct {
	Time      time.Time `json:"time"`
	Night     string    `json:"night"`
	Mode      string    `json:"mode"`
	ExposureS float64   `json:"exposure_s"`
	Gain      int       `json:"gain"`
	TempC     *float64  `json:"temp_c"`
	SunAlt    float64   `json:"sun_alt"`
	MoonAlt   float64   `json:"moon_alt"`
	MoonIllum float64   `json:"moon_illum"`
	Stars     int       `json:"stars"`
	Cloud     *float64  `json:"cloud"`
	Sky       string    `json:"sky"`
	SkyLevel  float64   `json:"sky_level"`
	SkyMag    *float64  `json:"sky_mag"`
	Roof      string    `json:"roof"`
	Rain      string    `json:"rain"`
	NINA      any       `json:"nina"`
	Live      bool      `json:"live"`
	Stale     bool      `json:"stale"`
}

func ptr(v float64) *float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return &v
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	v := s.latest()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	if !v.Has {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]any{"error": "no frame yet", "live": v.Live})
		return
	}
	rec := v.Rec
	nina := any(nil)
	if rec.Obs.NINA.Connected {
		nina = map[string]any{"mount": rec.Obs.NINA.Mount, "filter": rec.Obs.NINA.Filter, "camera_c": ptr(rec.Obs.NINA.CameraC)}
	}
	json.NewEncoder(w).Encode(statusJSON{
		Time: rec.Time, Night: rec.Night, Mode: rec.Mode, ExposureS: rec.Exposure.Seconds(), Gain: rec.Gain,
		TempC: ptr(rec.TempC), SunAlt: rec.SunAlt, MoonAlt: rec.MoonAlt, MoonIllum: rec.MoonIllum,
		Stars: rec.Stars, Cloud: ptr(rec.Cloud), Sky: rec.Sky(), SkyLevel: rec.SkyLevel, SkyMag: ptr(rec.SkyMag),
		Roof: rec.Obs.Roof, Rain: rec.Obs.Rain, NINA: nina, Live: v.Live, Stale: v.Stale,
	})
}

func (s *Server) basicAuth(next http.Handler) http.Handler {
	user, pass := s.Cfg.ImagesUser, s.Cfg.ImagesPassword
	if user == "" && pass == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(u), []byte(user)) != 1 || subtle.ConstantTimeCompare([]byte(p), []byte(pass)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="skycam images"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				s.Log.Error("panic serving request", "path", r.URL.Path, "panic", v)
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func cacheForever(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		h.ServeHTTP(w, r)
	})
}

func noCache(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		h.ServeHTTP(w, r)
	})
}

// noDirs serves files but never directory listings.
type noDirs struct{ fs http.FileSystem }

func (n noDirs) Open(name string) (http.File, error) {
	f, err := n.fs.Open(name)
	if err != nil {
		return nil, err
	}
	if st, err := f.Stat(); err == nil && st.IsDir() {
		f.Close()
		return nil, os.ErrNotExist
	}
	return f, nil
}

// Serve runs the server until ctx ends.
func Serve(ctx context.Context, addr string, h http.Handler, log *slog.Logger) error {
	// BaseContext ties every request to ctx, so open live streams end on
	// shutdown instead of holding it up.
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second,
		BaseContext: func(net.Listener) context.Context { return ctx }}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("web server listening", "addr", addr)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(sctx)
	}
}

// ago formats an age for people: "12s", "4m 10s", "3h 05m", "2d 4h".
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
