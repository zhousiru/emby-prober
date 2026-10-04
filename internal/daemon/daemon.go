package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/zhousiru/emby-prober/internal/config"
	"github.com/zhousiru/emby-prober/internal/emby"
	"github.com/zhousiru/emby-prober/internal/mihomo"
	"github.com/zhousiru/emby-prober/internal/probe"
	"github.com/zhousiru/emby-prober/internal/storage"
)

type Score struct {
	Node  string  `json:"node"`
	Mbps  float64 `json:"median_mbps"`
	Valid bool    `json:"valid"`
}
type Status struct {
	RTT          []Availability `json:"rtt,omitempty"`
	ProbeOrder   []string       `json:"probe_order,omitempty"`
	Skipped      []string       `json:"skipped,omitempty"`
	StoppedEarly bool           `json:"stopped_early"`
	StopNode     string         `json:"stop_node,omitempty"`
	StopMbps     float64        `json:"stop_mbps"`
	Started      time.Time      `json:"started_at"`
	Finished     time.Time      `json:"finished_at"`
	Controller   string         `json:"controller"`
	Group        string         `json:"group"`
	Item         string         `json:"item_id,omitempty"`
	Previous     string         `json:"previous"`
	Selected     string         `json:"selected"`
	Winner       string         `json:"winner,omitempty"`
	Reason       string         `json:"reason"`
	LastSwitch   time.Time      `json:"last_switch"`
	DryRun       bool           `json:"dry_run"`
	Results      []probe.Result `json:"results"`
	Scores       []Score        `json:"scores"`
}
type Runner struct {
	cfg        config.Config
	emby       *emby.Client
	mihomo     *mihomo.Client
	log        *slog.Logger
	lastSwitch time.Time
}

