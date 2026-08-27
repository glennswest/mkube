package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/glennswest/mkube/pkg/bmc"
	"github.com/glennswest/mkube/pkg/cluster"
	"github.com/glennswest/mkube/pkg/config"
	"github.com/glennswest/mkube/pkg/gitbackup"
	"github.com/glennswest/mkube/pkg/lifecycle"
	"github.com/glennswest/mkube/pkg/namespace"
	"github.com/glennswest/mkube/pkg/network"
	"github.com/glennswest/mkube/pkg/registry"
	"github.com/glennswest/mkube/pkg/routeros"
	"github.com/glennswest/mkube/pkg/runtime"
	"github.com/glennswest/mkube/pkg/safemap"
	"github.com/glennswest/mkube/pkg/storage"
	"github.com/glennswest/mkube/pkg/store"
	"github.com/glennswest/mkube/pkg/stormbase"
)

const (
	// annotationNetwork selects which network a pod's containers are placed on.
	annotationNetwork = "vkube.io/network"
	// annotationFile specifies a local tarball path on the host, bypassing OCI pull.
	annotationFile = "vkube.io/file"
	// annotationNamespace selects a DZO namespace for DNS registration.
	annotationNamespace = "vkube.io/namespace"
	// annotationAliases defines extra DNS aliases for pod containers.
	// Format: "alias=container,alias2=container2,alias3" (no =container means first container).
	annotationAliases = "vkube.io/aliases"
	// annotationStaticIP requests a specific IP address for the pod's containers.
	annotationStaticIP = "vkube.io/static-ip"

	// annotationImagePolicy controls automatic image updates.
	// "auto" triggers a rolling update when the registry digest changes.
	annotationImagePolicy = "vkube.io/image-policy"

	// annotationNode tracks which cluster node a pod is assigned to.
	annotationNode = "vkube.io/node"

	// annotationImageDigest stores the registry digest of the image used
	// to create/update the pod. Survives restart via NATS persistence.
	// Used to detect stale images on boot (session memory is empty).
	annotationImageDigest = "vkube.io/image-digest"

	// Device passthrough annotations (StormBase only)
	annotationDeviceClass      = "stormbase.io/device-class"
	annotationDeviceCount      = "stormbase.io/device-count"
	annotationDeviceProfile    = "stormbase.io/device-profile"
	annotationDeviceAllocation = "stormbase.io/device-allocation"
)

// Deps holds injected dependencies for the provider.
type Deps struct {
	Config       *config.Config
	Runtime      runtime.ContainerRuntime
	NetworkMgr   *network.Manager
	StorageMgr   *storage.Manager
	LifecycleMgr *lifecycle.Manager
	Namespace    *namespace.Manager        // optional, nil if namespace management is disabled
	Store        *store.Store              // optional, nil if NATS is not configured
	PushEvents   <-chan registry.PushEvent // optional, receives push events from embedded registry
	Logger       *zap.SugaredLogger
	Version      string // build version (git describe)
	Commit       string // build commit (git rev-parse --short HEAD)
}

// MicroKubeProvider implements the Virtual Kubelet provider interface.
// It translates Kubernetes Pod specifications into RouterOS
// container operations, managing the full lifecycle including networking,
// storage, and boot ordering.
type MicroKubeProvider struct {
	deps               Deps
	nodeName           string
	startTime          time.Time
	pvcUsage           atomic.Pointer[pvcUsageIndexes]                     // cached /file + /disk indexes for PVC usage enrichment
	pods               *safemap.Map[string, *corev1.Pod]                   // namespace/name -> pod
	configMaps         *safemap.Map[string, *corev1.ConfigMap]             // namespace/name -> configmap
	secrets            *safemap.Map[string, *corev1.Secret]                // namespace/name -> secret
	bareMetalHosts     *safemap.Map[string, *BareMetalHost]                // namespace/name -> BMH
	deployments        *safemap.Map[string, *Deployment]                   // namespace/name -> deployment
	pvcs               *safemap.Map[string, *corev1.PersistentVolumeClaim] // namespace/name -> PVC
	networks           *safemap.Map[string, *Network]                      // name -> Network (cluster-scoped)
	registries         *safemap.Map[string, *Registry]                     // name -> Registry (cluster-scoped)
	iscsiCdroms        *safemap.Map[string, *ISCSICdrom]                   // name -> ISCSICdrom (cluster-scoped)
	iscsiDisks         *safemap.Map[string, *ISCSIDisk]                    // name -> ISCSIDisk (cluster-scoped)
	bootConfigs        *safemap.Map[string, *BootConfig]                   // name -> BootConfig (cluster-scoped)
	hostReservations   *safemap.Map[string, *HostReservation]              // namespace/name -> HostReservation
	jobRunners         *safemap.Map[string, *JobRunner]                    // name -> JobRunner (cluster-scoped)
	jobs               *safemap.Map[string, *Job]                          // namespace/name -> Job
	storagePools       *safemap.Map[string, *StoragePool]                  // name -> StoragePool (cluster-scoped)
	redeploying        *safemap.Map[string, bool]                          // pod keys currently being redeployed
	cowPrewarm         *safemap.Map[string, bool]                          // in-flight golden prewarms by repo
	createFailures     *safemap.Map[string, int]                           // pod key -> consecutive CreatePod failures
	createBackoff      *safemap.Map[string, *containerRestartState]        // pod key -> creation backoff tracking
	dnsHealthFails     *safemap.Map[string, int]                           // network -> consecutive failed DNS health queries
	networkFailures    *safemap.Map[string, int]                           // pod key -> consecutive network health failures
	restartBackoff     *safemap.Map[string, *containerRestartState]        // container name -> restart backoff tracking
	cleanupTickCounter int                                                 // scheduler tick counter for auto-cleanup
	jobLogBuf          *jobLogStore                                        // in-memory job log buffers
	runnerLogBuf       *jobLogStore                                        // in-memory runner activity log buffers
	dhcpMu             sync.RWMutex                                        // protects dhcpIndex
	dhcpIndex          *dhcpNetworkIndex                                   // precomputed DHCP reservation/subnet lookup
	eventsMu           sync.Mutex                                          // protects events slice
	events             []corev1.Event                                      // recent events (ring buffer, max 256)
	notifyPodStatus    func(*corev1.Pod)                                   // callback for pod status updates
	pushNotify         chan registry.PushEvent                             // internal channel for API push notifications
	consistencyRunning atomic.Bool                                         // guards CheckConsistencyAsync against goroutine leaks
	consistencyCache   *ConsistencyReport                                  // cached consistency report (lock-free HTTP reads)
	consistencyCacheMu sync.Mutex                                          // guards consistencyCache writes
	consistencyCacheAt time.Time                                           // when the cache was last refreshed
	reseedRunning      atomic.Bool                                         // guards triggerNetworkReseed against goroutine leaks
	clusterMgr         *cluster.Manager                                    // nil if clustering is disabled
	bmcController      *bmc.Controller                                     // nil if no BMHs have BMC addresses
	dnsSnapshotter     *gitbackup.DNSSnapshotter                           // nil if DNS snapshots are disabled
	kickReconcile      chan struct{}                                       // event-driven reconcile trigger (buffered 1)
	kickScheduler      chan struct{}                                       // event-driven scheduler trigger (buffered 1)
	micrologsBreaker   micrologsCircuitBreaker                             // circuit breaker for micrologs service
	micrologsClient    *http.Client                                        // persistent HTTP client for micrologs (2s timeout)
	migrationTracker   *MigrationTracker                                   // tracks in-flight PVC/disk migrations
	lastNATCheck       time.Time                                           // last DHCP relay NAT exemption check
	dnsPodCooldown     *safemap.Map[string, time.Time]                     // network name → earliest retry time for managed DNS pod
	podWorker          *PodWorker                                          // concurrent pod lifecycle dispatch
}

// containerRestartState tracks restart attempts for exponential backoff.
type containerRestartState struct {
	attempts    int
	lastAttempt time.Time
	lastRunning time.Time
	backoff     time.Duration
}

// micrologsCircuitBreaker skips micrologs requests after consecutive failures.
// After 3 failures, it opens for 30 seconds. After cooldown, allows one probe.
type micrologsCircuitBreaker struct {
	mu        sync.Mutex
	failures  int
	openUntil time.Time
}

func (cb *micrologsCircuitBreaker) isOpen() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	if cb.failures < 3 {
		return false
	}
	if time.Now().After(cb.openUntil) {
		// Allow one probe attempt
		cb.failures = 2
		return false
	}
	return true
}

func (cb *micrologsCircuitBreaker) recordSuccess() {
	cb.mu.Lock()
	cb.failures = 0
	cb.mu.Unlock()
}

func (cb *micrologsCircuitBreaker) recordFailure() {
	cb.mu.Lock()
	cb.failures++
	if cb.failures >= 3 {
		cb.openUntil = time.Now().Add(30 * time.Second)
	}
	cb.mu.Unlock()
}

// MigrationTracker tracks in-flight PVC/disk migrations with SSE progress streaming.
type MigrationTracker struct {
	running atomic.Bool
	mu      sync.Mutex
	current *MigrationProgress
	nextSub int
	subs    map[int]chan MigrationProgress
}

// MigrationProgress describes the state of an in-flight migration.
type MigrationProgress struct {
	ID           string `json:"id"`
	ResourceType string `json:"resourceType"`
	ResourceName string `json:"resourceName"`
	TargetPool   string `json:"targetPool"`
	Phase        string `json:"phase"`
	BytesCopied  int64  `json:"bytesCopied"`
	TotalBytes   int64  `json:"totalBytes"`
	StartedAt    string `json:"startedAt"`
	Error        string `json:"error,omitempty"`
}

func newMigrationTracker() *MigrationTracker {
	return &MigrationTracker{
		subs: make(map[int]chan MigrationProgress),
	}
}

// TryStart attempts to begin a new migration. Returns false if one is already running.
func (mt *MigrationTracker) TryStart(id, resType, resName, targetPool string) bool {
	if !mt.running.CompareAndSwap(false, true) {
		return false
	}
	mt.mu.Lock()
	mt.current = &MigrationProgress{
		ID:           id,
		ResourceType: resType,
		ResourceName: resName,
		TargetPool:   targetPool,
		Phase:        "starting",
		StartedAt:    time.Now().UTC().Format(time.RFC3339),
	}
	mt.mu.Unlock()
	return true
}

// Update sets the current phase and byte counts, broadcasting to SSE subscribers.
func (mt *MigrationTracker) Update(phase string, bytesCopied, totalBytes int64) {
	mt.mu.Lock()
	if mt.current != nil {
		mt.current.Phase = phase
		mt.current.BytesCopied = bytesCopied
		mt.current.TotalBytes = totalBytes
	}
	snap := *mt.current
	subs := make([]chan MigrationProgress, 0, len(mt.subs))
	for _, ch := range mt.subs {
		subs = append(subs, ch)
	}
	mt.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- snap:
		default: // slow subscriber, skip
		}
	}
}

// Complete marks the migration as done (success or failure) and releases the guard.
func (mt *MigrationTracker) Complete(errMsg string) {
	mt.mu.Lock()
	if mt.current != nil {
		if errMsg != "" {
			mt.current.Phase = "failed"
			mt.current.Error = errMsg
		} else {
			mt.current.Phase = "complete"
		}
	}
	snap := *mt.current
	subs := make([]chan MigrationProgress, 0, len(mt.subs))
	for _, ch := range mt.subs {
		subs = append(subs, ch)
	}
	mt.mu.Unlock()

	// Broadcast final event
	for _, ch := range subs {
		select {
		case ch <- snap:
		default:
		}
	}

	mt.running.Store(false)
}

// Current returns a snapshot of the current migration, or nil if none.
func (mt *MigrationTracker) Current() *MigrationProgress {
	mt.mu.Lock()
	defer mt.mu.Unlock()
	if mt.current == nil {
		return nil
	}
	snap := *mt.current
	return &snap
}

// Subscribe returns a channel that receives progress updates.
func (mt *MigrationTracker) Subscribe() (int, <-chan MigrationProgress) {
	mt.mu.Lock()
	defer mt.mu.Unlock()
	mt.nextSub++
	id := mt.nextSub
	ch := make(chan MigrationProgress, 16)
	mt.subs[id] = ch
	return id, ch
}

// Unsubscribe removes a subscriber.
func (mt *MigrationTracker) Unsubscribe(id int) {
	mt.mu.Lock()
	defer mt.mu.Unlock()
	if ch, ok := mt.subs[id]; ok {
		close(ch)
		delete(mt.subs, id)
	}
}

// SetStore sets the NATS store on the provider (used for deferred NATS connection).
func (p *MicroKubeProvider) SetStore(s *store.Store) {
	p.deps.Store = s
	p.deps.Logger.Infow("NATS store attached to provider")
	// ConfigMaps and Secrets load FIRST: almost everything else depends on
	// them. Pods mount them, deployments reference them, and a managed
	// registry keeps its TLS certificate in one — LoadRegistriesFromStore
	// syncs that certificate into the image-pull trust pool as it loads.
	// Loaded after their dependents, the dependents come up referencing
	// objects that are not in memory yet: the registry case surfaced as every
	// pull failing "certificate signed by unknown authority", because the
	// trust pool was built 9ms before the ConfigMap holding the cert arrived.
	p.LoadConfigMapsFromStore(context.Background())
	p.LoadSecretsFromStore(context.Background())
	p.LoadBMHFromStore(context.Background())
	p.LoadDeploymentsFromStore(context.Background())
	p.LoadPVCsFromStore(context.Background())
	p.LoadNetworksFromStore(context.Background())
	p.MigrateNetworkConfig(context.Background())
	p.dhcpMu.Lock()
	p.rebuildDHCPIndex()
	p.dhcpMu.Unlock()
	p.LoadRegistriesFromStore(context.Background())
	p.MigrateRegistryConfig(context.Background())
	p.ReconcileNetworkConfigMaps(context.Background())
	p.LoadISCSICdromsFromStore(context.Background())
	p.LoadISCSIDisksFromStore(context.Background())
	p.LoadBootConfigsFromStore(context.Background())
	p.LoadHostReservationsFromStore(context.Background())
	p.LoadJobRunnersFromStore(context.Background())
	p.LoadJobsFromStore(context.Background())
	p.LoadStoragePoolsFromStore(context.Background())
	p.DiscoverStoragePools(context.Background())
	p.startDHCPSubscription(context.Background())
	if p.bmcController != nil {
		p.bmcController.SetStore(s)
	}
	go p.RunResourceWatchers(context.Background())
	// Reclaim root-dirs swapped aside by swapRootDirAside, off the reconcile path.
	go p.runRootDirGC(context.Background())
	// Reap static bridge-port entries left pointing at deleted interfaces (#14).
	go p.runBridgePortGC(context.Background())
	// Pre-stage image tarballs (+ digest sidecars) for tracked pods so pulls stay
	// off the pod-(re)create critical path. Off the reconcile path.
	go p.runImageStager(context.Background())
	// Seed consistency cache after startup settles — delay 30s so the initial
	// heavy checks (pod liveness, microdns services) don't block API endpoints
	// during the first page loads after deploy.
	go func() {
		time.Sleep(30 * time.Second)
		p.refreshConsistencyCache("startup")
	}()
	go p.runConsistencyCacheTimer()
}

// SetClusterManager sets the cluster manager on the provider.
func (p *MicroKubeProvider) SetClusterManager(mgr *cluster.Manager) {
	p.clusterMgr = mgr
}

// isLocalPod returns true if the pod should be reconciled by this node.
// Pods without a vkube.io/node annotation are local (legacy/unassigned).
func (p *MicroKubeProvider) isLocalPod(pod *corev1.Pod) bool {
	if p.clusterMgr == nil {
		return true // no clustering, everything is local
	}
	targetNode := pod.Annotations[annotationNode]
	if targetNode == "" {
		return true // unassigned = local (legacy compat)
	}
	return targetNode == p.nodeName
}

// NewMicroKubeProvider creates a new provider instance.
func NewMicroKubeProvider(deps Deps) (*MicroKubeProvider, error) {
	p := &MicroKubeProvider{
		deps:             deps,
		nodeName:         deps.Config.NodeName,
		startTime:        time.Now(),
		pods:             safemap.New[string, *corev1.Pod](),
		configMaps:       safemap.New[string, *corev1.ConfigMap](),
		secrets:          safemap.New[string, *corev1.Secret](),
		bareMetalHosts:   safemap.New[string, *BareMetalHost](),
		deployments:      safemap.New[string, *Deployment](),
		pvcs:             safemap.New[string, *corev1.PersistentVolumeClaim](),
		networks:         safemap.New[string, *Network](),
		registries:       safemap.New[string, *Registry](),
		iscsiCdroms:      safemap.New[string, *ISCSICdrom](),
		iscsiDisks:       safemap.New[string, *ISCSIDisk](),
		bootConfigs:      safemap.New[string, *BootConfig](),
		hostReservations: safemap.New[string, *HostReservation](),
		jobRunners:       safemap.New[string, *JobRunner](),
		jobs:             safemap.New[string, *Job](),
		storagePools:     safemap.New[string, *StoragePool](),
		redeploying:      safemap.New[string, bool](),
		cowPrewarm:       safemap.New[string, bool](),
		createFailures:   safemap.New[string, int](),
		createBackoff:    safemap.New[string, *containerRestartState](),
		dnsHealthFails:   safemap.New[string, int](),
		networkFailures:  safemap.New[string, int](),
		restartBackoff:   safemap.New[string, *containerRestartState](),
		jobLogBuf:        newJobLogStore(),
		runnerLogBuf:     newJobLogStore(),
		dhcpIndex:        buildDHCPIndex(deps.Config.Networks),
		pushNotify:       make(chan registry.PushEvent, 16),
		kickReconcile:    make(chan struct{}, 1),
		kickScheduler:    make(chan struct{}, 1),
		micrologsClient: &http.Client{
			Timeout: 2 * time.Second,
			Transport: &http.Transport{
				MaxConnsPerHost:     2,
				IdleConnTimeout:     60 * time.Second,
				TLSHandshakeTimeout: 2 * time.Second,
			},
		},
		migrationTracker: newMigrationTracker(),
		dnsPodCooldown:   safemap.New[string, time.Time](),
		podWorker:        NewPodWorker(deps.Logger),
	}

	// Initialize BMC controller for IPMI power management
	p.bmcController = p.initBMCController(deps.Store)

	// Initialize DNS snapshotter for git-backed microdns config backup
	if deps.Config.GitBackup.DNSSnapshot && deps.Config.GitBackup.RepoURL != "" {
		p.dnsSnapshotter = gitbackup.NewDNSSnapshotter(
			deps.Config.GitBackup,
			deps.NetworkMgr.DNSClient(),
			deps.Logger,
		)
	}

	// Load built-in default ConfigMaps derived from mkube config
	for _, cm := range generateDefaultConfigMaps(deps.Config) {
		p.configMaps.Set(cm.Namespace+"/"+cm.Name, cm)
	}

	// Register lifecycle failed callback so containers that exceed max
	// restarts trigger a full pod recreate (fresh veth allocation).
	if deps.LifecycleMgr != nil {
		deps.LifecycleMgr.OnFailed = func(containerName string) {
			p.handleLifecycleFailed(containerName)
		}

		// Register state change callback for immediate pod status updates
		// and reconcile kicks when containers stop/fail.
		deps.LifecycleMgr.OnStateChanged = func(containerName, oldStatus, newStatus string) {
			p.deps.Logger.Debugw("container state changed",
				"container", containerName, "from", oldStatus, "to", newStatus)

			// Find owning pod and push immediate status update
			var ownerPod *corev1.Pod
			p.pods.Range(func(_ string, pod *corev1.Pod) bool {
				for _, c := range pod.Spec.Containers {
					if sanitizeName(pod, c.Name) == containerName {
						ownerPod = pod
						return false
					}
				}
				return true
			})

			if ownerPod != nil {
				p.notifyPodChange(context.Background(), ownerPod)
			}

			// Kick reconcile on stopped/failed so auto-recovery runs immediately
			if newStatus == "stopped" || newStatus == "failed" || newStatus == "unhealthy" {
				p.triggerReconcile()
			}
		}
	}

	return p, nil
}

