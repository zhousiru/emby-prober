// Package emby implements server login and static video discovery. It never logs
// request URLs, access tokens, response bodies, or passwords.
package emby

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zhousiru/emby-prober/internal/config"
	"github.com/zhousiru/emby-prober/internal/storage"
)

type HTTPError struct{ Status int }

func (e *HTTPError) Error() string { return fmt.Sprintf("Emby HTTP %d", e.Status) }
func Unauthorized(err error) bool  { var e *HTTPError; return errors.As(err, &e) && e.Status == 401 }

type Session struct {
	URL      string `json:"url"`
	Username string `json:"username"`
	DeviceID string `json:"device_id"`
	Token    string `json:"token"`
	UserID   string `json:"user_id"`
	ServerID string `json:"server_id"`
}
type Client struct {
	cfg       config.Emby
	base      *url.URL
	http      *http.Client
	session   Session
	cachePath string
	refreshed bool
}

// NewTransport explicitly disables environment proxies. Every media probe gets
// its own transport so a connection to the previous node cannot be reused.
func NewTransport(proxyURL string) (*http.Transport, error) {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	if proxyURL != "" {
		u, err := url.Parse(proxyURL)
		if err != nil {
			return nil, errors.New("invalid proxy URL")
		}
		t.Proxy = http.ProxyURL(u)
	}
	t.DisableKeepAlives = true
	t.ResponseHeaderTimeout = 30 * time.Second
	return t, nil
}
func SameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

// RedirectPolicy permits CDN redirects but removes Emby credentials when the
// origin changes. Signed query parameters supplied by the CDN are preserved.
func RedirectPolicy(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("too many redirects")
	}
	if len(via) > 0 && via[len(via)-1].URL.Scheme == "https" && req.URL.Scheme != "https" {
		return errors.New("refusing HTTPS downgrade")
	}
	if len(via) > 0 && !SameOrigin(req.URL, via[0].URL) {
		req.Header.Del("X-Emby-Token")
		req.Header.Del("X-Emby-Authorization")
		req.Header.Del("Authorization")
		req.Header.Del("Cookie")
	}
	return nil
}
func New(c config.Emby, stateDir string) (*Client, error) {
	b, err := url.Parse(strings.TrimRight(c.URL, "/"))
	if err != nil {
		return nil, errors.New("invalid Emby URL")
	}
	t, err := NewTransport(c.APIProxyURL)
	if err != nil {
		return nil, err
	}
	client := &Client{cfg: c, base: b, http: &http.Client{Transport: t, Timeout: 30 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) > 0 && !SameOrigin(req.URL, via[0].URL) {
			return errors.New("Emby API redirected to another origin")
		}
		return RedirectPolicy(req, via)
	}}, cachePath: filepath.Join(stateDir, "session.json")}
	client.session = Session{URL: b.String(), Username: c.Username, DeviceID: c.DeviceID, Token: c.Token, UserID: c.UserID}
	if c.Username != "" {
		raw, e := os.ReadFile(client.cachePath)
		if e == nil {
			var s Session
			if json.Unmarshal(raw, &s) == nil && s.URL == b.String() && s.Username == c.Username && s.DeviceID == c.DeviceID && (c.UserID == "" || c.UserID == s.UserID) {
				client.session = s
			}
		} else if !os.IsNotExist(e) {
			return nil, errors.New("cannot read Emby session cache")
		}
	}
	return client, nil
}
func (c *Client) Close()      { c.http.CloseIdleConnections() }
func (c *Client) BeginRound() { c.refreshed = false }
func (c *Client) endpoint(path string) string {
	return strings.TrimRight(c.base.String(), "/") + "/" + strings.TrimLeft(path, "/")
}
func (c *Client) identity() string {
	return fmt.Sprintf("Emby Client=%q, Device=%q, DeviceId=%q, Version=%q", "emby-prober", "daemon", c.cfg.DeviceID, "0.1.0")
}
func (c *Client) Headers() http.Header {
	h := make(http.Header)
	h.Set("X-Emby-Authorization", c.identity())
	h.Set("X-Emby-Token", c.session.Token)
	h.Set("User-Agent", "emby-prober/0.1.0")
	return h
}
func (c *Client) raw(ctx context.Context, method, path string, body any, out any, token bool) error {
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint(path), bytes.NewReader(data))
	if err != nil {
		return errors.New("invalid Emby request")
	}
	req.Header.Set("X-Emby-Authorization", c.identity())
	if token {
		req.Header.Set("X-Emby-Token", c.session.Token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("Emby API request failed (network, timeout or redirect)")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &HTTPError{resp.StatusCode}
	}
	if out != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out); err != nil {
			return errors.New("invalid Emby API response")
		}
	}
	return nil
}
func (c *Client) login(ctx context.Context) error {
	if c.cfg.Username == "" {
		return errors.New("Emby token rejected; replace the configured API key/access token")
	}
	var result struct {
		AccessToken string
		ServerID    string
		User        struct{ ID string }
	}
	err := c.raw(ctx, http.MethodPost, "Users/AuthenticateByName", map[string]string{"Username": c.cfg.Username, "Pw": c.cfg.Password}, &result, false)
	if err != nil {
		return fmt.Errorf("Emby login: %w", err)
	}
	if result.AccessToken == "" || result.User.ID == "" {
		return errors.New("Emby login returned an empty token or user ID")
	}
	c.session.Token = result.AccessToken
	c.session.UserID = result.User.ID
	c.session.ServerID = result.ServerID
	if err := storage.WriteJSON(c.cachePath, c.session); err != nil {
		return errors.New("cannot save Emby session cache")
	}
	return nil
}

