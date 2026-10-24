package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
)

// The settings page can't write /etc/skycam/config.json (the service runs
// with ProtectSystem=strict), so it keeps its changes in
// <data_dir>/settings.json, read over config.json at start-up. That file
// holds only the keys that differ from config.json, by their dotted names.

// Locked keys are never taken from settings.json: secrets, paths, the
// listeners, and the contracts other tools depend on. They are changed in
// config.json only. A key ending in "." locks everything under it.
var Locked = []string{
	"listen", "data_dir", "images_user", "images_password",
	"camera.driver", "camera.sdk_path", "camera.fake_dir", "camera.fake_realtime",
	"analysis.band_top", "analysis.band_bottom", // skyq measures this band
	"timelapse.ffmpeg", // a program to run
	"s3.",
	"relay.url", "relay.token",
	"alpaca.listen",
	"openweathermap.api_key",
	"monitor_url", "monitor_api_key",
}

// Secret keys are shown only as set or not set.
var Secret = []string{
	"images_password", "s3.access_key_id", "s3.secret_access_key",
	"relay.token", "openweathermap.api_key", "monitor_api_key",
}

// IsLocked reports whether key may only be set in config.json.
func IsLocked(key string) bool {
	for _, l := range Locked {
		if key == l || (strings.HasSuffix(l, ".") && strings.HasPrefix(key, l)) {
			return true
		}
	}
	return false
}

// SettingsPath is the settings page's overrides file.
func (c Config) SettingsPath() string { return filepath.Join(c.DataDir, "settings.json") }

// Flatten returns c's JSON leaves by dotted key ("exposure.night.max_exposure").
// Lists, such as exposure.exclude, are one leaf. Values are as encoding/json
// decodes them: string, float64, bool or []any.
func Flatten(c Config) map[string]any {
	b, _ := json.Marshal(c)
	var m map[string]any
	json.Unmarshal(b, &m)
	return flattenMap(m)
}

func flattenMap(m map[string]any) map[string]any {
	out := map[string]any{}
	var walk func(prefix string, m map[string]any)
	walk = func(prefix string, m map[string]any) {
		for k, v := range m {
			if sub, ok := v.(map[string]any); ok {
				walk(prefix+k+".", sub)
				continue
			}
			out[prefix+k] = v
		}
	}
	walk("", m)
	return out
}

func unflatten(flat map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range flat {
		parts := strings.Split(k, ".")
		m := out
		for _, p := range parts[:len(parts)-1] {
			sub, ok := m[p].(map[string]any)
			if !ok {
				sub = map[string]any{}
				m[p] = sub
			}
			m = sub
		}
		m[parts[len(parts)-1]] = v
	}
	return out
}

// Apply returns base with the overrides (dotted keys) laid over it,
// validated. Locked keys in overrides are ignored; unknown keys are an
// error.
func Apply(base Config, overrides map[string]any) (Config, error) {
	flat := Flatten(base)
	var unknown []string
	for k, v := range overrides {
		if IsLocked(k) {
			continue
		}
		if _, ok := flat[k]; !ok {
			unknown = append(unknown, k)
			continue
		}
		flat[k] = v
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return base, fmt.Errorf("unknown settings: %s", strings.Join(unknown, ", "))
	}
	b, err := json.Marshal(unflatten(flat))
	if err != nil {
		return base, err
	}
	cfg := Default()
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return base, err
	}
	if err := cfg.validate(); err != nil {
		return base, err
	}
	return cfg, nil
}

// Overrides returns the keys where cur differs from base: what
// settings.json needs to hold to turn base into cur.
func Overrides(base, cur Config) map[string]any {
	fb, fc := Flatten(base), Flatten(cur)
	out := map[string]any{}
	for k, v := range fc {
		if !IsLocked(k) && !reflect.DeepEqual(fb[k], v) {
			out[k] = v
		}
	}
	return out
}

// ReadSettings reads settings.json as dotted keys; a missing file is no
// overrides.
func ReadSettings(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return flattenMap(m), nil
}

// EncodeSettings is the settings.json form of overrides: nested, so it
// reads like config.json.
func EncodeSettings(overrides map[string]any) ([]byte, error) {
	b, err := json.MarshalIndent(unflatten(overrides), "", "  ")
	return append(b, '\n'), err
}

// WithSettings lays base's settings.json over it. On any error it returns
// base unchanged with the error, so a bad save can never stop the camera.
func WithSettings(base Config) (Config, error) {
	o, err := ReadSettings(base.SettingsPath())
	if err != nil {
		return base, err
	}
	if len(o) == 0 {
		return base, nil
	}
	cfg, err := Apply(base, o)
	if err != nil {
		return base, fmt.Errorf("%s ignored: %w", base.SettingsPath(), err)
	}
	return cfg, nil
}
