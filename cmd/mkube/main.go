// mkube: A single-binary Virtual Kubelet provider for heterogeneous clusters.
// Supports MikroTik RouterOS (REST API), StormBase (gRPC), and Proxmox VE (REST API) backends.

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

	"github.com/glennswest/mkube/pkg/cluster"
	"github.com/glennswest/mkube/pkg/config"
	"github.com/glennswest/mkube/pkg/discovery"
	"github.com/glennswest/mkube/pkg/dns"
	"github.com/glennswest/mkube/pkg/dzo"
	"github.com/glennswest/mkube/pkg/lifecycle"
	embeddednats "github.com/glennswest/mkube/pkg/nats"
	"github.com/glennswest/mkube/pkg/namespace"
	"github.com/glennswest/mkube/pkg/network"
	netdriver "github.com/glennswest/mkube/pkg/network/driver"
	"github.com/glennswest/mkube/pkg/provider"
	"github.com/glennswest/mkube/pkg/proxmox"
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
		Short:   "Virtual Kubelet provider for heterogeneous clusters (RouterOS + StormBase + Proxmox)",
		Version: fmt.Sprintf("%s (%s)", version, commit),
		RunE:    run,
	}

	// Global flags
	f := rootCmd.Flags()
	f.String("config", "/etc/mkube/config.yaml", "Path to configuration file")
	f.String("kubeconfig", "", "Path to kubeconfig (optional, for standalone mode)")
	f.String("node-name", "mkube-node", "Kubernetes node name for this device")
	f.Bool("standalone", false, "Run without a Kubernetes API server (local reconciler only)")
	f.String("backend", "", "Backend type: routeros (default) or stormbase")

	// RouterOS connection
	f.String("routeros-address", "192.168.200.1:8728", "RouterOS API address")
	f.String("routeros-rest-url", "https://192.168.200.1/rest", "RouterOS REST API URL")
	f.String("routeros-user", "admin", "RouterOS API username")
	f.String("routeros-password", "", "RouterOS API password")

	// Proxmox connection
	f.String("proxmox-url", "", "Proxmox VE API URL (e.g. https://pvex.gw.lo:8006)")
	f.String("proxmox-token-id", "", "Proxmox API token ID (e.g. mkube@pve!mkube-token)")
	f.String("proxmox-token-secret", "", "Proxmox API token secret")
	f.String("proxmox-node", "", "Proxmox node name (e.g. pvex)")

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

	if cfg.IsProxmox() {
		return runProxmox(ctx, cfg, log)
	}
	if cfg.IsStormBase() {
		return runStormBase(ctx, cfg, log)
	}
	return runRouterOS(ctx, cfg, log)
}

// runRouterOS is the original RouterOS backend initialization path.
func runRouterOS(ctx context.Context, cfg *config.Config, log *zap.SugaredLogger) error {
	bootStart := time.Now()
	phaseStart := time.Now()

	// ── RouterOS API Client ─────────────────────────────────────────
	rosClient, err := routeros.NewClient(cfg.RouterOS)
	if err != nil {
		return fmt.Errorf("connecting to RouterOS: %w", err)
	}
	defer rosClient.Close()
	log.Infow("BOOT: RouterOS connected", "phase_ms", time.Since(phaseStart).Milliseconds(), "total_ms", time.Since(bootStart).Milliseconds())

	rt := runtime.NewRouterOSRuntime(rosClient)

	// ── DNS Client ──────────────────────────────────────────────────
	dnsClient := dns.NewClient(log)
	defer dnsClient.Close()

	// ── Device Discovery ────────────────────────────────────────────
	phaseStart = time.Now()
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
	log.Infow("BOOT: discovery complete", "phase_ms", time.Since(phaseStart).Milliseconds(), "total_ms", time.Since(bootStart).Milliseconds())

	// ── Network Driver ──────────────────────────────────────────────
	rosDriver := netdriver.NewRouterOS(rosClient, cfg.NodeName, log)

	// ── Network Manager (IPAM + bridge/veth + DNS) ──────────────────
	phaseStart = time.Now()
	netMgr, err := network.NewManager(cfg.Networks, rosDriver, dnsClient, log)
	if err != nil {
		return fmt.Errorf("initializing network manager: %w", err)
	}
	log.Infow("BOOT: network manager created", "phase_ms", time.Since(phaseStart).Milliseconds(), "total_ms", time.Since(bootStart).Milliseconds())

	phaseStart = time.Now()
	netMgr.InitDNSZones(ctx)
	for _, n := range cfg.Networks {
		log.Infow("network ready", "name", n.Name, "cidr", n.CIDR, "bridge", n.Bridge, "dns_zone", n.DNS.Zone)
	}
	log.Infow("BOOT: DNS zones initialized", "phase_ms", time.Since(phaseStart).Milliseconds(), "total_ms", time.Since(bootStart).Milliseconds())

	// ── Storage Manager ─────────────────────────────────────────────
	phaseStart = time.Now()
	storageMgr, err := storage.NewManager(cfg.Storage, cfg.Registry, rosClient, log)
	if err != nil {
		return fmt.Errorf("initializing storage manager: %w", err)
	}
	go storageMgr.RunGarbageCollector(ctx)
	log.Infow("BOOT: storage manager ready", "phase_ms", time.Since(phaseStart).Milliseconds(), "total_ms", time.Since(bootStart).Milliseconds())

	// ── Lifecycle Manager (boot ordering + probes + keepalive) ──────
	phaseStart = time.Now()
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

	log.Infow("BOOT: lifecycle manager ready", "phase_ms", time.Since(phaseStart).Milliseconds(), "total_ms", time.Since(bootStart).Milliseconds())

	return runSharedServices(ctx, cfg, rt, netMgr, storageMgr, lcMgr, dnsClient, rosClient, log, bootStart)
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

	return runSharedServices(ctx, cfg, sbClient, netMgr, storageMgr, lcMgr, dnsClient, nil, log, time.Now())
}

