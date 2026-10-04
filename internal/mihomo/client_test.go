package mihomo

import "testing"

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
