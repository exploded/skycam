package web

import (
	"context"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/exploded/skycam/internal/config"
	"github.com/exploded/skycam/internal/fsutil"
	"github.com/exploded/skycam/internal/observatory"
)

// The settings page edits everything in config.json except the locked
// keys (config.Locked). It has no login: it answers only clients on the
// home network, which is the only place the Pi can be reached from. Saved
// changes go to <data_dir>/settings.json and apply when skycam restarts.

type option struct{ Value, Label string }

type field struct {
	Key     string
	Label   string
	Help    string
	Kind    string // text, url, int, float, duration, bool, rect, rects
	Options []option
	Check   string // observatory source the Check button reads
}

type group struct {
	Title  string
	Fields []field
}

func modeFields(mode, name string) []field {
	p := "exposure." + mode + "."
	return []field{
		{Key: p + "interval", Label: name + " interval", Help: "Start to start, e.g. 20s.", Kind: "duration"},
		{Key: p + "min_exposure", Label: name + " shortest exposure", Kind: "duration"},
		{Key: p + "max_exposure", Label: name + " longest exposure", Kind: "duration"},
		{Key: p + "min_gain", Label: name + " lowest gain", Kind: "int"},
		{Key: p + "max_gain", Label: name + " highest gain", Help: "Gain rises only once exposure is at its longest.", Kind: "int"},
		{Key: p + "target", Label: name + " target brightness", Help: "Mean brightness to aim for, 0–255.", Kind: "float"},
		{Key: p + "save", Label: name + ": keep frames", Help: "Save the stills under images/ for skyq and the timelapse.", Kind: "bool"},
	}
}

