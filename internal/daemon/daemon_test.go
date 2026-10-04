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
	"sync"
	"testing"
	"time"

	"github.com/zhousiru/emby-prober/internal/config"
	"github.com/zhousiru/emby-prober/internal/mihomo"
	"github.com/zhousiru/emby-prober/internal/probe"
)

func TestChoose(t *testing.T) {
	cases := []struct {
		name, current string
		scores        []Score
		held          bool
		want          string
	}{
		{"improvement", "a", []Score{{"a", 10, true}, {"b", 30, true}}, false, "b"},
		{"hysteresis", "a", []Score{{"a", 10, true}, {"b", 11, true}}, false, "a"},
		{"hold", "a", []Score{{"a", 10, true}, {"b", 30, true}}, true, "a"},
		{"failed current overrides hold", "a", []Score{{"a", 0, false}, {"b", 30, true}}, true, "b"},
		{"all failed", "a", []Score{{"a", 0, false}, {"b", 0, false}}, false, "a"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _ := Choose(c.scores, c.current, .2, c.held)
			if got != c.want {
				t.Fatal(got)
			}
		})
	}
}
func TestScoresRejectIntermittentNode(t *testing.T) {
	scores := Scores([]string{"a", "b"}, []probe.Result{{Node: "a", Mbps: 100, Valid: true}, {Node: "a", Mbps: 200, Valid: false}, {Node: "b", Mbps: 10, Valid: true}, {Node: "b", Mbps: 30, Valid: true}}, 2)
	if scores[0].Valid || !scores[1].Valid || scores[1].Mbps != 20 {
		t.Fatal(scores)
	}
}
func TestRoundThroughDedicatedProxy(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		dry, blocked, manual bool
	}{{name: "select fastest"}, {name: "dry run", dry: true}, {name: "all forbidden", blocked: true}, {name: "respect manual change", manual: true}} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			selected, testNode := "slow", "slow"
			completed, commits := 0, 0
			controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				groups := map[string]mihomo.Proxy{"slow": {Name: "slow", Type: "Shadowsocks"}, "fast": {Name: "fast", Type: "Shadowsocks"}, "Emby Prober": {Name: "Emby Prober", Type: "Selector", All: []string{"slow", "fast"}, Now: selected}, "Emby Probe Test": {Name: "Emby Probe Test", Type: "Selector", All: []string{"slow", "fast"}, Now: testNode}}
				if req.URL.Path == "/proxies" {
					json.NewEncoder(w).Encode(map[string]any{"proxies": groups})
					return
				}
				name := req.URL.Path[len("/proxies/"):]
				if req.Method == "GET" {
					json.NewEncoder(w).Encode(groups[name])
					return
				}
				if req.Method != "PUT" {
					t.Error("unexpected controller request")
					w.WriteHeader(400)
					return
				}
				var in struct{ Name string }
				json.NewDecoder(req.Body).Decode(&in)
				if name == "Emby Probe Test" {
					testNode = in.Name
				} else if name == "Emby Prober" {
					if completed != 4 {
						t.Error("managed group changed before all samples completed")
					}
					selected = in.Name
					commits++
				} else {
					t.Error("modified unexpected group")
				}
				w.WriteHeader(204)
			}))
			defer controller.Close()
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path != "/Items/item/PlaybackInfo" {
					t.Error("unexpected Emby path", req.URL.Path)
				}
				if req.Header.Get("X-Emby-Token") != "token" {
					t.Error("missing API token")
				}
				fmt.Fprint(w, `{"MediaSources":[{"Id":"media","Size":10000000,"SupportsDirectPlay":true}]}`)
			}))
			defer api.Close()
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				mu.Lock()
				node := testNode
				mu.Unlock()
				if tc.blocked {
					w.WriteHeader(403)
				} else {
					if node == "slow" {
						time.Sleep(30 * time.Millisecond)
					} else {
						time.Sleep(time.Millisecond)
					}
					if req.Header.Get("Range") != "bytes=0-65535" {
						t.Error("incorrect Range")
					}
					w.Header().Set("Content-Range", "bytes 0-65535/10000000")
					w.Header().Set("Content-Type", "video/mp4")
					w.WriteHeader(206)
					w.Write(make([]byte, 65536))
				}
				mu.Lock()
				completed++
				if tc.manual && completed == 4 {
					selected = "manual"
				}
				mu.Unlock()
			}))
			defer proxy.Close()
			cfg := config.Defaults()
			cfg.Emby = config.Emby{URL: api.URL, Token: "token", UserID: "u", ItemID: "item", DeviceID: "d"}
			cfg.Mihomo.URL = controller.URL
			cfg.Mihomo.ProbeProxyURL = proxy.URL
			cfg.StateDir = t.TempDir()
			cfg.Probe.Offset = 0
			cfg.Probe.MaxBytes = 65536
			cfg.Probe.MinBytes = 1024
			cfg.Probe.Timeout = config.Duration(time.Second)
			r, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			if err = r.Round(context.Background(), tc.dry); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			want := "fast"
			wantCommits := 1
			if tc.dry || tc.blocked {
				want = "slow"
				wantCommits = 0
			}
			if tc.manual {
				want = "manual"
				wantCommits = 0
			}
			if selected != want || commits != wantCommits || testNode != "slow" {
				t.Fatalf("selected=%s commits=%d test=%s", selected, commits, testNode)
			}
			raw, err := os.ReadFile(filepath.Join(cfg.StateDir, "status.json"))
			if err != nil {
				t.Fatal(err)
			}
			var status Status
			if err = json.Unmarshal(raw, &status); err != nil {
				t.Fatal(err)
			}
			if len(status.Results) != 4 {
				t.Fatal("missing results")
			}
		})
	}
}