// triggerReconcile sends a non-blocking signal to run reconcile immediately.
func (p *MicroKubeProvider) triggerReconcile() {
	select {
	case p.kickReconcile <- struct{}{}:
	default:
	}
}

// triggerScheduler sends a non-blocking signal to run the job scheduler immediately.
func (p *MicroKubeProvider) triggerScheduler() {
	select {
	case p.kickScheduler <- struct{}{}:
	default:
	}
}

// notifyDNSSnapshot fires a debounced DNS config snapshot for the given network.
func (p *MicroKubeProvider) notifyDNSSnapshot(networkName, endpoint, zone string) {
	if p.dnsSnapshotter != nil {
		p.dnsSnapshotter.NotifyChange(networkName, endpoint, zone)
	}
}

// ─── PodLifecycleHandler Interface ──────────────────────────────────────────

// CreatePod takes a Kubernetes Pod spec and creates the corresponding
// RouterOS container(s). This includes:
//  1. Pulling/caching the image as an OCI tarball
//  2. Allocating a veth interface and IP address
//  3. Creating volume mounts
//  4. Registering boot ordering if restartPolicy=Always
//  5. Creating and starting the RouterOS container
//
// rootDirTrashSuffix marks a container root-dir that has been renamed aside and
// is awaiting lazy deletion by reapStaleRootDirs. The full token is
// ".trash-<uuid>" so names stay unique under concurrent pod creates.
const rootDirTrashSuffix = ".trash-"

// routerOSClient returns the underlying RouterOS client when the active runtime
// is RouterOS, else nil. Used for operations not in the ContainerRuntime
// interface (notably the atomic directory rename used to swap a root-dir aside).
func (p *MicroKubeProvider) routerOSClient() *routeros.Client {
	if r, ok := p.deps.Runtime.(*runtime.RouterOSRuntime); ok {
		return r.Client()
	}
	return nil
}

// swapRootDirAside renames a container root-dir out of the way via an atomic
// RouterOS rename so a fresh tarball extraction can proceed without a "root-dir
// overlap" error (TODO #12). The previous guard — an in-line recursive
// RemoveDirectory — was slow and could fail/leave a partial dir, wedging
// retries in a permanent CreateFailed loop. The displaced dir is reclaimed
// later by reapStaleRootDirs. Falls back to in-place RemoveDirectory when
// rename is unavailable (non-RouterOS runtime) or fails.
func (p *MicroKubeProvider) swapRootDirAside(ctx context.Context, rootDir string) {
	if rootDir == "" {
		return
	}
	if cli := p.routerOSClient(); cli != nil {
		if exists, err := cli.FileExists(ctx, rootDir); err == nil && !exists {
			return // nothing to move
		}
		trash := fmt.Sprintf("%s%s%s", strings.TrimSuffix(rootDir, "/"), rootDirTrashSuffix, uuid.NewString())
		err := cli.MoveDirectory(ctx, rootDir, trash)
		if err == nil {
			p.deps.Logger.Infow("swapped root-dir aside for lazy GC", "rootDir", rootDir, "trash", trash)
			return
		}
		// Already gone is done, not a reason to delete it again. Removing a
		// container wipes its root-dir, so by the time this runs there is
		// usually nothing to move — and the in-place fallback then walks
		// RouterOS's entire file table to remove a path that is not there.
		// That is minutes of router CPU spent on nothing, and it degrades
		// every other service on the device while it runs.
		if errors.Is(err, routeros.ErrSourceNotFound) {
			p.deps.Logger.Debugw("root-dir already gone — nothing to swap aside", "rootDir", rootDir)
			return
		}
		p.deps.Logger.Warnw("root-dir rename failed, falling back to in-place delete",
			"rootDir", rootDir, "error", err)
	}
	if err := p.deps.Runtime.RemoveDirectory(ctx, rootDir); err != nil {
		p.deps.Logger.Debugw("root-dir cleanup (may not exist yet)", "rootDir", rootDir, "error", err)
	}
}

// runRootDirGC periodically reclaims root-dirs swapped aside by
// swapRootDirAside. It runs on its own goroutine — never on the reconcile path —
// because deleting a large root-dir tree is slow and must not stall pod
// reconciliation.
// runBridgePortGC reaps static bridge-port entries whose interface no longer
// exists. RemoveVeth now removes a veth's entry before deleting it, so this is
// a safety net rather than the primary mechanism: it self-heals installs that
// already leaked (20,737 orphans on rose1, #14) and covers any path that
// deletes an interface without going through RemoveVeth.
//
// Runs once shortly after startup, then hourly — orphans accumulate one per
// container recreate, so there is nothing to gain from a tight loop, and the
// unfiltered port listing is the expensive kind of call to repeat.
func (p *MicroKubeProvider) runBridgePortGC(ctx context.Context) {
	cli := p.routerOSClient()
	if cli == nil {
		return // static bridge ports are a RouterOS concept
	}
	sweep := func() {
		removed, err := cli.GCOrphanedBridgePorts(ctx)
		if err != nil {
			p.deps.Logger.Warnw("bridge-port GC failed", "error", err)
			return
		}
		if removed > 0 {
			p.deps.Logger.Infow("reaped orphaned bridge ports", "count", removed)
		}
	}

	select {
	case <-ctx.Done():
		return
	case <-time.After(90 * time.Second):
		sweep()
	}

	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
	}
}

func (p *MicroKubeProvider) runRootDirGC(ctx context.Context) {
	if p.routerOSClient() == nil {
		return // rename/trash scheme is RouterOS-only
	}
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.reapStaleRootDirs(ctx)
		}
	}
}

// reapStaleRootDirs best-effort removes ".trash-*" directories left under the
// storage base path by swapRootDirAside. Failures are retried on the next tick.
// Only meaningful for the RouterOS runtime.
func (p *MicroKubeProvider) reapStaleRootDirs(ctx context.Context) {
	cli := p.routerOSClient()
	if cli == nil {
		return
	}
	base := p.deps.Config.Storage.BasePath
	if base == "" {
		return
	}
	entries, err := cli.ListDirectory(ctx, base)
	if err != nil {
		return
	}
	for _, name := range entries {
		if !strings.Contains(name, rootDirTrashSuffix) {
			continue
		}
		full := strings.TrimSuffix(base, "/") + "/" + name
		if err := cli.RemoveDirectory(ctx, full); err != nil {
			p.deps.Logger.Debugw("stale root-dir reap deferred", "path", full, "error", err)
			continue
		}
		p.deps.Logger.Infow("reaped stale root-dir", "path", full)
	}
}

// runImageStager pre-pulls and stages the tarball (+ .digest sidecar) for every
// image referenced by a tracked pod, ahead of any CreatePod needing it. This
// keeps image pulls off the pod-(re)create critical path: after an mkube
// restart the in-memory image map is empty, so without pre-staging the first
// (re)create of each image would pull + flatten + upload under the storage
// mutex — the multi-minute stall that made DNS auto-recovery crawl. The stager
// warms /raid1/cache in the background while existing containers keep running,
// so any later recreate is a digest-validated disk-cache hit (~1s untar). Runs
// once shortly after startup, then on a slow timer. Off the reconcile path.
func (p *MicroKubeProvider) runImageStager(ctx context.Context) {
	if p.deps.StorageMgr == nil {
		return
	}
	// Let startup settle so the stager doesn't contend with the first
	// reconcile's own EnsureImage calls, then do the initial warm.
	select {
	case <-ctx.Done():
		return
	case <-time.After(45 * time.Second):
	}
	p.stageDesiredImages(ctx)

	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.stageDesiredImages(ctx)
		}
	}
}

// stageDesiredImages ensures every distinct image referenced by a tracked pod
// has a staged tarball + digest sidecar. EnsureImage is idempotent and cheap on
// a cache hit (one registry HEAD); a miss pulls and stages. Each distinct image
// is touched once per pass. Each op is bounded so a hung registry can't wedge
// the loop.
func (p *MicroKubeProvider) stageDesiredImages(ctx context.Context) {
	seen := make(map[string]struct{})
	staged := 0
	for _, pod := range p.pods.Values() {
		if pod == nil {
			continue
		}
		for _, c := range pod.Spec.Containers {
			ref := c.Image
			if ref == "" {
				continue
			}
			if _, ok := seen[ref]; ok {
				continue
			}
			seen[ref] = struct{}{}
			opCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
			if _, err := p.deps.StorageMgr.EnsureImage(opCtx, ref); err != nil {
				p.deps.Logger.Warnw("image stager: failed to stage", "ref", ref, "error", err)
			} else {
				staged++
			}
			cancel()
		}
	}
	if len(seen) > 0 {
		p.deps.Logger.Infow("image stager pass complete", "distinct_images", len(seen), "staged_ok", staged)
	}
}

