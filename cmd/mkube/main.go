// mkube: A single-binary Virtual Kubelet provider for heterogeneous clusters.
// Supports MikroTik RouterOS (REST API) and StormBase (gRPC) backends.

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

	"github.com/glennswest/mkube/pkg/config"
	"github.com/glennswest/mkube/pkg/discovery"
	"github.com/glennswest/mkube/pkg/dns"
	"github.com/glennswest/mkube/pkg/dzo"
	"github.com/glennswest/mkube/pkg/lifecycle"
	"github.com/glennswest/mkube/pkg/namespace"
	"github.com/glennswest/mkube/pkg/network"
	netdriver "github.com/glennswest/mkube/pkg/network/driver"
	"github.com/glennswest/mkube/pkg/provider"
	"github.com/glennswest/mkube/pkg/registry"
	"github.com/glennswest/mkube/pkg/routeros"
	"github.com/glennswest/mkube/pkg/runtime"
	"github.com/glennswest/mkube/pkg/storage"
	"github.com/glennswest/mkube/pkg/store"
	"github.com/glennswest/mkube/pkg/stormbase"
)

var (
	version = "dev"
	commit  = "none"
)

func main() {
	rootCmd := &cobra.Command{
		Use:     "mkube",
		Short:   "Virtual Kubelet provider for heterogeneous clusters (RouterOS + StormBase)",
		Version: fmt.Sprintf("%s (%s)", version, commit),
		RunE:    run,
	}

	// Global flags
	f := rootCmd.Flags()
	f.String("config", "/etc/mkube/config.yaml", "Path to configuration file")
	f.String("kubeconfig", "", "Path to kubeconfig (optional, for standalone mode)")
	f.String("node-name", "mkube-node", "Kubernetes node name for this device")
	f.Bool("standalone", false, "Run without a Kubernetes API server (local reconciler only)")
	f.Bool("enable-registry", true, "Enable embedded Zot OCI registry")
	f.String("backend", "", "Backend type: routeros (default) or stormbase")

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
	defer func() { _ = logger.Sync() }()
	log := logger.Sugar()

	log.Infow("starting mkube", "version", version)

	// ── Configuration ───────────────────────────────────────────────
	cfg, err := config.Load(cmd.Flags())
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	// ── Context with signal handling ────────────────────────────────
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if cfg.IsStormBase() {
		return runStormBase(ctx, cfg, log)
	}
	return runRouterOS(ctx, cfg, log)
}

// runRouterOS is the original RouterOS backend initialization path.
func runRouterOS(ctx context.Context, cfg *config.Config, log *zap.SugaredLogger) error {
	// ── RouterOS API Client ─────────────────────────────────────────
	rosClient, err := routeros.NewClient(cfg.RouterOS)
	if err != nil {
		return fmt.Errorf("connecting to RouterOS: %w", err)
	}
	defer rosClient.Close()
	log.Info("connected to RouterOS")

	rt := runtime.NewRouterOSRuntime(rosClient)

	// ── DNS Client ──────────────────────────────────────────────────
	dnsClient := dns.NewClient(log)
	defer dnsClient.Close()

	// ── Device Discovery ────────────────────────────────────────────
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

	// ── Network Driver ──────────────────────────────────────────────
	rosDriver := netdriver.NewRouterOS(rosClient, cfg.NodeName, log)

	// ── Network Manager (IPAM + bridge/veth + DNS) ──────────────────
	netMgr, err := network.NewManager(cfg.Networks, rosDriver, dnsClient, log)
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
	lcMgr := lifecycle.NewManager(cfg.Lifecycle, rt, log)

	if inv != nil {
		units := discovery.BuildLifecycleUnits(inv)
		lcMgr.SyncDiscoveredContainers(units)
		log.Infow("registered discovered containers for keepalive", "count", len(units))
	}

	go lcMgr.RunWatchdog(ctx)

	// Periodic re-discovery
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

	return runSharedServices(ctx, cfg, rt, netMgr, storageMgr, lcMgr, dnsClient, rosClient, log)
}