var settingsGroups = []group{
	{"Site", []field{
		{Key: "timezone", Label: "Time zone", Help: "IANA name, e.g. Australia/Melbourne.", Kind: "text"},
		{Key: "latitude", Label: "Latitude", Help: "Degrees, south negative.", Kind: "float"},
		{Key: "longitude", Label: "Longitude", Help: "Degrees, west negative.", Kind: "float"},
	}},
	{"Camera", []field{
		{Key: "camera.label", Label: "Name on the overlay", Help: "Empty uses the name the camera reports.", Kind: "text"},
		{Key: "camera.offset", Label: "Offset", Help: "Black level (ASI_OFFSET).", Kind: "int"},
		{Key: "camera.bandwidth", Label: "USB bandwidth", Help: "Percent (ASI_BANDWIDTHOVERLOAD).", Kind: "int"},
		{Key: "camera.flip", Label: "Flip", Kind: "int", Options: []option{{"0", "None"}, {"1", "Horizontal"}, {"2", "Vertical"}, {"3", "Both"}}},
		{Key: "camera.snap", Label: "Snap mode for stills", Help: "Off takes each still as a one-frame video capture, the reliable path on the ASI120.", Kind: "bool"},
		{Key: "camera.max_failures", Label: "Failures before restart", Help: "Captures that may fail in a row before skycam exits and systemd restarts it.", Kind: "int"},
		{Key: "jpeg_quality", Label: "Still JPEG quality", Help: "1–100.", Kind: "int"},
	}},
	{"Exposure", slices.Concat(
		[]field{{Key: "exposure.sun_angle", Label: "Night below sun altitude", Help: "Degrees. −6 is civil twilight, allsky's default.", Kind: "float"}},
		modeFields("day", "Day"),
		modeFields("night", "Night"),
		[]field{
			{Key: "exposure.deadband", Label: "Deadband", Help: "Fraction of the target within which the settings stay put, so the timelapse doesn't flicker.", Kind: "float"},
			{Key: "exposure.meter", Label: "Meter region", Help: "x0, y0, x1, y1 as fractions of the frame.", Kind: "rect"},
			{Key: "exposure.exclude", Label: "Left out of metering", Help: "One x0, y0, x1, y1 per line: the overlay text, the telescope. Also left out of star detection.", Kind: "rects"},
		},
	)},
	{"Overlay", []field{
		{Key: "overlay.enabled", Label: "Draw the overlay", Kind: "bool"},
		{Key: "overlay.observatory", Label: "Observatory lines", Help: "Roof, rain and N.I.N.A. at the bottom left.", Kind: "bool"},
	}},
	{"Observatory", []field{
		{Key: "observatory.roof_url", Label: "Roof controller", Help: "The roll-off's status.json on port 80. Empty turns it off.", Kind: "url", Check: "roof"},
		{Key: "observatory.rain_url", Label: "Rain sensor (safety monitor)", Help: "The RG11's status.json on port 80. Empty turns it off.", Kind: "url", Check: "rain"},
		{Key: "observatory.nina_url", Label: "N.I.N.A. Advanced API", Help: "Base address, e.g. http://192.168.1.144:1888. Empty turns it off.", Kind: "url", Check: "nina"},
		{Key: "observatory.poll_interval", Label: "Poll every", Kind: "duration"},
		{Key: "observatory.stale_after", Label: "Unknown after", Help: "An older reading shows as unknown, never closed or safe.", Kind: "duration"},
		{Key: "observatory.timeout", Label: "Request timeout", Kind: "duration"},
	}},
	{"Cloud and stars", []field{
		{Key: "analysis.sky", Label: "Sky region", Help: "x0, y0, x1, y1 for stars and sky brightness.", Kind: "rect"},
		{Key: "analysis.star_sigma", Label: "Star threshold", Help: "Standard deviations above the background.", Kind: "float"},
		{Key: "analysis.min_delta", Label: "Threshold floor", Help: "ADU. Stops JPEG ringing on a flat frame counting as stars.", Kind: "float"},
		{Key: "analysis.max_star_area", Label: "Largest star", Help: "Pixels in a 7×7 box; bigger is a light, the moon or a roof edge.", Kind: "int"},
		{Key: "analysis.dark_frac", Label: "Skip dark blocks below", Help: "Fraction of the sky level: the telescope, roof edge and trees.", Kind: "float"},
		{Key: "analysis.hot_decay", Label: "Hot-pixel memory", Help: "Share of a pixel's rate kept each frame; 0.95 remembers about 20 frames.", Kind: "float"},
		{Key: "analysis.hot_threshold", Label: "Hot-pixel threshold", Help: "Detection rate above which a pixel is masked.", Kind: "float"},
		{Key: "analysis.clear_stars", Label: "Clear-sky stars", Help: "Stars in a clear, moonless frame. 0 learns it from recent nights.", Kind: "int"},
		{Key: "analysis.baseline_window", Label: "Baseline frames kept", Kind: "int"},
		{Key: "analysis.baseline_min", Label: "Baseline needs", Help: "Frames before the learnt baseline is used.", Kind: "int"},
		{Key: "analysis.baseline_percentile", Label: "Baseline percentile", Help: "Of recent dark, roof-open star counts. Cloudy frames sit below it.", Kind: "float"},
		{Key: "analysis.black_level", Label: "Black level", Help: "ADU subtracted before the sky brightness.", Kind: "float"},
		{Key: "analysis.sky_zero_point", Label: "Sky zero point", Help: "Turns flux into a relative magnitude. Not calibrated.", Kind: "float"},
		{Key: "analysis.sqm_offset", Label: "SQM offset", Help: "Added to the sky magnitude for N.I.N.A.'s SkyQuality. Set it from a real SQM reading on a clear, moonless night.", Kind: "float"},
	}},
	{"Live view", []field{
		{Key: "live.night_limit", Label: "Night session limit", Help: "Live view pauses stills, so the timelapse loses frames.", Kind: "duration"},
		{Key: "live.block_for", Label: "Then off for", Help: "So a forgotten tab can't hold the stills off.", Kind: "duration"},
		{Key: "live.idle_stop", Label: "Stop after last viewer", Kind: "duration"},
		{Key: "live.max_exposure", Label: "Longest exposure", Kind: "duration"},
		{Key: "live.jpeg_quality", Label: "JPEG quality", Kind: "int"},
	}},
	{"Timelapse and night products", []field{
		{Key: "timelapse.enabled", Label: "Make a timelapse", Kind: "bool"},
		{Key: "timelapse.fps", Label: "Frames per second", Kind: "int"},
		{Key: "timelapse.codec", Label: "Encoder", Kind: "text", Options: []option{{"libx264", "libx264 (software)"}, {"h264_v4l2m2m", "h264_v4l2m2m (Pi hardware)"}}},
		{Key: "timelapse.crf", Label: "libx264 quality (CRF)", Help: "Lower is better and bigger.", Kind: "int"},
		{Key: "timelapse.preset", Label: "libx264 preset", Help: "Slower makes smaller files and takes longer on the Pi.", Kind: "text", Options: presetOptions()},
		{Key: "timelapse.bitrate", Label: "Hardware bitrate", Help: "h264_v4l2m2m only, e.g. 8M.", Kind: "text"},
		{Key: "products.keogram_column", Label: "Keogram column", Help: "Fraction of the width; 0.5 is the centre.", Kind: "float"},
		{Key: "products.keogram_min_width", Label: "Keogram minimum width", Kind: "int"},
		{Key: "products.startrails_max_mean", Label: "Startrails: brightest frame", Help: "Mean, 0–255. Brighter frames are left out.", Kind: "float"},
		{Key: "products.startrails_min_frames", Label: "Startrails: fewest frames", Kind: "int"},
		{Key: "products.jpeg_quality", Label: "Product JPEG quality", Help: "Keogram, startrails and thumbnail.", Kind: "int"},
	}},
	{"Retention", []field{
		{Key: "retention_nights", Label: "Keep frames for", Help: "Nights. 0 keeps them for ever.", Kind: "int"},
		{Key: "product_days", Label: "Keep products for", Help: "Days. 0 keeps them for ever.", Kind: "int"},
	}},
	{"Relay", []field{
		{Key: "relay.every", Label: "Upload a still every", Kind: "duration"},
		{Key: "relay.live_fps", Label: "Live frames per second", Kind: "float"},
		{Key: "relay.live_width", Label: "Live width", Help: "Pixels; frames are scaled down to it.", Kind: "int"},
		{Key: "relay.live_quality", Label: "Live JPEG quality", Kind: "int"},
	}},
	{"N.I.N.A. weather device", []field{
		{Key: "alpaca.discovery", Label: "Answer Alpaca discovery", Kind: "bool"},
		{Key: "alpaca.max_age", Label: "Frame too old after", Kind: "duration"},
		{Key: "openweathermap.interval", Label: "OpenWeatherMap every", Kind: "duration"},
		{Key: "openweathermap.max_age", Label: "Weather too old after", Kind: "duration"},
	}},
}