func (p *MicroKubeProvider) CreatePod(ctx context.Context, pod *corev1.Pod) error {
	log := p.deps.Logger.With("pod", podKey(pod))
	log.Infow("creating pod")
	tracker := newPhaseTracker()

	// Determine target network from annotation
	networkName := pod.Annotations[annotationNetwork]
	namespaceName := pod.Annotations[annotationNamespace]

	containerIPs := make(map[string]string) // container name → bare IP

	// Device passthrough: allocate devices if annotations are present (StormBase only)
	if sb, ok := p.deps.Runtime.(*stormbase.Client); ok {
		if deviceClass := pod.Annotations[annotationDeviceClass]; deviceClass != "" {
			count := uint32(1)
			if countStr := pod.Annotations[annotationDeviceCount]; countStr != "" {
				if n, err := strconv.ParseUint(countStr, 10, 32); err == nil {
					count = uint32(n)
				}
			}
			log.Infow("allocating devices", "class", deviceClass, "count", count)
			alloc, err := sb.AllocateDevices(ctx, podKey(pod), deviceClass, count)
			if err != nil {
				return fmt.Errorf("allocating devices: %w", err)
			}
			if pod.Annotations == nil {
				pod.Annotations = make(map[string]string)
			}
			pod.Annotations[annotationDeviceAllocation] = alloc.AllocationID
			log.Infow("devices allocated",
				"allocation", alloc.AllocationID,
				"devices", len(alloc.Devices),
				"paths", alloc.DevicePaths,
				"caps", alloc.Capabilities,
			)
		}
	}

	for i, container := range pod.Spec.Containers {
		name := sanitizeName(pod, container.Name)

		// 0. Pre-creation cleanup: remove any stale RouterOS container with
		// the same name from a previous failed CreatePod attempt. Without this,
		// the orphaned container holds the veth interface and blocks recreation.
		// NOTE: Do NOT RemoveMountsByList here — PVC mounts must survive across
		// container recreation. ReconcileMounts (step 3) handles stale cleanup.
		tracker.start(PhaseCleanup)
		if ct, err := p.deps.Runtime.GetContainer(ctx, name); err == nil {
			log.Warnw("stale container found, cleaning up before recreation",
				"name", name, "status", ct.Status, "id", ct.ID)
			p.stopAndRemoveContainer(ctx, name, ct.ID)
		}

		tracker.done()

		// 1. Resolve image → tarball path.
		//
		// CoW pods deliberately skip this: their rootfs comes from a golden
		// clone, and the container is fed a 5 KB stub. Staging the full
		// docker-save tarball here would defeat the point — with an external
		// golden builder mkube would still hold every image on disk purely
		// to read a digest and an entrypoint. Step 1b resolves both from the
		// registry, and fails if the registry cannot answer — it never falls
		// back to staging, because a digest from a staged tarball is not the
		// manifest digest the golden is named for.
		tracker.start(PhaseImageResolve)
		var tarballPath string
		if filePath := pod.Annotations[annotationFile]; filePath != "" {
			// Use local tarball directly (skip OCI pull)
			tarballPath = filePath
		} else if !isCoWPod(pod) {
			var err error
			tarballPath, err = p.deps.StorageMgr.EnsureImage(ctx, container.Image)
			if err != nil {
				return fmt.Errorf("ensuring image %s: %w", container.Image, err)
			}
		}

		tracker.done()

		// 1b. CoW image mode: the rootfs comes from a stormblock clone of a
		// golden per-digest template; RouterOS only ever extracts a tiny
		// generic stub. See cow_catalog.go for the proven recipe.
		cowMode := isCoWPod(pod)
		var cowPayloadMount, cowEntrypoint, cowCmd string
		if cowMode {
			rosC := p.getRouterOSClient()
			if rosC == nil {
				return fmt.Errorf("cow image mode requires the RouterOS backend")
			}
			if err := p.ensureGenericStub(ctx, rosC); err != nil {
				return fmt.Errorf("ensuring cow stub: %w", err)
			}
			// Digest and entrypoint come from the REGISTRY and nowhere else: a
			// manifest HEAD and a few-KB config blob.
			//
			// There used to be a fallback that staged the whole image as a
			// docker-save tarball and read the digest from a sidecar beside
			// it. That is wrong twice over. The golden is named for the OCI
			// manifest digest, which is what the builder publishing
			// img-<digest12> uses, and a tarball sidecar is a different
			// number — so the two disagreed and the pod waited out goldenWait
			// for a template nobody would ever publish. Observed live: a
			// busybox pod waited for img-dabc0d074642, which is the digest of
			// an unrelated image entirely. And staging defeats the point of a
			// CoW pod, which exists so the image is never pulled to the device.
			//
			// If the registry cannot answer, that is the error. Do not guess.
			digest, dErr := p.deps.StorageMgr.GetCurrentDigest(ctx, container.Image)
			if dErr != nil {
				return fmt.Errorf("cow image mode: resolving the manifest digest for %s: %w",
					container.Image, dErr)
			}
			if digest == "" {
				return fmt.Errorf("cow image mode: registry returned no manifest digest for %s",
					container.Image)
			}

			var imgCfg *dockerSaveConfig
			if blob, cErr := p.deps.StorageMgr.RemoteImageConfig(ctx, container.Image); cErr == nil {
				if parsed, pErr := parseImageConfigJSON(blob); pErr == nil {
					imgCfg = parsed
				} else {
					log.Warnw("cow: cannot parse registry image config", "error", pErr)
				}
			} else {
				// Not fatal: the pod's own command/args can carry it. Staging
				// the image to recover two fields is not worth it here.
				log.Warnw("cow: registry image config unavailable, relying on pod command",
					"image", container.Image, "error", cErr)
			}

			// No tarball: in sbregistry mode nothing is staged, and the
			// mkube-seeding path stages for itself only if it actually builds.
			templateName, err := p.ensureGoldenTemplate(ctx, rosC, container.Image, "", digest)
			if err != nil {
				return fmt.Errorf("cow golden template: %w", err)
			}
			payloadRootfs, volID, err := p.provisionCoWRoot(ctx, rosC, pod, container.Name, templateName)
			if err != nil {
				return fmt.Errorf("cow root volume: %w", err)
			}
			cowPayloadMount = payloadRootfs
			if pod.Annotations == nil {
				pod.Annotations = map[string]string{}
			}
			pod.Annotations[annCoWVolumeID] = volID
			pod.Annotations[annCoWTemplate] = templateName
			if p.deps.Store != nil {
				storeKey := pod.Namespace + "." + pod.Name
				_, _ = p.deps.Store.Pods.PutJSON(ctx, storeKey, pod)
			}
			// Whether this build can put stormpivot in the stub decides
			// how the entrypoint is expressed — see
			// rewriteEntrypointForCoW.
			cowEntrypoint, cowCmd = rewriteEntrypointForCoW(pod, &container, imgCfg, haveStormPivot())
			if cowEntrypoint == "" {
				return fmt.Errorf("cow image mode: no entrypoint (set the pod command or use an image that has one)")
			}
			tarballPath = cowStubDevicePath
			log.Infow("cow root provisioned", "template", templateName, "payload", cowPayloadMount, "entrypoint", cowEntrypoint)
		}

		// 2. Allocate network (registers containerName.podName in network zone)
		tracker.start(PhaseNetworkAlloc)
		vethName := vethName(pod, i)
		containerHostname := container.Name + "." + pod.Name
		staticIP := pod.Annotations[annotationStaticIP]
		ip, gw, dnsServer, err := p.deps.NetworkMgr.AllocateInterface(ctx, vethName, containerHostname, networkName, staticIP)
		if err != nil {
			// If veth/IP exists from a previous failed attempt, clean up and retry.
			errMsg := err.Error()
			if strings.Contains(errMsg, "already have interface") || strings.Contains(errMsg, "already allocated to") {
				log.Warnw("cleaning up orphaned veth", "veth", vethName, "reason", errMsg)

				// If a staging veth (__stg) holds the IP, release it first.
				// This happens when a blue-green update leaked the staging veth.
				if strings.Contains(errMsg, "__stg") {
					stgVeth := truncate(vethName, 58) + "__stg"
					stgName := truncate(name, 58) + "__stg"
					log.Warnw("releasing leaked staging veth", "stgVeth", stgVeth)
					p.cleanupStagingResources(ctx, stgName, stgVeth)
				}

				// If the IP is held by a differently-named veth (e.g. renamed
				// stale veth), extract that name and release it too.
				if strings.Contains(errMsg, "already allocated to") {
					if staleVeth := extractAllocHolder(errMsg); staleVeth != "" && staleVeth != vethName {
						log.Warnw("releasing stale veth holding IP", "staleVeth", staleVeth)
						if releaseErr := p.deps.NetworkMgr.ReleaseInterface(ctx, staleVeth); releaseErr != nil {
							log.Warnw("stale veth release failed, force-releasing", "staleVeth", staleVeth, "error", releaseErr)
							p.forceReleaseVeth(ctx, staleVeth)
						}
					}
				}

				if releaseErr := p.deps.NetworkMgr.ReleaseInterface(ctx, vethName); releaseErr != nil {
					// ReleaseInterface may fail if a container still holds the veth.
					// Find and forcibly remove the container holding it.
					log.Warnw("release failed, force-releasing veth", "veth", vethName, "error", releaseErr)
					p.forceReleaseVeth(ctx, vethName)
				}
				ip, gw, dnsServer, err = p.deps.NetworkMgr.AllocateInterface(ctx, vethName, containerHostname, networkName, staticIP)
			}
			if err != nil {
				return fmt.Errorf("allocating network for %s: %w", name, err)
			}
		}
		bareIP := strings.Split(ip, "/")[0]
		containerIPs[container.Name] = bareIP
		log.Infow("allocated network", "veth", vethName, "ip", ip, "gateway", gw, "dns", dnsServer,
			"container_hostname", containerHostname)

		// 2b. If namespace is specified, register container subdomain in namespace zone too
		if namespaceName != "" && p.deps.Namespace != nil {
			endpoint, zoneID, err := p.deps.Namespace.ResolveNamespace(namespaceName)
			if err != nil {
				log.Warnw("failed to resolve namespace, using default DNS", "namespace", namespaceName, "error", err)
			} else {
				dnsClient := p.deps.NetworkMgr.DNSClient()
				if dnsClient != nil {
					_ = dnsClient.CleanStaleRecords(ctx, endpoint, zoneID, containerHostname, bareIP)
					if regErr := dnsClient.RegisterHost(ctx, endpoint, zoneID, containerHostname, bareIP, 60); regErr != nil {
						log.Warnw("failed to register container in namespace zone", "namespace", namespaceName, "error", regErr)
					}
				}
				p.deps.Namespace.AddContainerToNamespace(namespaceName, name)
			}
		}

		tracker.done()

		// 3. Provision volumes, write ConfigMap data, and reconcile mount entries.
		// Uses ReconcileMounts to preserve PVC-backed mounts across recreation.
		tracker.start(PhaseVolumeMount)
		var desiredMounts []runtime.DesiredMount
		for _, vm := range container.VolumeMounts {
			var hostPath string
			isPVC := false

			// Three-way volume resolution:
			// 1. PVC-backed volume — persistent, bypasses ProvisionVolume/GC
			// 2. ConfigMap-backed volume — write data files
			// 3. Ephemeral (default) — ProvisionVolume, subject to GC
			if pvcPath, ok := p.resolvePVCVolume(ctx, pod, vm.Name); ok {
				hostPath = pvcPath
				isPVC = true
				log.Infow("using PVC volume", "volume", vm.Name, "path", hostPath)
			} else if data := p.resolveConfigMapVolume(pod, vm.Name); data != nil {
				// ConfigMap volume: provision ephemeral host dir, then write data files
				var err error
				hostPath, err = p.deps.StorageMgr.ProvisionVolume(ctx, name, vm.Name, vm.MountPath)
				if err != nil {
					_ = p.deps.NetworkMgr.ReleaseInterface(ctx, vethName)
					return fmt.Errorf("provisioning volume %s: %w", vm.Name, err)
				}
				localDir := fmt.Sprintf("/data/configmaps/%s/%s", name, vm.Name)
				if mkErr := os.MkdirAll(localDir, 0o755); mkErr != nil {
					log.Warnw("failed to create configmap dir", "path", localDir, "error", mkErr)
				} else {
					for filename, content := range data {
						if wErr := os.WriteFile(localDir+"/"+filename, []byte(content), 0o644); wErr != nil {
							log.Warnw("failed to write configmap file", "path", localDir+"/"+filename, "error", wErr)
						}
					}
					hostPath = p.deps.StorageMgr.HostVisiblePath(localDir)
				}
			} else if data := p.resolveSecretVolume(pod, vm.Name); data != nil {
				// Secret volume: provision ephemeral host dir, then write decrypted data files
				var err error
				hostPath, err = p.deps.StorageMgr.ProvisionVolume(ctx, name, vm.Name, vm.MountPath)
				if err != nil {
					_ = p.deps.NetworkMgr.ReleaseInterface(ctx, vethName)
					return fmt.Errorf("provisioning volume %s: %w", vm.Name, err)
				}
				localDir := fmt.Sprintf("/data/secrets/%s/%s", name, vm.Name)
				if mkErr := os.MkdirAll(localDir, 0o700); mkErr != nil {
					log.Warnw("failed to create secret dir", "path", localDir, "error", mkErr)
				} else {
					for filename, content := range data {
						if wErr := os.WriteFile(localDir+"/"+filename, []byte(content), 0o600); wErr != nil {
							log.Warnw("failed to write secret file", "path", localDir+"/"+filename, "error", wErr)
						}
					}
					hostPath = p.deps.StorageMgr.HostVisiblePath(localDir)
				}
			} else {
				// Ephemeral volume (default)
				var err error
				hostPath, err = p.deps.StorageMgr.ProvisionVolume(ctx, name, vm.Name, vm.MountPath)
				if err != nil {
					_ = p.deps.NetworkMgr.ReleaseInterface(ctx, vethName)
					return fmt.Errorf("provisioning volume %s: %w", vm.Name, err)
				}
			}

			// Under CoW with the pivot, a mount has to land *inside* the
			// payload. The container chroots into it before the image runs,
			// so anything mounted beside the payload is unreachable by name
			// afterwards — a PVC at /data would simply not be there. Mounted
			// at <payload>/data it is /data once pivoted, which is where the
			// image expects it. Verified on rose1: a directory placed inside
			// the payload appeared at /data after the chroot.
			dst := vm.MountPath
			if cowMode && haveStormPivot() {
				dst = cowPayloadDst + "/" + strings.TrimPrefix(vm.MountPath, "/")
			}
			desiredMounts = append(desiredMounts, runtime.DesiredMount{
				Src:   hostPath,
				Dst:   dst,
				IsPVC: isPVC,
			})
		}

		if cowMode && cowPayloadMount != "" {
			// NOT IsPVC: the clone's mount slot drifts across re-attaches,
			// and PVC-flagged entries are never removed by ReconcileMounts —
			// a stale slot entry from the previous create then fails every
			// container start ("error creating src /iscsiN/rootfs"). The
			// mount is re-declared with the current slot on every create,
			// so reconciler-managed is exactly right.
			desiredMounts = append(desiredMounts, runtime.DesiredMount{
				Src: cowPayloadMount,
				Dst: cowPayloadDst,
			})
		}

		mountListName := ""
		if len(desiredMounts) > 0 {
			mountListName = name
			if err := p.deps.Runtime.ReconcileMounts(ctx, name, desiredMounts); err != nil {
				_ = p.deps.NetworkMgr.ReleaseInterface(ctx, vethName)
				return fmt.Errorf("reconciling mounts for %s: %w", name, err)
			}
		}

		tracker.done()

		// 4. Determine boot behavior
		startOnBoot := "false"
		if pod.Spec.RestartPolicy == corev1.RestartPolicyAlways {
			startOnBoot = "true"
		}

		// RouterOS's start-on-boot is a separate question from whether mkube
		// manages the container. A CoW container's rootfs is a bind mount from
		// a network-attached clone (/flash/rw/disk/<slot>/rootfs -> /payload).
		// At boot RouterOS starts containers before that disk is attached and
		// mounted, so it tries to create the bind-mount source under an
		// unmounted mountpoint and fails with "Read-only file system". mkube
		// starts these itself once the clone is attached, mounted and
		// writable, so RouterOS must not race it at boot.
		rosStartOnBoot := startOnBoot
		if cowMode {
			rosStartOnBoot = "false"
		}

		// 5. Swap any old root-dir aside (atomic rename) to force tarball
		// re-extraction. RouterOS skips extraction when root-dir already has
		// content, so without this stale images persist. Renaming rather than
		// deleting in-line also avoids the "root-dir overlap" wedge a slow/failed
		// recursive delete would leave for the next retry (TODO #12).
		rootDir := fmt.Sprintf("%s/%s", p.deps.Config.Storage.BasePath, name)
		p.swapRootDirAside(ctx, rootDir)

		// 6. Create the container
		tracker.start(PhaseContainerCreate)
		spec := runtime.ContainerSpec{
			Name:        name,
			Image:       tarballPath,
			Interface:   vethName,
			RootDir:     rootDir,
			MountLists:  mountListName,
			Cmd:         strings.Join(container.Command, " "),
			Command:     container.Command,
			Hostname:    pod.Name,
			DNS:         dnsServer,
			Logging:     "true",
			StartOnBoot: rosStartOnBoot,
		}

		if cowMode {
			spec.Entrypoint = cowEntrypoint
			spec.Cmd = cowCmd
			spec.Command = nil
		}

		// Set root user for containers that need privileged port binding
		// (e.g. DHCP on port 67). Check if this network serves DHCP
		// either locally or via serverNetwork targeting it.
		if p.networkHasDHCP(networkName) {
			spec.User = "0:0"
		}

		// Resolve environment variables from Secrets, ConfigMaps, and plain values
		_ = p.deps.Runtime.RemoveEnvsByList(ctx, name)
		envVars := p.resolveContainerEnv(pod, &container)
		if len(envVars) > 0 {
			for _, env := range envVars {
				k, v, _ := strings.Cut(env, "=")
				_ = p.deps.Runtime.CreateEnv(ctx, name, k, v)
			}
			spec.Envlist = name // RouterOS: reference the env list
			spec.Env = envVars  // StormBase: pass via gRPC
		}

		if err := p.deps.Runtime.CreateContainer(ctx, spec); err != nil {
			log.Warnw("cleaning up partial root-dir and veth after container creation failure", "veth", vethName, "rootDir", rootDir)
			p.swapRootDirAside(ctx, rootDir)
			_ = p.deps.NetworkMgr.ReleaseInterface(ctx, vethName)
			return fmt.Errorf("creating container %s: %w", name, err)
		}

		tracker.done()

		// 7. Wait for tarball extraction then start the container.
		// After creation RouterOS extracts the tarball; the container is
		// not yet "stopped" until extraction finishes.
		tracker.start(PhaseTarballExtract)
		ct, err := p.waitForStopped(ctx, name, 120*time.Second)
		if err != nil {
			p.swapRootDirAside(ctx, rootDir)
			_ = p.deps.NetworkMgr.ReleaseInterface(ctx, vethName)
			return fmt.Errorf("waiting for container %s to be ready: %w", name, err)
		}

		// Start with retry — MikroTik REST API can return EOF if the
		// previous container hasn't fully torn down yet (race between
		// delete and create).
		tracker.done()
		tracker.start(PhaseContainerStart)
		startBackoffs := []time.Duration{
			2 * time.Second, 2 * time.Second,
			3 * time.Second, 3 * time.Second,
			5 * time.Second, 5 * time.Second,
		}
		var startErr error
		for attempt := 0; attempt <= len(startBackoffs); attempt++ {
			if startErr = p.deps.Runtime.StartContainer(ctx, ct.ID); startErr == nil {
				break
			}
			if attempt < len(startBackoffs) {
				log.Warnw("container start failed, retrying",
					"name", name, "attempt", attempt+1, "error", startErr)
				time.Sleep(startBackoffs[attempt])
				// Re-fetch container in case ID changed
				if updated, err := p.deps.Runtime.GetContainer(ctx, name); err == nil {
					ct = updated
				}
			}
		}
		if startErr != nil {
			p.swapRootDirAside(ctx, rootDir)
			_ = p.deps.NetworkMgr.ReleaseInterface(ctx, vethName)
			return fmt.Errorf("starting container %s after %d attempts: %w", name, len(startBackoffs)+1, startErr)
		}

		tracker.done()

		// 8. Register with lifecycle manager for boot ordering / health probes
		tracker.start(PhaseLifecycleReg)
		if startOnBoot == "true" {
			p.deps.LifecycleMgr.Register(name, lifecycle.ContainerUnit{
				Name:          name,
				ContainerID:   ct.ID,
				ContainerIP:   bareIP,
				RestartPolicy: string(pod.Spec.RestartPolicy),
				StartOnBoot:   true,
				Managed:       true,
				Probes:        extractProbes(container),
				HealthCheck:   extractHealthCheck(container),
				DependsOn:     extractDependencies(pod),
				Priority:      extractPriority(pod, i),
			})
		}

		tracker.done()
		log.Infow("container created and started", "name", name, "id", ct.ID)
	}

	// 9. Register DNS aliases (pod-level default + custom aliases from annotation)
	tracker.start(PhaseDNSRegister)
	p.registerPodAliases(ctx, pod, networkName, namespaceName, containerIPs, log)

	tracker.done()

	// 10. Push pod→container mappings to micrologs
	tracker.start(PhasePodReady)
	p.pushLogMappings(ctx, pod, log)

	// Stamp the deployed image digest so we can detect stale images on restart.
	p.stampImageDigest(ctx, pod)

	// Stamp the allocated IP as a static reservation so the pod keeps the same IP forever.
	p.stampAssignedIP(ctx, pod, containerIPs)

	// Track the pod
	p.pods.Set(podKey(pod), pod.DeepCopy())

	// Record events
	p.recordEvent(pod, "Scheduled", fmt.Sprintf("Successfully assigned %s/%s to %s", pod.Namespace, pod.Name, p.nodeName), "Normal")
	for _, c := range pod.Spec.Containers {
		p.recordEvent(pod, "Pulling", fmt.Sprintf("Pulling image %q", c.Image), "Normal")
		p.recordEvent(pod, "Created", fmt.Sprintf("Created container %s", c.Name), "Normal")
		p.recordEvent(pod, "Started", fmt.Sprintf("Started container %s", c.Name), "Normal")
	}

	tracker.done()

	// Run async consistency check to clean up any orphaned resources
	p.CheckConsistencyAsync("create-pod/" + podKey(pod))

	return nil
}

// waitForStopped polls until the container reaches the "stopped" state
// (tarball extraction complete) or the timeout expires.
func (p *MicroKubeProvider) waitForStopped(ctx context.Context, name string, timeout time.Duration) (*runtime.Container, error) {
	deadline := time.After(timeout)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		ct, err := p.deps.Runtime.GetContainer(ctx, name)
		if err != nil {
			return nil, err
		}
		if ct.IsStopped() {
			return ct, nil
		}
		p.deps.Logger.Debugw("waiting for container extraction", "name", name)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline:
			return nil, fmt.Errorf("timed out waiting for container %s to reach stopped state", name)
		case <-ticker.C:
		}
	}
}

// stopAndRemoveContainer stops a running container, waits for it to stop,
// then removes it with retry. Returns true if the container was successfully
// removed, false if it could not be removed after all retries.
func (p *MicroKubeProvider) stopAndRemoveContainer(ctx context.Context, name, id string) bool {
	log := p.deps.Logger

	// Resolve the actual container ID if not provided (some callers pass empty ID)
	if ct, err := p.deps.Runtime.GetContainer(ctx, name); err == nil {
		if id == "" {
			id = ct.ID
		}
		if ct.IsRunning() {
			_ = p.deps.Runtime.StopContainer(ctx, id)
			for j := 0; j < 30; j++ {
				time.Sleep(500 * time.Millisecond)
				if updated, err := p.deps.Runtime.GetContainer(ctx, name); err != nil || !updated.IsRunning() {
					break
				}
			}
		}
	} else {
		// Container not found — already gone
		return true
	}

	// Retry removal with progressive backoff (matches DeletePod robustness).
	// RouterOS may reject removal if the container hasn't fully stopped yet.
	backoffs := []time.Duration{
		500 * time.Millisecond, 1 * time.Second,
		1 * time.Second, 2 * time.Second,
		2 * time.Second, 3 * time.Second,
	}
	for attempt := 0; attempt <= len(backoffs); attempt++ {
		if err := p.deps.Runtime.RemoveContainer(ctx, id); err != nil {
			// Check if the container is already gone (e.g. removed by another path)
			if _, gerr := p.deps.Runtime.GetContainer(ctx, name); gerr != nil {
				log.Infow("container gone after retry", "name", name)
				return true
			}

			errMsg := err.Error()
			if attempt < len(backoffs) {
				// If still running, re-issue stop before next attempt
				if strings.Contains(errMsg, "running") {
					log.Warnw("container still running, re-issuing stop before retry",
						"name", name, "attempt", attempt+1, "error", err)
					_ = p.deps.Runtime.StopContainer(ctx, id)
				} else {
					log.Warnw("container removal failed, retrying",
						"name", name, "attempt", attempt+1, "error", err)
				}
				time.Sleep(backoffs[attempt])
			} else {
				log.Errorw("failed to remove container after all retries",
					"name", name, "id", id, "attempts", len(backoffs)+1, "error", err)
				return false
			}
		} else {
			log.Infow("removed container", "name", name, "id", id)
			return true
		}
	}
	return false
}

// forceReleaseVeth finds the RouterOS container holding a veth interface,
// stops and removes it, then releases the veth. Used during CreatePod to
// recover when an orphaned container blocks veth allocation.
func (p *MicroKubeProvider) forceReleaseVeth(ctx context.Context, vethName string) {
	log := p.deps.Logger

	containers, err := p.deps.Runtime.ListContainers(ctx)
	if err != nil {
		log.Warnw("failed to list containers for veth force-release", "error", err)
		return
	}

	for _, ct := range containers {
		if ct.Interface == vethName {
			log.Warnw("found container holding orphaned veth, removing",
				"container", ct.Name, "veth", vethName, "id", ct.ID)
			p.stopAndRemoveContainer(ctx, ct.Name, ct.ID)
			// NOTE: Do NOT RemoveMountsByList here — PVC mounts must survive.
			// ReconcileMounts during the subsequent CreatePod handles stale cleanup.
			break
		}
	}

	// Retry veth release after container removal
	if err := p.deps.NetworkMgr.ReleaseInterface(ctx, vethName); err != nil {
		log.Warnw("veth release still failed after container removal", "veth", vethName, "error", err)
	}
}

// UpdatePod handles pod spec updates by recreating the pod.
//
// This used to be a blue-green cutover: a staging container extracted the new
// tarball while the old one kept serving, then a fast swap took the
// pre-extracted root-dir, because RouterOS skips extraction when root-dir
// already has content. All of that existed to avoid paying for an extraction.
//
// A CoW pod does not extract anything — its root is a clone of the image's
// golden volume, which is a metadata operation — so there is nothing for
// staging to buy. For a pod still served from a tarball the cost is the untar,
// which the digest-validated staging cache already brought down to about a
// second. That is the whole of what was traded away, and against it: staging
// doubled the container count during every update, and could hang mid-cutover
// and strand the pod's redeploying flag, after which the reconciler skipped
// that pod for good.
func (p *MicroKubeProvider) UpdatePod(ctx context.Context, pod *corev1.Pod) error {
	log := p.deps.Logger.With("pod", podKey(pod))

	// Explicit network/static-ip change path. Blue-green reuses the existing
	// veth and IP, so it can never move a pod to another network — the old
	// implicit path cut over on the OLD network, then the reconciler tore the
	// container down and the pod was stranded. Tear down using the OLD pod
	// (so veth/alias/DNS cleanup targets the old network), then create fresh
	// on the new one.
	if old, ok := p.pods.Get(podKey(pod)); ok {
		oldNet := old.Annotations[annotationNetwork]
		newNet := pod.Annotations[annotationNetwork]
		oldIP := old.Annotations[annotationStaticIP]
		newIP := pod.Annotations[annotationStaticIP]
		if (newNet != "" && newNet != oldNet) || (newIP != "" && oldIP != "" && newIP != oldIP) {
			log.Infow("pod network/static-ip changed — destructive recreate on new network",
				"oldNetwork", oldNet, "newNetwork", newNet, "oldIP", oldIP, "newIP", newIP)
			p.recordEvent(pod, "NetworkChange",
				fmt.Sprintf("Recreating pod %s/%s on network %s (%s)", pod.Namespace, pod.Name, newNet, newIP), "Normal")
			// Moving networks: the old veth is wrong now and must go.
			p.teardownForUpdate(ctx, old, false)
			return p.CreatePod(ctx, pod)
		}
	}

	log.Infow("updating pod (recreate)")

	// Hold the reconciler off for the whole recreate, and clear it here rather
	// than anywhere further in. Blue-green set this flag inside a routine that
	// could hang mid-cutover, and a hung routine never returns to clear it —
	// after which the reconciler skipped the pod permanently. Set and cleared
	// in one function with a defer, it cannot outlive the update.
	key := podKey(pod)
	p.redeploying.Set(key, true)
	defer p.redeploying.Delete(key)

	// teardownForUpdate rather than DeletePod: DeletePod calls
	// RemoveMountsByList, which destroys PVC and ConfigMap mounts. This keeps
	// mounts intact so CreatePod's ReconcileMounts reconciles them instead of
	// rebuilding from scratch.
	p.teardownForUpdate(ctx, pod, true)
	if err := p.CreatePod(ctx, pod); err != nil {
		return err
	}
	// Stamp the deployed digest after successful update
	p.stampImageDigest(ctx, pod)
	p.pods.Set(key, pod.DeepCopy())
	return nil
}

