package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/glennswest/mkube/pkg/config"
	"github.com/glennswest/mkube/pkg/lifecycle"
	"github.com/glennswest/mkube/pkg/namespace"
	"github.com/glennswest/mkube/pkg/network"
	"github.com/glennswest/mkube/pkg/runtime"
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

	// Device passthrough annotations (StormBase only)
	annotationDeviceClass = "stormbase.io/device-class"
	annotationDeviceCount = "stormbase.io/device-count"
	annotationDeviceProfile = "stormbase.io/device-profile"
	annotationDeviceAllocation = "stormbase.io/device-allocation"
)

// Deps holds injected dependencies for the provider.
type Deps struct {
	Config       *config.Config
	Runtime      runtime.ContainerRuntime
	NetworkMgr   *network.Manager
	StorageMgr   *storage.Manager
	LifecycleMgr *lifecycle.Manager
	Namespace    *namespace.Manager // optional, nil if namespace management is disabled
	Store        *store.Store       // optional, nil if NATS is not configured
	Logger       *zap.SugaredLogger
}

// MicroKubeProvider implements the Virtual Kubelet provider interface.
// It translates Kubernetes Pod specifications into RouterOS
// container operations, managing the full lifecycle including networking,
// storage, and boot ordering.
type MicroKubeProvider struct {
	deps            Deps
	nodeName        string
	startTime       time.Time
	pods            map[string]*corev1.Pod       // namespace/name -> pod
	configMaps      map[string]*corev1.ConfigMap // namespace/name -> configmap
	bareMetalHosts  map[string]*BareMetalHost   // namespace/name -> BMH
	dhcpIndex       *dhcpNetworkIndex            // precomputed DHCP reservation/subnet lookup
	events          []corev1.Event               // recent events (ring buffer, max 256)
	notifyPodStatus func(*corev1.Pod)            // callback for pod status updates
}

// SetStore sets the NATS store on the provider (used for deferred NATS connection).
func (p *MicroKubeProvider) SetStore(s *store.Store) {
	p.deps.Store = s
	p.deps.Logger.Infow("NATS store attached to provider")
	p.LoadBMHFromStore(context.Background())
	p.startDHCPSubscription(context.Background())
}

// NewMicroKubeProvider creates a new provider instance.
func NewMicroKubeProvider(deps Deps) (*MicroKubeProvider, error) {
	p := &MicroKubeProvider{
		deps:       deps,
		nodeName:   deps.Config.NodeName,
		startTime:  time.Now(),
		pods:           make(map[string]*corev1.Pod),
		configMaps:     make(map[string]*corev1.ConfigMap),
		bareMetalHosts: make(map[string]*BareMetalHost),
		dhcpIndex:      buildDHCPIndex(deps.Config.Networks),
	}

	// Load built-in default ConfigMaps derived from mkube config
	for _, cm := range generateDefaultConfigMaps(deps.Config) {
		p.configMaps[cm.Namespace+"/"+cm.Name] = cm
	}

	return p, nil
}

// ─── PodLifecycleHandler Interface ──────────────────────────────────────────

