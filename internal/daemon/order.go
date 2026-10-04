package daemon

import (
	"context"
	"sort"
	"sync"
)

type Availability struct {
	Node      string `json:"node"`
	Available bool   `json:"available"`
	RTTMillis int    `json:"rtt_ms"`
	Error     string `json:"error,omitempty"`
}

// order runs small health checks with bounded concurrency, then sorts viable
// nodes by fresh RTT. No video bytes are downloaded during this phase.
func (r *Runner) order(ctx context.Context, nodes []string, current string) ([]string, []Availability, error) {
	order := append([]string(nil), nodes...)
	var checks []Availability
	if r.cfg.Probe.RTTURL != "" {
		checks = make([]Availability, len(nodes))
		jobs := make(chan int, len(nodes))
		for i := range nodes {
			jobs <- i
		}
		close(jobs)
		var wg sync.WaitGroup
		for worker := 0; worker < min(4, len(nodes)); worker++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range jobs {
					if ctx.Err() != nil {
						return
					}
					delay, err := r.mihomo.Delay(ctx, nodes[i], r.cfg.Probe.RTTURL, r.cfg.Probe.RTTTimeout.Value())
					result := Availability{Node: nodes[i], Available: err == nil, RTTMillis: delay}
					if err != nil {
						result.Error = err.Error()
					}
					checks[i] = result
				}
			}()
		}
		wg.Wait()
		if ctx.Err() != nil {
			return nil, checks, ctx.Err()
		}
		sort.SliceStable(checks, func(i, j int) bool {
			if checks[i].Available != checks[j].Available {
				return checks[i].Available
			}
			return checks[i].RTTMillis < checks[j].RTTMillis
		})
		order = nil
		for _, c := range checks {
			r.log.Info("RTT check", "node", c.Node, "available", c.Available, "rtt_ms", c.RTTMillis, "error", c.Error)
			if c.Available {
				order = append(order, c.Node)
			}
		}
	}
	// In target mode, measure the current viable exit first. This lets us stop
	// without switching if it is already fast enough, and ensures hysteresis and
	// hold-time decisions have a measured baseline rather than an untested node.
	if r.cfg.Probe.StopMbps > 0 {
		for i, node := range order {
			if node == current {
				copy(order[1:i+1], order[:i])
				order[0] = current
				break
			}
		}
	}
	return order, checks, nil
}
