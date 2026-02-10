package provider

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
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

	"github.com/glenneth/mikrotik-kube/pkg/config"
	"github.com/glenneth/mikrotik-kube/pkg/network"
	"github.com/glenneth/mikrotik-kube/pkg/routeros"
	"github.com/glenneth/mikrotik-kube/pkg/storage"
	"github.com/glenneth/mikrotik-kube/pkg/lifecycle"
)

const (
	// annotationNetwork selects which network a pod's containers are placed on.
	annotationNetwork = "vkube.io/network"
	// annotationFile specifies a local tarball path on the host, bypassing OCI pull.
	annotationFile = "vkube.io/file"
	// annotationImagePolicy controls automatic redeployment when an image is
	// pushed to the embedded registry. Values: "auto" (redeploy on push).
	annotationImagePolicy = "vkube.io/image-policy"
)

// Deps holds injected dependencies for the provider.
type Deps struct {
	Config     *config.Config
	ROS        *routeros.Client
	NetworkMgr *network.Manager
	StorageMgr *storage.Manager
	LifecycleMgr *lifecycle.Manager
	Logger     *zap.SugaredLogger
}

// MikroTikProvider implements the Virtual Kubelet provider interface.
// It translates Kubernetes Pod specifications into MikroTik RouterOS
// container operations, managing the full lifecycle including networking,
// storage, and boot ordering.
type MikroTikProvider struct {
	deps            Deps
	nodeName        string
	startTime       time.Time
	pods            map[string]*corev1.Pod // namespace/name -> pod
	notifyPodStatus func(*corev1.Pod)      // callback for pod status updates
}

// NewMikroTikProvider creates a new provider instance.
func NewMikroTikProvider(deps Deps) (*MikroTikProvider, error) {
	return &MikroTikProvider{
		deps:      deps,
		nodeName:  deps.Config.NodeName,
		startTime: time.Now(),
		pods:      make(map[string]*corev1.Pod),
	}, nil
}

// ─── PodLifecycleHandler Interface ──────────────────────────────────────────

// CreatePod takes a Kubernetes Pod spec and creates the corresponding
// RouterOS container(s). This includes:
//  1. Pulling/caching the image as an OCI tarball
//  2. Allocating a veth interface and IP address
//  3. Creating volume mounts
//  4. Registering boot ordering if restartPolicy=Always
//  5. Creating and starting the RouterOS container
func (p *MikroTikProvider) CreatePod(ctx context.Context, pod *corev1.Pod) error {
	log := p.deps.Logger.With("pod", podKey(pod))
	log.Infow("creating pod")

	// Determine target network from annotation
	networkName := pod.Annotations[annotationNetwork]

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

		// 2. Allocate network (with DNS registration)
		vethName := fmt.Sprintf("veth-%s-%d", truncate(pod.Name, 8), i)
		ip, gw, dnsServer, err := p.deps.NetworkMgr.AllocateInterface(ctx, vethName, pod.Name, networkName)
		if err != nil {
			return fmt.Errorf("allocating network for %s: %w", name, err)
		}
		log.Infow("allocated network", "veth", vethName, "ip", ip, "gateway", gw, "dns", dnsServer)

		// 3. Provision volumes (mount lists are managed via RouterOS mounts API)
		for _, vm := range container.VolumeMounts {
			if _, err := p.deps.StorageMgr.ProvisionVolume(ctx, name, vm.Name, vm.MountPath); err != nil {
				return fmt.Errorf("provisioning volume %s: %w", vm.Name, err)
			}
		}

		// 4. Determine boot behavior
		startOnBoot := "false"
		if pod.Spec.RestartPolicy == corev1.RestartPolicyAlways {
			startOnBoot = "true"
		}

		// 6. Create the RouterOS container
		spec := routeros.ContainerSpec{
			Name:        name,
			File:        tarballPath,
			Interface:   vethName,
			RootDir:     fmt.Sprintf("%s/%s", p.deps.Config.Storage.BasePath, name),
			Cmd:         strings.Join(container.Command, " "),
			Hostname:    pod.Name,
			DNS:         dnsServer,
			Logging:     "true",
			StartOnBoot: startOnBoot,
		}

		if err := p.deps.ROS.CreateContainer(ctx, spec); err != nil {
			return fmt.Errorf("creating container %s: %w", name, err)
		}

		// 7. Start the container
		ct, err := p.deps.ROS.GetContainer(ctx, name)
		if err != nil {
			return fmt.Errorf("getting created container %s: %w", name, err)
		}
		if err := p.deps.ROS.StartContainer(ctx, ct.ID); err != nil {
			return fmt.Errorf("starting container %s: %w", name, err)
		}

		// 8. Register with lifecycle manager for boot ordering / health probes
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

	// Track the pod
	p.pods[podKey(pod)] = pod.DeepCopy()

	return nil
}

