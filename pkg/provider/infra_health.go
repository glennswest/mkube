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

	corev1 "k8s.io/api/core/v1"
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
	// Shared HTTP transport for infra health watchers — limits goroutine
	// growth from HTTP transport connections to unreachable endpoints.
	infraTransport = &http.Transport{
		MaxIdleConns:        5,
		MaxIdleConnsPerHost: 1,
		MaxConnsPerHost:     2,
		IdleConnTimeout:     60 * time.Second,
	}
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
			// No overall timeout — SSE connection stays open indefinitely.
			// Shared transport limits goroutine growth on reconnect cycles.
			Timeout:   0,
			Transport: infraTransport,
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

// dnsHealthFailures tracks consecutive health check failures per network.
// podHealthFailures tracks consecutive port-probe failures per pod container.
// Both protected by infraMu.
var dnsHealthFailures = make(map[string]int)
var podHealthFailures = make(map[string]int)

// dnsHealthFailureThreshold is the number of consecutive failures (each ~10s
// reconcile tick) before triggering forced pod recreation instead of restart.
const dnsHealthFailureThreshold = 3

// checkInfraHealth is the polling fallback called from the reconcile loop.
// It checks microdns REST API and port 53 for each managed network. If both
// are dead, it triggers immediate repair (restart or recreate) instead of
// waiting for the next consistency check cycle.
func (p *MicroKubeProvider) checkInfraHealth(ctx context.Context) {
	dnsClient := p.deps.NetworkMgr.DNSClient()
	if dnsClient == nil {
		return
	}

	for _, netObj := range p.networks {
		if netObj.Spec.ExternalDNS || netObj.Spec.DNS.Zone == "" || netObj.Spec.DNS.Server == "" {
			continue
		}

		endpoint := netObj.Spec.DNS.Endpoint
		if endpoint == "" {
			endpoint = "http://" + netObj.Spec.DNS.Server + ":8080"
		}

		// REST API healthy → microdns is alive, reset failure counter
		if err := dnsClient.HealthCheck(ctx, endpoint); err == nil {
			infraMu.Lock()
			delete(dnsHealthFailures, netObj.Name)
			infraMu.Unlock()
			continue
		}

		// REST API dead — check if port 53 is also dead (full zombie)
		if probeDNSPort(netObj.Spec.DNS.Server, netObj.Spec.DNS.Zone, 3*time.Second) {
			// Port 53 is alive but REST API is down — unusual but not critical.
			// DNS resolution still works; DHCP management is impaired.
			p.deps.Logger.Warnw("microdns REST API down but port 53 alive",
				"network", netObj.Name, "endpoint", endpoint)
			continue
		}

		// Both REST API and port 53 are dead — track consecutive failures
		// and trigger immediate repair.
		infraMu.Lock()
		dnsHealthFailures[netObj.Name]++
		failures := dnsHealthFailures[netObj.Name]
		infraMu.Unlock()

		p.deps.Logger.Warnw("microdns fully unresponsive",
			"network", netObj.Name, "endpoint", endpoint,
			"consecutiveFailures", failures, "threshold", dnsHealthFailureThreshold)

		if failures >= dnsHealthFailureThreshold {
			// Forced pod recreation — restart wasn't enough
			p.deps.Logger.Errorw("DNS container dead beyond threshold, forcing pod recreation",
				"network", netObj.Name, "failures", failures)

			podKey := netObj.Name + "/dns"
			pod, exists := p.pods[podKey]
			if exists {
				p.recordEvent(pod, "DNSCriticalFailure",
					fmt.Sprintf("DNS fully dead for %d consecutive checks, forcing recreation", failures),
					"Warning")
				if err := p.DeletePod(ctx, pod); err != nil {
					p.deps.Logger.Errorw("failed to delete dead DNS pod for recreation",
						"pod", podKey, "error", err)
					continue
				}
				if p.deps.Store != nil {
					storeKey := netObj.Name + ".dns"
					var storePod corev1.Pod
					if _, err := p.deps.Store.Pods.GetJSON(ctx, storeKey, &storePod); err == nil {
						if err := p.CreatePod(ctx, &storePod); err != nil {
							p.deps.Logger.Errorw("failed to recreate DNS pod",
								"pod", podKey, "error", err)
						}
					}
				}
			}

			infraMu.Lock()
			delete(dnsHealthFailures, netObj.Name)
			infraMu.Unlock()
		} else {
			// Attempt immediate repair via repairDNSLiveness logic
			p.deps.Logger.Warnw("attempting immediate DNS repair",
				"network", netObj.Name, "failures", failures)
			p.repairDNSLiveness(ctx)
		}
	}

	// Check all tracked pods with declared TCP ports.
	// This catches zombie containers where RouterOS says "running"
	// but the process inside is dead or unresponsive.
	p.checkPodPortHealth(ctx)
}