func presetOptions() []option {
	out := make([]option, len(config.X264Presets))
	for i, p := range config.X264Presets {
		out[i] = option{p, p}
	}
	return out
}

type fieldView struct {
	field
	ID        string
	Value     string
	Default   string
	On, DefOn bool // bool fields
	Changed   bool
	Error     string
}

type groupView struct {
	Title  string
	Fields []fieldView
}

type lockedView struct{ Key, Value string }

func formatValue(flat map[string]any, f field) string {
	switch f.Kind {
	case "rect":
		var parts []string
		for _, k := range []string{"x0", "y0", "x1", "y1"} {
			parts = append(parts, formatScalar(flat[f.Key+"."+k]))
		}
		return strings.Join(parts, ", ")
	case "rects":
		list, _ := flat[f.Key].([]any)
		var lines []string
		for _, item := range list {
			m, _ := item.(map[string]any)
			lines = append(lines, fmt.Sprintf("%s, %s, %s, %s", formatScalar(m["x0"]), formatScalar(m["y0"]), formatScalar(m["x1"]), formatScalar(m["y1"])))
		}
		return strings.Join(lines, "\n")
	}
	return formatScalar(flat[f.Key])
}

func formatScalar(v any) string {
	switch v := v.(type) {
	case nil:
		return ""
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case string:
		// Go prints microseconds as µs; keep the form plain ASCII.
		return strings.ReplaceAll(v, "µs", "us")
	}
	return fmt.Sprint(v)
}