// UpdatePod handles pod spec updates. RouterOS containers are immutable,
// so this performs a rolling update: create new → verify → remove old.
func (p *MikroTikProvider) UpdatePod(ctx context.Context, pod *corev1.Pod) error {
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
func (p *MikroTikProvider) DeletePod(ctx context.Context, pod *corev1.Pod) error {
	log := p.deps.Logger.With("pod", podKey(pod))
	log.Infow("deleting pod")

	for i, container := range pod.Spec.Containers {
		name := sanitizeName(pod, container.Name)

		// Stop and remove the container
		ct, err := p.deps.ROS.GetContainer(ctx, name)
		if err != nil {
			log.Warnw("container not found during delete", "name", name, "error", err)
			continue
		}

		if ct.IsRunning() {
			if err := p.deps.ROS.StopContainer(ctx, ct.ID); err != nil {
				log.Warnw("error stopping container", "name", name, "error", err)
			}
		}

		if err := p.deps.ROS.RemoveContainer(ctx, ct.ID); err != nil {
			log.Warnw("error removing container", "name", name, "error", err)
		}

		// Release network resources
		vethName := fmt.Sprintf("veth-%s-%d", truncate(pod.Name, 8), i)
		if err := p.deps.NetworkMgr.ReleaseInterface(ctx, vethName); err != nil {
			log.Warnw("error releasing network", "veth", vethName, "error", err)
		}

		// Unregister from systemd manager
		p.deps.LifecycleMgr.Unregister(name)

		// Note: storage cleanup is deferred to GC
		log.Infow("container removed", "name", name)
	}

	delete(p.pods, podKey(pod))
	return nil
}

// GetPod returns the tracked pod object.
func (p *MikroTikProvider) GetPod(ctx context.Context, namespace, name string) (*corev1.Pod, error) {
	key := namespace + "/" + name
	if pod, ok := p.pods[key]; ok {
		return pod, nil
	}
	return nil, fmt.Errorf("pod %s not found", key)
}

// GetPodStatus queries RouterOS for the actual container status and maps
// it back to Kubernetes pod status.
func (p *MikroTikProvider) GetPodStatus(ctx context.Context, namespace, name string) (*corev1.PodStatus, error) {
	pod, err := p.GetPod(ctx, namespace, name)
	if err != nil {
		return nil, err
	}

	var containerStatuses []corev1.ContainerStatus
	allRunning := true

	for _, container := range pod.Spec.Containers {
		rosName := sanitizeName(pod, container.Name)
		ct, err := p.deps.ROS.GetContainer(ctx, rosName)

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

	return &corev1.PodStatus{
		Phase:             phase,
		ContainerStatuses: containerStatuses,
		StartTime:         &metav1.Time{Time: p.startTime},
		HostIP:            p.deps.Config.DefaultNetwork().Gateway,
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
	}, nil
}

// GetPods returns all tracked pods.
func (p *MikroTikProvider) GetPods(ctx context.Context) ([]*corev1.Pod, error) {
	pods := make([]*corev1.Pod, 0, len(p.pods))
	for _, pod := range p.pods {
		pods = append(pods, pod)
	}
	return pods, nil
}

// ─── NodeProvider Interface ─────────────────────────────────────────────────

// ConfigureNode sets up the Kubernetes node object that represents this
// MikroTik device in the cluster.
func (p *MikroTikProvider) ConfigureNode(ctx context.Context, node *corev1.Node) {
	node.Status.Capacity = corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("4"),     // typical CHR/RB capacity
		corev1.ResourceMemory: resource.MustParse("1Gi"),
		corev1.ResourcePods:   resource.MustParse("20"),
	}
	node.Status.Allocatable = node.Status.Capacity
	node.Status.NodeInfo = corev1.NodeSystemInfo{
		Architecture:    "arm64",
		OperatingSystem: "linux",
		KubeletVersion:  "v1.29.0-mikrotik-kube",
	}
	node.Status.Conditions = []corev1.NodeCondition{
		{
			Type:   corev1.NodeReady,
			Status: corev1.ConditionTrue,
		},
	}
	node.Labels = map[string]string{
		"type":                     "virtual-kubelet",
		"kubernetes.io/os":         "linux",
		"kubernetes.io/arch":       "arm64",
		"node.kubernetes.io/role":  "mikrotik",
		"mikrotik.io/device-type":  "routeros",
	}

	// Add taint so normal pods aren't scheduled here
	node.Spec.Taints = []corev1.Taint{
		{
			Key:    "virtual-kubelet.io/provider",
			Value:  "mikrotik",
			Effect: corev1.TaintEffectNoSchedule,
		},
	}
}

// ─── Standalone Reconciler ──────────────────────────────────────────────────

// RunStandaloneReconciler runs a local reconciliation loop without requiring
// a Kubernetes API server. Reads desired state from a local YAML file and
// reconciles against actual RouterOS container state.
func (p *MikroTikProvider) RunStandaloneReconciler(ctx context.Context) error {
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

func (p *MikroTikProvider) reconcile(ctx context.Context) error {
	log := p.deps.Logger

	// 1. Load desired pods from boot manifest
	desiredPods, err := loadPodManifests(p.deps.Config.Lifecycle.BootManifestPath)
	if err != nil {
		return fmt.Errorf("loading pod manifests: %w", err)
	}

	// 2. List actual containers on RouterOS
	actual, err := p.deps.ROS.ListContainers(ctx)
	if err != nil {
		return fmt.Errorf("listing containers: %w", err)
	}
	actualByName := make(map[string]routeros.Container, len(actual))
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
		}
	}

	return nil
}