// teardownForUpdate removes containers and veths for a pod that is about to
// be recreated by CreatePod. Unlike DeletePod, it does NOT remove mount
// entries — this preserves PVC and ConfigMap mounts so that CreatePod's
// ReconcileMounts can add missing mounts rather than recreating from scratch.
// teardownForUpdate stops a pod's containers so CreatePod can rebuild them.
//
// keepNetwork keeps the veth, its IP and its DNS registration in place. An
// in-place update rebuilds the same pod, on the same network, at the same
// static IP — so tearing the veth down only to have CreatePod add an
// identical one back is churn with a gap in the middle where the address does
// not exist. Every layer below is already idempotent for the same owner
// (`AllocateStatic` returns nil when the same key holds the IP, `CreateVeth`
// returns nil on a match, `AddBridgePort` returns nil when already on the
// right bridge), so re-allocating over a veth that was never removed is a
// no-op rather than a conflict.
//
// It is false only when the pod is moving: a changed network or static IP
// means the existing veth is now wrong, and it has to go.
func (p *MicroKubeProvider) teardownForUpdate(ctx context.Context, pod *corev1.Pod, keepNetwork bool) {
	log := p.deps.Logger.With("pod", podKey(pod))

	// Unregister from lifecycle manager to prevent watchdog interference
	for _, container := range pod.Spec.Containers {
		name := sanitizeName(pod, container.Name)
		p.deps.LifecycleMgr.Unregister(name)
	}

	// Collect container IPs for alias cleanup
	networkName := pod.Annotations[annotationNetwork]
	namespaceName := pod.Annotations[annotationNamespace]
	containerIPs := make(map[string]string)
	for i, container := range pod.Spec.Containers {
		vn := vethName(pod, i)
		if portIP, _, ok := p.deps.NetworkMgr.GetPortInfo(vn); ok {
			containerIPs[container.Name] = portIP
		}
	}
	p.deregisterPodAliases(ctx, pod, networkName, namespaceName, containerIPs, log)

	for i, container := range pod.Spec.Containers {
		name := sanitizeName(pod, container.Name)

		// Stop and remove the container
		if ct, err := p.deps.Runtime.GetContainer(ctx, name); err == nil {
			p.stopAndRemoveContainer(ctx, name, ct.ID)
		}

		// NOTE: Do NOT RemoveMountsByList — mount entries must survive so
		// ReconcileMounts during CreatePod can preserve PVC mounts and
		// reconcile ConfigMap mounts without data loss.

		// Release the veth only when the pod is actually moving. Otherwise
		// keep it: CreatePod re-allocates onto the same one, and the address
		// never goes away in between.
		vn := vethName(pod, i)
		if keepNetwork {
			log.Debugw("keeping veth across update", "veth", vn)
		} else if err := p.deps.NetworkMgr.ReleaseInterface(ctx, vn); err != nil {
			log.Warnw("error releasing network during update teardown", "veth", vn, "error", err)
		}

		// Remove from namespace (CreatePod will re-register)
		if nsName := namespaceName; nsName != "" && p.deps.Namespace != nil {
			p.deps.Namespace.RemoveContainerFromNamespace(nsName, name)
		}
	}

	p.recordEvent(pod, "Killing", fmt.Sprintf("Tearing down pod %s/%s for update", pod.Namespace, pod.Name), "Normal")
	p.pods.Delete(podKey(pod))
	p.createFailures.Delete(podKey(pod))
	p.createBackoff.Delete(podKey(pod))
}

// createContainerMounts provisions volumes and creates mount entries for a container.
// Uses ReconcileMounts to preserve PVC-backed mounts across container recreation.
// Returns the mount list name (empty string if no volumes) or an error.
func (p *MicroKubeProvider) createContainerMounts(
	ctx context.Context, pod *corev1.Pod, containerName string,
	container corev1.Container, log *zap.SugaredLogger,
) (string, error) {
	if len(container.VolumeMounts) == 0 {
		return "", nil
	}

	var desired []runtime.DesiredMount

	for _, vm := range container.VolumeMounts {
		var hostPath string
		isPVC := false

		if pvcPath, ok := p.resolvePVCVolume(ctx, pod, vm.Name); ok {
			hostPath = pvcPath
			isPVC = true
		} else if data := p.resolveConfigMapVolume(pod, vm.Name); data != nil {
			var provErr error
			hostPath, provErr = p.deps.StorageMgr.ProvisionVolume(ctx, containerName, vm.Name, vm.MountPath)
			if provErr != nil {
				return "", fmt.Errorf("provisioning configmap volume %s: %w", vm.Name, provErr)
			}
			localDir := fmt.Sprintf("/data/configmaps/%s/%s", containerName, vm.Name)
			if mkErr := os.MkdirAll(localDir, 0o755); mkErr != nil {
				log.Warnw("failed to create configmap dir", "path", localDir, "error", mkErr)
			} else {
				for filename, content := range data {
					if wErr := os.WriteFile(localDir+"/"+filename, []byte(content), 0o644); wErr != nil {
						log.Warnw("failed to write configmap file", "path", localDir+"/"+filename, "error", wErr)
					}
				}
				hostPath = p.deps.StorageMgr.HostVisiblePath(localDir)
			}
		} else if data := p.resolveSecretVolume(pod, vm.Name); data != nil {
			var provErr error
			hostPath, provErr = p.deps.StorageMgr.ProvisionVolume(ctx, containerName, vm.Name, vm.MountPath)
			if provErr != nil {
				return "", fmt.Errorf("provisioning secret volume %s: %w", vm.Name, provErr)
			}
			localDir := fmt.Sprintf("/data/secrets/%s/%s", containerName, vm.Name)
			if mkErr := os.MkdirAll(localDir, 0o700); mkErr != nil {
				log.Warnw("failed to create secret dir", "path", localDir, "error", mkErr)
			} else {
				for filename, content := range data {
					if wErr := os.WriteFile(localDir+"/"+filename, []byte(content), 0o600); wErr != nil {
						log.Warnw("failed to write secret file", "path", localDir+"/"+filename, "error", wErr)
					}
				}
				hostPath = p.deps.StorageMgr.HostVisiblePath(localDir)
			}
		} else {
			var provErr error
			hostPath, provErr = p.deps.StorageMgr.ProvisionVolume(ctx, containerName, vm.Name, vm.MountPath)
			if provErr != nil {
				return "", fmt.Errorf("provisioning volume %s: %w", vm.Name, provErr)
			}
		}

		desired = append(desired, runtime.DesiredMount{
			Src:   hostPath,
			Dst:   vm.MountPath,
			IsPVC: isPVC,
		})
	}

	if err := p.deps.Runtime.ReconcileMounts(ctx, containerName, desired); err != nil {
		return "", fmt.Errorf("reconciling mounts for %s: %w", containerName, err)
	}

	return containerName, nil
}

// normalizePath strips leading "/" for consistent path comparison.
// RouterOS returns disk-relative paths (e.g. "raid1/images/foo") but
// mkube config uses absolute-style paths (e.g. "/raid1/images/foo").
func normalizePath(p string) string {
	return strings.TrimPrefix(p, "/")
}

// waitForRunning polls until the container reaches "running" state or timeout.
func (p *MicroKubeProvider) waitForRunning(ctx context.Context, name string, timeout time.Duration) bool {
	deadline := time.After(timeout)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		if ct, err := p.deps.Runtime.GetContainer(ctx, name); err == nil && ct.IsRunning() {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-deadline:
			return false
		case <-ticker.C:
		}
	}
}

// stopAndWait stops a container and waits for it to reach stopped state.
func (p *MicroKubeProvider) stopAndWait(ctx context.Context, name string) {
	ct, err := p.deps.Runtime.GetContainer(ctx, name)
	if err != nil || !ct.IsRunning() {
		return
	}
	_ = p.deps.Runtime.StopContainer(ctx, ct.ID)
	for j := 0; j < 15; j++ {
		time.Sleep(time.Second)
		if updated, err := p.deps.Runtime.GetContainer(ctx, name); err != nil || !updated.IsRunning() {
			return
		}
	}
}

// cleanupStagingResources removes a single staging container, its mounts,
// veth, and local configmap data.
// cleanupStagingResources removes a leftover `__stg` container and its veth.
//
// Blue-green is gone, but the debris it could leave is not: a cutover that
// died partway left a staging container holding the pod's IP, and CreatePod
// still has to be able to take that IP back. This is the janitor for that,
// not a live code path — when no `__stg` names remain on the router it can go.
func (p *MicroKubeProvider) cleanupStagingResources(ctx context.Context, stgName, stgVeth string) {
	if ct, err := p.deps.Runtime.GetContainer(ctx, stgName); err == nil {
		p.stopAndRemoveContainer(ctx, stgName, ct.ID)
	}
	_ = p.deps.Runtime.RemoveMountsByList(ctx, stgName)
	_ = p.deps.NetworkMgr.ReleaseInterface(ctx, stgVeth)
	_ = os.RemoveAll(fmt.Sprintf("/data/configmaps/%s", stgName))
}

// DeletePod removes all containers associated with a pod and cleans up
// networking and storage resources.
func (p *MicroKubeProvider) DeletePod(ctx context.Context, pod *corev1.Pod) error {
	log := p.deps.Logger.With("pod", podKey(pod))
	log.Infow("deleting pod")

	networkName := pod.Annotations[annotationNetwork]
	namespaceName := pod.Annotations[annotationNamespace]

	// Release device allocation if present (StormBase only)
	if sb, ok := p.deps.Runtime.(*stormbase.Client); ok {
		if allocID := pod.Annotations[annotationDeviceAllocation]; allocID != "" {
			log.Infow("releasing device allocation", "allocation", allocID)
			if err := sb.ReleaseDevices(ctx, allocID); err != nil {
				log.Warnw("failed to release device allocation", "allocation", allocID, "error", err)
			}
		}
	}

	// Unregister ALL containers from lifecycle manager FIRST to prevent
	// the watchdog from restarting containers while we're deleting them.
	for _, container := range pod.Spec.Containers {
		name := sanitizeName(pod, container.Name)
		p.deps.LifecycleMgr.Unregister(name)
	}

	// CoW image mode: hand the clone volume back (detach + delete) so the
	// thin volume, its export and its portal are not leaked.
	if volID := pod.Annotations[annCoWVolumeID]; volID != "" {
		if rosC := p.getRouterOSClient(); rosC != nil {
			defer p.deprovisionCoWRoot(ctx, rosC, volID)
		}
	}

	// Collect container IPs before releasing anything (needed for alias cleanup)
	containerIPs := make(map[string]string)
	for i, container := range pod.Spec.Containers {
		vethName := vethName(pod, i)
		if portIP, _, ok := p.deps.NetworkMgr.GetPortInfo(vethName); ok {
			containerIPs[container.Name] = portIP
		}
	}

	// Deregister DNS aliases before releasing interfaces
	p.deregisterPodAliases(ctx, pod, networkName, namespaceName, containerIPs, log)

	// Progressive backoff durations for container removal retries.
	backoffs := []time.Duration{
		1 * time.Second, 1 * time.Second,
		2 * time.Second, 2 * time.Second,
		3 * time.Second, 3 * time.Second,
		4 * time.Second, 5 * time.Second,
	}

	var lastErr error
	for i, container := range pod.Spec.Containers {
		name := sanitizeName(pod, container.Name)

		// Stop and remove the container
		ct, err := p.deps.Runtime.GetContainer(ctx, name)
		if err != nil {
			log.Warnw("container not found during delete", "name", name, "error", err)
			// Container doesn't exist — still clean up mounts, veth, namespace
			goto cleanup
		}

		if ct.IsRunning() {
			if err := p.deps.Runtime.StopContainer(ctx, ct.ID); err != nil {
				log.Warnw("error stopping container", "name", name, "error", err)
			}
			// Wait for the container to actually stop before removing
			for j := 0; j < 15; j++ {
				time.Sleep(time.Second)
				updated, err := p.deps.Runtime.GetContainer(ctx, name)
				if err != nil || !updated.IsRunning() {
					break
				}
			}
		}

		// Retry RemoveContainer with progressive backoff.
		// On "cannot remove running" errors, re-issue stop before retrying.
		for attempt := 0; attempt < len(backoffs); attempt++ {
			if err := p.deps.Runtime.RemoveContainer(ctx, ct.ID); err != nil {
				errMsg := err.Error()
				log.Warnw("error removing container, retrying", "name", name, "attempt", attempt+1, "error", err)

				// Re-fetch container to check if it's gone
				if _, gerr := p.deps.Runtime.GetContainer(ctx, name); gerr != nil {
					log.Infow("container gone after retry", "name", name)
					break
				}

				// If still running, re-issue stop before next attempt
				if strings.Contains(errMsg, "cannot remove running") || strings.Contains(errMsg, "running") {
					log.Infow("container still running, re-issuing stop", "name", name)
					_ = p.deps.Runtime.StopContainer(ctx, ct.ID)
				}

				if attempt == len(backoffs)-1 {
					lastErr = fmt.Errorf("failed to remove container %s after %d attempts: %w", name, len(backoffs), err)
					log.Errorw("giving up on container removal", "name", name, "error", err)
				}
				time.Sleep(backoffs[attempt])
			} else {
				log.Infow("container removed", "name", name)
				break
			}
		}

	cleanup:
		// Remove mount entries for this container
		if err := p.deps.Runtime.RemoveMountsByList(ctx, name); err != nil {
			log.Warnw("error removing mounts", "name", name, "error", err)
		}

		// ReleaseInterface deregisters the container subdomain record and removes the veth
		vn := vethName(pod, i)
		if err := p.deps.NetworkMgr.ReleaseInterface(ctx, vn); err != nil {
			log.Warnw("error releasing network", "veth", vn, "error", err)
		}

		// Remove from namespace if applicable
		if nsName := pod.Annotations[annotationNamespace]; nsName != "" && p.deps.Namespace != nil {
			p.deps.Namespace.RemoveContainerFromNamespace(nsName, name)
		}
	}

	p.recordEvent(pod, "Killing", fmt.Sprintf("Stopping pod %s/%s", pod.Namespace, pod.Name), "Normal")
	p.pods.Delete(podKey(pod))
	p.createFailures.Delete(podKey(pod))
	p.createBackoff.Delete(podKey(pod))

	// Run async consistency check to clean up any orphaned resources
	p.CheckConsistencyAsync("delete-pod/" + podKey(pod))

	return lastErr
}

// GetPod returns the tracked pod object.
func (p *MicroKubeProvider) GetPod(ctx context.Context, namespace, name string) (*corev1.Pod, error) {
	key := namespace + "/" + name
	pod, ok := p.pods.Get(key)
	if ok {
		return pod, nil
	}
	// Fall back to NATS store for pods that exist but aren't tracked
	if p.deps.Store != nil && p.deps.Store.Connected() {
		storeKey := namespace + "." + name
		var storePod corev1.Pod
		if _, err := p.deps.Store.Pods.GetJSON(ctx, storeKey, &storePod); err == nil {
			return &storePod, nil
		}
	}
	return nil, fmt.Errorf("pod %s not found", key)
}

// GetPodStatus queries RouterOS for the actual container status and maps
// it back to Kubernetes pod status.
func (p *MicroKubeProvider) GetPodStatus(ctx context.Context, namespace, name string) (*corev1.PodStatus, error) {
	pod, err := p.GetPod(ctx, namespace, name)
	if err != nil {
		return nil, err
	}

	var containerStatuses []corev1.ContainerStatus
	allRunning := true

	for _, container := range pod.Spec.Containers {
		rosName := sanitizeName(pod, container.Name)
		ct, err := p.deps.Runtime.GetContainer(ctx, rosName)

		cs := corev1.ContainerStatus{
			Name:  container.Name,
			Image: container.Image,
			Ready: false,
		}

		if err != nil {
			cs.State = corev1.ContainerState{
				Waiting: &corev1.ContainerStateWaiting{
					Reason:  "ContainerNotFound",
					Message: err.Error(),
				},
			}
			allRunning = false
		} else {
			cs.ContainerID = ct.ID
			// Populate ImageID from storage manager's cached digest
			if p.deps.StorageMgr != nil {
				if cached := p.deps.StorageMgr.GetCachedDigest(container.Image); cached != "" {
					cs.ImageID = cached
				}
			}

			switch {
			case ct.IsRunning():
				cs.Ready = p.deps.LifecycleMgr.GetUnitReady(rosName)
				cs.State = corev1.ContainerState{
					Running: &corev1.ContainerStateRunning{
						StartedAt: metav1.Now(),
					},
				}
			case ct.IsStopped():
				reason := "Stopped"
				if ct.Comment != "" {
					reason = "Stopped: " + ct.Comment
				}
				cs.State = corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{
						Reason: reason,
					},
				}
				allRunning = false
			default:
				cs.State = corev1.ContainerState{
					Waiting: &corev1.ContainerStateWaiting{
						Reason: "Unknown",
					},
				}
				allRunning = false
			}
		}

		containerStatuses = append(containerStatuses, cs)
	}

	phase := corev1.PodRunning
	if !allRunning {
		phase = corev1.PodPending
	}

	// Look up pod IP from first container's veth
	var podIP string
	if len(pod.Spec.Containers) > 0 {
		vn := vethName(pod, 0)
		if ip, _, ok := p.deps.NetworkMgr.GetPortInfo(vn); ok {
			podIP = ip
		}
	}

	status := &corev1.PodStatus{
		Phase:             phase,
		ContainerStatuses: containerStatuses,
		StartTime:         &metav1.Time{Time: p.startTime},
		HostIP:            p.deps.Config.DefaultNetwork().Gateway,
		PodIP:             podIP,
		Conditions: []corev1.PodCondition{
			{
				Type:   corev1.PodReady,
				Status: boolToConditionStatus(allRunning),
			},
			{
				Type:   corev1.PodScheduled,
				Status: corev1.ConditionTrue,
			},
		},
	}
	if podIP != "" {
		status.PodIPs = []corev1.PodIP{{IP: podIP}}
	}
	return status, nil
}

// GetPods returns all tracked pods.
func (p *MicroKubeProvider) GetPods(ctx context.Context) ([]*corev1.Pod, error) {
	return p.pods.Values(), nil
}

// ─── NodeProvider Interface ─────────────────────────────────────────────────

