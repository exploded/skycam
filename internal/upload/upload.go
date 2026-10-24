// Package upload copies skycam's output off the Pi: images and videos to
// S3, and the latest frame, live frames and night reports to the relay at
// skycam.mchugh.au. Every upload is a copy — a failure is logged and
// retried later, and never blocks capture or the local products.
package upload

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"html"
	"image"
	"image/jpeg"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	xdraw "golang.org/x/image/draw"

	"github.com/exploded/skycam/internal/capture"
	"github.com/exploded/skycam/internal/config"
	"github.com/exploded/skycam/internal/night"
	"github.com/exploded/skycam/internal/store/db"
)

type Uploader struct {
	cfg  config.Config
	loc  *time.Location
	q    *db.Queries
	log  *slog.Logger
	s3   *s3.Client
	http *http.Client
}

func New(ctx context.Context, cfg config.Config, loc *time.Location, q *db.Queries, log *slog.Logger) (*Uploader, error) {
	u := &Uploader{cfg: cfg, loc: loc, q: q, log: log, http: &http.Client{Timeout: 60 * time.Second}}
	if cfg.S3.Enabled {
		u.s3 = s3.New(s3.Options{
			Region:      cfg.S3.Region,
			Credentials: aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider(cfg.S3.AccessKeyID, cfg.S3.SecretAccessKey, "")),
		})
	}
	return u, nil
}

func (u *Uploader) relayOn() bool { return u.cfg.Relay.URL != "" }

// key builds an S3 object key under the configured prefix.
func (u *Uploader) key(parts ...string) string {
	return strings.TrimLeft(u.cfg.S3.Prefix+path.Join(parts...), "/")
}

// productKeys maps a night's product files to their S3 keys. The layout is
// allsky's, so deepspaceplace-allsky carries on from the archive allsky
// built: videos/, videos/thumbnails/, keograms/, startrails/.
func (u *Uploader) productKeys(res night.Result) map[string]string {
	keys := map[string]string{}
	if res.Timelapse != "" {
		keys[res.Timelapse] = u.key("videos", res.Timelapse)
	}
	if res.Thumbnail != "" {
		keys[res.Thumbnail] = u.key("videos", "thumbnails", "allsky-"+res.Night+".jpg")
	}
	if res.Keogram != "" {
		keys[res.Keogram] = u.key("keograms", res.Keogram)
	}
	if res.Startrails != "" {
		keys[res.Startrails] = u.key("startrails", res.Startrails)
	}
	return keys
}

// PublicURL is how a browser reaches an S3 object.
func (u *Uploader) PublicURL(key string) string {
	base := u.cfg.S3.PublicBaseURL
	if base == "" {
		base = fmt.Sprintf("https://%s.s3.%s.amazonaws.com", u.cfg.S3.Bucket, u.cfg.S3.Region)
	}
	return strings.TrimRight(base, "/") + "/" + key
}

// TimelapseURL is where the report should point its video player: S3 when
// it will be uploaded there, otherwise the file beside the report.
func (u *Uploader) TimelapseURL(res night.Result) string {
	if res.Timelapse == "" {
		return ""
	}
	if u.s3 != nil {
		return u.PublicURL(u.key("videos", res.Timelapse))
	}
	return res.Timelapse
}

func (u *Uploader) put(ctx context.Context, key string, body []byte, contentType, cache string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	_, err := u.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket:       aws.String(u.cfg.S3.Bucket),
		Key:          aws.String(key),
		Body:         bytes.NewReader(body),
		ContentType:  aws.String(contentType),
		CacheControl: aws.String(cache),
	})
	return err
}

func (u *Uploader) putFile(ctx context.Context, key, file string) error {
	b, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	ct := mime.TypeByExtension(filepath.Ext(file))
	if ct == "" {
		ct = "application/octet-stream"
	}
	return u.put(ctx, key, b, ct, "public, max-age=31536000, immutable")
}