// loadPodManifests reads a multi-document YAML file containing Pod specs.
func loadPodManifests(path string) ([]*corev1.Pod, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading manifest %s: %w", path, err)
	}

	var pods []*corev1.Pod
	reader := yaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	for {
		doc, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading YAML document: %w", err)
		}

		doc = bytes.TrimSpace(doc)
		if len(doc) == 0 {
			continue
		}

		var pod corev1.Pod
		if err := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(doc), 4096).Decode(&pod); err != nil {
			return nil, fmt.Errorf("decoding pod: %w", err)
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

	return pods, nil
}

// NotifyPods is called by the Virtual Kubelet framework to set up a callback
// for pod status updates. The provider calls this function whenever a pod's
// status changes so the framework can update the API server.
func (p *MikroTikProvider) NotifyPods(ctx context.Context, cb func(*corev1.Pod)) {
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
func (p *MikroTikProvider) RunVirtualKubelet(ctx context.Context) error {
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
func (p *MikroTikProvider) runNodeHeartbeat(ctx context.Context, clientset kubernetes.Interface, nodeName string) {
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
			if _, err := clientset.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{}); err != nil {
				p.deps.Logger.Warnw("heartbeat: failed to update", "error", err)
			}
		}
	}
}

// watchPods uses the Kubernetes API to watch for pod events targeting this node
// and dispatches create/update/delete operations.
func (p *MikroTikProvider) watchPods(ctx context.Context, clientset kubernetes.Interface, nodeName string) error {
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

// ─── Auto-Update (CI/CD) ────────────────────────────────────────────────

// PushEvent mirrors registry.PushEvent to avoid a circular import.
type PushEvent struct {
	Repo      string
	Reference string
}

// RunAutoUpdater watches for image push events and redeploys pods that
// have vkube.io/image-policy: auto and reference the pushed image.
// Multiple pods sharing the same image (e.g., 4 MicroDNS instances) are
// all updated, one at a time with a delay between them.
func (p *MikroTikProvider) RunAutoUpdater(ctx context.Context, events <-chan PushEvent) {
	log := p.deps.Logger.Named("auto-update")
	log.Info("auto-updater watching for push events")

	for {
		select {
		case <-ctx.Done():
			log.Info("auto-updater shutting down")
			return
		case ev := <-events:
			log.Infow("push event received", "repo", ev.Repo, "ref", ev.Reference)
			p.handlePushEvent(ctx, ev, log)
		}
	}
}

func (p *MikroTikProvider) handlePushEvent(ctx context.Context, ev PushEvent, log *zap.SugaredLogger) {
	// Find all tracked pods with auto-update enabled whose containers
	// reference the pushed repo:tag
	var toUpdate []*corev1.Pod
	for _, pod := range p.pods {
		if pod.Annotations[annotationImagePolicy] != "auto" {
			continue
		}
		// A pod using vkube.io/file bypasses the registry — skip it
		if pod.Annotations[annotationFile] != "" {
			continue
		}
		for _, c := range pod.Spec.Containers {
			if imageMatchesPush(c.Image, ev.Repo, ev.Reference) {
				toUpdate = append(toUpdate, pod)
				break // only need to match one container per pod
			}
		}
	}

	if len(toUpdate) == 0 {
		log.Debugw("no auto-update pods match push", "repo", ev.Repo, "ref", ev.Reference)
		return
	}

	log.Infow("auto-updating pods",
		"repo", ev.Repo, "ref", ev.Reference, "count", len(toUpdate))

	// Invalidate the storage manager cache so EnsureImage re-pulls
	for _, pod := range toUpdate {
		for _, c := range pod.Spec.Containers {
			if imageMatchesPush(c.Image, ev.Repo, ev.Reference) {
				p.deps.StorageMgr.ReleaseImage(c.Image)
			}
		}
	}

	// Rolling update: one pod at a time
	for _, pod := range toUpdate {
		log.Infow("auto-updating pod", "pod", podKey(pod))
		if err := p.UpdatePod(ctx, pod); err != nil {
			log.Errorw("auto-update failed", "pod", podKey(pod), "error", err)
			continue
		}
		log.Infow("auto-update complete", "pod", podKey(pod))
		// Brief pause between updates to avoid overwhelming RouterOS
		time.Sleep(5 * time.Second)
	}
}

// imageMatchesPush checks if an image reference matches a push event.
// The push event has repo="microdns" ref="latest".
// The image might be "192.168.200.2:5000/microdns:latest" or
// "localhost:5000/library/microdns:latest" or just "microdns:latest".
//
// We strip the registry host (anything before the first / that contains
// a dot or colon) and compare repo:tag.
func imageMatchesPush(image, pushRepo, pushRef string) bool {
	repo, tag := parseImageRepoTag(image)
	return repo == pushRepo && tag == pushRef
}

// parseImageRepoTag splits an image ref into (repo, tag) with the registry
// host stripped. Examples:
//
//	"192.168.200.2:5000/microdns:latest"   → ("microdns", "latest")
//	"192.168.200.2:5000/lib/app:v2"        → ("lib/app", "v2")
//	"microdns:latest"                       → ("microdns", "latest")
//	"microdns"                              → ("microdns", "latest")
func parseImageRepoTag(image string) (repo, tag string) {
	// Split off tag
	tag = "latest"
	// Find last colon that's after the last slash (to avoid port colons)
	lastSlash := strings.LastIndex(image, "/")
	lastColon := strings.LastIndex(image, ":")
	if lastColon > lastSlash {
		tag = image[lastColon+1:]
		image = image[:lastColon]
	}

	// Strip registry host: the first component is a host if it contains
	// a dot or colon (e.g., "192.168.200.2:5000", "docker.io", "localhost:5000")
	firstSlash := strings.Index(image, "/")
	if firstSlash > 0 {
		firstPart := image[:firstSlash]
		if strings.ContainsAny(firstPart, ".:") {
			image = image[firstSlash+1:]
		}
	}

	return image, tag
}

// ─── Helpers ────────────────────────────────────────────────────────────────

func podKey(pod *corev1.Pod) string {
	return pod.Namespace + "/" + pod.Name
}

// sanitizeName converts a pod/container name pair into a valid RouterOS
// container name (alphanumeric + hyphens, max 32 chars).
func sanitizeName(pod *corev1.Pod, containerName string) string {
	name := fmt.Sprintf("%s-%s", pod.Name, containerName)
	name = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		if r >= 'A' && r <= 'Z' {
			return r + 32 // lowercase
		}
		return '-'
	}, name)
	return truncate(name, 32)
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