// ConfigureNode sets up the Kubernetes node object that represents this
// device in the cluster. Node labels vary by backend.
func (p *MicroKubeProvider) ConfigureNode(ctx context.Context, node *corev1.Node) {
	deviceType := p.deps.Runtime.Backend()
	arch := "arm64"
	cpu := resource.MustParse("4")
	mem := resource.MustParse("1Gi")
	maxPods := resource.MustParse("20")

	if deviceType == "stormbase" {
		arch = "amd64"
		cpu = resource.MustParse("16")
		mem = resource.MustParse("32Gi")
		maxPods = resource.MustParse("110")
	}

	node.Status.Capacity = corev1.ResourceList{
		corev1.ResourceCPU:    cpu,
		corev1.ResourceMemory: mem,
		corev1.ResourcePods:   maxPods,
	}
	node.Status.Allocatable = node.Status.Capacity
	node.Status.NodeInfo = corev1.NodeSystemInfo{
		Architecture:    arch,
		OperatingSystem: "linux",
		KubeletVersion:  "v1.29.0-mkube",
	}
	node.Status.Conditions = []corev1.NodeCondition{
		{
			Type:   corev1.NodeReady,
			Status: corev1.ConditionTrue,
		},
	}
	node.Labels = map[string]string{
		"type":                    "virtual-kubelet",
		"kubernetes.io/os":        "linux",
		"kubernetes.io/arch":      arch,
		"node.kubernetes.io/role": "mkube",
		"mkube.io/device-type":    deviceType,
	}

	// Add taint so normal pods aren't scheduled here
	node.Spec.Taints = []corev1.Taint{
		{
			Key:    "virtual-kubelet.io/provider",
			Value:  "mkube",
			Effect: corev1.TaintEffectNoSchedule,
		},
	}
}

// ─── Standalone Reconciler ──────────────────────────────────────────────────

// RunStandaloneReconciler runs a local reconciliation loop without requiring
// a Kubernetes API server. Reads desired state from a local YAML file and
// reconciles against actual RouterOS container state.
func (p *MicroKubeProvider) RunStandaloneReconciler(ctx context.Context) error {
	log := p.deps.Logger
	log.Info("standalone reconciler starting")

	go p.podWorker.Run(ctx)
	go p.runPVCUsageRefresher(ctx)
	go p.runResourceTrace(ctx)

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info("standalone reconciler shutting down")
			return nil
		case <-ticker.C:
			if err := p.reconcile(ctx); err != nil {
				log.Errorw("reconciliation error", "error", err)
			}
		case <-p.kickReconcile:
			if err := p.reconcile(ctx); err != nil {
				log.Errorw("reconciliation error (kick-triggered)", "error", err)
			}
		case evt, ok := <-p.pushEventsChan():
			if !ok {
				continue
			}
			log.Infow("registry push event, clearing digest cache and reconciling",
				"repo", evt.Repo, "ref", evt.Reference)
			p.deps.StorageMgr.ClearImageDigestByRepo(evt.Repo)
			p.prewarmGoldenTemplates(evt.Repo)
			if err := p.reconcile(ctx); err != nil {
				log.Errorw("reconciliation error (push-triggered)", "error", err)
			}
		case evt := <-p.pushNotify:
			log.Infow("push-notify received, clearing digest cache and reconciling",
				"repo", evt.Repo, "ref", evt.Reference)
			p.deps.StorageMgr.ClearImageDigestByRepo(evt.Repo)
			p.prewarmGoldenTemplates(evt.Repo)
			if err := p.reconcile(ctx); err != nil {
				log.Errorw("reconciliation error (push-notify)", "error", err)
			}
		}
	}
}

// runResourceTrace logs node memory, CPU and disk every 30s. RouterOS keeps
// its own logs in RAM, so after a node-level freeze the power cycle erases
// every kernel-side trace — this persisted trail is the only resource history
// a post-mortem gets, with at most a 30s gap to the moment the node died.
func (p *MicroKubeProvider) runResourceTrace(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			res, err := p.deps.Runtime.GetSystemResource(rctx)
			cancel()
			if err != nil {
				p.deps.Logger.Warnw("resource trace failed", "error", err)
				continue
			}
			total, _ := strconv.ParseUint(res.TotalMemory, 10, 64)
			free, _ := strconv.ParseUint(res.FreeMemory, 10, 64)
			var usedPct uint64
			if total > 0 {
				usedPct = (total - free) * 100 / total
			}
			p.deps.Logger.Infow("resource trace",
				"free_mem_mb", free>>20,
				"total_mem_mb", total>>20,
				"mem_used_pct", usedPct,
				"cpu_load", res.CPULoad,
				"disk_avail_mb", res.DiskAvailable>>20)
		}
	}
}

// pushEventsChan returns the PushEvents channel or a nil channel (blocks forever) if unset.
func (p *MicroKubeProvider) pushEventsChan() <-chan registry.PushEvent {
	if p.deps.PushEvents != nil {
		return p.deps.PushEvents
	}
	return nil
}

func (p *MicroKubeProvider) reconcile(ctx context.Context) error {
	log := p.deps.Logger
	reconcileStart := time.Now()

	// 0. Reconcile deployments — ensure each deployment has the correct replica pods
	p.reconcileDeployments(ctx)

	// 1. Load desired pods and configmaps — from NATS store if available, else from YAML manifest
	var desiredPods []*corev1.Pod
	var manifestCMs []*corev1.ConfigMap

	stepStart := time.Now()
	if p.deps.Store != nil && p.deps.Store.Connected() {
		desiredPods, manifestCMs = p.loadFromStore(ctx)
	}

	// Fall back to boot manifest if store is unavailable or returned nothing
	if len(desiredPods) == 0 {
		var err error
		desiredPods, manifestCMs, err = loadManifests(p.deps.Config.Lifecycle.BootManifestPath)
		if err != nil {
			return fmt.Errorf("loading manifests: %w", err)
		}
	}
	log.Debugw("RECONCILE: step 1 load manifests", "pods", len(desiredPods), "ms", time.Since(stepStart).Milliseconds())

	// 1b. Ensure boot-order pods exist in NATS so infrastructure (DNS)
	// is always in the desired state. Only adds pods that are completely
	// missing from the store — existing entries are left untouched.
	if p.deps.Store != nil && p.deps.Store.Connected() && p.deps.Config.Lifecycle.BootManifestPath != "" {
		bootPods, _, err := loadManifests(p.deps.Config.Lifecycle.BootManifestPath)
		if err == nil {
			desiredByKey := make(map[string]bool, len(desiredPods))
			for _, pod := range desiredPods {
				desiredByKey[podKey(pod)] = true
			}
			for _, bootPod := range bootPods {
				key := podKey(bootPod)
				if desiredByKey[key] {
					continue
				}
				storeKey := bootPod.Namespace + "." + bootPod.Name
				if _, err := p.deps.Store.Pods.PutJSON(ctx, storeKey, bootPod); err != nil {
					log.Warnw("failed to persist boot-order pod to store", "pod", key, "error", err)
				} else {
					log.Infow("persisted boot-order pod to store", "pod", key)
				}
				desiredPods = append(desiredPods, bootPod)
			}
		}
	}

	// Store ConfigMaps from manifest, then re-apply generated defaults
	// so that config-derived ConfigMaps (DNS, DHCP) always reflect the
	// live mkube config rather than stale copies persisted in NATS.
	for _, cm := range manifestCMs {
		p.configMaps.Set(cm.Namespace+"/"+cm.Name, cm)
	}
	for _, cm := range generateDefaultConfigMaps(p.deps.Config) {
		p.configMaps.Set(cm.Namespace+"/"+cm.Name, cm)
	}
	// Override static-config-derived DNS ConfigMaps with Network CRD
	// versions for migrated networks. Network CRDs are the source of
	// truth for DHCP reservations and DNS config once migrated.
	for _, net := range p.networks.Values() {
		hasDNS := net.Spec.DNS.Zone != "" && net.Spec.DNS.Server != ""
		if !hasDNS {
			continue
		}
		cmKey := net.Name + "/dns-config"
		if cm, ok := p.configMaps.Get(cmKey); ok {
			cm.Data["microdns.toml"] = p.generateMinimalTOML(net)
			p.configMaps.Set(cmKey, cm)
		}
	}

	// 1c. Stamp vkube.io/node on pods that lack it (one-time migration for clustering)
	var allClusterPods []*corev1.Pod // unfiltered, used for stale container cleanup
	if p.clusterMgr != nil {
		for _, pod := range desiredPods {
			if pod.Annotations == nil {
				pod.Annotations = make(map[string]string)
			}
			if pod.Annotations[annotationNode] == "" {
				pod.Annotations[annotationNode] = p.nodeName
				if p.deps.Store != nil {
					storeKey := pod.Namespace + "." + pod.Name
					if _, err := p.deps.Store.Pods.PutJSON(ctx, storeKey, pod); err != nil {
						log.Warnw("failed to stamp node annotation", "pod", podKey(pod), "error", err)
					}
				}
			}
		}
		allClusterPods = desiredPods // save before filter
	}

	// 1d. Filter to local pods only (multi-node clustering)
	if p.clusterMgr != nil {
		localPods := make([]*corev1.Pod, 0, len(desiredPods))
		for _, pod := range desiredPods {
			if p.isLocalPod(pod) {
				localPods = append(localPods, pod)
			}
		}
		log.Debugw("RECONCILE: filtered to local pods", "total", len(desiredPods), "local", len(localPods))
		desiredPods = localPods
	}

	// 2. List actual containers on RouterOS
	stepStart = time.Now()
	actual, err := p.deps.Runtime.ListContainers(ctx)
	if err != nil {
		return fmt.Errorf("listing containers: %w", err)
	}
	actualByName := make(map[string]runtime.Container, len(actual))
	for _, c := range actual {
		actualByName[c.Name] = c
	}
	log.Debugw("RECONCILE: step 2 list containers", "count", len(actual), "ms", time.Since(stepStart).Milliseconds())

	// 2c. Auto-recover stopped/faulted containers.
	// Containers that are stopped but belong to tracked pods with start-on-boot=yes
	// should be restarted. Uses exponential backoff after 3 rapid restart attempts
	// to avoid restart storms on persistent failures. If restart fails (e.g. veth gone),
	// remove the container from actualByName so step 3 recreates the entire pod.
	const (
		recoveryBackoffThreshold = 3
		recoveryInitialBackoff   = 30 * time.Second
		recoveryMaxBackoff       = 5 * time.Minute
		recoveryStableWindow     = 2 * time.Minute
	)
	for name, ct := range actualByName {
		if ct.IsRunning() {
			// Track running state for backoff reset
			if rs, ok := p.restartBackoff.Get(name); ok && rs.attempts > 0 {
				rs.lastRunning = time.Now()
				if time.Since(rs.lastAttempt) > recoveryStableWindow {
					log.Infow("RECOVERY: container stable, resetting backoff", "container", name)
					rs.attempts = 0
					rs.backoff = 0
				}
				p.restartBackoff.Set(name, rs)
			}
			continue
		}
		// Find the tracked pod owning this container
		var ownerPod *corev1.Pod
		for _, pod := range desiredPods {
			for _, c := range pod.Spec.Containers {
				if sanitizeName(pod, c.Name) == name {
					ownerPod = pod
					break
				}
			}
			if ownerPod != nil {
				break
			}
		}
		if ownerPod == nil {
			continue // orphan, handled elsewhere
		}

		// Whether a stopped container should be recovered is a desired-state
		// question. RouterOS's start-on-boot flag cannot answer it: CoW
		// containers deliberately carry start-on-boot=false (their rootfs is a
		// network clone mkube attaches), and the lifecycle registry is
		// in-memory — empty after an mkube restart, which left adopted CoW
		// containers stopped until the next full recreate. The pod's restart
		// policy survives restarts; the flag and the registry remain only for
		// pods without one.
		if ownerPod.Spec.RestartPolicy != corev1.RestartPolicyAlways &&
			ct.StartOnBoot != "true" && !p.deps.LifecycleMgr.IsRegistered(name) {
			continue
		}

		// Check restart backoff
		rs, _ := p.restartBackoff.Get(name)
		if rs == nil {
			rs = &containerRestartState{}
			p.restartBackoff.Set(name, rs)
		}
		if rs.backoff > 0 && time.Since(rs.lastAttempt) < rs.backoff {
			log.Debugw("RECOVERY: container stopped but in backoff, skipping",
				"container", name, "backoff", rs.backoff,
				"remaining", rs.backoff-time.Since(rs.lastAttempt))
			continue
		}

		comment := ct.Comment
		if comment == "" {
			comment = "no error detail"
		}

		rs.attempts++
		rs.lastAttempt = time.Now()

		// Calculate backoff for next attempt if past threshold
		if rs.attempts > recoveryBackoffThreshold {
			if rs.backoff == 0 {
				rs.backoff = recoveryInitialBackoff
			} else {
				rs.backoff *= 2
				if rs.backoff > recoveryMaxBackoff {
					rs.backoff = recoveryMaxBackoff
				}
			}
			log.Warnw("RECOVERY: stopped container detected (backoff active)",
				"container", name, "comment", comment, "id", ct.ID,
				"attempt", rs.attempts, "nextBackoff", rs.backoff)
		} else {
			log.Warnw("RECOVERY: stopped container detected",
				"container", name, "comment", comment, "id", ct.ID,
				"attempt", rs.attempts)
		}

		p.recordEvent(ownerPod, "ContainerStopped",
			fmt.Sprintf("Container %s stopped (attempt %d): %s", name, rs.attempts, comment), "Warning")

		// Attempt restart first — cheapest fix
		if err := p.deps.Runtime.StartContainer(ctx, ct.ID); err == nil {
			// Verify it actually came up
			time.Sleep(2 * time.Second)
			if updated, err := p.deps.Runtime.GetContainer(ctx, name); err == nil && updated.IsRunning() {
				log.Infow("RECOVERY: container restarted successfully",
					"container", name, "attempt", rs.attempts)
				p.recordEvent(ownerPod, "Restarted",
					fmt.Sprintf("Container %s restarted after fault (attempt %d): %s", name, rs.attempts, comment), "Normal")
				globalStats.RecordRestart(true, name, comment)
				actualByName[name] = *updated
				continue
			}
		}

		// Restart failed. Before anything is destroyed, prove a runnable
		// replacement exists: destroy-then-fail-to-pull is how a registry
		// serving broken images became eight deleted DNS primaries (#26).
		// EnsureImage stages the tarball AND verifies the entrypoint binary
		// is in the rootfs; if it cannot, the stopped container is kept —
		// stopped-but-recreatable beats gone. CoW pods stage nothing here
		// (their golden template is the runnable artifact, checked on the
		// create path), so only tarball-backed pods gate on this.
		if !isCoWPod(ownerPod) {
			img := ""
			for _, c := range ownerPod.Spec.Containers {
				if sanitizeName(ownerPod, c.Name) == name {
					img = c.Image
					break
				}
			}
			if img != "" {
				if _, imgErr := p.deps.StorageMgr.EnsureImage(ctx, img); imgErr != nil {
					log.Errorw("RECOVERY: no runnable replacement image — keeping stopped container",
						"container", name, "image", img, "error", imgErr)
					p.recordEvent(ownerPod, "RecoveryBlocked",
						fmt.Sprintf("Container %s left stopped: replacement image %s is not runnable: %v", name, img, imgErr), "Warning")
					continue
				}
			}
		}

		// Destroy container and veths so step 3 recreates.
		// NOTE: Do NOT RemoveMountsByList here — PVC mounts must survive.
		// ReconcileMounts during recreation (step 3) handles stale cleanup.
		log.Warnw("RECOVERY: restart failed, destroying for full recreation",
			"container", name, "comment", comment, "attempt", rs.attempts)
		globalStats.RecordRestart(false, name, comment)
		removed := p.stopAndRemoveContainer(ctx, name, ct.ID)

		// Release this container's veth + staging veth so IPAM is freed before recreation.
		// Find the container index to derive the correct veth name.
		for j, c := range ownerPod.Spec.Containers {
			if sanitizeName(ownerPod, c.Name) == name {
				prodVeth := vethName(ownerPod, j)
				if !removed {
					// Container still alive — force-release the veth
					log.Warnw("RECOVERY: container not removed, force-releasing veth",
						"container", name, "veth", prodVeth)
					p.forceReleaseVeth(ctx, prodVeth)
				} else if err := p.deps.NetworkMgr.ReleaseInterface(ctx, prodVeth); err != nil {
					log.Warnw("RECOVERY: error releasing production veth, force-releasing",
						"veth", prodVeth, "error", err)
					p.forceReleaseVeth(ctx, prodVeth)
				}
				// Clean leftover staging resources from failed blue-green updates
				stgVeth := truncate(prodVeth, 58) + "__stg"
				stgName := truncate(name, 58) + "__stg"
				p.cleanupStagingResources(ctx, stgName, stgVeth)

				// Clean root-dir to prevent RouterOS "root-dir overlap" on recreation
				rootDir := fmt.Sprintf("%s/%s", p.deps.Config.Storage.BasePath, name)
				if err := p.deps.Runtime.RemoveDirectory(ctx, rootDir); err != nil {
					log.Debugw("RECOVERY: root-dir cleanup", "rootDir", rootDir, "error", err)
				}
				break
			}
		}

		delete(actualByName, name)
		// Untrack the pod so step 3 sees it as missing
		p.pods.Delete(podKey(ownerPod))
		globalStats.RecordRecreate(name)
		p.recordEvent(ownerPod, "RecoveryRecreate",
			fmt.Sprintf("Container %s destroyed for recreation after persistent fault: %s", name, comment), "Warning")
	}

	// 2b. Clean up stale containers from pods migrated to other nodes.
	// If a pod was migrated away (vkube.io/node != local), but its containers
	// still exist here (e.g. crash during migration), clean them up.
	if p.clusterMgr != nil && len(allClusterPods) > 0 {
		for _, pod := range allClusterPods {
			if p.isLocalPod(pod) {
				continue
			}
			for _, c := range pod.Spec.Containers {
				cName := sanitizeName(pod, c.Name)
				if ct, exists := actualByName[cName]; exists {
					log.Infow("cleaning up migrated-away container",
						"pod", podKey(pod), "container", cName,
						"assignedTo", pod.Annotations[annotationNode])
					p.stopAndRemoveContainer(ctx, cName, ct.ID)
					delete(actualByName, cName)
				}
			}
			// Release veths for migrated-away pods
			for i := range pod.Spec.Containers {
				veth := vethName(pod, i)
				_ = p.deps.NetworkMgr.ReleaseInterface(ctx, veth)
			}
			// Untrack if still in memory
			p.pods.Delete(podKey(pod))
		}
	}

	// 3. Create missing containers and collect stale-image pods
	type staleEntry struct {
		key string
		pod *corev1.Pod
	}
	bootStale := make(map[string][]staleEntry)
	bootCheckedImages := make(map[string]bool) // image ref → changed?

	stepStart = time.Now()
	for _, pod := range desiredPods {
		key := podKey(pod)
		tracked := p.pods.Has(key)
		isRedeploying, _ := p.redeploying.Get(key)
		priorFailures, _ := p.createFailures.Get(key)
		if tracked {
			continue
		}
		// Skip pods currently being redeployed by the redeploy API
		if isRedeploying {
			log.Debugw("skipping pod during redeploy", "pod", key)
			continue
		}
		// Skip pods already queued in the pod worker
		if p.podWorker.IsPendingOrProcessing(key) {
			log.Debugw("skipping pod already in worker queue", "pod", key)
			continue
		}

		// Check if all containers for this pod exist on RouterOS
		allExist := true
		for _, c := range pod.Spec.Containers {
			name := sanitizeName(pod, c.Name)
			if _, exists := actualByName[name]; !exists {
				allExist = false
				break
			}
		}

		if !allExist {
			// A create loop that never stops is net-destructive: each cycle
			// adds and removes veths and bridge ports, and three days of it
			// drained rose1's bridge-port table (761 adds / 779 removes, #26).
			// Backoff alone only paces the loop — this caps it. Past the cap
			// the pod stays CreateFailed and untouched until something
			// deliberate (spec update, delete, redeploy, mkube restart)
			// clears its failure count.
			if priorFailures >= createHardFailureCap {
				if priorFailures == createHardFailureCap {
					log.Errorw("pod exceeded creation failure cap — retries stopped until spec change or restart",
						"pod", key, "failures", priorFailures, "cap", createHardFailureCap)
					p.recordEvent(pod, "CreateAbandoned",
						fmt.Sprintf("Pod failed creation %d times; automatic retries stopped. Update, delete or redeploy the pod to retry.", priorFailures), "Warning")
					p.createFailures.Set(key, priorFailures+1) // log/event once, not every cycle
				}
				continue
			}

			// Check creation backoff — skip if we're still in cooldown from prior failures
			cbs, _ := p.createBackoff.Get(key)
			if cbs != nil && cbs.backoff > 0 && time.Since(cbs.lastAttempt) < cbs.backoff {
				log.Debugw("pod creation in backoff, skipping",
					"pod", key, "backoff", cbs.backoff,
					"remaining", cbs.backoff-time.Since(cbs.lastAttempt),
					"attempts", cbs.attempts)
				continue
			}

			// If this pod has failed creation before, force-release stale
			// IPAM + veth state that may be blocking it.
			if priorFailures >= 2 {
				log.Warnw("SELF-HEAL: pod stuck in CreateFailed, releasing stale network state",
					"pod", key, "failures", priorFailures)
				for i := range pod.Spec.Containers {
					vn := vethName(pod, i)
					_ = p.deps.NetworkMgr.ReleaseInterface(ctx, vn)
				}
			}
			// Hand to the pod worker and move on. Enqueue does not queue: it
			// runs the create in its own goroutine (per-key in-flight guard
			// drops duplicates), so reconcile never blocks on a create and
			// slow steps inside one — a clone attach waiting for RouterOS to
			// mount, say — hold up only that pod. Concurrent pods overlap.
			podCopy := pod.DeepCopy()
			capturedKey := key
			p.podWorker.Enqueue(key, "missing containers", func(ctx context.Context) error {
				err := p.CreatePod(ctx, podCopy)
				p.updateCreateResult(capturedKey, podCopy, err)
				return err
			})
		} else {
			// Track already-existing pods
			p.pods.Set(key, pod.DeepCopy())
			p.recordEvent(pod, "Reconciled", fmt.Sprintf("Existing pod %s/%s tracked on node %s", pod.Namespace, pod.Name, p.nodeName), "Normal")

			// For pods with image-policy=auto, check if the registry has a
			// newer image than what's currently running. Deferred to after
			// all pods are tracked — restarted one-at-a-time per image group.
			// Use bootCheckedImages to call RefreshImage once per unique image,
			// then mark ALL pods with that image as stale (same fix as step 3c).
			// Pass the stored image-digest annotation as hint so first-check-after-
			// restart can detect stale images (session memory is empty on boot).
			if pod.Annotations[annotationImagePolicy] == "auto" && pod.Annotations[annotationFile] == "" {
				deployedDigest := pod.Annotations[annotationImageDigest]
				for _, c := range pod.Spec.Containers {
					changed, alreadyChecked := bootCheckedImages[c.Image]
					if !alreadyChecked {
						_, freshChanged, err := p.deps.StorageMgr.RefreshImageWithHint(ctx, c.Image, deployedDigest)
						if err != nil {
							log.Warnw("failed to check image freshness", "pod", key, "image", c.Image, "error", err)
							bootCheckedImages[c.Image] = false
							changed = false
						} else {
							bootCheckedImages[c.Image] = freshChanged
							changed = freshChanged
						}
					}
					if changed {
						bootStale[c.Image] = append(bootStale[c.Image], staleEntry{key: key, pod: pod.DeepCopy()})
						break
					}
				}
			}
		}
	}

	// 3a-stale. Process boot-time stale images one-at-a-time per image group.
	for image, entries := range bootStale {
		for i, entry := range entries {
			log.Infow("boot stale image update: restarting pod",
				"pod", entry.key, "image", image,
				"index", i+1, "total", len(entries))
			if err := p.UpdatePod(ctx, entry.pod); err != nil {
				log.Errorw("failed to update pod for stale image", "pod", entry.key, "error", err)
				continue
			}
			if i < len(entries)-1 {
				if !p.waitForPodLiveness(ctx, entry.pod, 60*time.Second) {
					log.Errorw("pod failed liveness after boot update, halting image rollout",
						"pod", entry.key, "image", image)
					break
				}
			}
		}
	}

	// 3b. Check tracked pods for missing containers (orphan detection).
	// If a container was manually removed or orphaned, untrack and recreate.
	// Skip pods currently being redeployed to avoid racing with the redeploy goroutine.
	podsSnap3b := p.pods.Snapshot()
	redeploySnap3b := p.redeploying.Snapshot()
	for key, pod := range podsSnap3b {
		if redeploySnap3b[key] {
			continue
		}
		for _, c := range pod.Spec.Containers {
			name := sanitizeName(pod, c.Name)
			if _, exists := actualByName[name]; !exists {
				log.Warnw("tracked pod has missing container, enqueuing recreation",
					"pod", key, "container", name)
				p.pods.Delete(key)
				podCopy := pod.DeepCopy()
				capturedKey := key
				p.podWorker.Enqueue(key, "missing container (orphan)", func(ctx context.Context) error {
					err := p.CreatePod(ctx, podCopy)
					p.updateCreateResult(capturedKey, podCopy, err)
					return err
				})
				break
			}
		}
	}

	// 3c. Check tracked auto-update pods for stale images.
	// When a new image is pushed to the local registry, the registry
	// emits a PushEvent that triggers this reconcile. We must check
	// every running pod with image-policy=auto against the current
	// registry digest to detect updates from podman push.
	// IMPORTANT: Pods sharing the same image are restarted one at a time
	// with liveness verification between each, to prevent simultaneous
	// outages (e.g., all DNS pods going down at once).
	//
	// Call RefreshImage ONCE per unique image to avoid a bug where the
	// first call updates the cache, causing subsequent pods with the same
	// image to see the new digest and miss the change.
	imageToStale := make(map[string][]staleEntry)
	checkedImages := make(map[string]bool) // image ref → changed?
	podsSnap3c := p.pods.Snapshot()
	redeploySnap3c := p.redeploying.Snapshot()
	for key, pod := range podsSnap3c {
		if redeploySnap3c[key] {
			continue
		}
		if pod.Annotations[annotationImagePolicy] != "auto" || pod.Annotations[annotationFile] != "" {
			continue
		}
		for _, c := range pod.Spec.Containers {
			changed, alreadyChecked := checkedImages[c.Image]
			if !alreadyChecked {
				deployedDigest := pod.Annotations[annotationImageDigest]
				_, freshChanged, err := p.deps.StorageMgr.RefreshImageWithHint(ctx, c.Image, deployedDigest)
				if err != nil {
					log.Warnw("image freshness check failed", "pod", key, "image", c.Image, "error", err)
					checkedImages[c.Image] = false
					changed = false
				} else {
					checkedImages[c.Image] = freshChanged
					changed = freshChanged
				}
			}
			if changed {
				imageToStale[c.Image] = append(imageToStale[c.Image], staleEntry{key: key, pod: pod.DeepCopy()})
				break
			}
		}
	}
	for image, entries := range imageToStale {
		for i, entry := range entries {
			log.Infow("staggered image update: restarting pod",
				"pod", entry.key, "image", image,
				"index", i+1, "total", len(entries))
			if err := p.UpdatePod(ctx, entry.pod); err != nil {
				log.Errorw("failed to update pod for new image", "pod", entry.key, "error", err)
				continue
			}
			// Wait for liveness before restarting the next pod with same image
			if i < len(entries)-1 {
				if !p.waitForPodLiveness(ctx, entry.pod, 60*time.Second) {
					log.Errorw("pod failed liveness after update, halting image rollout",
						"pod", entry.key, "image", image)
					break
				}
			}
		}
	}

	log.Debugw("RECONCILE: step 3 create/track pods", "ms", time.Since(stepStart).Milliseconds())

	// 4. Re-sync IPAM allocations from actual veths on the device.
	// Pods tracked via the "already exists" path above don't call
	// AllocateInterface, so their veths may not be in IPAM yet.
	// This ensures GetPodStatus can always return pod IPs.
	stepStart = time.Now()
	if err := p.deps.NetworkMgr.ResyncAllocations(ctx); err != nil {
		log.Warnw("failed to re-sync IPAM allocations", "error", err)
	}
	log.Debugw("RECONCILE: step 4 IPAM resync", "ms", time.Since(stepStart).Milliseconds())

	// 4b. Validate static IP pods have the correct veth IP.
	// The "already exists" path above tracks pods but never calls
	// AllocateInterface, so a stale veth with a wrong IP is silently
	// accepted. Fix that now by checking static-IP annotations against
	// actual veth state.
	podsSnap4b := p.pods.Snapshot()
	redeploySnap4b := p.redeploying.Snapshot()
	for key, pod := range podsSnap4b {
		staticIP := pod.Annotations[annotationStaticIP]
		if staticIP == "" {
			continue
		}
		if redeploySnap4b[key] {
			continue
		}
		for i := range pod.Spec.Containers {
			veth := vethName(pod, i)
			ip, _, ok := p.deps.NetworkMgr.GetPortInfo(veth)
			if !ok {
				continue
			}
			if ip != staticIP {
				log.Warnw("static IP mismatch on tracked pod, enqueuing recreation",
					"pod", key, "expected", staticIP, "actual", ip, "veth", veth)
				p.pods.Delete(key)
				podCopy := pod.DeepCopy()
				capturedKey := key
				p.podWorker.Enqueue(key, "static IP mismatch", func(ctx context.Context) error {
					if err := p.DeletePod(ctx, podCopy); err != nil {
						log.Errorw("worker: delete for IP repair failed", "pod", capturedKey, "error", err)
					}
					err := p.CreatePod(ctx, podCopy)
					p.updateCreateResult(capturedKey, podCopy, err)
					return err
				})
				break // move to next pod
			}
		}
	}

	// 4c. Stamp assigned IPs for pods that don't have static-ip yet.
	// This covers pods that existed before the IP reservation feature.
	podsSnap4c := p.pods.Snapshot()
	for key, pod := range podsSnap4c {
		if pod.Annotations[annotationStaticIP] != "" {
			continue
		}
		vn := vethName(pod, 0)
		if ip, _, ok := p.deps.NetworkMgr.GetPortInfo(vn); ok {
			if pod.Annotations == nil {
				pod.Annotations = make(map[string]string)
			}
			pod.Annotations[annotationStaticIP] = ip
			p.pods.Set(key, pod.DeepCopy())
			if p.deps.Store != nil {
				storeKey := pod.Namespace + "." + pod.Name
				p.deps.Store.Pods.PutJSON(ctx, storeKey, pod)
			}
			log.Infow("stamped existing pod with IP reservation", "pod", key, "ip", ip)
		}
	}

	// 5. Sync ConfigMap data to disk and recreate pods whose ConfigMaps changed
	stepStart = time.Now()
	p.syncConfigMapsToDisk(ctx)
	log.Debugw("RECONCILE: step 5 sync configmaps", "ms", time.Since(stepStart).Milliseconds())

	// 5b. Sync Secret data to disk and recreate pods whose Secrets changed
	stepStart = time.Now()
	p.syncSecretsToDisk(ctx)
	log.Debugw("RECONCILE: step 5b sync secrets", "ms", time.Since(stepStart).Milliseconds())

	// 6. Ensure DNS zones exist and records are seeded from config
	stepStart = time.Now()
	p.deps.NetworkMgr.InitDNSZones(ctx)
	log.Debugw("RECONCILE: step 6 init DNS zones", "ms", time.Since(stepStart).Milliseconds())

	// 6a2. Ensure managed networks have DNS pods (auto-recreate if deleted)
	p.reconcileManagedDNSPods(ctx)

	// 6b. Reconcile DHCP pools/reservations/forwarders via microdns REST API
	stepStart = time.Now()
	p.reconcileDNSConfig(ctx)
	log.Debugw("RECONCILE: step 6b reconcile DNS config", "ms", time.Since(stepStart).Milliseconds())

	// 7. Re-register DNS aliases for all tracked pods so they survive DNS container restarts
	stepStart = time.Now()
	p.reregisterPodDNS(ctx)
	log.Debugw("RECONCILE: step 7 reregister DNS", "ms", time.Since(stepStart).Milliseconds())

	// 8. Async consistency check for orphaned veths/IPAM
	p.CheckConsistencyAsync("reconcile")

	// 9. Infrastructure health checks (registry, mkube-update)
	p.checkInfraHealth(ctx)

	// 10. Refresh storage pool capacity + disk file stats + reconcile iSCSI targets
	stepStart = time.Now()
	p.DiscoverStoragePools(ctx)
	p.refreshISCSIDiskFileStats(ctx)
	p.ReconcileISCSIDiskTargets(ctx)
	log.Debugw("RECONCILE: step 10 storage refresh", "ms", time.Since(stepStart).Milliseconds())

	log.Debugw("RECONCILE: complete", "total_ms", time.Since(reconcileStart).Milliseconds(), "tracked_pods", p.pods.Len())
	return nil
}