// podHealthFailureThreshold is the number of consecutive failures before
// triggering a container restart for non-DNS pods with unreachable ports.
const podHealthFailureThreshold = 3

// checkPodPortHealth probes declared TCP ports on all tracked running pods.
// On consecutive failures, restarts the container.
func (p *MicroKubeProvider) checkPodPortHealth(ctx context.Context) {
	for _, pod := range p.pods {
		for i, c := range pod.Spec.Containers {
			tcpPorts := collectTCPPorts(c)
			if len(tcpPorts) == 0 {
				continue
			}

			// Get pod IP
			vn := vethName(pod, i)
			podIP, _, ok := p.deps.NetworkMgr.GetPortInfo(vn)
			if !ok || podIP == "" {
				continue
			}

			// Verify container is "running" in RouterOS
			rosName := sanitizeName(pod, c.Name)
			ct, err := p.deps.Runtime.GetContainer(ctx, rosName)
			if err != nil || ct == nil || !ct.IsRunning() {
				continue // stopped containers handled by reconcile auto-recovery
			}

			// Probe each declared TCP port
			anyReachable := false
			for _, port := range tcpPorts {
				addr := fmt.Sprintf("%s:%d", podIP, port)
				conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
				if err == nil {
					conn.Close()
					anyReachable = true
					break
				}
			}

			healthKey := fmt.Sprintf("%s/%s", pod.Name, c.Name)
			if anyReachable {
				// Healthy — reset failure counter
				infraMu.Lock()
				delete(podHealthFailures, healthKey)
				infraMu.Unlock()
				continue
			}

			// All ports unreachable — track consecutive failures
			infraMu.Lock()
			podHealthFailures[healthKey]++
			failures := podHealthFailures[healthKey]
			infraMu.Unlock()

			p.deps.Logger.Warnw("pod ports unreachable on running container",
				"pod", pod.Name, "container", c.Name,
				"ip", podIP, "consecutiveFailures", failures,
				"threshold", podHealthFailureThreshold)

			if failures >= podHealthFailureThreshold {
				p.deps.Logger.Errorw("pod container dead beyond threshold, restarting",
					"pod", pod.Name, "container", c.Name, "failures", failures)

				p.recordEvent(pod, "ContainerUnresponsive",
					fmt.Sprintf("Container %s has %d consecutive port failures, restarting", c.Name, failures),
					"Warning")

				// Restart the container
				if err := p.deps.Runtime.StopContainer(ctx, ct.ID); err != nil {
					p.deps.Logger.Errorw("failed to stop unresponsive container",
						"container", rosName, "error", err)
				} else {
					time.Sleep(3 * time.Second)
					if err := p.deps.Runtime.StartContainer(ctx, ct.ID); err != nil {
						p.deps.Logger.Errorw("failed to restart unresponsive container",
							"container", rosName, "error", err)
					} else {
						p.deps.Logger.Infow("restarted unresponsive container",
							"container", rosName, "pod", pod.Name)
						p.recordEvent(pod, "Restarted",
							fmt.Sprintf("Container %s restarted after port unreachable", c.Name),
							"Normal")
					}
				}

				infraMu.Lock()
				delete(podHealthFailures, healthKey)
				infraMu.Unlock()
			}
		}
	}
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