// runProxmox initializes the Proxmox VE LXC backend.
func runProxmox(ctx context.Context, cfg *config.Config, log *zap.SugaredLogger) error {
	bootStart := time.Now()
	phaseStart := time.Now()

	// ── Proxmox API Client ──────────────────────────────────────────
	pveClient, err := proxmox.NewClient(proxmox.ClientConfig{
		URL:            cfg.Proxmox.URL,
		TokenID:        cfg.Proxmox.TokenID,
		TokenSecret:    cfg.Proxmox.TokenSecret,
		Node:           cfg.Proxmox.Node,
		InsecureVerify: cfg.Proxmox.InsecureVerify,
		VMIDRange:      cfg.Proxmox.VMIDRange,
		Storage:        cfg.Proxmox.Storage,
		RootFSStorage:  cfg.Proxmox.RootFSStorage,
		RootFSSize:     cfg.Proxmox.RootFSSize,
	})
	if err != nil {
		return fmt.Errorf("connecting to Proxmox VE: %w", err)
	}
	defer pveClient.Close()
	log.Infow("BOOT: Proxmox connected", "node", cfg.Proxmox.Node, "phase_ms", time.Since(phaseStart).Milliseconds(), "total_ms", time.Since(bootStart).Milliseconds())

	// Sync existing VMIDs from Proxmox
	if err := pveClient.SyncVMIDs(ctx); err != nil {
		log.Warnw("failed to sync VMIDs from Proxmox", "error", err)
	}

	rt := runtime.NewProxmoxRuntime(
		pveClient,
		cfg.Proxmox.Storage,
		cfg.Proxmox.RootFSStorage,
		cfg.Proxmox.RootFSSize,
		cfg.Proxmox.Unprivileged,
		cfg.Proxmox.Features,
	)

	// ── DNS Client ──────────────────────────────────────────────────
	dnsClient := dns.NewClient(log)
	defer dnsClient.Close()

	// ── Proxmox Discovery ───────────────────────────────────────────
	phaseStart = time.Now()
	inv, err := discovery.DiscoverProxmox(ctx, pveClient, log)
	if err != nil {
		log.Warnw("proxmox discovery failed, continuing with static config", "error", err)
	} else {
		log.Infow("proxmox discovery complete",
			"containers", len(inv.Containers),
			"networks", len(inv.Networks),
		)
	}
	log.Infow("BOOT: discovery complete", "phase_ms", time.Since(phaseStart).Milliseconds(), "total_ms", time.Since(bootStart).Milliseconds())

	// ── Network Driver ──────────────────────────────────────────────
	pveDriver := netdriver.NewProxmox(pveClient, cfg.NodeName, log)

	// ── Network Manager (IPAM + DNS) ────────────────────────────────
	phaseStart = time.Now()
	netMgr, err := network.NewManager(cfg.Networks, pveDriver, dnsClient, log)
	if err != nil {
		return fmt.Errorf("initializing network manager: %w", err)
	}
	netMgr.InitDNSZones(ctx)
	for _, n := range cfg.Networks {
		log.Infow("network ready", "name", n.Name, "cidr", n.CIDR, "bridge", n.Bridge, "dns_zone", n.DNS.Zone)
	}
	log.Infow("BOOT: network manager created", "phase_ms", time.Since(phaseStart).Milliseconds(), "total_ms", time.Since(bootStart).Milliseconds())

	// ── Storage Manager ─────────────────────────────────────────────
	phaseStart = time.Now()
	storageMgr, err := storage.NewManager(cfg.Storage, cfg.Registry, nil, log)
	if err != nil {
		return fmt.Errorf("initializing storage manager: %w", err)
	}
	go storageMgr.RunGarbageCollector(ctx)
	log.Infow("BOOT: storage manager ready", "phase_ms", time.Since(phaseStart).Milliseconds(), "total_ms", time.Since(bootStart).Milliseconds())

	// ── Lifecycle Manager ───────────────────────────────────────────
	phaseStart = time.Now()
	lcMgr := lifecycle.NewManager(cfg.Lifecycle, rt, log)

	if inv != nil {
		units := discovery.BuildLifecycleUnitsFromProxmox(inv)
		lcMgr.SyncDiscoveredContainers(units)
		log.Infow("registered proxmox containers for lifecycle", "count", len(units))
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
				newInv, err := discovery.DiscoverProxmox(ctx, pveClient, log)
				if err != nil {
					log.Warnw("periodic proxmox re-discovery failed", "error", err)
					continue
				}
				units := discovery.BuildLifecycleUnitsFromProxmox(newInv)
				lcMgr.SyncDiscoveredContainers(units)
			}
		}
	}()

	log.Infow("BOOT: lifecycle manager ready", "phase_ms", time.Since(phaseStart).Milliseconds(), "total_ms", time.Since(bootStart).Milliseconds())

	return runSharedServices(ctx, cfg, rt, netMgr, storageMgr, lcMgr, dnsClient, nil, log, bootStart)
}

