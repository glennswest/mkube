// mikrotik-kube: A single-binary Virtual Kubelet provider for MikroTik RouterOS
// with integrated network management, storage management, systemd boot services,
// and an optional embedded OCI registry (Zot).
//
// Architecture:
//
//	┌──────────────────────────────────────────────────────────────────┐
//	│  mikrotik-kube (single Go binary)                                 │
//	│                                                                  │
//	│  ┌──────────────┐  ┌──────────────┐  ┌────────────────────────┐ │
//	│  │ Virtual       │  │ Network      │  │ Storage Manager        │ │
//	│  │ Kubelet Core  │  │ Manager      │  │ - volume provisioning  │ │
//	│  │ + RouterOS    │  │ - IPAM       │  │ - garbage collection   │ │
//	│  │   Provider    │  │ - VETH/bridge│  │ - tarball cache        │ │
//	│  └──────┬───────┘  └──────┬───────┘  └────────────┬───────────┘ │
//	│         │                 │                        │             │
//	│  ┌──────┴─────────────────┴────────────────────────┴───────────┐ │
//	│  │                  RouterOS API Client                         │ │
//	│  │            (REST + RouterOS protocol)                        │ │
//	│  └─────────────────────────┬───────────────────────────────────┘ │
//	│                            │                                     │
//	│  ┌─────────────────────────┴───────────────────────────────────┐ │
//	│  │  Systemd Manager (boot ordering, health watchdog)           │ │
//	│  └─────────────────────────────────────────────────────────────┘ │
//	│                                                                  │
//	│  ┌─────────────────────────────────────────────────────────────┐ │
//	│  │  Embedded Zot Registry (optional, :5000)                    │ │
//	│  └─────────────────────────────────────────────────────────────┘ │
//	└──────────────────────────────────────────────────────────────────┘
//	         │
//	         ▼  RouterOS REST API (/rest/container/*)
//	┌──────────────────────────────────────────────────────────────────┐
//	│  MikroTik RouterOS Container Runtime                             │
//	│  ┌─────┐ ┌─────┐ ┌─────┐ ┌─────┐                               │
//	│  │ C1  │ │ C2  │ │ C3  │ │ C4  │  ...                          │
//	│  └─────┘ └─────┘ └─────┘ └─────┘                               │
//	└──────────────────────────────────────────────────────────────────┘

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/glenneth/mikrotik-kube/pkg/config"
	"github.com/glenneth/mikrotik-kube/pkg/discovery"
	"github.com/glenneth/mikrotik-kube/pkg/dns"
	"github.com/glenneth/mikrotik-kube/pkg/network"
	"github.com/glenneth/mikrotik-kube/pkg/provider"
	"github.com/glenneth/mikrotik-kube/pkg/registry"
	"github.com/glenneth/mikrotik-kube/pkg/routeros"
	"github.com/glenneth/mikrotik-kube/pkg/storage"
	"github.com/glenneth/mikrotik-kube/pkg/lifecycle"
)

var (
	version = "dev"
	commit  = "none"
)