// syncConfigMapsToDisk writes all ConfigMap-backed volume data to disk for
// every tracked pod. It compares ConfigMap content against what is on disk
// and triggers a rolling update (delete + create) for pods whose ConfigMap
// files are stale or missing.
func (p *MicroKubeProvider) syncConfigMapsToDisk(ctx context.Context) {
	log := p.deps.Logger

	// Track which pods need recreation due to ConfigMap changes
	podsToRecreate := make(map[string]*corev1.Pod)

	// Snapshot pods (called from reconciler goroutine)
	podsSnapCM := p.pods.Values()

	for _, pod := range podsSnapCM {
		for _, container := range pod.Spec.Containers {
			name := sanitizeName(pod, container.Name)
			for _, vm := range container.VolumeMounts {
				data := p.resolveConfigMapVolume(pod, vm.Name)
				if data == nil {
					continue
				}

				localDir := fmt.Sprintf("/data/configmaps/%s/%s", name, vm.Name)
				if mkErr := os.MkdirAll(localDir, 0o755); mkErr != nil {
					log.Warnw("failed to create configmap dir", "path", localDir, "error", mkErr)
					continue
				}

				// Compare each file with what's on disk
				for filename, content := range data {
					filePath := localDir + "/" + filename
					existing, readErr := os.ReadFile(filePath)
					if readErr != nil || string(existing) != content {
						if err := os.WriteFile(filePath, []byte(content), 0o644); err != nil {
							log.Warnw("failed to write configmap file", "path", filePath, "error", err)
							continue
						}
						key := podKey(pod)
						if _, already := podsToRecreate[key]; !already {
							log.Infow("ConfigMap file updated on disk",
								"pod", key,
								"file", filePath,
								"new", readErr != nil,
							)
							podsToRecreate[key] = pod
						}
					}
				}
			}
		}
	}

	// Trigger rolling updates for pods whose ConfigMap files changed,
	// but only if all container images are available in the registry.
	for key, pod := range podsToRecreate {
		// Pre-flight: verify images are pullable before destroying the container
		imagesMissing := false
		if pod.Annotations[annotationFile] == "" {
			for _, c := range pod.Spec.Containers {
				if _, err := p.deps.StorageMgr.CheckImageAvailable(ctx, c.Image); err != nil {
					log.Warnw("skipping ConfigMap recreation: image not available",
						"pod", key, "image", c.Image, "error", err)
					imagesMissing = true
					break
				}
			}
		}
		if imagesMissing {
			continue // will retry on next reconcile cycle
		}

		log.Infow("recreating pod for ConfigMap update", "pod", key)
		if err := p.UpdatePod(ctx, pod); err != nil {
			log.Errorw("failed to recreate pod after ConfigMap change", "pod", key, "error", err)
		}
	}
}

// loadFromStore reads desired pods and configmaps from the NATS KV store.
func (p *MicroKubeProvider) loadFromStore(ctx context.Context) ([]*corev1.Pod, []*corev1.ConfigMap) {
	var pods []*corev1.Pod
	var cms []*corev1.ConfigMap

	podKeys, err := p.deps.Store.Pods.Keys(ctx, "")
	if err != nil {
		p.deps.Logger.Warnw("failed to list pods from store", "error", err)
		return nil, nil
	}
	seen := make(map[string]bool) // track ns.name to dedup
	for _, key := range podKeys {
		var pod corev1.Pod
		if _, err := p.deps.Store.Pods.GetJSON(ctx, key, &pod); err != nil {
			p.deps.Logger.Warnw("failed to read pod from store", "key", key, "error", err)
			continue
		}
		// Migration: clean up stale "/" key entries (stampImageDigest bug).
		// Canonical key format is "ns.name". Delete "/" variants and re-save as ".".
		if strings.Contains(key, "/") {
			canonicalKey := pod.Namespace + "." + pod.Name
			_ = p.deps.Store.Pods.Delete(ctx, key)
			if _, err := p.deps.Store.Pods.PutJSON(ctx, canonicalKey, &pod); err != nil {
				p.deps.Logger.Warnw("failed to migrate pod key", "old", key, "new", canonicalKey, "error", err)
			} else {
				p.deps.Logger.Infow("migrated pod store key", "old", key, "new", canonicalKey)
			}
		}
		// Deduplicate — same pod may exist under both key formats
		dedupKey := pod.Namespace + "." + pod.Name
		if seen[dedupKey] {
			continue
		}
		seen[dedupKey] = true
		// Migration: fix DNS pods with orphaned volumeMounts (mount exists but no volume definition).
		// This was caused by boot-order.yaml missing PVC volume definitions for DNS data volumes.
		if p.fixOrphanedVolumeMounts(&pod, ctx) {
			storeKey := pod.Namespace + "." + pod.Name
			if _, err := p.deps.Store.Pods.PutJSON(ctx, storeKey, &pod); err != nil {
				p.deps.Logger.Warnw("failed to persist pod volume fix", "key", storeKey, "error", err)
			}
		}
		pods = append(pods, &pod)
	}

	cmKeys, err := p.deps.Store.ConfigMaps.Keys(ctx, "")
	if err != nil {
		p.deps.Logger.Warnw("failed to list configmaps from store", "error", err)
		return pods, nil
	}
	for _, key := range cmKeys {
		var cm corev1.ConfigMap
		if _, err := p.deps.Store.ConfigMaps.GetJSON(ctx, key, &cm); err != nil {
			p.deps.Logger.Warnw("failed to read configmap from store", "key", key, "error", err)
			continue
		}
		cms = append(cms, &cm)
	}

	return pods, cms
}

