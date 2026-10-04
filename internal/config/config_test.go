package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSecretsAndStrictConfig(t *testing.T) {
	t.Setenv("TEST_EMBY_PASSWORD", "special\"\\password")
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	valid := `{"emby":{"url":"https://emby.test/emby","username":"u","password_env":"TEST_EMBY_PASSWORD"},"mihomo":{"url":"http://localhost:9090","probe_proxy_url":"socks5h://localhost:17891"}}`
	os.WriteFile(p, []byte(valid), 0600)
	c, err := Load(p)
	if err != nil || c.Emby.Password != "special\"\\password" {
		t.Fatal("secret parsing", err)
	}
	for _, bad := range []string{valid + `{}`, `{"typo":true}`, `{"emby":{"url":"https://emby.test/emby","username":"u","password_env":"TEST_EMBY_PASSWORD","api_key_env":"KEY"}}`} {
		os.WriteFile(p, []byte(bad), 0600)
		if _, err := Load(p); err == nil {
			t.Fatal("bad config accepted")
		}
	}
}
