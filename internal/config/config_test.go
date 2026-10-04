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

func TestProbeControlsValidation(t *testing.T) {
	c := Defaults()
	c.Emby.URL = "https://emby.test"
	c.Emby.Username = "u"
	c.Emby.PasswordEnv = "PASSWORD"
	c.Mihomo.URL = "http://localhost:9090"
	c.Mihomo.ProbeProxyURL = "http://localhost:17891"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Config){func(c *Config) { c.Probe.StopMbps = -1 }, func(c *Config) { c.Probe.RTTTimeout = 0 }, func(c *Config) { c.Probe.RTTURL = "file:///etc/passwd" }} {
		invalid := c
		mutate(&invalid)
		if err := invalid.Validate(); err == nil {
			t.Fatal("invalid probe setting accepted")
		}
	}
}