// loadManifests reads a multi-document YAML file containing Pod and ConfigMap specs.
func loadManifests(path string) ([]*corev1.Pod, []*corev1.ConfigMap, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("reading manifest %s: %w", path, err)
	}

	var pods []*corev1.Pod
	var configMaps []*corev1.ConfigMap
	reader := yaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	for {
		doc, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("reading YAML document: %w", err)
		}

		doc = bytes.TrimSpace(doc)
		if len(doc) == 0 {
			continue
		}

		// Peek at document kind to route decoding
		var meta metav1.TypeMeta
		if err := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(doc), 4096).Decode(&meta); err != nil {
			continue
		}

		switch meta.Kind {
		case "ConfigMap":
			var cm corev1.ConfigMap
			if err := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(doc), 4096).Decode(&cm); err != nil {
				return nil, nil, fmt.Errorf("decoding configmap: %w", err)
			}
			if cm.Name == "" {
				continue
			}
			if cm.Namespace == "" {
				cm.Namespace = "default"
			}
			configMaps = append(configMaps, &cm)
		default:
			var pod corev1.Pod
			if err := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(doc), 4096).Decode(&pod); err != nil {
				return nil, nil, fmt.Errorf("decoding pod: %w", err)
			}
			if pod.Kind != "" && pod.Kind != "Pod" {
				continue
			}
			if pod.Name == "" {
				continue
			}
			if pod.Namespace == "" {
				pod.Namespace = "default"
			}
			pods = append(pods, &pod)
		}
	}

	return pods, configMaps, nil
}

// NotifyPods is called by the Virtual Kubelet framework to set up a callback
// for pod status updates. The provider calls this function whenever a pod's
// status changes so the framework can update the API server.
func (p *MicroKubeProvider) NotifyPods(ctx context.Context, cb func(*corev1.Pod)) {
	p.notifyPodStatus = cb
	// Background goroutine pushes full status updates as a fallback.
	// Primary status updates come from notifyPodChange called by CRUD handlers
	// and lifecycle callbacks, so this can run at a slower interval.
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// Snapshot pods (background goroutine)
				notifySnap := p.pods.Values()
				for _, pod := range notifySnap {
					if cb != nil {
						status, err := p.GetPodStatus(ctx, pod.Namespace, pod.Name)
						if err == nil {
							updated := pod.DeepCopy()
							updated.Status = *status
							cb(updated)
						}
					}
				}
			}
		}
	}()
}

// notifyPodChange pushes a single pod's status to the VK framework immediately.
func (p *MicroKubeProvider) notifyPodChange(ctx context.Context, pod *corev1.Pod) {
	if p.notifyPodStatus == nil {
		return
	}
	status, err := p.GetPodStatus(ctx, pod.Namespace, pod.Name)
	if err != nil {
		return
	}
	updated := pod.DeepCopy()
	updated.Status = *status
	p.notifyPodStatus(updated)
}

// stampImageDigest fetches the current registry digest for each container
// image and stores it in a pod annotation. This annotation survives restart
// (via NATS persistence) and allows boot-time image freshness checks to
// compare against the actual deployed digest rather than empty session memory.
func (p *MicroKubeProvider) stampImageDigest(ctx context.Context, pod *corev1.Pod) {
	if pod.Annotations == nil {
		pod.Annotations = make(map[string]string)
	}
	for _, c := range pod.Spec.Containers {
		if c.Image == "" || pod.Annotations[annotationFile] != "" {
			continue
		}
		digest, err := p.deps.StorageMgr.GetCurrentDigest(ctx, c.Image)
		if err != nil {
			p.deps.Logger.Warnw("failed to get digest for annotation stamp",
				"pod", podKey(pod), "image", c.Image, "error", err)
			continue
		}
		pod.Annotations[annotationImageDigest] = digest
		break // one digest per pod (all containers usually share the same image)
	}
	// Persist to NATS so the annotation survives restart
	if p.deps.Store != nil {
		storeKey := pod.Namespace + "." + pod.Name
		if _, err := p.deps.Store.Pods.PutJSON(context.Background(), storeKey, pod); err != nil {
			p.deps.Logger.Warnw("failed to persist digest annotation", "pod", storeKey, "error", err)
		}
	}
}

// stampAssignedIP writes the first container's allocated IP back to
// the pod's vkube.io/static-ip annotation and persists to NATS.
// This makes dynamic IP assignments "sticky" — on pod recreation,
// CreatePod reads the annotation and reuses the same IP.
func (p *MicroKubeProvider) stampAssignedIP(ctx context.Context, pod *corev1.Pod, containerIPs map[string]string) {
	if pod.Annotations[annotationStaticIP] != "" {
		return // already has a static IP, don't overwrite
	}
	if len(pod.Spec.Containers) == 0 || len(containerIPs) == 0 {
		return
	}
	ip := containerIPs[pod.Spec.Containers[0].Name]
	if ip == "" {
		return
	}
	if pod.Annotations == nil {
		pod.Annotations = make(map[string]string)
	}
	pod.Annotations[annotationStaticIP] = ip
	p.deps.Logger.Infow("stamped assigned IP as static reservation",
		"pod", podKey(pod), "ip", ip)
	// Persist to NATS so the annotation survives restart
	if p.deps.Store != nil {
		storeKey := pod.Namespace + "." + pod.Name
		if _, err := p.deps.Store.Pods.PutJSON(ctx, storeKey, pod); err != nil {
			p.deps.Logger.Debugw("failed to persist IP reservation", "pod", storeKey, "error", err)
		}
	}
}

// RunVirtualKubelet starts the full Virtual Kubelet node, registering
// with a Kubernetes API server. It loads kubeconfig, creates a Kubernetes
// clientset, and runs a node controller that watches for pods scheduled
// to this virtual node.
func (p *MicroKubeProvider) RunVirtualKubelet(ctx context.Context) error {
	log := p.deps.Logger
	cfg := p.deps.Config

	log.Infow("starting Virtual Kubelet node",
		"node", cfg.NodeName,
		"kubeconfig", cfg.KubeConfig,
	)

	// Build Kubernetes client config
	var restConfig *restclient.Config
	var err error

	if cfg.KubeConfig != "" {
		restConfig, err = clientcmd.BuildConfigFromFlags("", cfg.KubeConfig)
	} else {
		restConfig, err = restclient.InClusterConfig()
	}
	if err != nil {
		return fmt.Errorf("building kubernetes config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("creating kubernetes clientset: %w", err)
	}

	// Create the virtual node object
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: cfg.NodeName,
		},
	}
	p.ConfigureNode(ctx, node)

	// Register or update the node in the API server
	existingNode, err := clientset.CoreV1().Nodes().Get(ctx, cfg.NodeName, metav1.GetOptions{})
	if err != nil {
		log.Infow("registering new node", "name", cfg.NodeName)
		if _, err := clientset.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("registering node: %w", err)
		}
	} else {
		existingNode.Status = node.Status
		existingNode.Labels = node.Labels
		existingNode.Spec.Taints = node.Spec.Taints
		if _, err := clientset.CoreV1().Nodes().UpdateStatus(ctx, existingNode, metav1.UpdateOptions{}); err != nil {
			log.Warnw("failed to update node status", "error", err)
		}
	}

	log.Infow("node registered", "name", cfg.NodeName)

	// Start node lease / heartbeat updater
	go p.runNodeHeartbeat(ctx, clientset, cfg.NodeName)

	// Watch for pods assigned to this node
	return p.watchPods(ctx, clientset, cfg.NodeName)
}

// runNodeHeartbeat periodically updates the node status so the API server
// knows the node is still alive.
func (p *MicroKubeProvider) runNodeHeartbeat(ctx context.Context, clientset kubernetes.Interface, nodeName string) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			node, err := clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
			if err != nil {
				p.deps.Logger.Warnw("heartbeat: failed to get node", "error", err)
				continue
			}
			// Update the Ready condition timestamp
			for i, cond := range node.Status.Conditions {
				if cond.Type == corev1.NodeReady {
					node.Status.Conditions[i].LastHeartbeatTime = metav1.Now()
				}
			}

			// Check if node is cordoned (stormbase backend only)
			if sb, ok := p.deps.Runtime.(*stormbase.Client); ok {
				cordoned, reason := sb.IsNodeCordoned(ctx)
				node.Spec.Unschedulable = cordoned

				// Add/remove NoSchedule taint for cordoned nodes
				cordonTaint := corev1.Taint{
					Key:    "stormbase.io/cordoned",
					Value:  reason,
					Effect: corev1.TaintEffectNoSchedule,
				}
				if cordoned {
					hasTaint := false
					for _, t := range node.Spec.Taints {
						if t.Key == "stormbase.io/cordoned" {
							hasTaint = true
							break
						}
					}
					if !hasTaint {
						node.Spec.Taints = append(node.Spec.Taints, cordonTaint)
						p.deps.Logger.Infow("node cordoned — added taint", "reason", reason)
					}
				} else {
					filtered := make([]corev1.Taint, 0, len(node.Spec.Taints))
					for _, t := range node.Spec.Taints {
						if t.Key != "stormbase.io/cordoned" {
							filtered = append(filtered, t)
						}
					}
					node.Spec.Taints = filtered
				}
			}

			if _, err := clientset.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{}); err != nil {
				p.deps.Logger.Warnw("heartbeat: failed to update", "error", err)
			}
		}
	}
}

// watchPods uses the Kubernetes API to watch for pod events targeting this node
// and dispatches create/update/delete operations.
func (p *MicroKubeProvider) watchPods(ctx context.Context, clientset kubernetes.Interface, nodeName string) error {
	log := p.deps.Logger

	for {
		podList, err := clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{
			FieldSelector: "spec.nodeName=" + nodeName,
		})
		if err != nil {
			return fmt.Errorf("listing pods: %w", err)
		}

		// Reconcile listed pods
		desiredKeys := make(map[string]bool)
		for i := range podList.Items {
			pod := &podList.Items[i]
			key := podKey(pod)
			desiredKeys[key] = true

			tracked := p.pods.Has(key)
			if !tracked {
				log.Infow("new pod scheduled", "pod", key)
				// CreatePod manages its own p.pods lock internally
				if err := p.CreatePod(ctx, pod); err != nil {
					log.Errorw("failed to create pod", "pod", key, "error", err)
				}
			}
		}

		// Remove pods no longer scheduled here — snapshot first
		wpSnap := p.pods.Snapshot()
		for key, pod := range wpSnap {
			if !desiredKeys[key] {
				log.Infow("pod removed from node", "pod", key)
				// DeletePod manages its own p.pods lock internally
				if err := p.DeletePod(ctx, pod); err != nil {
					log.Errorw("failed to delete pod", "pod", key, "error", err)
				}
			}
		}

		// Push status updates for tracked pods
		wpStatusSnap := p.pods.Values()
		for _, pod := range wpStatusSnap {
			if p.notifyPodStatus != nil {
				status, err := p.GetPodStatus(ctx, pod.Namespace, pod.Name)
				if err == nil {
					updated := pod.DeepCopy()
					updated.Status = *status
					p.notifyPodStatus(updated)
				}
			}
		}

		select {
		case <-ctx.Done():
			log.Info("pod watcher shutting down")
			return nil
		case <-time.After(10 * time.Second):
		}
	}
}

// ─── Update API (for mkube-update self-replacement) ─────────────────────

// UpdateContainerRequest is the JSON body for the update-container API.
type UpdateContainerRequest struct {
	Name    string `json:"name"`              // RouterOS container name
	Tag     string `json:"tag"`               // new registry image ref
	Tarball string `json:"tarball,omitempty"` // RouterOS-relative tarball path (preferred over Tag)
}

// RunUpdateAPI starts an HTTP server that exposes an internal API for
// mkube-update to request container replacements (used for self-update).
func (p *MicroKubeProvider) RunUpdateAPI(ctx context.Context, listenAddr string) {
	log := p.deps.Logger.Named("update-api")

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/update-container", func(w http.ResponseWriter, r *http.Request) {
		var req UpdateContainerRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.Name == "" || req.Tag == "" {
			http.Error(w, `"name" and "tag" are required`, http.StatusBadRequest)
			return
		}

		log.Infow("update-container request", "name", req.Name, "tag", req.Tag, "tarball", req.Tarball)

		if err := p.replaceContainer(r.Context(), req.Name, req.Tag, req.Tarball); err != nil {
			log.Errorw("update-container failed", "name", req.Name, "error", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		log.Infow("update-container complete", "name", req.Name, "tag", req.Tag)
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `{"status":"ok"}`)
	})

	srv := &http.Server{Addr: listenAddr, Handler: mux}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Infow("update API listening", "addr", listenAddr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Errorw("update API error", "error", err)
	}
}

// replaceContainer stops, removes, and recreates a container with a new image.
// If tarball is provided, uses local file (file=); otherwise uses remote-image (tag=).
// It preserves the existing container's config (interface, root-dir, mounts, etc.).
func (p *MicroKubeProvider) replaceContainer(ctx context.Context, name, newTag, tarball string) error {
	log := p.deps.Logger.Named("update-api")

	// Get the existing container to preserve its config
	ct, err := p.deps.Runtime.GetContainer(ctx, name)
	if err != nil {
		return fmt.Errorf("getting container %s: %w", name, err)
	}

	// Stop if running
	if ct.IsRunning() {
		log.Infow("stopping container", "name", name)
		if err := p.deps.Runtime.StopContainer(ctx, ct.ID); err != nil {
			return fmt.Errorf("stopping container %s: %w", name, err)
		}
		// Wait for stopped state
		for i := 0; i < 30; i++ {
			time.Sleep(time.Second)
			ct, err = p.deps.Runtime.GetContainer(ctx, name)
			if err != nil {
				return fmt.Errorf("checking container %s: %w", name, err)
			}
			if ct.IsStopped() {
				break
			}
		}
		if !ct.IsStopped() {
			return fmt.Errorf("container %s did not stop within timeout", name)
		}
	}

	// Extra settle time — RouterOS may report stopped before fully releasing resources
	time.Sleep(2 * time.Second)

	// Remove with retry — RouterOS sometimes needs a moment after stop
	log.Infow("removing container", "name", name)
	var removeErr error
	for attempt := 0; attempt < 3; attempt++ {
		removeErr = p.deps.Runtime.RemoveContainer(ctx, ct.ID)
		if removeErr == nil {
			break
		}
		log.Warnw("remove failed, retrying", "name", name, "attempt", attempt+1, "error", removeErr)
		time.Sleep(2 * time.Second)
	}
	if removeErr != nil {
		return fmt.Errorf("removing container %s: %w", name, removeErr)
	}

	// Wait for RouterOS to fully release the root-dir after removal.
	// The container may be gone from the list but the directory lock isn't released yet.
	for i := 0; i < 15; i++ {
		if _, gerr := p.deps.Runtime.GetContainer(ctx, name); gerr != nil {
			break // container truly gone
		}
		time.Sleep(time.Second)
	}

	// Recreate with new image, preserving config.
	// Prefer tarball (file=) over remote-image (tag=) when available.
	spec := runtime.ContainerSpec{
		Name:        ct.Name,
		Interface:   ct.Interface,
		RootDir:     ct.RootDir,
		MountLists:  ct.MountLists,
		Cmd:         ct.Cmd,
		Entrypoint:  ct.Entrypoint,
		WorkDir:     ct.WorkDir,
		Hostname:    ct.Hostname,
		DNS:         ct.DNS,
		Logging:     ct.Logging,
		StartOnBoot: ct.StartOnBoot,
	}
	if tarball != "" {
		spec.Image = tarball // maps to RouterOS file= parameter
		log.Infow("creating container from tarball", "name", name, "tarball", tarball)
	} else {
		spec.Tag = newTag // maps to RouterOS remote-image= parameter
		log.Infow("creating container with remote-image", "name", name, "tag", newTag)
	}
	// Retry create — RouterOS may still hold the root-dir lock briefly after removal
	var createErr error
	for attempt := 0; attempt < 5; attempt++ {
		createErr = p.deps.Runtime.CreateContainer(ctx, spec)
		if createErr == nil {
			break
		}
		if strings.Contains(createErr.Error(), "root-dir overlap") {
			log.Warnw("root-dir overlap, waiting for cleanup", "name", name, "attempt", attempt+1)
			time.Sleep(3 * time.Second)
			continue
		}
		break // non-retryable error
	}
	if createErr != nil {
		return fmt.Errorf("creating container %s: %w", name, createErr)
	}

	// Wait for extraction then start
	var newCt *runtime.Container
	for i := 0; i < 60; i++ {
		time.Sleep(time.Second)
		newCt, err = p.deps.Runtime.GetContainer(ctx, name)
		if err == nil {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("waiting for container %s after create: %w", name, err)
	}

	// Start with retry — MikroTik REST API can return EOF if the
	// previous container hasn't fully torn down yet.
	log.Infow("starting container", "name", name)
	replaceBackoffs := []time.Duration{
		2 * time.Second, 2 * time.Second,
		3 * time.Second, 3 * time.Second,
		5 * time.Second, 5 * time.Second,
	}
	var startErr error
	for attempt := 0; attempt <= len(replaceBackoffs); attempt++ {
		if startErr = p.deps.Runtime.StartContainer(ctx, newCt.ID); startErr == nil {
			break
		}
		if attempt < len(replaceBackoffs) {
			log.Warnw("container start failed, retrying",
				"name", name, "attempt", attempt+1, "error", startErr)
			time.Sleep(replaceBackoffs[attempt])
			if updated, gerr := p.deps.Runtime.GetContainer(ctx, name); gerr == nil {
				newCt = updated
			}
		}
	}
	if startErr != nil {
		return fmt.Errorf("starting container %s after %d attempts: %w", name, len(replaceBackoffs)+1, startErr)
	}

	return nil
}

// waitForPodLiveness waits for a pod's containers to be running and healthy.
// For DNS pods (pod.Name == "dns"), also probes port 53 to verify the recursor.
// Returns true if the pod is confirmed alive within the timeout.
func (p *MicroKubeProvider) waitForPodLiveness(ctx context.Context, pod *corev1.Pod, timeout time.Duration) bool {
	log := p.deps.Logger
	key := podKey(pod)
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		// Check that all containers are running on RouterOS
		allRunning := true
		for _, c := range pod.Spec.Containers {
			name := sanitizeName(pod, c.Name)
			ct, err := p.deps.Runtime.GetContainer(ctx, name)
			if err != nil || ct.Status != "running" {
				allRunning = false
				break
			}
		}

		if !allRunning {
			time.Sleep(2 * time.Second)
			continue
		}

		// For DNS pods, also probe port 53
		if pod.Name == "dns" {
			networkName := pod.Annotations[annotationNetwork]
			if netDef, ok := p.deps.NetworkMgr.NetworkDef(networkName); ok && netDef.DNS.Server != "" {
				if probeDNSPort(netDef.DNS.Server, netDef.DNS.Zone, 3*time.Second) {
					log.Infow("pod liveness confirmed (DNS port 53 alive)", "pod", key)
					return true
				}
				time.Sleep(2 * time.Second)
				continue
			}
		}

		// For non-DNS pods, check basic connectivity via veth IP
		for i := range pod.Spec.Containers {
			vn := vethName(pod, i)
			if ip, _, ok := p.deps.NetworkMgr.GetPortInfo(vn); ok && ip != "" {
				conn, err := net.DialTimeout("tcp", ip+":1", 1*time.Second)
				if conn != nil {
					conn.Close()
				}
				// Even if connection is refused, the IP is reachable — container is up
				if err == nil || !isTimeout(err) {
					log.Infow("pod liveness confirmed (IP reachable)", "pod", key, "ip", ip)
					return true
				}
			}
		}

		time.Sleep(2 * time.Second)
	}

	log.Warnw("pod liveness check timed out", "pod", key)
	return false
}

