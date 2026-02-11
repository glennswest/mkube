// microkube: A single-binary Virtual Kubelet provider for MikroTik RouterOS
// with integrated network management, storage management, systemd boot services,
// and an optional embedded OCI registry (Zot).
//
// Architecture:
//
//	┌──────────────────────────────────────────────────────────────────┐
//	│  microkube (single Go binary)                                      │
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
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/glenneth/microkube/pkg/config"
	"github.com/glenneth/microkube/pkg/discovery"
	"github.com/glenneth/microkube/pkg/dns"
	"github.com/glenneth/microkube/pkg/dzo"
	"github.com/glenneth/microkube/pkg/lifecycle"
	"github.com/glenneth/microkube/pkg/namespace"
	"github.com/glenneth/microkube/pkg/network"
	"github.com/glenneth/microkube/pkg/provider"
	"github.com/glenneth/microkube/pkg/registry"
	"github.com/glenneth/microkube/pkg/routeros"
	"github.com/glenneth/microkube/pkg/storage"
)

var (
	version = "dev"
	commit  = "none"
)

func main() {
	rootCmd := &cobra.Command{
		Use:     "microkube",
		Short:   "Virtual Kubelet provider for MikroTik RouterOS containers",
		Version: fmt.Sprintf("%s (%s)", version, commit),
		RunE:    run,
	}

	// Global flags
	f := rootCmd.Flags()
	f.String("config", "/etc/microkube/config.yaml", "Path to configuration file")
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

	log.Infow("starting microkube", "version", version)

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

	// ── Domain Zone Operator + Namespace Manager (optional) ─────────
	var dzoOp *dzo.Operator
	var nsMgr *namespace.Manager
	if cfg.DZO.Enabled {
		dzoOp = dzo.NewOperator(cfg.DZO, cfg.Networks, dnsClient, rosClient, netMgr, lcMgr, log)
		if err := dzoOp.Bootstrap(ctx); err != nil {
			log.Warnw("DZO bootstrap failed, continuing without DZO", "error", err)
			dzoOp = nil
		} else {
			// Create namespace manager using DZO as ZoneResolver
			nsMgr = namespace.NewManager(cfg.Namespace, cfg.DZO, cfg.Networks, dzoOp, log)
			if err := nsMgr.Bootstrap(ctx); err != nil {
				log.Warnw("namespace bootstrap failed", "error", err)
				nsMgr = nil
			}

			// Shared HTTP mux for DZO + namespace API
			listenAddr := cfg.DZO.ListenAddr
			if listenAddr == "" {
				listenAddr = ":8082"
			}
			mux := http.NewServeMux()
			dzoOp.RegisterRoutes(mux)
			if nsMgr != nil {
				nsMgr.RegisterRoutes(mux)
			}
			go func() {
				srv := &http.Server{Addr: listenAddr, Handler: mux}
				go func() {
					<-ctx.Done()
					shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					srv.Shutdown(shutdownCtx)
				}()
				log.Infow("DZO+namespace API listening", "addr", listenAddr)
				if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
					log.Errorw("DZO+namespace API error", "error", err)
				}
			}()
		}
	}

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
	p, err := provider.NewMicroKubeProvider(provider.Deps{
		Config:       cfg,
		ROS:          rosClient,
		NetworkMgr:   netMgr,
		StorageMgr:   storageMgr,
		LifecycleMgr: lcMgr,
		Namespace:    nsMgr,
		Logger:       log,
	})
	if err != nil {
		return fmt.Errorf("creating provider: %w", err)
	}

	// ── Image Watcher (mirrors GHCR → local registry) ──────────────
	if reg != nil && len(cfg.Registry.WatchImages) > 0 {
		watcher := registry.NewImageWatcher(cfg.Registry, reg.Store(), reg.PushEvents, log)
		go watcher.Run(ctx)
		log.Infow("image watcher started", "images", len(cfg.Registry.WatchImages))
	}

	// ── Update API (internal, for mkube-update self-replacement) ────
	go p.RunUpdateAPI(ctx, ":8080")

	if cfg.Standalone {
		log.Info("running in standalone mode (local reconciler)")
		return p.RunStandaloneReconciler(ctx)
	}

	// Full Virtual Kubelet mode — registers as a node in a K8s cluster
	log.Infow("starting Virtual Kubelet", "node", cfg.NodeName)
	return p.RunVirtualKubelet(ctx)
}