// CreatePod takes a Kubernetes Pod spec and creates the corresponding
// RouterOS container(s). This includes:
//  1. Pulling/caching the image as an OCI tarball
//  2. Allocating a veth interface and IP address
//  3. Creating volume mounts
//  4. Registering boot ordering if restartPolicy=Always
//  5. Creating and starting the RouterOS container
func (p *MicroKubeProvider) CreatePod(ctx context.Context, pod *corev1.Pod) error {
	log := p.deps.Logger.With("pod", podKey(pod))
	log.Infow("creating pod")

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

		// 1. Resolve image → tarball path
		var tarballPath string
		if filePath := pod.Annotations[annotationFile]; filePath != "" {
			// Use local tarball directly (skip OCI pull)
			tarballPath = filePath
		} else {
			var err error
			tarballPath, err = p.deps.StorageMgr.EnsureImage(ctx, container.Image)
			if err != nil {
				return fmt.Errorf("ensuring image %s: %w", container.Image, err)
			}
		}

		// 2. Allocate network (registers containerName.podName in network zone)
		vethName := vethName(pod, i)
		containerHostname := container.Name + "." + pod.Name
		staticIP := pod.Annotations[annotationStaticIP]
		ip, gw, dnsServer, err := p.deps.NetworkMgr.AllocateInterface(ctx, vethName, containerHostname, networkName, staticIP)
		if err != nil {
			// If veth exists from a previous failed attempt, clean up and retry
			if strings.Contains(err.Error(), "already have interface") {
				log.Warnw("cleaning up orphaned veth", "veth", vethName)
				if releaseErr := p.deps.NetworkMgr.ReleaseInterface(ctx, vethName); releaseErr != nil {
					log.Warnw("failed to release orphaned veth", "veth", vethName, "error", releaseErr)
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
					if regErr := dnsClient.RegisterHost(ctx, endpoint, zoneID, containerHostname, bareIP, 60); regErr != nil {
						log.Warnw("failed to register container in namespace zone", "namespace", namespaceName, "error", regErr)
					}
				}
				p.deps.Namespace.AddContainerToNamespace(namespaceName, name)
			}
		}

		// 3. Provision volumes, write ConfigMap data, and create mount entries.
		// Clean up any orphaned mounts from previous failed attempts first.
		_ = p.deps.Runtime.RemoveMountsByList(ctx, name)
		mountListName := ""
		for _, vm := range container.VolumeMounts {
			hostPath, err := p.deps.StorageMgr.ProvisionVolume(ctx, name, vm.Name, vm.MountPath)
			if err != nil {
				return fmt.Errorf("provisioning volume %s: %w", vm.Name, err)
			}

			// Write ConfigMap data files if this volume references a ConfigMap.
			// Files are written locally and the path is translated via selfRootDir
			// so RouterOS can see them.
			if data := p.resolveConfigMapVolume(pod, vm.Name); data != nil {
				localDir := fmt.Sprintf("/data/configmaps/%s/%s", name, vm.Name)
				if mkErr := os.MkdirAll(localDir, 0o755); mkErr != nil {
					log.Warnw("failed to create configmap dir", "path", localDir, "error", mkErr)
				} else {
					for filename, content := range data {
						if wErr := os.WriteFile(localDir+"/"+filename, []byte(content), 0o644); wErr != nil {
							log.Warnw("failed to write configmap file", "path", localDir+"/"+filename, "error", wErr)
						}
					}
					// Translate local path to RouterOS-visible path
					if p.deps.Config.Storage.SelfRootDir != "" {
						hostPath = p.deps.Config.Storage.SelfRootDir + "/" + strings.TrimPrefix(localDir, "/")
					}
				}
			}

			// Create mount entry on the runtime
			mountListName = name
			if err := p.deps.Runtime.CreateMount(ctx, mountListName, hostPath, vm.MountPath); err != nil {
				log.Warnw("failed to create mount", "volume", vm.Name, "error", err)
			}
		}

		// 4. Determine boot behavior
		startOnBoot := "false"
		if pod.Spec.RestartPolicy == corev1.RestartPolicyAlways {
			startOnBoot = "true"
		}

		// 5. Create the container
		spec := runtime.ContainerSpec{
			Name:        name,
			Image:       tarballPath,
			Interface:   vethName,
			RootDir:     fmt.Sprintf("%s/%s", p.deps.Config.Storage.BasePath, name),
			MountLists:  mountListName,
			Cmd:         strings.Join(container.Command, " "),
			Command:     container.Command,
			Hostname:    pod.Name,
			DNS:         dnsServer,
			Logging:     "true",
			StartOnBoot: startOnBoot,
		}

		if err := p.deps.Runtime.CreateContainer(ctx, spec); err != nil {
			return fmt.Errorf("creating container %s: %w", name, err)
		}

		// 6. Wait for tarball extraction then start the container.
		// After creation RouterOS extracts the tarball; the container is
		// not yet "stopped" until extraction finishes.
		ct, err := p.waitForStopped(ctx, name, 120*time.Second)
		if err != nil {
			return fmt.Errorf("waiting for container %s to be ready: %w", name, err)
		}
		if err := p.deps.Runtime.StartContainer(ctx, ct.ID); err != nil {
			return fmt.Errorf("starting container %s: %w", name, err)
		}

		// 7. Register with lifecycle manager for boot ordering / health probes
		if startOnBoot == "true" {
			p.deps.LifecycleMgr.Register(name, lifecycle.ContainerUnit{
				Name:          name,
				ContainerID:   ct.ID,
				ContainerIP:   ip,
				RestartPolicy: string(pod.Spec.RestartPolicy),
				StartOnBoot:   true,
				Managed:       true,
				Probes:        extractProbes(container),
				HealthCheck:   extractHealthCheck(container),
				DependsOn:     extractDependencies(pod),
				Priority:      extractPriority(pod, i),
			})
		}

		log.Infow("container created and started", "name", name, "id", ct.ID)
	}

	// 8. Register DNS aliases (pod-level default + custom aliases from annotation)
	p.registerPodAliases(ctx, pod, networkName, namespaceName, containerIPs, log)

	// 9. Push pod→container mappings to micrologs
	p.pushLogMappings(ctx, pod, log)

	// Track the pod
	p.pods[podKey(pod)] = pod.DeepCopy()

	// Record events
	p.recordEvent(pod, "Scheduled", fmt.Sprintf("Successfully assigned %s/%s to %s", pod.Namespace, pod.Name, p.nodeName), "Normal")
	for _, c := range pod.Spec.Containers {
		p.recordEvent(pod, "Pulling", fmt.Sprintf("Pulling image %q", c.Image), "Normal")
		p.recordEvent(pod, "Created", fmt.Sprintf("Created container %s", c.Name), "Normal")
		p.recordEvent(pod, "Started", fmt.Sprintf("Started container %s", c.Name), "Normal")
	}

	return nil
}