// runStormBase initializes the StormBase gRPC backend.
func runStormBase(ctx context.Context, cfg *config.Config, log *zap.SugaredLogger) error {
	// ── StormBase gRPC Client ───────────────────────────────────────
	sbClient, err := stormbase.NewClient(stormbase.ClientConfig{
		Address:    cfg.StormBase.Address,
		CACert:     cfg.StormBase.CACert,
		ClientCert: cfg.StormBase.ClientCert,
		ClientKey:  cfg.StormBase.ClientKey,
		Insecure:   cfg.StormBase.Insecure,
	})
	if err != nil {
		return fmt.Errorf("connecting to stormd: %w", err)
	}
	defer sbClient.Close()
	log.Infow("connected to stormd", "address", cfg.StormBase.Address)

	// ── DNS Client ──────────────────────────────────────────────────
	dnsClient := dns.NewClient(log)
	defer dnsClient.Close()

	// ── StormBase Discovery ─────────────────────────────────────────
	inv, err := discovery.DiscoverStormBase(ctx, sbClient, log)
	if err != nil {
		log.Warnw("stormbase discovery failed, continuing with static config", "error", err)
	} else {
		log.Infow("stormbase discovery complete",
			"workloads", len(inv.Containers),
		)
	}

	// ── Network Driver ──────────────────────────────────────────────
	sbDriver := netdriver.NewStormBase(sbClient, cfg.NodeName, log)

	// ── Network Manager (IPAM + veth + DNS) ─────────────────────────
	netMgr, err := network.NewManager(cfg.Networks, sbDriver, dnsClient, log)
	if err != nil {
		return fmt.Errorf("initializing network manager: %w", err)
	}
	netMgr.InitDNSZones(ctx)
	for _, n := range cfg.Networks {
		log.Infow("network ready", "name", n.Name, "cidr", n.CIDR, "dns_zone", n.DNS.Zone)
	}

	// ── Storage Manager ─────────────────────────────────────────────
	// StormBase manages images via gRPC; pass nil for rosClient.
	storageMgr, err := storage.NewManager(cfg.Storage, cfg.Registry, nil, log)
	if err != nil {
		return fmt.Errorf("initializing storage manager: %w", err)
	}
	go storageMgr.RunGarbageCollector(ctx)

	// ── Lifecycle Manager ───────────────────────────────────────────
	// StormBase lifecycle uses gRPC health checks instead of RouterOS polling.
	// Pass nil for rosClient — stormd handles keepalive internally.
	lcMgr := lifecycle.NewManager(cfg.Lifecycle, nil, log)

	if inv != nil {
		units := discovery.BuildLifecycleUnitsFromStormBase(inv)
		lcMgr.SyncDiscoveredContainers(units)
		log.Infow("registered stormbase workloads for lifecycle", "count", len(units))
	}

	go lcMgr.RunWatchdog(ctx)
	log.Info("lifecycle manager ready (stormbase)")

	return runSharedServices(ctx, cfg, sbClient, netMgr, storageMgr, lcMgr, dnsClient, nil, log)
}

