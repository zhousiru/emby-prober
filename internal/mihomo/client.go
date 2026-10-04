package mihomo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Proxy struct {
	Name string   `json:"name"`
	Type string   `json:"type"`
	Now  string   `json:"now"`
	All  []string `json:"all"`
}
type Client struct {
	base, secret string
	http         *http.Client
}

func New(base, secret string) *Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	return &Client{strings.TrimRight(base, "/"), secret, &http.Client{Transport: t, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}
func (c *Client) call(ctx context.Context, method, path string, body, out any) error {
	var b []byte
	var err error
	if body != nil {
		b, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(b))
	if err != nil {
		return errors.New("invalid Mihomo request")
	}
	if c.secret != "" {
		req.Header.Set("Authorization", "Bearer "+c.secret)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("Mihomo controller request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Mihomo controller HTTP %d", resp.StatusCode)
	}
	if out != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(out); err != nil {
			return errors.New("invalid Mihomo controller response")
		}
	}
	return nil
}
func (c *Client) Proxies(ctx context.Context) (map[string]Proxy, error) {
	var out struct {
		Proxies map[string]Proxy `json:"proxies"`
	}
	err := c.call(ctx, http.MethodGet, "/proxies", nil, &out)
	return out.Proxies, err
}
func (c *Client) Group(ctx context.Context, name string) (Proxy, error) {
	var out Proxy
	err := c.call(ctx, http.MethodGet, "/proxies/"+url.PathEscape(name), nil, &out)
	return out, err
}
func (c *Client) Select(ctx context.Context, group, node string) error {
	return c.call(ctx, http.MethodPut, "/proxies/"+url.PathEscape(group), map[string]string{"name": node}, nil)
}

// Candidates refuses nested or automatic groups: a moving underlying proxy
// would make per-node bandwidth measurements meaningless.
func Candidates(all map[string]Proxy, group, testGroup string) ([]string, error) {
	g, ok := all[group]
	if !ok || g.Type != "Selector" {
		return nil, fmt.Errorf("%s must exist and be type select", group)
	}
	t, ok := all[testGroup]
	if !ok || t.Type != "Selector" {
		return nil, fmt.Errorf("%s must exist and be type select", testGroup)
	}
	available := map[string]bool{}
	for _, n := range t.All {
		available[n] = true
	}
	seen := map[string]bool{}
	var nodes []string
	for _, n := range g.All {
		p, ok := all[n]
		if !ok {
			return nil, fmt.Errorf("candidate %s is missing", n)
		}
		if p.All != nil {
			return nil, fmt.Errorf("candidate %s is a nested group; use individual nodes", n)
		}
		switch strings.ToLower(p.Type) {
		case "direct", "reject", "rejectdrop", "pass", "compatible", "dns":
			continue
		}
		if !available[n] {
			return nil, fmt.Errorf("candidate %s is absent from test group", n)
		}
		if !seen[n] {
			nodes = append(nodes, n)
			seen[n] = true
		}
	}
	if len(nodes) == 0 {
		return nil, errors.New("managed group contains no proxy nodes")
	}
	return nodes, nil
}