// Reauthenticate is bounded to once per round. A 403 is deliberately NOT a
// signal to log in: it may be a blocked exit IP or a permissions policy.
func (c *Client) Reauthenticate(ctx context.Context) error {
	if c.refreshed {
		return errors.New("Emby authentication still failing after one login attempt")
	}
	c.refreshed = true
	return c.login(ctx)
}
func (c *Client) Ensure(ctx context.Context) error {
	if c.session.Token == "" {
		if err := c.Reauthenticate(ctx); err != nil {
			return err
		}
	}
	if c.session.UserID == "" {
		var user struct{ ID string }
		if err := c.do(ctx, http.MethodGet, "Users/Me", nil, &user); err != nil {
			return err
		}
		if user.ID == "" {
			return errors.New("Emby did not return a user ID; set user_id")
		}
		c.session.UserID = user.ID
	}
	return nil
}
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	err := c.raw(ctx, method, path, body, out, true)
	if !Unauthorized(err) {
		return err
	}
	if err = c.Reauthenticate(ctx); err != nil {
		return err
	}
	return c.raw(ctx, method, path, body, out, true)
}
func (c *Client) Item(ctx context.Context) (string, error) {
	if err := c.Ensure(ctx); err != nil {
		return "", err
	}
	if c.cfg.ItemID != "" {
		return c.cfg.ItemID, nil
	}
	q := url.Values{"Recursive": {"true"}, "IncludeItemTypes": {"Movie,Episode"}, "IsFolder": {"false"}, "Limit": {"20"}, "SortBy": {"DateCreated"}, "SortOrder": {"Descending"}, "Fields": {"MediaSources"}}
	var items struct {
		Items []struct {
			ID           string
			MediaSources []Source
		}
	}
	err := c.do(ctx, http.MethodGet, "Users/"+url.PathEscape(c.session.UserID)+"/Items?"+q.Encode(), nil, &items)
	if err != nil {
		return "", err
	}
	for _, i := range items.Items {
		for _, s := range i.MediaSources {
			if s.Size >= 64<<20 && !s.IsInfiniteStream && !s.RequiresOpening {
				return i.ID, nil
			}
		}
	}
	return "", errors.New("no suitable video in the latest 20 items; set emby.item_id explicitly")
}