// waitForStopped polls until the container reaches the "stopped" state
// (tarball extraction complete) or the timeout expires.
func (p *MicroKubeProvider) waitForStopped(ctx context.Context, name string, timeout time.Duration) (*runtime.Container, error) {
	deadline := time.After(timeout)
	ticker := time.NewTicker(2 * time.Second)
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

// UpdatePod handles pod spec updates. RouterOS containers are immutable,
// so this performs a rolling update: create new → verify → remove old.
func (p *MicroKubeProvider) UpdatePod(ctx context.Context, pod *corev1.Pod) error {
	log := p.deps.Logger.With("pod", podKey(pod))
	log.Infow("updating pod (rolling replacement)")

	// Delete and recreate — RouterOS containers are immutable
	if err := p.DeletePod(ctx, pod); err != nil {
		log.Warnw("error deleting old pod during update", "error", err)
	}
	return p.CreatePod(ctx, pod)
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
			for j := 0; j < 30; j++ {
				time.Sleep(time.Second)
				updated, err := p.deps.Runtime.GetContainer(ctx, name)
				if err != nil || !updated.IsRunning() {
					break
				}
			}
		}

		// Retry RemoveContainer — the container may still be transitioning
		for attempt := 0; attempt < 5; attempt++ {
			if err := p.deps.Runtime.RemoveContainer(ctx, ct.ID); err != nil {
				log.Warnw("error removing container, retrying", "name", name, "attempt", attempt+1, "error", err)
				time.Sleep(2 * time.Second)
				// Re-fetch container to check if it's gone
				if _, gerr := p.deps.Runtime.GetContainer(ctx, name); gerr != nil {
					log.Infow("container gone after retry", "name", name)
					break // container disappeared
				}
				if attempt == 4 {
					lastErr = fmt.Errorf("failed to remove container %s after 5 attempts: %w", name, err)
					log.Errorw("giving up on container removal", "name", name, "error", err)
				}
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
	delete(p.pods, podKey(pod))
	return lastErr
}

// GetPod returns the tracked pod object.
func (p *MicroKubeProvider) GetPod(ctx context.Context, namespace, name string) (*corev1.Pod, error) {
	key := namespace + "/" + name
	if pod, ok := p.pods[key]; ok {
		return pod, nil
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
			switch {
			case ct.IsRunning():
				cs.Ready = p.deps.LifecycleMgr.GetUnitReady(rosName)
				cs.State = corev1.ContainerState{
					Running: &corev1.ContainerStateRunning{
						StartedAt: metav1.Now(),
					},
				}
			case ct.IsStopped():
				cs.State = corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{
						Reason: "Stopped",
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
	pods := make([]*corev1.Pod, 0, len(p.pods))
	for _, pod := range p.pods {
		pods = append(pods, pod)
	}
	return pods, nil
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
		}
	}
}

func (p *MicroKubeProvider) reconcile(ctx context.Context) error {
	log := p.deps.Logger

	// 1. Load desired pods and configmaps — from NATS store if available, else from YAML manifest
	var desiredPods []*corev1.Pod
	var manifestCMs []*corev1.ConfigMap

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

	// Store ConfigMaps from manifest, then re-apply generated defaults
	// so that config-derived ConfigMaps (DNS, DHCP) always reflect the
	// live mkube config rather than stale copies persisted in NATS.
	for _, cm := range manifestCMs {
		p.configMaps[cm.Namespace+"/"+cm.Name] = cm
	}
	for _, cm := range generateDefaultConfigMaps(p.deps.Config) {
		p.configMaps[cm.Namespace+"/"+cm.Name] = cm
	}

	// 2. List actual containers on RouterOS
	actual, err := p.deps.Runtime.ListContainers(ctx)
	if err != nil {
		return fmt.Errorf("listing containers: %w", err)
	}
	actualByName := make(map[string]runtime.Container, len(actual))
	for _, c := range actual {
		actualByName[c.Name] = c
	}

	// 3. Create missing containers
	for _, pod := range desiredPods {
		key := podKey(pod)
		if _, tracked := p.pods[key]; tracked {
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
			log.Infow("creating missing pod", "pod", key)
			if err := p.CreatePod(ctx, pod); err != nil {
				log.Errorw("failed to create pod", "pod", key, "error", err)
			}
		} else {
			// Track already-existing pods
			p.pods[key] = pod.DeepCopy()
			p.recordEvent(pod, "Reconciled", fmt.Sprintf("Existing pod %s/%s tracked on node %s", pod.Namespace, pod.Name, p.nodeName), "Normal")
		}
	}

	return nil
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
	for _, key := range podKeys {
		var pod corev1.Pod
		if _, err := p.deps.Store.Pods.GetJSON(ctx, key, &pod); err != nil {
			p.deps.Logger.Warnw("failed to read pod from store", "key", key, "error", err)
			continue
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
	// Start a background goroutine that periodically pushes status updates
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				for _, pod := range p.pods {
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

			if _, tracked := p.pods[key]; !tracked {
				log.Infow("new pod scheduled", "pod", key)
				if err := p.CreatePod(ctx, pod); err != nil {
					log.Errorw("failed to create pod", "pod", key, "error", err)
				}
			}
		}

		// Remove pods no longer scheduled here
		for key, pod := range p.pods {
			if !desiredKeys[key] {
				log.Infow("pod removed from node", "pod", key)
				if err := p.DeletePod(ctx, pod); err != nil {
					log.Errorw("failed to delete pod", "pod", key, "error", err)
				}
			}
		}

		// Push status updates for tracked pods
		for _, pod := range p.pods {
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
	Name string `json:"name"` // RouterOS container name
	Tag  string `json:"tag"`  // new registry image ref
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

		log.Infow("update-container request", "name", req.Name, "tag", req.Tag)

		if err := p.replaceContainer(r.Context(), req.Name, req.Tag); err != nil {
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

// replaceContainer stops, removes, and recreates a container with a new image tag.
// It preserves the existing container's config (interface, root-dir, mounts, etc.).
func (p *MicroKubeProvider) replaceContainer(ctx context.Context, name, newTag string) error {
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

	// Remove
	log.Infow("removing container", "name", name)
	if err := p.deps.Runtime.RemoveContainer(ctx, ct.ID); err != nil {
		return fmt.Errorf("removing container %s: %w", name, err)
	}

	// Recreate with new tag, preserving config
	spec := runtime.ContainerSpec{
		Name:        ct.Name,
		Tag:         newTag,
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

	log.Infow("creating container with new tag", "name", name, "tag", newTag)
	if err := p.deps.Runtime.CreateContainer(ctx, spec); err != nil {
		return fmt.Errorf("creating container %s: %w", name, err)
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

	log.Infow("starting container", "name", name)
	if err := p.deps.Runtime.StartContainer(ctx, newCt.ID); err != nil {
		return fmt.Errorf("starting container %s: %w", name, err)
	}

	return nil
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

		// Register in network zone
		if regErr := p.deps.NetworkMgr.RegisterDNS(ctx, networkName, a.hostname, ip); regErr != nil {
			log.Warnw("failed to register DNS alias", "alias", a.hostname, "ip", ip, "error", regErr)
		} else {
			log.Infow("DNS alias registered", "alias", a.hostname, "container", a.containerName, "ip", ip)
		}

		// Register in namespace zone
		if nsZoneID != "" && dnsClient != nil {
			if regErr := dnsClient.RegisterHost(ctx, nsEndpoint, nsZoneID, a.hostname, ip, 60); regErr != nil {
				log.Warnw("failed to register DNS alias in namespace zone", "alias", a.hostname, "error", regErr)
			}
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

		resp, err := http.DefaultClient.Do(req)
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
	return fmt.Sprintf("veth_%s_%s_%d", truncate(ns, 8), truncate(pod.Name, 8), index)
}

func truncate(s string, max int) string {
	if len(s) > max {
		return s[:max]
	}
	return s
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
func extractProbes(c corev1.Container) *lifecycle.ProbeSet {
	ps := &lifecycle.ProbeSet{
		Startup:   probeToConfig(c.StartupProbe),
		Liveness:  probeToConfig(c.LivenessProbe),
		Readiness: probeToConfig(c.ReadinessProbe),
	}
	if ps.Startup == nil && ps.Liveness == nil && ps.Readiness == nil {
		return nil
	}
	return ps
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
		LastTimestamp:   now,
		Count:          1,
		Source:         corev1.EventSource{Component: "mkube", Host: p.nodeName},
	}
	p.events = append(p.events, evt)
	if len(p.events) > maxEvents {
		p.events = p.events[len(p.events)-maxEvents:]
	}
}