func main() {
	rootCmd := &cobra.Command{
		Use:     "mikrotik-kube",
		Short:   "Virtual Kubelet provider for MikroTik RouterOS containers",
		Version: fmt.Sprintf("%s (%s)", version, commit),
		RunE:    run,
	}

	// Global flags
	f := rootCmd.Flags()
	f.String("config", "/etc/mikrotik-kube/config.yaml", "Path to configuration file")
	f.String("kubeconfig", "", "Path to kubeconfig (optional, for standalone mode)")
	f.String("node-name", "mikrotik-node", "Kubernetes node name for this device")
	f.Bool("standalone", false, "Run without a Kubernetes API server (local reconciler only)")
	f.Bool("enable-registry", true, "Enable embedded Zot OCI registry")

	// RouterOS connection
	f.String("routeros-address", "192.168.200.1:8728", "RouterOS API address")
	f.String("routeros-rest-url", "https://192.168.200.1/rest", "RouterOS REST API URL")
	f.String("routeros-user", "admin", "RouterOS API username")
	f.String("routeros-password", "", "RouterOS API password")

	// Network
	f.String("pod-cidr", "192.168.200.0/24", "CIDR range for pod IP allocation")
	f.String("bridge-name", "containers", "RouterOS bridge interface for containers")

	// Storage
	f.String("storage-path", "/raid1/images", "Base path for container volumes on RouterOS")
	f.String("tarball-cache", "/raid1/cache", "Path for image tarball cache")
	f.Int("gc-interval-minutes", 30, "Garbage collection interval in minutes")

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func run(cmd *cobra.Command, args []string) error {
	// ── Logger ──────────────────────────────────────────────────────
	logger, _ := zap.NewProduction()
	defer logger.Sync()
	log := logger.Sugar()

	log.Infow("starting mikrotik-kube", "version", version)

	// ── Configuration ───────────────────────────────────────────────
	cfg, err := config.Load(cmd.Flags())
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	// ── Context with signal handling ────────────────────────────────
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// ── RouterOS API Client ─────────────────────────────────────────
	rosClient, err := routeros.NewClient(cfg.RouterOS)
	if err != nil {
		return fmt.Errorf("connecting to RouterOS: %w", err)
	}
	defer rosClient.Close()
	log.Info("connected to RouterOS")

	// ── DNS Client ──────────────────────────────────────────────────
	dnsClient := dns.NewClient(log)
	defer dnsClient.Close()

	// ── Device Discovery ────────────────────────────────────────────
	// Scan RouterOS for existing containers, networks, and MicroDNS
	// instances. Enrich the network config with discovered DNS servers
	// and auto-create network definitions for discovered subnets.
	// Retry a few times since the veth network may not be ready immediately.
	var inv *discovery.Inventory
	for attempt := 1; attempt <= 3; attempt++ {
		inv, err = discovery.Discover(ctx, rosClient, log)
		if err == nil {
			break
		}
		log.Warnw("discovery attempt failed", "attempt", attempt, "error", err)
		if attempt < 3 {
			time.Sleep(5 * time.Second)
		}
	}
	if err != nil {
		log.Warnw("device discovery failed after retries, continuing with static config", "error", err)
	} else {
		cfg.Networks = discovery.EnrichNetworks(cfg.Networks, inv, "kube.gt.lo", log)
		log.Infow("discovery enriched config",
			"networks", len(cfg.Networks),
			"containers_found", len(inv.Containers),
		)
	}

	// ── Network Manager (IPAM + bridge/veth + DNS) ──────────────────
	netMgr, err := network.NewManager(cfg.Networks, rosClient, dnsClient, log)
	if err != nil {
		return fmt.Errorf("initializing network manager: %w", err)
	}
	netMgr.InitDNSZones(ctx)
	for _, n := range cfg.Networks {
		log.Infow("network ready", "name", n.Name, "cidr", n.CIDR, "bridge", n.Bridge, "dns_zone", n.DNS.Zone)
	}

	// ── Storage Manager ─────────────────────────────────────────────
	storageMgr, err := storage.NewManager(cfg.Storage, cfg.Registry, rosClient, log)
	if err != nil {
		return fmt.Errorf("initializing storage manager: %w", err)
	}
	go storageMgr.RunGarbageCollector(ctx)
	log.Info("storage manager ready, GC started")

	// ── Lifecycle Manager (boot ordering + probes + keepalive) ──────
	lcMgr := lifecycle.NewManager(cfg.Lifecycle, rosClient, log)

	// Register all discovered start-on-boot containers for keepalive + auto-probes
	if inv != nil {
		units := discovery.BuildLifecycleUnits(inv)
		lcMgr.SyncDiscoveredContainers(units)
		log.Infow("registered discovered containers for keepalive", "count", len(units))
	}

	go lcMgr.RunWatchdog(ctx)

	// Periodic re-discovery to pick up new containers
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				newInv, err := discovery.Discover(ctx, rosClient, log)
				if err != nil {
					log.Warnw("periodic re-discovery failed", "error", err)
					continue
				}
				units := discovery.BuildLifecycleUnits(newInv)
				lcMgr.SyncDiscoveredContainers(units)
			}
		}
	}()

	log.Info("lifecycle manager ready")

	// ── Embedded Registry (optional) ────────────────────────────────
	var reg *registry.Registry
	if cfg.Registry.Enabled {
		reg, err = registry.Start(ctx, cfg.Registry, log)
		if err != nil {
			return fmt.Errorf("starting embedded registry: %w", err)
		}
		defer reg.Shutdown(ctx)
		log.Infow("embedded registry started", "addr", cfg.Registry.ListenAddr)
	}

	// ── Provider + Virtual Kubelet ──────────────────────────────────
	p, err := provider.NewMikroTikProvider(provider.Deps{
		Config:       cfg,
		ROS:          rosClient,
		NetworkMgr:   netMgr,
		StorageMgr:   storageMgr,
		LifecycleMgr: lcMgr,
		Logger:       log,
	})
	if err != nil {
		return fmt.Errorf("creating provider: %w", err)
	}

	// ── Auto-Updater (watches registry push events → redeploys pods) ─
	if reg != nil {
		// Bridge registry.PushEvent → provider.PushEvent to avoid circular import
		providerEvents := make(chan provider.PushEvent, 64)
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case ev := <-reg.PushEvents:
					providerEvents <- provider.PushEvent{
						Repo:      ev.Repo,
						Reference: ev.Reference,
					}
				}
			}
		}()
		go p.RunAutoUpdater(ctx, providerEvents)
		log.Info("auto-updater started, watching registry push events")
	}

	if cfg.Standalone {
		log.Info("running in standalone mode (local reconciler)")
		return p.RunStandaloneReconciler(ctx)
	}

	// Full Virtual Kubelet mode — registers as a node in a K8s cluster
	log.Infow("starting Virtual Kubelet", "node", cfg.NodeName)
	return p.RunVirtualKubelet(ctx)
}