// parseForm turns the submitted form into dotted-key overrides, with an
// error per field it could not read.
func parseForm(r *http.Request) (map[string]any, map[string]string) {
	out, errs := map[string]any{}, map[string]string{}
	for _, g := range settingsGroups {
		for _, f := range g.Fields {
			raw := strings.TrimSpace(r.PostFormValue(f.Key))
			if f.Kind == "bool" {
				out[f.Key] = raw != ""
				continue
			}
			if len(f.Options) > 0 && !slices.ContainsFunc(f.Options, func(o option) bool { return o.Value == raw }) {
				errs[f.Key] = "Choose one of the listed options."
				continue
			}
			switch f.Kind {
			case "text", "url":
				out[f.Key] = raw
			case "int":
				n, err := strconv.Atoi(raw)
				if err != nil {
					errs[f.Key] = "Enter a whole number."
					continue
				}
				out[f.Key] = n
			case "float":
				v, err := strconv.ParseFloat(raw, 64)
				if err != nil {
					errs[f.Key] = "Enter a number."
					continue
				}
				out[f.Key] = v
			case "duration":
				d, err := time.ParseDuration(raw)
				if err != nil || d <= 0 {
					errs[f.Key] = "Enter a positive duration, such as 500ms, 20s or 10m."
					continue
				}
				out[f.Key] = d.String()
			case "rect":
				v, err := parseRect(raw)
				if err != nil {
					errs[f.Key] = err.Error()
					continue
				}
				for k, x := range v {
					out[f.Key+"."+k] = x
				}
			case "rects":
				var list []any
				for i, line := range strings.Split(raw, "\n") {
					if strings.TrimSpace(line) == "" {
						continue
					}
					v, err := parseRect(line)
					if err != nil {
						errs[f.Key] = fmt.Sprintf("Line %d: %v", i+1, err)
						break
					}
					list = append(list, v)
				}
				out[f.Key] = list
			}
		}
	}
	return out, errs
}

func parseRect(s string) (map[string]any, error) {
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\r' })
	if len(parts) != 4 {
		return nil, fmt.Errorf("enter four numbers: x0, y0, x1, y1")
	}
	out := map[string]any{}
	for i, k := range []string{"x0", "y0", "x1", "y1"} {
		v, err := strconv.ParseFloat(parts[i], 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not a number", parts[i])
		}
		out[k] = v
	}
	return out, nil
}

// settingsView builds the page from cur (the values in the form) against
// base (config.json alone).
func (s *Server) settingsView(cur map[string]any, errs map[string]string) []groupView {
	base := config.Flatten(s.Base)
	var out []groupView
	for _, g := range settingsGroups {
		gv := groupView{Title: g.Title}
		for _, f := range g.Fields {
			fv := fieldView{field: f, ID: "f-" + strings.ReplaceAll(f.Key, ".", "-"), Value: formatValue(cur, f), Default: formatValue(base, f), Error: errs[f.Key]}
			if f.Kind == "bool" {
				fv.On, _ = cur[f.Key].(bool)
				fv.DefOn, _ = base[f.Key].(bool)
				fv.Default = map[bool]string{true: "on", false: "off"}[fv.DefOn]
				fv.Changed = fv.On != fv.DefOn
			} else {
				fv.Changed = fv.Value != fv.Default
			}
			gv.Fields = append(gv.Fields, fv)
		}
		out = append(out, gv)
	}
	return out
}