// Run follows the capture loop until ctx ends: latest frame and (if
// configured) every frame to S3, stills and live frames to the relay, and
// the relay's live-view demand back to the hub.
func (u *Uploader) Run(ctx context.Context, hub *capture.Hub) {
	if u.s3 == nil && !u.relayOn() {
		return
	}
	if u.relayOn() {
		go u.demandLoop(ctx, hub)
		go u.liveLoop(ctx, hub)
	}
	frames, stop := hub.Subscribe()
	defer stop()
	var lastS3, lastRelay time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case r := <-frames:
			now := time.Now()
			if u.s3 != nil {
				if now.Sub(lastS3) >= u.cfg.S3.LatestEvery.D() {
					if err := u.put(ctx, u.key("image.jpg"), r.JPEG, "image/jpeg", "no-cache"); err != nil {
						u.log.Warn("s3 latest image", "err", err)
					} else {
						lastS3 = now
					}
				}
				if u.cfg.S3.AllFrames && r.File != "" {
					if err := u.put(ctx, u.key("images", r.Night, r.File), r.JPEG, "image/jpeg", "public, max-age=31536000, immutable"); err != nil {
						u.log.Warn("s3 frame", "file", r.File, "err", err)
					}
				}
			}
			if u.relayOn() && now.Sub(lastRelay) >= u.cfg.Relay.Every.D() {
				if err := u.postFrame(ctx, "/api/frame", r.JPEG, Status(r)); err != nil {
					u.log.Info("relay frame", "err", err)
				} else {
					lastRelay = now
				}
			}
		}
	}
}

// StatusJSON is the frame summary sent with each still to the relay.
type StatusJSON struct {
	Time      time.Time `json:"time"`
	Mode      string    `json:"mode"`
	ExposureS float64   `json:"exposure_s"`
	Gain      int       `json:"gain"`
	Stars     int       `json:"stars"`
	Sky       string    `json:"sky"`
	Roof      string    `json:"roof"`
	Rain      string    `json:"rain"`
	NINA      string    `json:"nina"`
	SunAlt    float64   `json:"sun_alt"`
}

func Status(r *capture.Record) StatusJSON {
	s := StatusJSON{Time: r.Time, Mode: r.Mode, ExposureS: r.Exposure.Seconds(), Gain: r.Gain, Stars: r.Stars,
		Sky: r.Sky(), Roof: r.Obs.Roof, Rain: r.Obs.Rain, SunAlt: r.SunAlt}
	if n := r.Obs.NINA; n.Connected {
		var parts []string
		if n.Mount != "" {
			parts = append(parts, "mount "+n.Mount)
		}
		if n.Filter != "" {
			parts = append(parts, n.Filter)
		}
		s.NINA = strings.Join(parts, " · ")
	}
	return s
}

func (u *Uploader) request(ctx context.Context, method, p string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(u.cfg.Relay.URL, "/")+p, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+u.cfg.Relay.Token)
	return req, nil
}

func (u *Uploader) do(req *http.Request, client *http.Client) error {
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: %s", req.Method, req.URL.Path, resp.Status)
	}
	return nil
}

func (u *Uploader) postFrame(ctx context.Context, p string, jpg []byte, st any) error {
	req, err := u.request(ctx, http.MethodPost, p, bytes.NewReader(jpg))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "image/jpeg")
	if st != nil {
		js, _ := json.Marshal(st)
		req.Header.Set("X-Skycam-Status", string(js))
	}
	return u.do(req, u.http)
}

// demandLoop long-polls the relay for remote live viewers. While anyone
// is watching there, it keeps poking the hub so live mode stays on.
func (u *Uploader) demandLoop(ctx context.Context, hub *capture.Hub) {
	client := &http.Client{Timeout: 40 * time.Second}
	backoff := time.Second
	for ctx.Err() == nil {
		req, err := u.request(ctx, http.MethodGet, "/api/demand?wait=25", nil)
		if err != nil {
			return
		}
		resp, err := client.Do(req)
		if err == nil && resp.StatusCode == http.StatusOK {
			var d struct {
				Live bool `json:"live"`
			}
			json.NewDecoder(io.LimitReader(resp.Body, 1<<10)).Decode(&d)
			resp.Body.Close()
			backoff = time.Second
			if d.Live {
				hub.Demand()
				// The relay answers at once while demand holds; don't spin.
				sleep(ctx, 2*time.Second)
			}
			continue
		}
		if resp != nil {
			resp.Body.Close()
		}
		sleep(ctx, backoff)
		backoff = min(backoff*2, 2*time.Minute)
	}
}

// liveLoop forwards live frames to the relay, scaled down for the uplink.
func (u *Uploader) liveLoop(ctx context.Context, hub *capture.Hub) {
	frames, stop := hub.LiveSubscribe()
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-frames:
			small := u.shrink(j)
			if err := u.postFrame(ctx, "/api/live", small, nil); err != nil {
				u.log.Info("relay live frame", "err", err)
			}
		}
	}
}