func New(c config.Config, l *slog.Logger) (*Runner, error) {
	e, err := emby.New(c.Emby, c.StateDir)
	if err != nil {
		return nil, err
	}
	r := &Runner{cfg: c, emby: e, mihomo: mihomo.New(c.Mihomo.URL, c.Mihomo.Secret), log: l}
	if b, err := os.ReadFile(filepath.Join(c.StateDir, "status.json")); err == nil {
		var s Status
		if json.Unmarshal(b, &s) == nil && s.Controller == c.Mihomo.URL && s.Group == c.Mihomo.Group {
			r.lastSwitch = s.LastSwitch
		}
	}
	return r, nil
}
func (r *Runner) Close() { r.emby.Close() }
func Scores(nodes []string, results []probe.Result, samples int) []Score {
	var scores []Score
	for _, node := range nodes {
		var speeds []float64
		good := true
		for _, v := range results {
			if v.Node == node {
				if !v.Valid {
					good = false
				}
				speeds = append(speeds, v.Mbps)
			}
		}
		s := Score{Node: node, Valid: good && len(speeds) == samples}
		if s.Valid {
			sort.Float64s(speeds)
			mid := len(speeds) / 2
			s.Mbps = speeds[mid]
			if len(speeds)%2 == 0 {
				s.Mbps = (speeds[mid-1] + speeds[mid]) / 2
			}
		}
		scores = append(scores, s)
	}
	return scores
}
func Choose(scores []Score, current string, improvement float64, held bool) (string, string) {
	var best, old *Score
	for i := range scores {
		s := &scores[i]
		if !s.Valid {
			continue
		}
		if best == nil || s.Mbps > best.Mbps || (s.Mbps == best.Mbps && s.Node == current) {
			best = s
		}
		if s.Node == current {
			old = s
		}
	}
	if best == nil {
		return current, "all probes failed; keep current selection"
	}
	if best.Node == current {
		return current, "current node is fastest"
	}
	if old != nil {
		if held {
			return current, "minimum hold time has not elapsed"
		}
		if best.Mbps <= old.Mbps*(1+improvement) {
			return current, "improvement below switch threshold"
		}
	}
	return best.Node, "select fastest valid node"
}
func (r *Runner) Check(ctx context.Context) error {
	all, err := r.mihomo.Proxies(ctx)
	if err != nil {
		return err
	}
	nodes, err := mihomo.Candidates(all, r.cfg.Mihomo.Group, r.cfg.Mihomo.ProbeGroup)
	if err != nil {
		return err
	}
	r.log.Info("Mihomo groups validated", "candidates", len(nodes))
	r.emby.BeginRound()
	if err = r.emby.Ensure(ctx); err != nil {
		return err
	}
	item, err := r.emby.Item(ctx)
	if err != nil {
		return err
	}
	_, err = r.emby.Target(ctx, item)
	return err
}
func (r *Runner) Round(ctx context.Context, dryRun bool) (retErr error) {
	s := Status{Started: time.Now().UTC(), Controller: r.cfg.Mihomo.URL, Group: r.cfg.Mihomo.Group, DryRun: dryRun, LastSwitch: r.lastSwitch}
	defer func() {
		s.Finished = time.Now().UTC()
		s.LastSwitch = r.lastSwitch
		if retErr != nil {
			s.Reason = retErr.Error()
		}
		if err := storage.WriteJSON(filepath.Join(r.cfg.StateDir, "status.json"), s); err != nil {
			retErr = errors.Join(retErr, errors.New("cannot save probe results"))
		}
	}()
	all, err := r.mihomo.Proxies(ctx)
	if err != nil {
		return err
	}
	nodes, err := mihomo.Candidates(all, r.cfg.Mihomo.Group, r.cfg.Mihomo.ProbeGroup)
	if err != nil {
		return err
	}
	s.Previous = all[r.cfg.Mihomo.Group].Now
	s.Selected = s.Previous
	// Restore the test group's initial selection on success, cancellation or error.
	// Do not clobber a concurrent external change to the test selector.
	original := all[r.cfg.Mihomo.ProbeGroup].Now
	lastTest := ""
	defer func() {
		if lastTest == "" || original == "" {
			return
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		g, e := r.mihomo.Group(cleanup, r.cfg.Mihomo.ProbeGroup)
		if e == nil && g.Now == lastTest {
			e = r.mihomo.Select(cleanup, r.cfg.Mihomo.ProbeGroup, original)
		}
		if e != nil {
			r.log.Warn("could not restore test group", "error", e)
		}
	}()
	order, checks, err := r.order(ctx, nodes, s.Previous)
	s.RTT = checks
	s.ProbeOrder = order
	s.StopMbps = r.cfg.Probe.StopMbps
	if err != nil {
		return err
	}
	if len(order) == 0 {
		s.Reason = "no nodes passed RTT check; keep current selection"
		r.log.Warn(s.Reason)
		return nil
	}
	r.emby.BeginRound()
	s.Item, err = r.emby.Item(ctx)
	if err != nil {
		return err
	}
	r.log.Info("probe round started", "candidates", len(nodes), "available", len(order), "samples", r.cfg.Probe.Samples, "item_id", s.Item)
	for position, node := range order {
		for sample := 0; sample < r.cfg.Probe.Samples; sample++ {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err = r.mihomo.Select(ctx, r.cfg.Mihomo.ProbeGroup, node); err != nil {
				return err
			}
			lastTest = node
			v := probe.Result{Node: node, Sample: sample + 1}
			target, e := r.emby.Target(ctx, s.Item)
			if e == nil {
				v = probe.Measure(ctx, r.cfg.Mihomo.ProbeProxyURL, target, r.cfg.Probe)
				v.Node = node
				v.Sample = sample + 1
				if v.Status == 401 && r.emby.IsServerURL(v.FinalURL) {
					if e = r.emby.Reauthenticate(ctx); e == nil {
						target, e = r.emby.Target(ctx, s.Item)
						if e == nil {
							v = probe.Measure(ctx, r.cfg.Mihomo.ProbeProxyURL, target, r.cfg.Probe)
							v.Node = node
							v.Sample = sample + 1
						}
					}
				}
			}
			if e != nil {
				v.Valid = false
				v.Error = e.Error()
			}
			// An operator changing the probe group during a download invalidates it.
			g, e := r.mihomo.Group(ctx, r.cfg.Mihomo.ProbeGroup)
			if e != nil {
				return e
			}
			if g.Now != node {
				return errors.New("test selector was changed externally during measurement")
			}
			s.Results = append(s.Results, v)
			r.log.Info("sample", "node", node, "sample", sample+1, "mbps", fmt.Sprintf("%.2f", v.Mbps), "ttfb_seconds", v.TTFB, "valid", v.Valid, "http_status", v.Status, "error", v.Error)
			if !v.Valid {
				break
			} // no need to spend more data on an ineligible node
		}
		score := Scores([]string{node}, s.Results, r.cfg.Probe.Samples)[0]
		if r.cfg.Probe.StopMbps > 0 && score.Valid && score.Mbps >= r.cfg.Probe.StopMbps {
			s.StoppedEarly = true
			s.StopNode = node
			s.Skipped = append(s.Skipped, order[position+1:]...)
			r.log.Info("throughput target reached; stop probing", "node", node, "mbps", score.Mbps, "target_mbps", r.cfg.Probe.StopMbps, "skipped", len(s.Skipped))
			break
		}
	}

	s.Scores = Scores(nodes, s.Results, r.cfg.Probe.Samples)
	next, reason := Choose(s.Scores, s.Previous, r.cfg.Probe.SwitchImprovement, time.Since(r.lastSwitch) < r.cfg.Probe.MinHold.Value())
	s.Winner = next
	s.Reason = reason
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if next != s.Previous && !dryRun {
		// Re-check the live group immediately before committing; preserve manual changes.
		g, e := r.mihomo.Group(ctx, r.cfg.Mihomo.Group)
		if e != nil {
			return e
		}
		if g.Now != s.Previous {
			s.Selected = g.Now
			s.Reason = "managed group changed externally; skipped selection"
			return nil
		}
		if err = r.mihomo.Select(ctx, r.cfg.Mihomo.Group, next); err != nil {
			return err
		}
		r.lastSwitch = time.Now().UTC()
		s.Selected = next
	}
	r.log.Info("probe round finished", "selected", s.Selected, "recommended", next, "reason", reason, "dry_run", dryRun)
	return nil
}
func (r *Runner) Run(ctx context.Context, dryRun bool) error {
	for {
		if err := r.Round(ctx, dryRun); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			r.log.Error("probe round failed", "error", err)
		}
		// Intervals are measured from completion, so slow rounds never overlap.
		timer := time.NewTimer(r.cfg.Probe.Interval.Value())
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}
