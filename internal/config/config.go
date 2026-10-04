package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"time"
)

type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return errors.New("duration must be a string such as 15s or 30m")
	}
	v, err := time.ParseDuration(s)
	*d = Duration(v)
	return err
}
func (d Duration) Value() time.Duration { return time.Duration(d) }

type Emby struct {
	URL            string `json:"url"`
	Username       string `json:"username"`
	PasswordEnv    string `json:"password_env"`
	APIKeyEnv      string `json:"api_key_env"`
	AccessTokenEnv string `json:"access_token_env"`
	UserID         string `json:"user_id"`
	DeviceID       string `json:"device_id"`
	ItemID         string `json:"item_id"`
	MediaSourceID  string `json:"media_source_id"`
	StreamPath     string `json:"stream_path"`
	APIProxyURL    string `json:"api_proxy_url"`
	Password       string `json:"-"`
	Token          string `json:"-"`
}
type Mihomo struct {
	URL           string `json:"url"`
	SecretEnv     string `json:"secret_env"`
	Group         string `json:"group"`
	ProbeGroup    string `json:"probe_group"`
	ProbeProxyURL string `json:"probe_proxy_url"`
	Secret        string `json:"-"`
}
type Probe struct {
	Interval          Duration `json:"interval"`
	Timeout           Duration `json:"timeout"`
	MaxBytes          int64    `json:"max_bytes"`
	MinBytes          int64    `json:"min_bytes"`
	Offset            int64    `json:"offset"`
	Samples           int      `json:"samples"`
	SwitchImprovement float64  `json:"switch_improvement"`
	MinHold           Duration `json:"min_hold"`
}
type Config struct {
	Emby     Emby   `json:"emby"`
	Mihomo   Mihomo `json:"mihomo"`
	Probe    Probe  `json:"probe"`
	StateDir string `json:"state_dir"`
}

func Defaults() Config {
	return Config{StateDir: "state", Emby: Emby{DeviceID: "emby-prober"}, Mihomo: Mihomo{Group: "Emby Prober", ProbeGroup: "Emby Probe Test"}, Probe: Probe{Interval: Duration(30 * time.Minute), Timeout: Duration(15 * time.Second), MaxBytes: 64 << 20, MinBytes: 256 << 10, Offset: 1 << 20, Samples: 2, SwitchImprovement: 0.2, MinHold: Duration(5 * time.Minute)}}
}
func Load(path string) (Config, error) {
	c := Defaults()
	f, err := os.Open(path)
	if err != nil {
		return c, err
	}
	defer f.Close()
	d := json.NewDecoder(f)
	d.DisallowUnknownFields()
	if err = d.Decode(&c); err != nil {
		return c, err
	}
	if err = d.Decode(new(any)); err != io.EOF {
		return c, errors.New("config must contain one JSON object")
	}
	if err = c.Validate(); err != nil {
		return c, err
	}
	for _, s := range []struct {
		name string
		dst  *string
	}{{c.Emby.PasswordEnv, &c.Emby.Password}, {c.Emby.APIKeyEnv, &c.Emby.Token}, {c.Emby.AccessTokenEnv, &c.Emby.Token}, {c.Mihomo.SecretEnv, &c.Mihomo.Secret}} {
		if s.name == "" {
			continue
		}
		v, ok := os.LookupEnv(s.name)
		if !ok || v == "" {
			return c, fmt.Errorf("environment variable %s is missing or empty", s.name)
		}
		*s.dst = v
	}
	return c, nil
}
func validURL(raw string, proxy bool) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Fragment != "" {
		return false
	}
	if proxy {
		return (u.Scheme == "http" || u.Scheme == "https" || u.Scheme == "socks5" || u.Scheme == "socks5h") && u.RawQuery == "" && (u.Path == "" || u.Path == "/")
	}
	return (u.Scheme == "http" || u.Scheme == "https") && u.User == nil && u.RawQuery == ""
}
func (c Config) Validate() error {
	if !validURL(c.Emby.URL, false) || !validURL(c.Mihomo.URL, false) {
		return errors.New("emby.url and mihomo.url must be HTTP(S) base URLs without credentials or query strings")
	}
	if !validURL(c.Mihomo.ProbeProxyURL, true) {
		return errors.New("mihomo.probe_proxy_url must specify a dedicated HTTP(S) or SOCKS5 proxy")
	}
	if c.Emby.APIProxyURL != "" && !validURL(c.Emby.APIProxyURL, true) {
		return errors.New("invalid emby.api_proxy_url")
	}
	modes := 0
	if c.Emby.Username != "" || c.Emby.PasswordEnv != "" {
		modes++
		if c.Emby.Username == "" || c.Emby.PasswordEnv == "" {
			return errors.New("username and password_env must be supplied together")
		}
	}
	if c.Emby.APIKeyEnv != "" {
		modes++
	}
	if c.Emby.AccessTokenEnv != "" {
		modes++
	}
	if modes != 1 {
		return errors.New("choose exactly one Emby auth mode: username/password_env, api_key_env, or access_token_env")
	}
	if c.Emby.APIKeyEnv != "" && c.Emby.UserID == "" {
		return errors.New("API key mode requires user_id")
	}
	if c.Mihomo.Group == "" || c.Mihomo.ProbeGroup == "" || c.Mihomo.Group == c.Mihomo.ProbeGroup {
		return errors.New("managed group and probe group must be nonempty and different")
	}
	if c.StateDir == "" || c.Emby.DeviceID == "" {
		return errors.New("state_dir and emby.device_id must not be empty")
	}
	if c.Probe.Interval.Value() <= 0 || c.Probe.Timeout.Value() <= 0 || c.Probe.MinHold.Value() < 0 {
		return errors.New("invalid probe durations")
	}
	if c.Probe.MinBytes < 1 || c.Probe.MaxBytes < c.Probe.MinBytes || c.Probe.MaxBytes > 1<<40 || c.Probe.Offset < 0 || c.Probe.Offset > 1<<50 {
		return errors.New("invalid probe byte limits")
	}
	if c.Probe.Samples < 1 || c.Probe.Samples > 10 || c.Probe.SwitchImprovement < 0 {
		return errors.New("samples must be 1..10 and switch_improvement must be nonnegative")
	}
	if c.Emby.StreamPath != "" {
		u, err := url.Parse(c.Emby.StreamPath)
		if err != nil || u.IsAbs() || u.Host != "" || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("stream_path must be a relative path without queries, e.g. Videos/{item_id}/original.mkv")
		}
	}
	return nil
}