// runSharedServices starts services common to all backends (DZO, registry,
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
	bootStart time.Time,
) error {
	phaseStart := time.Now()

	// ── Embedded NATS (optional) ─────────────────────────────────────
	var embeddedNATS *embeddednats.EmbeddedServer
	if cfg.NATS.Embedded {
		var err error
		embeddedNATS, err = embeddednats.NewEmbedded(embeddednats.EmbeddedConfig{
			Host:     "0.0.0.0",
			Port:     cfg.NATS.Port,
			StoreDir: cfg.NATS.StoreDir,
		}, log)
		if err != nil {
			return fmt.Errorf("starting embedded NATS: %w", err)
		}
		defer embeddedNATS.Shutdown()
		cfg.NATS.URL = embeddedNATS.ClientURL()
		log.Infow("BOOT: embedded NATS started", "url", cfg.NATS.URL)
	}

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
	log.Infow("BOOT: NATS store init", "phase_ms", time.Since(phaseStart).Milliseconds(), "deferred", natsDeferred, "total_ms", time.Since(bootStart).Milliseconds())

	// ── HTTP API mux ────────────────────────────────────────────────
	mux := http.NewServeMux()
	listenAddr := ":8082"

	// ── Domain Zone Operator + Namespace Manager (optional) ─────────
	phaseStart = time.Now()
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
	log.Infow("BOOT: DZO/namespace bootstrap", "phase_ms", time.Since(phaseStart).Milliseconds(), "total_ms", time.Since(bootStart).Milliseconds())

	// ── Provider ────────────────────────────────────────────────────
	// Registry runs as a separate container (mkube-registry) — push events
	// arrive via the POST /api/v1/registry/push-notify HTTP webhook.
	phaseStart = time.Now()
	p, err := provider.NewMicroKubeProvider(provider.Deps{
		Config:       cfg,
		Runtime:      rt,
		NetworkMgr:   netMgr,
		StorageMgr:   storageMgr,
		LifecycleMgr: lcMgr,
		Namespace:    nsMgr,
		Store:        kvStore,
		Logger:       log,
	})
	if err != nil {
		return fmt.Errorf("creating provider: %w", err)
	}
	log.Infow("BOOT: provider created", "phase_ms", time.Since(phaseStart).Milliseconds(), "total_ms", time.Since(bootStart).Milliseconds())

	// ── Load resources from store ────────────────────────────────────
	if kvStore != nil {
		p.LoadBMHFromStore(ctx)
		p.LoadDeploymentsFromStore(ctx)
		p.LoadPVCsFromStore(ctx)
		p.LoadNetworksFromStore(ctx)
		p.MigrateNetworkConfig(ctx)
		p.LoadRegistriesFromStore(ctx)
		p.MigrateRegistryConfig(ctx)
		p.LoadISCSICdromsFromStore(ctx)
		p.LoadBootConfigsFromStore(ctx)
	}
	go p.RunDHCPWatcher(ctx)
	go p.RunSubnetScanner(ctx)
	go p.RunBMHReconciler(ctx)
	p.StartInfraHealthWatchers(ctx)
	p.StartISOScanner(ctx, 30*time.Second)

	// ── Cluster Manager (optional) ──────────────────────────────────
	if cfg.Cluster.Enabled && kvStore != nil {
		// Determine architecture from backend type
		arch := "arm64"
		switch rt.Backend() {
		case "proxmox", "stormbase":
			arch = "amd64"
		}
		clusterMgr := cluster.New(cfg.NodeName, cfg.Cluster, kvStore, arch, log)
		clusterMgr.Start(ctx)
		p.SetClusterManager(clusterMgr)
		clusterMgr.RegisterRoutes(mux)
		log.Infow("BOOT: cluster manager started", "peers", len(cfg.Cluster.Peers), "arch", arch)
	}

	// ── Register routes and start HTTP server ───────────────────────
	p.RegisterRoutes(mux)

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
		log.Infow("BOOT: complete, entering standalone reconciler", "total_ms", time.Since(bootStart).Milliseconds())
		return p.RunStandaloneReconciler(ctx)
	}

	log.Infow("starting Virtual Kubelet", "node", cfg.NodeName, "backend", rt.Backend())
	return p.RunVirtualKubelet(ctx)
}
