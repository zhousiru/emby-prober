package config

import (
	"testing"
	"time"
)

func TestCronShanghaiSlots(t *testing.T) {
	s, err := (Probe{Cron: "CRON_TZ=Asia/Shanghai 0 1,7,13,19 * * *"}).Schedule()
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"2026-10-04T16:59:59Z": "2026-10-04T17:00:00Z",
		"2026-10-04T17:00:00Z": "2026-10-04T23:00:00Z",
		"2026-10-04T23:00:00Z": "2026-10-05T05:00:00Z",
		"2026-10-05T05:00:00Z": "2026-10-05T11:00:00Z",
		"2026-10-05T11:00:00Z": "2026-10-05T17:00:00Z",
	}
	for input, want := range cases {
		now, _ := time.Parse(time.RFC3339, input)
		if got := s.Next(now).UTC().Format(time.RFC3339); got != want {
			t.Errorf("after %s: got %s, want %s", input, got, want)
		}
	}
}

func TestCronValidationAndDefaultZone(t *testing.T) {
	for _, input := range []string{"invalid", "0 0 1 1 1 1", "CRON_TZ=Missing/Zone 0 1 * * *", "0 0 31 2 *", "@every 6h"} {
		if _, err := (Probe{Cron: input}).Schedule(); err == nil {
			t.Errorf("accepted %q", input)
		}
	}
	s, err := (Probe{Cron: "0 1 * * *"}).Schedule()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	if got := s.Next(now); !got.Equal(now.Add(time.Hour)) {
		t.Fatal(got)
	}
	if s, err := (Probe{}).Schedule(); s != nil || err != nil {
		t.Fatal(s, err)
	}
}