// runSharedServices starts services common to both backends (DZO, registry,
// provider, HTTP API, etc.).
func runSharedServices(
	ctx context.Context,
	cfg *config.Config,
	rt runtime.ContainerRuntime,
	netMgr *network.Manager,
	storageMgr *storage.Manager,
	lcMgr *lifecycle.Manager,
	dnsClient *dns.Client,
	rosClient *routeros.Client, // nil for stormbase
	log *zap.SugaredLogger,
) error {
	// ── NATS State Store (optional, deferred if NATS not ready yet) ──
	var kvStore *store.Store
	natsDeferred := false
	if cfg.NATS.URL != "" {
		var err error
		kvStore, err = store.New(ctx, cfg.NATS, log)
		if err != nil {
			log.Warnw("NATS store not ready, will retry in background", "error", err)
			natsDeferred = true
		} else {
			defer kvStore.Close()
			if _, err := kvStore.MigrateIfEmpty(ctx, cfg.Lifecycle.BootManifestPath, log); err != nil {
				log.Warnw("NATS migration failed", "error", err)
			}
		}
	}

	// ── HTTP API mux ────────────────────────────────────────────────
	mux := http.NewServeMux()
	listenAddr := ":8082"

	// ── Domain Zone Operator + Namespace Manager (optional) ─────────
	var nsMgr *namespace.Manager
	if cfg.DZO.Enabled && rosClient != nil {
		dzoOp := dzo.NewOperator(cfg.DZO, cfg.Networks, dnsClient, rosClient, netMgr, lcMgr, log)
		if err := dzoOp.Bootstrap(ctx); err != nil {
			log.Warnw("DZO bootstrap failed, continuing without DZO", "error", err)
		} else {
			nsMgr = namespace.NewManager(cfg.Namespace, cfg.DZO, cfg.Networks, dzoOp, log)
			if kvStore != nil {
				nsMgr.SetStore(kvStore)
			}
			if err := nsMgr.Bootstrap(ctx); err != nil {
				log.Warnw("namespace bootstrap failed", "error", err)
				nsMgr = nil
			}

			if cfg.DZO.ListenAddr != "" {
				listenAddr = cfg.DZO.ListenAddr
			}
			dzoOp.RegisterRoutes(mux)
			if nsMgr != nil {
				nsMgr.RegisterRoutes(mux)
			}
		}
	}
	netMgr.RegisterRoutes(mux)

	// ── Embedded Registry (optional) ────────────────────────────────
	var reg *registry.Registry
	if cfg.Registry.Enabled {
		var err error
		reg, err = registry.Start(ctx, cfg.Registry, log)
		if err != nil {
			return fmt.Errorf("starting embedded registry: %w", err)
		}
		defer func() { _ = reg.Shutdown(ctx) }()
		log.Infow("embedded registry started", "addr", cfg.Registry.ListenAddr)
	}

	// ── Provider ────────────────────────────────────────────────────
	var pushEvents <-chan registry.PushEvent
	if reg != nil {
		pushEvents = reg.PushEvents
	}

	p, err := provider.NewMicroKubeProvider(provider.Deps{
		Config:       cfg,
		Runtime:      rt,
		NetworkMgr:   netMgr,
		StorageMgr:   storageMgr,
		LifecycleMgr: lcMgr,
		Namespace:    nsMgr,
		Store:        kvStore,
		PushEvents:   pushEvents,
		Logger:       log,
	})
	if err != nil {
		return fmt.Errorf("creating provider: %w", err)
	}

	// ── Image Watcher ───────────────────────────────────────────────
	var watcher *registry.ImageWatcher
	if reg != nil && len(cfg.Registry.WatchImages) > 0 {
		watcher = registry.NewImageWatcher(cfg.Registry, reg.Store(), reg.PushEvents, log)
		go watcher.Run(ctx)
		log.Infow("image watcher started", "images", len(cfg.Registry.WatchImages))
	}

	// ── Upstream Syncer (local → GHCR backup) ───────────────────────
	if reg != nil && cfg.Registry.UpstreamSyncEnabled {
		syncer := registry.NewUpstreamSyncer(cfg.Registry, reg.Store(), reg.SyncEvents, log)
		if syncer != nil {
			go syncer.Run(ctx)
			log.Info("upstream syncer started")
		}
	}

	// ── Load BMH from store + start DHCP watcher ────────────────────
	if kvStore != nil {
		p.LoadBMHFromStore(ctx)
	}
	go p.RunDHCPWatcher(ctx)
	go p.RunSubnetScanner(ctx)
	go p.RunBMHReconciler(ctx)

	// ── Register routes and start HTTP server ───────────────────────
	p.RegisterRoutes(mux)

	mux.HandleFunc("POST /api/v1/registry/poll", func(w http.ResponseWriter, r *http.Request) {
		if watcher == nil {
			http.NotFound(w, r)
			return
		}
		watcher.TriggerPoll()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}` + "\n"))
	})

	go func() {
		srv := &http.Server{Addr: listenAddr, Handler: mux}
		go func() {
			<-ctx.Done()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(shutdownCtx)
		}()
		log.Infow("API server listening", "addr", listenAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Errorw("API server error", "error", err)
		}
	}()

	// ── Deferred NATS connection (retry in background after boot) ───
	if natsDeferred && cfg.NATS.URL != "" {
		go func() {
			for i := 0; i < 120; i++ {
				select {
				case <-ctx.Done():
					return
				case <-time.After(5 * time.Second):
				}
				s, err := store.New(ctx, cfg.NATS, log)
				if err != nil {
					if i%12 == 0 {
						log.Warnw("NATS store retry", "attempt", i+1, "error", err)
					}
					continue
				}
				if _, err := s.MigrateIfEmpty(ctx, cfg.Lifecycle.BootManifestPath, log); err != nil {
					log.Warnw("NATS migration failed", "error", err)
				}
				p.SetStore(s)
				if nsMgr != nil {
					nsMgr.SetStore(s)
				}
				log.Infow("NATS store connected (deferred)", "attempt", i+1)
				return
			}
			log.Warn("gave up connecting to NATS store after 10 minutes")
		}()
	}

	// ── Update API ──────────────────────────────────────────────────
	go p.RunUpdateAPI(ctx, ":8080")

	if cfg.Standalone {
		log.Info("running in standalone mode (local reconciler)")
		return p.RunStandaloneReconciler(ctx)
	}

	log.Infow("starting Virtual Kubelet", "node", cfg.NodeName, "backend", rt.Backend())
	return p.RunVirtualKubelet(ctx)
}