type Source struct {
	ID                   string
	Size                 int64
	DirectStreamURL      string
	SupportsDirectStream bool
	SupportsDirectPlay   bool
	IsInfiniteStream     bool
	RequiresOpening      bool
	RequiredHTTPHeaders  map[string]string
}
type Target struct {
	URL     string
	Headers http.Header
	Size    int64
}

// Target obtains fresh playback information for every sample instead of caching
// potentially expired signed URLs. It never starts a transcode or live stream.
func (c *Client) Target(ctx context.Context, item string) (Target, error) {
	q := url.Values{"UserId": {c.session.UserID}, "IsPlayback": {"false"}, "AutoOpenLiveStream": {"false"}}
	if c.cfg.MediaSourceID != "" {
		q.Set("MediaSourceId", c.cfg.MediaSourceID)
	}
	var info struct {
		MediaSources  []Source
		PlaySessionID string
		ErrorCode     string
	}
	if err := c.do(ctx, http.MethodGet, "Items/"+url.PathEscape(item)+"/PlaybackInfo?"+q.Encode(), nil, &info); err != nil {
		return Target{}, err
	}
	if info.ErrorCode != "" {
		return Target{}, errors.New("Emby refused playback information")
	}
	var source *Source
	for i := range info.MediaSources {
		s := &info.MediaSources[i]
		if c.cfg.MediaSourceID != "" && s.ID != c.cfg.MediaSourceID {
			continue
		}
		if !s.IsInfiniteStream && !s.RequiresOpening && s.ID != "" && (s.SupportsDirectPlay || s.SupportsDirectStream || s.DirectStreamURL != "") {
			source = s
			break
		}
	}
	if source == nil {
		return Target{}, errors.New("no static video source available (transcoding/live streams are not probed)")
	}
	raw := source.DirectStreamURL
	if c.cfg.StreamPath != "" {
		raw = c.endpoint(strings.ReplaceAll(c.cfg.StreamPath, "{item_id}", url.PathEscape(item)))
	}
	if raw == "" {
		raw = c.endpoint("Videos/" + url.PathEscape(item) + "/stream")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return Target{}, errors.New("invalid stream URL")
	}
	if !u.IsAbs() {
		// Emby may return /videos/... without the /emby deployment prefix.
		if strings.HasPrefix(u.Path, "/") && c.base.Path != "" && !strings.HasPrefix(strings.ToLower(u.Path), strings.ToLower(c.base.Path)+"/") {
			u.Path = strings.TrimRight(c.base.Path, "/") + u.Path
		}
		base := *c.base
		base.Path = strings.TrimRight(base.Path, "/") + "/"
		u = base.ResolveReference(u)
	}
	if u.User != nil || (c.base.Scheme == "https" && u.Scheme != "https") {
		return Target{}, errors.New("unsafe stream URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return Target{}, errors.New("stream is not HTTP(S)")
	}
	if strings.Contains(strings.ToLower(u.Path), ".m3u8") || strings.HasSuffix(strings.ToLower(u.Path), ".mpd") {
		return Target{}, errors.New("segmented streams cannot be byte-range probed")
	}
	headers := make(http.Header)
	for k, v := range source.RequiredHTTPHeaders {
		headers.Set(k, v)
	}
	if SameOrigin(u, c.base) {
		for k, v := range c.Headers() {
			headers[k] = v
		}
		p := u.Query()
		p.Set("Static", "true")
		p.Set("MediaSourceId", source.ID)
		p.Set("DeviceId", c.cfg.DeviceID)
		if info.PlaySessionID != "" {
			p.Set("PlaySessionId", info.PlaySessionID)
		}
		// Some reverse proxies require the query form instead of X-Emby-Token.
		p.Set("api_key", c.session.Token)
		u.RawQuery = p.Encode()
	}
	return Target{URL: u.String(), Headers: headers, Size: source.Size}, nil
}
func (c *Client) IsServerURL(raw string) bool {
	u, e := url.Parse(raw)
	return e == nil && SameOrigin(u, c.base)
}
