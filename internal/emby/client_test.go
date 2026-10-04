package emby

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhousiru/emby-prober/internal/config"
)

func TestCachedTokenReauthAnd403(t *testing.T) {
	loginCount := 0
	token := "token1"
	deny := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/emby/Users/AuthenticateByName":
			loginCount++
			var in map[string]string
			json.NewDecoder(r.Body).Decode(&in)
			if r.Method != "POST" || in["Username"] != "u" || in["Pw"] != "p" || !strings.Contains(r.Header.Get("X-Emby-Authorization"), "DeviceId=") {
				t.Error("incorrect login request")
			}
			fmt.Fprintf(w, `{"AccessToken":%q,"ServerId":"server","User":{"Id":"uid"}}`, token)
		case "/emby/Items/item/PlaybackInfo":
			if deny {
				w.WriteHeader(403)
				return
			}
			if r.Header.Get("X-Emby-Token") != token {
				w.WriteHeader(401)
				return
			}
			fmt.Fprint(w, `{"MediaSources":[{"Id":"source","Size":100000000,"SupportsDirectPlay":true}],"PlaySessionId":"play"}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	cfg := config.Emby{URL: srv.URL + "/emby", Username: "u", Password: "p", DeviceID: "device", ItemID: "item"}
	dir := t.TempDir()
	c, err := New(cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err = c.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if loginCount != 1 {
		t.Fatal(loginCount)
	}
	stat, err := os.Stat(filepath.Join(dir, "session.json"))
	if err != nil || stat.Mode().Perm() != 0600 {
		t.Fatal("session permissions", err)
	}
	c2, err := New(cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if err = c2.Ensure(context.Background()); err != nil || loginCount != 1 {
		t.Fatal("cached token not reused", err)
	}
	token = "token2"
	c2.BeginRound()
	target, err := c2.Target(context.Background(), "item")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(target.URL)
	if loginCount != 2 || u.Query().Get("api_key") != "token2" || u.Query().Get("Static") != "true" || u.Path != "/emby/Videos/item/stream" {
		t.Fatalf("reauth/static stream failed: login count %d", loginCount)
	}
	deny = true
	c2.BeginRound()
	_, err = c2.Target(context.Background(), "item")
	if err == nil || loginCount != 2 {
		t.Fatal("403 must not trigger login", err)
	}
	cfg.URL = srv.URL + "/different"
	c3, _ := New(cfg, dir)
	defer c3.Close()
	if c3.session.Token != "" {
		t.Fatal("cache used for another server")
	}
	cfg.URL = srv.URL + "/emby"
	cfg.Username = "another-user"
	c4, _ := New(cfg, dir)
	defer c4.Close()
	if c4.session.Token != "" {
		t.Fatal("cache used for another user")
	}
}
func TestReauthenticationBounded(t *testing.T) {
	count := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "AuthenticateByName") {
			count++
			fmt.Fprint(w, `{"AccessToken":"bad","User":{"Id":"u"}}`)
		} else {
			w.WriteHeader(401)
		}
	}))
	defer srv.Close()
	c, _ := New(config.Emby{URL: srv.URL, Username: "u", Password: "p", DeviceID: "d"}, t.TempDir())
	defer c.Close()
	for i := 0; i < 3; i++ {
		c.Ensure(context.Background())
		c.Target(context.Background(), "i")
	}
	if count != 1 {
		t.Fatalf("login storm: %d", count)
	}
}
func TestStreamURLResolution(t *testing.T) {
	for _, raw := range []string{"/videos/i/stream.mkv?signature=s", "videos/i/stream.mkv?signature=s", "/emby/videos/i/stream.mkv?signature=s"} {
		t.Run(raw, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"MediaSources":[{"Id":"s","DirectStreamUrl":%q}]}`, raw)
			}))
			defer srv.Close()
			c, _ := New(config.Emby{URL: srv.URL + "/emby", Token: "secret", UserID: "u", DeviceID: "d"}, t.TempDir())
			defer c.Close()
			target, err := c.Target(context.Background(), "i")
			if err != nil {
				t.Fatal(err)
			}
			u, _ := url.Parse(target.URL)
			if u.Path != "/emby/videos/i/stream.mkv" || u.Query().Get("signature") != "s" {
				t.Fatalf("wrong stream path %s", u.Path)
			}
		})
	}
}
func TestAPIRejectsCrossOriginLoginRedirect(t *testing.T) {
	hit := false
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	defer destination.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	c, _ := New(config.Emby{URL: srv.URL, Username: "u", Password: "p", DeviceID: "d"}, t.TempDir())
	defer c.Close()
	if err := c.Ensure(context.Background()); err == nil {
		t.Fatal("expected redirect error")
	}
	if hit {
		t.Fatal("login password forwarded to another origin")
	}
}
