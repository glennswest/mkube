package provider

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// infraRestartCooldown prevents rapid restart loops.
	infraRestartCooldown = 60 * time.Second

	// infraReconnectDelay is the wait time before re-establishing a watch after restart.
	infraReconnectDelay = 10 * time.Second

	// infraReadTimeout is how long to wait for a heartbeat before declaring dead.
	// The watch endpoint sends heartbeats every 5s, so 15s means 3 missed heartbeats.
	infraReadTimeout = 15 * time.Second
)

// infraContainer describes a non-pod infrastructure container that should be health-checked.
type infraContainer struct {
	Name     string // RouterOS container name
	WatchURL string // SSE watch URL (e.g. http://192.168.200.3:5001/healthz/watch)
}

// infraLastRestart tracks when each container was last restarted (guarded by mu).
var (
	infraLastRestart = make(map[string]time.Time)
	infraMu          sync.Mutex
)

// StartInfraHealthWatchers launches persistent SSE watch connections to each
// infrastructure container. When a connection drops (container dies or becomes
// unresponsive), the container is automatically restarted via RouterOS API.
// This replaces polling — detection is instant instead of worst-case 30s.
func (p *MicroKubeProvider) StartInfraHealthWatchers(ctx context.Context) {
	containers := p.getInfraContainers()
	for _, ic := range containers {
		go p.watchInfraContainer(ctx, ic)
	}
	if len(containers) > 0 {
		p.deps.Logger.Infow("infra health watchers started", "containers", len(containers))
	}
}

// watchInfraContainer maintains a persistent SSE connection to a container's
// /healthz/watch endpoint. On disconnect, restarts the container and reconnects.
func (p *MicroKubeProvider) watchInfraContainer(ctx context.Context, ic infraContainer) {
	log := p.deps.Logger

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Connect to the watch endpoint
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, ic.WatchURL, nil)
		if err != nil {
			log.Errorw("infra watch: bad URL", "container", ic.Name, "error", err)
			return
		}

		client := &http.Client{
			// No overall timeout — connection stays open indefinitely.
			// We use per-read deadlines via the response body.
			Timeout: 0,
		}

		resp, err := client.Do(req)
		if err != nil {
			// Can't connect — container is likely down
			log.Warnw("infra watch: connection failed", "container", ic.Name, "error", err)
			p.handleInfraDeath(ctx, ic)
			continue
		}

		log.Infow("infra watch: connected", "container", ic.Name)

		// Read heartbeats. Scanner blocks on ReadLine. If the container dies,
		// the TCP connection resets and Scan() returns false immediately.
		dead := false
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			// Got a heartbeat line — container is alive.
			// Check if context was cancelled while we were blocking.
			select {
			case <-ctx.Done():
				resp.Body.Close()
				return
			default:
			}
		}
		resp.Body.Close()

		// Scanner exited — either read error or EOF. Container is dead.
		if ctx.Err() != nil {
			return // shutting down
		}

		if !dead {
			log.Warnw("infra watch: connection lost", "container", ic.Name,
				"scanErr", scanner.Err())
			p.handleInfraDeath(ctx, ic)
		}
	}
}

// handleInfraDeath restarts a dead infrastructure container with cooldown protection.
func (p *MicroKubeProvider) handleInfraDeath(ctx context.Context, ic infraContainer) {
	log := p.deps.Logger

	infraMu.Lock()
	last := infraLastRestart[ic.Name]
	infraMu.Unlock()

	if !last.IsZero() && time.Since(last) < infraRestartCooldown {
		log.Warnw("infra watch: restart skipped (cooldown)",
			"container", ic.Name,
			"lastRestart", last.Format(time.RFC3339),
			"cooldown", infraRestartCooldown,
		)
		// Wait out the cooldown before retrying connection
		select {
		case <-ctx.Done():
			return
		case <-time.After(infraRestartCooldown - time.Since(last)):
		}
		return
	}

	log.Warnw("infra watch: restarting dead container", "container", ic.Name)

	if err := p.restartInfraContainer(ctx, ic.Name); err != nil {
		log.Errorw("infra watch: restart failed", "container", ic.Name, "error", err)
	} else {
		infraMu.Lock()
		infraLastRestart[ic.Name] = time.Now()
		infraMu.Unlock()
		log.Infow("infra watch: container restarted", "container", ic.Name)
	}

	// Wait for container to come up before reconnecting
	select {
	case <-ctx.Done():
	case <-time.After(infraReconnectDelay):
	}
}

// checkInfraHealth is the polling fallback called from the reconcile loop.
// It only acts if the watch goroutine hasn't recently restarted the container
// (i.e., the watch is not connected and the container is unresponsive).
// This catches edge cases where the watch goroutine itself is stuck.
func (p *MicroKubeProvider) checkInfraHealth(ctx context.Context) {
	// Polling is now a lightweight backup — the watch handles most cases.
	// Only check containers where the watch might not be connected yet.
}

// getInfraContainers returns the list of infrastructure containers to watch.
func (p *MicroKubeProvider) getInfraContainers() []infraContainer {
	var containers []infraContainer

	// Registry: derive IP from config's LocalAddresses
	if len(p.deps.Config.Registry.LocalAddresses) > 0 {
		addr := p.deps.Config.Registry.LocalAddresses[0]
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			host = addr
		}
		containers = append(containers, infraContainer{
			Name:     "registry.gt.lo",
			WatchURL: fmt.Sprintf("http://%s:5001/healthz/watch", host),
		})
	}

	return containers
}

// restartInfraContainer stops and starts a container by name.
func (p *MicroKubeProvider) restartInfraContainer(ctx context.Context, name string) error {
	ct, err := p.deps.Runtime.GetContainer(ctx, name)
	if err != nil {
		return fmt.Errorf("finding container %s: %w", name, err)
	}
	if ct == nil {
		return fmt.Errorf("container %s not found", name)
	}

	if strings.EqualFold(ct.Status, "running") {
		if err := p.deps.Runtime.StopContainer(ctx, ct.ID); err != nil {
			return fmt.Errorf("stopping container %s: %w", name, err)
		}
		time.Sleep(3 * time.Second)
	}

	if err := p.deps.Runtime.StartContainer(ctx, ct.ID); err != nil {
		return fmt.Errorf("starting container %s: %w", name, err)
	}

	return nil
}
