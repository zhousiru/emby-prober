package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/zhousiru/emby-prober/internal/config"
	"github.com/zhousiru/emby-prober/internal/daemon"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		slog.Error("emby-prober stopped", "error", err)
		os.Exit(1)
	}
}
func run() error {
	file := flag.String("config", "config.json", "JSON configuration file")
	once := flag.Bool("once", false, "run one complete probe round, then exit")
	dry := flag.Bool("dry-run", false, "test nodes but do not change the managed group")
	check := flag.Bool("check", false, "check groups, authentication and playback metadata without downloading")
	ver := flag.Bool("version", false, "print version")
	flag.Parse()
	if *ver {
		fmt.Println("emby-prober", version)
		return nil
	}
	if flag.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	c, err := config.Load(*file)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(c.StateDir, 0700); err != nil {
		return err
	}
	// Local single-instance lock; all instances controlling this group must use
	// the same state directory. Kernel releases the lock even after a crash.
	lock, err := os.OpenFile(filepath.Join(c.StateDir, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("another emby-prober is using this state directory")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	runner, err := daemon.New(c, log)
	if err != nil {
		return err
	}
	defer runner.Close()
	if *check {
		if err = runner.Check(ctx); err != nil {
			return err
		}
		log.Info("configuration and Emby metadata check passed")
		return nil
	}
	if *once {
		return runner.Round(ctx, *dry)
	}
	return runner.Run(ctx, *dry)
}
