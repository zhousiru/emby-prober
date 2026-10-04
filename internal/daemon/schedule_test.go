package daemon

import (
	"context"
	"github.com/zhousiru/emby-prober/internal/config"
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestCronWaitsBeforeFirstRoundAndCancels(t *testing.T) {
	// Nil clients would fail if Run started an unscheduled round.
	r := &Runner{cfg: config.Config{Probe: config.Probe{Cron: "0 1,7,13,19 * * *"}}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx, false) }()
	select {
	case err := <-done:
		t.Fatalf("did not wait: %v", err)
	case <-time.After(20 * time.Millisecond):
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
}
