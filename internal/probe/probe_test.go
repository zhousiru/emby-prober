package probe

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/zhousiru/emby-prober/internal/config"
	"github.com/zhousiru/emby-prober/internal/emby"
)

func cfg() config.Probe {
	return config.Probe{Timeout: config.Duration(time.Second), MaxBytes: 1024, MinBytes: 16, Offset: 100}
}
func TestRangeAndInvalidResponses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		cr, ct string
		size   int
		valid  bool
	}{{"valid", 206, "bytes 100-1123/10000", "video/x-matroska", 1024, true}, {"403", 403, "", "text/html", 100, false}, {"ignored range", 200, "", "video/mp4", 1024, false}, {"wrong offset", 206, "bytes 0-1023/10000", "video/mp4", 1024, false}, {"error document", 206, "bytes 100-1123/10000", "application/json", 1024, false}, {"truncated", 206, "bytes 100-1123/10000", "video/mp4", 200, false}} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Range") != "bytes=100-1123" {
					t.Error("incorrect range")
				}
				w.Header().Set("Content-Range", tc.cr)
				w.Header().Set("Content-Type", tc.ct)
				w.WriteHeader(tc.status)
				w.Write(make([]byte, tc.size))
			}))
			defer srv.Close()
			r := Measure(context.Background(), "", emby.Target{URL: srv.URL, Headers: make(http.Header)}, cfg())
			if r.Valid != tc.valid {
				t.Fatalf("unexpected result: %+v", r)
			}
		})
	}
}
func TestDeadlineCountsDeliveredBytes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 100-1123/10000")
		w.Header().Set("Content-Length", "1024")
		w.Header().Set("Content-Type", "video/mp4")
		w.WriteHeader(206)
		w.Write(make([]byte, 512))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	c := cfg()
	c.Timeout = config.Duration(100 * time.Millisecond)
	r := Measure(context.Background(), "", emby.Target{URL: srv.URL, Headers: make(http.Header)}, c)
	if !r.Valid || !r.TimedOut || r.Bytes != 512 || r.Seconds < 0.08 {
		t.Fatalf("deadline result: %+v", r)
	}
}
func TestProxyActuallyUsedAndRedirectStripsToken(t *testing.T) {
	hits := 0
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Host == "emby.invalid" {
			if r.Header.Get("X-Emby-Token") != "private" {
				t.Error("missing initial token")
			}
			http.Redirect(w, r, "http://cdn.invalid/media?signed=yes", 302)
			return
		}
		if r.URL.Host != "cdn.invalid" || r.URL.Query().Get("signed") != "yes" {
			t.Error("wrong redirect")
		}
		if r.Header.Get("X-Emby-Token") != "" || r.Header.Get("X-Emby-Authorization") != "" {
			t.Error("token leaked to CDN")
		}
		w.Header().Set("Content-Range", "bytes 100-1123/10000")
		w.Header().Set("Content-Type", "video/mp4")
		w.WriteHeader(206)
		fmt.Fprint(w, string(make([]byte, 1024)))
	}))
	defer proxy.Close()
	h := make(http.Header)
	h.Set("X-Emby-Token", "private")
	h.Set("X-Emby-Authorization", "private identity")
	r := Measure(context.Background(), proxy.URL, emby.Target{URL: "http://emby.invalid/media", Headers: h}, cfg())
	if !r.Valid || hits != 2 {
		t.Fatalf("proxy/redirect failed: %+v, hits=%d", r, hits)
	}
}
