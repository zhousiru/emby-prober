package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhousiru/emby-prober/internal/config"
	"github.com/zhousiru/emby-prober/internal/mihomo"
)

type logWriter func([]byte) (int, error)

func (w logWriter) Write(p []byte) (int, error) { return w(p) }

func TestCronProbesOnStartupThenWaits(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("controller_failure=%t", fail), func(t *testing.T) {
			var rounds atomic.Int32
			controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path == "/proxies" {
					rounds.Add(1)
					if fail {
						w.WriteHeader(503)
						return
					}
					json.NewEncoder(w).Encode(map[string]any{"proxies": map[string]mihomo.Proxy{
						"node":            {Name: "node", Type: "Shadowsocks"},
						"Emby Prober":     {Name: "Emby Prober", Type: "Selector", All: []string{"node"}, Now: "node"},
						"Emby Probe Test": {Name: "Emby Probe Test", Type: "Selector", All: []string{"node"}, Now: "node"},
					}})
					return
				}
				// A completed round with no available nodes needs no Emby request.
				if req.URL.Path == "/proxies/node/delay" {
					w.WriteHeader(503)
					return
				}
				t.Error("unexpected request", req.URL.Path)
				w.WriteHeader(404)
			}))
			defer controller.Close()
			waiting := make(chan struct{}, 2)
			logger := slog.New(slog.NewTextHandler(logWriter(func(p []byte) (int, error) {
				if strings.Contains(string(p), "waiting for scheduled probe") {
					waiting <- struct{}{}
				}
				return len(p), nil
			}), nil))
			cfg := config.Defaults()
			cfg.StateDir = t.TempDir()
			cfg.Probe.Cron = "CRON_TZ=Asia/Shanghai 0 8,20,22 * * *"
			r := &Runner{cfg: cfg, mihomo: mihomo.New(controller.URL, ""), log: logger}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- r.Run(ctx, false) }()
			select {
			case <-waiting:
			case <-time.After(2 * time.Second):
				t.Fatal("startup round did not reach scheduled wait")
			}
			if got := rounds.Load(); got != 1 {
				t.Fatalf("startup rounds=%d", got)
			}
			if _, err := os.Stat(filepath.Join(cfg.StateDir, "status.json")); err != nil {
				t.Fatal(err)
			}
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("did not cancel scheduled wait")
			}
			if got := rounds.Load(); got != 1 {
				t.Fatalf("unexpected extra round: %d", got)
			}
		})
	}
}
