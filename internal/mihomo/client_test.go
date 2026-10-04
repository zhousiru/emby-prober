package mihomo

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCandidates(t *testing.T) {
	all := map[string]Proxy{"out": {Type: "Selector", All: []string{"a", "DIRECT"}}, "test": {Type: "Selector", All: []string{"a"}}, "a": {Type: "Shadowsocks"}, "DIRECT": {Type: "Direct"}}
	nodes, err := Candidates(all, "out", "test")
	if err != nil || len(nodes) != 1 || nodes[0] != "a" {
		t.Fatal(nodes, err)
	}
	all["a"] = Proxy{Type: "URLTest", All: []string{"nested"}}
	if _, err = Candidates(all, "out", "test"); err == nil {
		t.Fatal("nested group accepted")
	}
	all["a"] = Proxy{Type: "Shadowsocks"}
	all["test"] = Proxy{Type: "Selector", All: []string{}}
	if _, err = Candidates(all, "out", "test"); err == nil {
		t.Fatal("untestable member accepted")
	}
}

func TestDelayRequestAndErrors(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		code       int
		wantErr    bool
	}{{"valid", `{"delay":42}`, 200, false}, {"zero", `{"delay":0}`, 200, true}, {"missing", `{}`, 200, true}, {"offline", `{}`, 504, true}} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/proxies/🍃 日本/01/delay" || r.URL.Query().Get("timeout") != "2500" || r.URL.Query().Get("url") != "https://health.test/204" || r.Header.Get("Authorization") != "Bearer secret" {
					t.Error("incorrect delay API request")
				}
				w.WriteHeader(tc.code)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			delay, err := New(server.URL, "secret").Delay(context.Background(), "🍃 日本/01", "https://health.test/204", 2500*time.Millisecond)
			if (err != nil) != tc.wantErr || (!tc.wantErr && delay != 42) {
				t.Fatal(delay, err)
			}
		})
	}
}