func (s *Server) lockedView() []lockedView {
	flat := config.Flatten(s.Cfg)
	var out []lockedView
	for k, v := range flat {
		if !config.IsLocked(k) {
			continue
		}
		val := formatScalar(v)
		if slices.Contains(config.Secret, k) {
			val = map[bool]string{true: "set", false: "not set"}[val != ""]
		}
		out = append(out, lockedView{k, val})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func (s *Server) renderSettings(w http.ResponseWriter, cur map[string]any, errs map[string]string, problem string) {
	if problem == "" && len(errs) > 0 {
		problem = "Some values need fixing; nothing was saved."
	}
	changed := len(config.Overrides(s.Base, s.Cfg))
	s.render(w, "settings", "base", map[string]any{
		"Title": "Settings", "Groups": s.settingsView(cur, errs), "Locked": s.lockedView(),
		"Problem": problem, "SettingsErr": s.SettingsErr, "Changed": changed, "Path": s.Cfg.SettingsPath(),
	})
}

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request) {
	s.renderSettings(w, config.Flatten(s.Cfg), nil, "")
}

func (s *Server) saveSettings(w http.ResponseWriter, r *http.Request) {
	form, errs := parseForm(r)
	if len(errs) > 0 {
		// Show what was typed, not what is saved.
		cur := config.Flatten(s.Cfg)
		for k, v := range form {
			cur[k] = v
		}
		for k := range errs {
			cur[k] = r.PostFormValue(k)
		}
		s.renderSettings(w, cur, errs, "")
		return
	}
	next, err := config.Apply(s.Base, form)
	if err != nil {
		cur := config.Flatten(s.Cfg)
		for k, v := range form {
			cur[k] = v
		}
		s.renderSettings(w, cur, nil, strings.TrimPrefix(err.Error(), "config: "))
		return
	}
	s.writeSettings(w, config.Overrides(s.Base, next), next)
}

func (s *Server) resetSettings(w http.ResponseWriter, r *http.Request) {
	s.writeSettings(w, map[string]any{}, s.Base)
}

// writeSettings saves overrides and restarts skycam if that changes
// anything. It writes even when nothing changes, which replaces a
// settings.json that was ignored for being bad.
func (s *Server) writeSettings(w http.ResponseWriter, overrides map[string]any, next config.Config) {
	changes := diffKeys(config.Overrides(s.Cfg, next), config.Overrides(next, s.Cfg))
	b, err := config.EncodeSettings(overrides)
	if err == nil {
		err = fsutil.WriteFileAtomic(s.Cfg.SettingsPath(), b)
	}
	if err != nil {
		s.Log.Error("saving settings", "err", err)
		http.Error(w, "Could not save the settings: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if len(changes) == 0 {
		s.render(w, "settings", "base", map[string]any{"Title": "Settings", "Saved": true})
		return
	}
	s.Log.Info("settings saved", "changed", strings.Join(changes, ", "))
	s.render(w, "settings", "base", map[string]any{"Title": "Settings", "Saved": true, "Changes": changes, "Restarts": s.Restart != nil})
	if s.Restart != nil {
		// Let the page reach the browser first.
		time.AfterFunc(500*time.Millisecond, s.Restart)
	}
}

func diffKeys(a, b map[string]any) []string {
	seen := map[string]bool{}
	for k := range a {
		seen[k] = true
	}
	for k := range b {
		seen[k] = true
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// checkSource serves the Check buttons: one read of a roof, rain or
// N.I.N.A. address, without saving it.
func (s *Server) checkSource(w http.ResponseWriter, r *http.Request) {
	// The button posts the whole form; the address is the source's field.
	source := r.PostFormValue("source")
	if source != "roof" && source != "rain" && source != "nina" {
		http.Error(w, "unknown source", http.StatusBadRequest)
		return
	}
	addr := strings.TrimSpace(r.PostFormValue("observatory." + source + "_url"))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if addr == "" {
		fmt.Fprint(w, `<span class="muted">Off (no address).</span>`)
		return
	}
	if u, err := url.Parse(addr); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		fmt.Fprint(w, `<span class="bad">Want an http:// address.</span>`)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	v, err := observatory.Check(ctx, source, addr, s.Cfg.Observatory.Timeout.D())
	if err != nil {
		fmt.Fprintf(w, `<span class="bad">%s</span>`, template.HTMLEscapeString(err.Error()))
		return
	}
	fmt.Fprintf(w, `<span class="ok">Answered: %s</span>`, template.HTMLEscapeString(v))
}

// localOnly keeps the settings page to the home network. The Pi has no
// port open to the internet, so this is belt and braces against that
// changing, plus two browser attacks: a web page posting a form to the
// Pi (the Origin check), and a page whose domain is re-pointed at the
// Pi's address (the Host check: only an IP or a local name is accepted).
func localOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !privateClient(r.RemoteAddr) || !localHost(r.Host) {
			http.Error(w, "The settings page only answers on the home network.", http.StatusForbidden)
			return
		}
		if r.Method == http.MethodPost && !sameOrigin(r) {
			http.Error(w, "Cross-site request refused.", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func privateClient(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	a = a.Unmap()
	if a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() {
		return true
	}
	// Home IPv6 addresses are global, so a client on the home network can
	// only be told apart by sharing a subnet with one of the Pi's own.
	for _, p := range localPrefixes() {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// localPrefixes is the subnets of the Pi's own interfaces; tests replace it.
var localPrefixes = func() []netip.Prefix {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []netip.Prefix
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip, ok := netip.AddrFromSlice(n.IP)
		if !ok {
			continue
		}
		bits, _ := n.Mask.Size()
		out = append(out, netip.PrefixFrom(ip.Unmap(), bits).Masked())
	}
	return out
}

func localHost(hostport string) bool {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	host = strings.ToLower(strings.Trim(host, "[]"))
	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}
	if host == "localhost" || !strings.Contains(host, ".") {
		return true
	}
	for _, suffix := range []string{".local", ".lan", ".home.arpa", ".internal"} {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

func sameOrigin(r *http.Request) bool {
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		return err == nil && u.Host == r.Host
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		return site == "same-origin" || site == "none"
	}
	return true // not a browser
}