func (u *Uploader) shrink(j []byte) []byte {
	w := u.cfg.Relay.LiveWidth
	if w <= 0 {
		return j
	}
	src, err := jpeg.Decode(bytes.NewReader(j))
	if err != nil || src.Bounds().Dx() <= w {
		return j
	}
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, w, b.Dy()*w/b.Dx()))
	xdraw.ApproxBiLinear.Scale(dst, dst.Bounds(), src, b, xdraw.Src, nil)
	var buf bytes.Buffer
	if jpeg.Encode(&buf, dst, &jpeg.Options{Quality: u.cfg.Relay.LiveQuality}) != nil {
		return j
	}
	return buf.Bytes()
}

// NightJSON is what the relay keeps per night for its gallery and the
// report it publishes to deepspaceplace.
type NightJSON struct {
	Night         string `json:"night"`
	Summary       string `json:"summary"`
	ReportHTML    string `json:"report_html"`
	TimelapseURL  string `json:"timelapse_url,omitempty"`
	KeogramURL    string `json:"keogram_url,omitempty"`
	StartrailsURL string `json:"startrails_url,omitempty"`
}

// Night uploads a processed night: products to S3, then the report and
// gallery entry to the relay. Each half is recorded separately so a retry
// only redoes what failed.
func (u *Uploader) Night(ctx context.Context, res night.Result) {
	if u.s3 != nil {
		var failed bool
		for name, key := range u.productKeys(res) {
			if err := u.putFile(ctx, key, filepath.Join(res.Dir, name)); err != nil {
				u.log.Warn("s3 product", "file", name, "err", err)
				failed = true
			}
		}
		if !failed {
			u.q.MarkNightUploaded(ctx, db.MarkNightUploadedParams{UploadedAt: nullNow(), Night: res.Night})
		}
	}
	if u.relayOn() && res.Report != "" {
		page, err := os.ReadFile(filepath.Join(res.Dir, res.Report))
		if err != nil {
			u.log.Warn("reading report", "err", err)
			return
		}
		n := NightJSON{Night: res.Night, Summary: summary(page), ReportHTML: string(page), TimelapseURL: u.TimelapseURL(res)}
		if u.s3 != nil {
			if res.Keogram != "" {
				n.KeogramURL = u.PublicURL(u.key("keograms", res.Keogram))
			}
			if res.Startrails != "" {
				n.StartrailsURL = u.PublicURL(u.key("startrails", res.Startrails))
			}
		}
		js, _ := json.Marshal(n)
		req, err := u.request(ctx, http.MethodPost, "/api/night", bytes.NewReader(js))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		if err := u.do(req, u.http); err != nil {
			u.log.Warn("publishing report to relay", "night", res.Night, "err", err)
			return
		}
		u.q.MarkNightPublished(ctx, db.MarkNightPublishedParams{PublishedAt: nullNow(), Night: res.Night})
		u.log.Info("night published", "night", res.Night)
	}
}

// CatchUp retries nights whose upload or publish didn't finish.
func (u *Uploader) CatchUp(ctx context.Context) {
	if u.s3 == nil && !u.relayOn() {
		return
	}
	nights, err := u.q.NightsNotPublished(ctx)
	if err != nil {
		return
	}
	for _, n := range nights {
		needS3 := u.s3 != nil && !n.UploadedAt.Valid
		needRelay := u.relayOn() && !n.PublishedAt.Valid && n.Report != ""
		if !needS3 && !needRelay {
			continue
		}
		res := night.Result{Night: n.Night, Dir: filepath.Join(u.cfg.ProductsDir(), n.Night), Frames: int(n.Frames),
			Timelapse: n.Timelapse, Keogram: n.Keogram, Startrails: n.Startrails, Report: n.Report}
		if _, err := os.Stat(filepath.Join(res.Dir, night.ThumbnailName(n.Night))); err == nil {
			res.Thumbnail = night.ThumbnailName(n.Night)
		}
		if _, err := os.Stat(res.Dir); err != nil {
			continue
		}
		u.Night(ctx, res)
	}
}

// summary pulls the report's one-line verdict for the relay's index.
func summary(page []byte) string {
	s := string(page)
	i := strings.Index(s, `<p class="sub">`)
	if i < 0 {
		return ""
	}
	s = s[i+len(`<p class="sub">`):]
	if j := strings.Index(s, "</p>"); j >= 0 {
		s = s[:j]
	}
	return strings.TrimSpace(html.UnescapeString(s))
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

func nullNow() sql.NullInt64 { return sql.NullInt64{Int64: time.Now().UnixMilli(), Valid: true} }