// isTimeout returns true if the error is a network timeout.
func isTimeout(err error) bool {
	if err == nil {
		return false
	}
	netErr, ok := err.(net.Error)
	return ok && netErr.Timeout()
}

// ─── DNS Aliases ─────────────────────────────────────────────────────────────

// dnsAlias maps an alias hostname to a container name within the pod.
type dnsAlias struct {
	hostname      string
	containerName string
}

// parseAliases parses the vkube.io/aliases annotation.
// Format: "alias=container,alias2=container2,alias3"
// Aliases without "=container" target the default (first) container.
func parseAliases(annotation, defaultContainer string) []dnsAlias {
	var aliases []dnsAlias
	for _, part := range strings.Split(annotation, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if eq := strings.IndexByte(part, '='); eq >= 0 {
			aliases = append(aliases, dnsAlias{
				hostname:      strings.TrimSpace(part[:eq]),
				containerName: strings.TrimSpace(part[eq+1:]),
			})
		} else {
			aliases = append(aliases, dnsAlias{
				hostname:      part,
				containerName: defaultContainer,
			})
		}
	}
	return aliases
}

// registerPodAliases registers the default pod alias (podName → first container IP)
// and any custom aliases from vkube.io/aliases in both the network zone and
// namespace zone (if applicable).
func (p *MicroKubeProvider) registerPodAliases(ctx context.Context, pod *corev1.Pod, networkName, namespaceName string, containerIPs map[string]string, log *zap.SugaredLogger) {
	if len(pod.Spec.Containers) == 0 || len(containerIPs) == 0 {
		return
	}

	firstContainer := pod.Spec.Containers[0].Name

	// Build the full alias list: default pod alias + custom aliases
	aliases := []dnsAlias{{hostname: pod.Name, containerName: firstContainer}}
	if ann := pod.Annotations[annotationAliases]; ann != "" {
		aliases = append(aliases, parseAliases(ann, firstContainer)...)
	}

	// Resolve namespace zone (if applicable)
	var nsEndpoint, nsZoneID string
	if namespaceName != "" && p.deps.Namespace != nil {
		ep, zid, err := p.deps.Namespace.ResolveNamespace(namespaceName)
		if err == nil {
			nsEndpoint, nsZoneID = ep, zid
		}
	}

	dnsClient := p.deps.NetworkMgr.DNSClient()

	for _, a := range aliases {
		ip, ok := containerIPs[a.containerName]
		if !ok {
			log.Warnw("alias references unknown container", "alias", a.hostname, "container", a.containerName)
			continue
		}

		// Clean stale DNS records (old IPs) before registering the current one
		if cleanErr := p.deps.NetworkMgr.CleanStaleDNS(ctx, networkName, a.hostname, ip); cleanErr != nil {
			log.Warnw("failed to clean stale DNS records", "alias", a.hostname, "error", cleanErr)
		}

		// Register in network zone
		if regErr := p.deps.NetworkMgr.RegisterDNS(ctx, networkName, a.hostname, ip); regErr != nil {
			log.Warnw("failed to register DNS alias", "alias", a.hostname, "ip", ip, "error", regErr)
		} else {
			log.Debugw("DNS alias registered", "alias", a.hostname, "container", a.containerName, "ip", ip)
		}

		// Register in namespace zone (clean stale + register)
		if nsZoneID != "" && dnsClient != nil {
			_ = dnsClient.CleanStaleRecords(ctx, nsEndpoint, nsZoneID, a.hostname, ip)
			if regErr := dnsClient.RegisterHost(ctx, nsEndpoint, nsZoneID, a.hostname, ip, 60); regErr != nil {
				log.Warnw("failed to register DNS alias in namespace zone", "alias", a.hostname, "error", regErr)
			}
		}
	}
}

// reregisterPodDNS re-registers DNS records for all tracked pods.
// This ensures pod DNS records survive DNS container restarts that wipe the zone.
// Registers both container-level records (container.pod → IP) and pod-level
// aliases (podName → IP).
func (p *MicroKubeProvider) reregisterPodDNS(ctx context.Context) {
	// Snapshot pods (called from reconciler goroutine)
	dnsPodsSnap := p.pods.Values()

	for _, pod := range dnsPodsSnap {
		networkName := pod.Annotations[annotationNetwork]
		namespaceName := pod.Namespace

		// Rebuild containerIPs from the network manager's allocation records
		containerIPs := make(map[string]string)
		for i, c := range pod.Spec.Containers {
			veth := vethName(pod, i)
			if ip, _, ok := p.deps.NetworkMgr.GetPortInfo(veth); ok {
				containerIPs[c.Name] = ip

				// Register the container-level DNS record (container.pod → IP).
				// This is normally done by AllocateInterface during CreatePod,
				// but pods tracked via the "already exists" path never called it.
				containerHostname := c.Name + "." + pod.Name
				if err := p.deps.NetworkMgr.RegisterDNS(ctx, networkName, containerHostname, ip); err != nil {
					p.deps.Logger.Warnw("failed to re-register container DNS",
						"hostname", containerHostname, "ip", ip, "error", err)
				}
				// Clean stale IPs for this container hostname
				_ = p.deps.NetworkMgr.CleanStaleDNS(ctx, networkName, containerHostname, ip)
			}
		}

		if len(containerIPs) > 0 {
			p.registerPodAliases(ctx, pod, networkName, namespaceName, containerIPs, p.deps.Logger)
		}
	}
}

// deregisterPodAliases removes the default pod alias and custom aliases.
func (p *MicroKubeProvider) deregisterPodAliases(ctx context.Context, pod *corev1.Pod, networkName, namespaceName string, containerIPs map[string]string, log *zap.SugaredLogger) {
	if len(pod.Spec.Containers) == 0 || len(containerIPs) == 0 {
		return
	}

	firstContainer := pod.Spec.Containers[0].Name

	aliases := []dnsAlias{{hostname: pod.Name, containerName: firstContainer}}
	if ann := pod.Annotations[annotationAliases]; ann != "" {
		aliases = append(aliases, parseAliases(ann, firstContainer)...)
	}

	var nsEndpoint, nsZoneID string
	if namespaceName != "" && p.deps.Namespace != nil {
		ep, zid, err := p.deps.Namespace.ResolveNamespace(namespaceName)
		if err == nil {
			nsEndpoint, nsZoneID = ep, zid
		}
	}

	dnsClient := p.deps.NetworkMgr.DNSClient()

	for _, a := range aliases {
		ip, ok := containerIPs[a.containerName]
		if !ok {
			continue
		}

		if err := p.deps.NetworkMgr.DeregisterDNS(ctx, networkName, a.hostname, ip); err != nil {
			log.Warnw("error deregistering DNS alias", "alias", a.hostname, "ip", ip, "error", err)
		}

		if nsZoneID != "" && dnsClient != nil {
			if err := dnsClient.DeregisterHostByIP(ctx, nsEndpoint, nsZoneID, a.hostname, ip); err != nil {
				log.Warnw("error deregistering DNS alias from namespace zone", "alias", a.hostname, "error", err)
			}
		}
	}
}

// ─── Micrologs Integration ──────────────────────────────────────────────────

// pushLogMappings sends pod→container name mappings to the micrologs service.
func (p *MicroKubeProvider) pushLogMappings(ctx context.Context, pod *corev1.Pod, log *zap.SugaredLogger) {
	if !p.deps.Config.Logging.Enabled || p.deps.Config.Logging.URL == "" {
		return
	}

	url := strings.TrimRight(p.deps.Config.Logging.URL, "/") + "/metadata/mapping"

	for _, container := range pod.Spec.Containers {
		rosName := sanitizeName(pod, container.Name)
		payload := map[string]string{
			"namespace": pod.Namespace,
			"pod":       pod.Name,
			"container": container.Name,
			"ros_name":  rosName,
		}
		body, err := json.Marshal(payload)
		if err != nil {
			log.Warnw("failed to marshal log mapping", "error", err)
			continue
		}

		req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
		if err != nil {
			log.Warnw("failed to create log mapping request", "error", err)
			continue
		}
		req.Header.Set("Content-Type", "application/json")

		logsClient := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
			MaxConnsPerHost:   1,
			DisableKeepAlives: true,
		}}
		resp, err := logsClient.Do(req)
		if err != nil {
			log.Warnw("failed to push log mapping", "container", rosName, "error", err)
			continue
		}
		resp.Body.Close()

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			log.Warnw("micrologs rejected mapping", "container", rosName, "status", resp.StatusCode)
		}
	}
}

// ─── Lifecycle Failed Handler ────────────────────────────────────────────────

// handleLifecycleFailed is called by the lifecycle manager when a container
// exceeds max restarts. It finds the owning pod and triggers a full
// delete+create cycle with fresh veth allocation.
func (p *MicroKubeProvider) handleLifecycleFailed(containerName string) {
	log := p.deps.Logger.With("container", containerName)
	log.Infow("lifecycle manager reported container failed, attempting pod recreate")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Find the pod that owns this container (lifecycle callback goroutine)
	var foundKey string
	var foundPod *corev1.Pod
	redeploySnap := p.redeploying.Snapshot()
	for key, pod := range p.pods.Snapshot() {
		if redeploySnap[key] {
			continue
		}
		for _, c := range pod.Spec.Containers {
			if sanitizeName(pod, c.Name) == containerName {
				foundKey = key
				foundPod = pod
				break
			}
		}
		if foundPod != nil {
			break
		}
	}

	if foundPod != nil {
		log.Infow("found owning pod, recreating", "pod", foundKey)
		// DeletePod/CreatePod manage their own p.pods lock internally
		if err := p.DeletePod(ctx, foundPod); err != nil {
			log.Errorw("failed to delete pod for lifecycle recovery", "pod", foundKey, "error", err)
			return
		}
		if err := p.CreatePod(ctx, foundPod); err != nil {
			log.Errorw("failed to recreate pod for lifecycle recovery", "pod", foundKey, "error", err)
			return
		}
		log.Infow("pod recreated after lifecycle failure", "pod", foundKey)
		return
	}

	log.Warnw("no tracked pod found for failed container")
}

// ─── Helpers ────────────────────────────────────────────────────────────────

func podKey(pod *corev1.Pod) string {
	return pod.Namespace + "/" + pod.Name
}

// sanitizeName converts a pod/container name pair into a valid RouterOS
// container name using OpenShift-style naming: namespace_pod_container.
func sanitizeName(pod *corev1.Pod, containerName string) string {
	ns := pod.Namespace
	if ns == "" {
		ns = "default"
	}
	name := fmt.Sprintf("%s_%s_%s", ns, pod.Name, containerName)
	name = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		if r >= 'A' && r <= 'Z' {
			return r + 32 // lowercase
		}
		return '_'
	}, name)
	return truncate(name, 64)
}

// vethName generates a deterministic veth interface name for a container.
func vethName(pod *corev1.Pod, index int) string {
	ns := pod.Namespace
	if ns == "" {
		ns = "default"
	}
	return fmt.Sprintf("veth_%s_%s_%d", truncate(ns, 15), truncate(pod.Name, 15), index)
}

func truncate(s string, max int) string {
	if len(s) > max {
		return s[:max]
	}
	return s
}

// extractAllocHolder parses the veth name from an IPAM error like
// "IP 192.168.1.252 already allocated to veth_gw_dns_STALE".
func extractAllocHolder(errMsg string) string {
	const marker = "already allocated to "
	idx := strings.Index(errMsg, marker)
	if idx < 0 {
		return ""
	}
	return strings.TrimSpace(errMsg[idx+len(marker):])
}

func boolToConditionStatus(b bool) corev1.ConditionStatus {
	if b {
		return corev1.ConditionTrue
	}
	return corev1.ConditionFalse
}

func extractHealthCheck(c corev1.Container) *lifecycle.HealthCheck {
	if c.LivenessProbe != nil && c.LivenessProbe.HTTPGet != nil {
		return &lifecycle.HealthCheck{
			Type:     "http",
			Path:     c.LivenessProbe.HTTPGet.Path,
			Port:     int(c.LivenessProbe.HTTPGet.Port.IntVal),
			Interval: int(c.LivenessProbe.PeriodSeconds),
		}
	}
	if c.LivenessProbe != nil && c.LivenessProbe.TCPSocket != nil {
		return &lifecycle.HealthCheck{
			Type: "tcp",
			Port: int(c.LivenessProbe.TCPSocket.Port.IntVal),
		}
	}
	return nil
}

// extractProbes converts K8s probe specs into lifecycle ProbeSet.
// If the container declares TCP ports but has no explicit probes, default
// liveness+readiness TCP probes are auto-generated so that dead processes
// inside "running" containers are detected by the watchdog.
func extractProbes(c corev1.Container) *lifecycle.ProbeSet {
	ps := &lifecycle.ProbeSet{
		Startup:   probeToConfig(c.StartupProbe),
		Liveness:  probeToConfig(c.LivenessProbe),
		Readiness: probeToConfig(c.ReadinessProbe),
	}
	if ps.Startup == nil && ps.Liveness == nil && ps.Readiness == nil {
		// Auto-generate probes from declared TCP ports
		if port := firstTCPPort(c); port > 0 {
			ps.Liveness = &lifecycle.ProbeConfig{
				Type:                "tcp",
				Port:                port,
				InitialDelaySeconds: 10,
				PeriodSeconds:       30,
				TimeoutSeconds:      2,
				FailureThreshold:    3,
				SuccessThreshold:    1,
			}
			ps.Readiness = &lifecycle.ProbeConfig{
				Type:             "tcp",
				Port:             port,
				PeriodSeconds:    10,
				TimeoutSeconds:   2,
				FailureThreshold: 1,
				SuccessThreshold: 1,
			}
			return ps
		}
		return nil
	}
	return ps
}

// firstTCPPort returns the first TCP containerPort declared in the container spec,
// or 0 if none exists. Ports with empty protocol default to TCP per K8s convention.
func firstTCPPort(c corev1.Container) int {
	for _, p := range c.Ports {
		if p.Protocol == "" || p.Protocol == corev1.ProtocolTCP {
			return int(p.ContainerPort)
		}
	}
	return 0
}

// probeToConfig converts a single K8s probe to our ProbeConfig.
func probeToConfig(probe *corev1.Probe) *lifecycle.ProbeConfig {
	if probe == nil {
		return nil
	}

	pc := &lifecycle.ProbeConfig{
		InitialDelaySeconds: int(probe.InitialDelaySeconds),
		PeriodSeconds:       int(probe.PeriodSeconds),
		TimeoutSeconds:      int(probe.TimeoutSeconds),
		FailureThreshold:    int(probe.FailureThreshold),
		SuccessThreshold:    int(probe.SuccessThreshold),
	}

	switch {
	case probe.HTTPGet != nil:
		pc.Type = "http"
		pc.Path = probe.HTTPGet.Path
		pc.Port = int(probe.HTTPGet.Port.IntVal)
	case probe.TCPSocket != nil:
		pc.Type = "tcp"
		pc.Port = int(probe.TCPSocket.Port.IntVal)
	case probe.Exec != nil:
		pc.Type = "exec"
		pc.Command = probe.Exec.Command
	default:
		return nil
	}

	return pc
}

func extractDependencies(pod *corev1.Pod) []string {
	if deps, ok := pod.Annotations["vkube.io/depends-on"]; ok {
		return strings.Split(deps, ",")
	}
	return nil
}

func extractPriority(pod *corev1.Pod, index int) int {
	if v, ok := pod.Annotations["vkube.io/boot-priority"]; ok {
		if priority, err := strconv.Atoi(v); err == nil {
			return priority
		}
	}
	return index * 10
}

const maxEvents = 256

// Pod creation backoff constants (used by reconcile loop + pod worker).
const (
	createBackoffThreshold = 3 // retries before backoff kicks in
	createInitialBackoff   = 30 * time.Second
	createMaxBackoff       = 5 * time.Minute
	// createHardFailureCap ends automatic retries entirely. With backoff at
	// its 5-minute ceiling this is ~1.5h of trying — enough to ride out a
	// registry restart, and finite, because an unbounded create loop is
	// net-destructive on the device (#26). Cleared by the same paths that
	// clear createFailures: pod update, delete, redeploy, or mkube restart.
	createHardFailureCap = 20
)

// updateCreateResult updates backoff/failure tracking after a pod creation attempt.
// On success, clears both createFailures and createBackoff for the key.
// On failure, increments failures, records events, and updates exponential backoff.
func (p *MicroKubeProvider) updateCreateResult(key string, pod *corev1.Pod, err error) {
	log := p.deps.Logger
	if err != nil {
		failures := safemap.Increment(p.createFailures, key, 1)
		log.Errorw("failed to create pod", "pod", key, "error", err, "consecutiveFailures", failures)
		p.recordEvent(pod, "CreateFailed", fmt.Sprintf("Failed to create pod: %v", err), "Warning")

		cbs, _ := p.createBackoff.Get(key)
		if cbs == nil {
			cbs = &containerRestartState{}
		}
		cbs.attempts++
		cbs.lastAttempt = time.Now()
		if cbs.attempts > createBackoffThreshold {
			if cbs.backoff == 0 {
				cbs.backoff = createInitialBackoff
			} else {
				cbs.backoff *= 2
				if cbs.backoff > createMaxBackoff {
					cbs.backoff = createMaxBackoff
				}
			}
			p.recordEvent(pod, "BackOff", fmt.Sprintf("Back-off creating pod, next retry in %s (attempt %d)", cbs.backoff, cbs.attempts), "Warning")
		}
		p.createBackoff.Set(key, cbs)
	} else {
		p.createFailures.Delete(key)
		p.createBackoff.Delete(key)
	}
}

// recordEvent appends a Kubernetes event to the in-memory ring buffer.
func (p *MicroKubeProvider) recordEvent(pod *corev1.Pod, reason, message, eventType string) {
	now := metav1.Now()
	evt := corev1.Event{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Event"},
		ObjectMeta: metav1.ObjectMeta{
			Name:              fmt.Sprintf("%s.%x", pod.Name, now.UnixNano()),
			Namespace:         pod.Namespace,
			CreationTimestamp: now,
		},
		InvolvedObject: corev1.ObjectReference{
			Kind:      "Pod",
			Name:      pod.Name,
			Namespace: pod.Namespace,
		},
		Reason:         reason,
		Message:        message,
		Type:           eventType,
		FirstTimestamp: now,
		LastTimestamp:  now,
		Count:          1,
		Source:         corev1.EventSource{Component: "mkube", Host: p.nodeName},
	}
	p.events = append(p.events, evt)
	if len(p.events) > maxEvents {
		p.events = p.events[len(p.events)-maxEvents:]
	}
}
