package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhousiru/emby-prober/internal/config"
	"github.com/zhousiru/emby-prober/internal/mihomo"
)

func TestRTTOrder(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/proxies/fast/delay":
			fmt.Fprint(w, `{"delay":10}`)
		case "/proxies/current/delay":
			fmt.Fprint(w, `{"delay":80}`)
		default:
			w.WriteHeader(504)
		}
	}))
	defer server.Close()
	cfg := config.Defaults()
	r := &Runner{cfg: cfg, mihomo: mihomo.New(server.URL, ""), log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for _, target := range []float64{0, 100} {
		r.cfg.Probe.StopMbps = target
		order, checks, err := r.order(context.Background(), []string{"dead", "current", "fast"}, "current")
		want := []string{"fast", "current"}
		if target > 0 {
			want = []string{"current", "fast"}
		}
		if err != nil || !reflect.DeepEqual(order, want) || len(checks) != 3 || checks[2].Available {
			t.Fatal(order, checks, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := r.order(ctx, []string{"fast"}, "fast"); err != context.Canceled {
		t.Fatal("cancellation not propagated", err)
	}
}

func TestEarlyStopRound(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		currentGood, dry, allOffline bool
	}{{name: "skip dead and flaky, stop at good"}, {name: "current already sufficient", currentGood: true}, {name: "dry run", dry: true}, {name: "all RTT checks fail", allOffline: true}} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			nodes := []string{"dead", "unused", "flaky", "good", "current"}
			selected, testNode := "current", "unused"
			downloads := []string{}
			counts := map[string]int{}
			controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if strings.HasSuffix(req.URL.Path, "/delay") {
					node := strings.TrimSuffix(strings.TrimPrefix(req.URL.Path, "/proxies/"), "/delay")
					if node == "dead" || tc.allOffline {
						w.WriteHeader(504)
						return
					}
					delays := map[string]int{"flaky": 10, "good": 20, "unused": 30, "current": 50}
					fmt.Fprintf(w, `{"delay":%d}`, delays[node])
					return
				}
				groups := map[string]mihomo.Proxy{}
				for _, n := range nodes {
					groups[n] = mihomo.Proxy{Name: n, Type: "Shadowsocks"}
				}
				groups["Emby Prober"] = mihomo.Proxy{Type: "Selector", All: nodes, Now: selected}
				groups["Emby Probe Test"] = mihomo.Proxy{Type: "Selector", All: nodes, Now: testNode}
				if req.URL.Path == "/proxies" {
					json.NewEncoder(w).Encode(map[string]any{"proxies": groups})
					return
				}
				name := strings.TrimPrefix(req.URL.Path, "/proxies/")
				if req.Method == "GET" {
					json.NewEncoder(w).Encode(groups[name])
					return
				}
				var body struct{ Name string }
				json.NewDecoder(req.Body).Decode(&body)
				if name == "Emby Probe Test" {
					testNode = body.Name
				} else if name == "Emby Prober" {
					if counts["good"] != 2 {
						t.Error("selection committed without completed samples")
					}
					selected = body.Name
				} else {
					t.Error("unexpected mutation")
				}
				w.WriteHeader(204)
			}))
			defer controller.Close()
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.allOffline {
					t.Error("Emby requested despite no viable nodes")
				}
				fmt.Fprint(w, `{"MediaSources":[{"Id":"s","Size":10000000,"SupportsDirectPlay":true}]}`)
			}))
			defer api.Close()
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				node := testNode
				downloads = append(downloads, node)
				counts[node]++
				count := counts[node]
				mu.Unlock()
				if node == "dead" || node == "unused" {
					t.Error("downloaded a skipped node")
				}
				if (node == "current" && !tc.currentGood) || (node == "flaky" && count == 2) {
					w.WriteHeader(403)
					return
				}
				w.Header().Set("Content-Range", "bytes 0-65535/10000000")
				w.Header().Set("Content-Type", "video/mp4")
				w.WriteHeader(206)
				w.Write(make([]byte, 65536))
			}))
			defer proxy.Close()
			cfg := config.Defaults()
			cfg.Emby = config.Emby{URL: api.URL, Token: "token", UserID: "user", ItemID: "item", DeviceID: "d"}
			cfg.Mihomo.URL = controller.URL
			cfg.Mihomo.ProbeProxyURL = proxy.URL
			cfg.StateDir = t.TempDir()
			cfg.Probe.StopMbps = .00001
			cfg.Probe.Timeout = config.Duration(time.Second)
			cfg.Probe.Offset = 0
			cfg.Probe.MinBytes = 1024
			cfg.Probe.MaxBytes = 65536
			r, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			if err = r.Round(context.Background(), tc.dry); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(cfg.StateDir, "status.json"))
			if err != nil {
				t.Fatal(err)
			}
			var status Status
			if err = json.Unmarshal(data, &status); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			wantDownloads := []string{"current", "flaky", "flaky", "good", "good"}
			wantSelected := "good"
			wantStop := "good"
			wantSkipped := []string{"unused"}
			if tc.currentGood {
				wantDownloads = []string{"current", "current"}
				wantSelected = "current"
				wantStop = "current"
				wantSkipped = []string{"flaky", "good", "unused"}
			}
			if tc.dry {
				wantSelected = "current"
			}
			if tc.allOffline {
				wantDownloads = []string{}
				wantSelected = "current"
				wantStop = ""
				wantSkipped = nil
			}
			if !reflect.DeepEqual(downloads, wantDownloads) || selected != wantSelected || testNode != "unused" || status.StopNode != wantStop || status.StoppedEarly == tc.allOffline || !reflect.DeepEqual(status.Skipped, wantSkipped) {
				t.Fatalf("downloads=%v selected=%s test=%s status=%+v", downloads, selected, testNode, status)
			}
		})
	}
}
