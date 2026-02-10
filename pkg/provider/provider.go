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

	"github.com/glenneth/microkube/pkg/config"
	"github.com/glenneth/microkube/pkg/lifecycle"
	"github.com/glenneth/microkube/pkg/network"
	"github.com/glenneth/microkube/pkg/routeros"
	"github.com/glenneth/microkube/pkg/storage"
)

const (
	// annotationNetwork selects which network a pod's containers are placed on.
	annotationNetwork = "vkube.io/network"
	// annotationFile specifies a local tarball path on the host, bypassing OCI pull.
	annotationFile = "vkube.io/file"
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

// MicroKubeProvider implements the Virtual Kubelet provider interface.
// It translates Kubernetes Pod specifications into RouterOS
// container operations, managing the full lifecycle including networking,
// storage, and boot ordering.
type MicroKubeProvider struct {
	deps            Deps
	nodeName        string
	startTime       time.Time
	pods            map[string]*corev1.Pod // namespace/name -> pod
	notifyPodStatus func(*corev1.Pod)      // callback for pod status updates
}

// NewMicroKubeProvider creates a new provider instance.
func NewMicroKubeProvider(deps Deps) (*MicroKubeProvider, error) {
	return &MicroKubeProvider{
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
func (p *MicroKubeProvider) CreatePod(ctx context.Context, pod *corev1.Pod) error {
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
func (p *MicroKubeProvider) GetPods(ctx context.Context) ([]*corev1.Pod, error) {
	pods := make([]*corev1.Pod, 0, len(p.pods))
	for _, pod := range p.pods {
		pods = append(pods, pod)
	}
	return pods, nil
}

// ─── NodeProvider Interface ─────────────────────────────────────────────────

// ConfigureNode sets up the Kubernetes node object that represents this
// MikroTik device in the cluster.
func (p *MicroKubeProvider) ConfigureNode(ctx context.Context, node *corev1.Node) {
	node.Status.Capacity = corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("4"),     // typical CHR/RB capacity
		corev1.ResourceMemory: resource.MustParse("1Gi"),
		corev1.ResourcePods:   resource.MustParse("20"),
	}
	node.Status.Allocatable = node.Status.Capacity
	node.Status.NodeInfo = corev1.NodeSystemInfo{
		Architecture:    "arm64",
		OperatingSystem: "linux",
		KubeletVersion:  "v1.29.0-microkube",
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
		fmt.Fprintf(w, `{"status":"ok"}`)
	})

	srv := &http.Server{Addr: listenAddr, Handler: mux}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
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
	ct, err := p.deps.ROS.GetContainer(ctx, name)
	if err != nil {
		return fmt.Errorf("getting container %s: %w", name, err)
	}

	// Stop if running
	if ct.IsRunning() {
		log.Infow("stopping container", "name", name)
		if err := p.deps.ROS.StopContainer(ctx, ct.ID); err != nil {
			return fmt.Errorf("stopping container %s: %w", name, err)
		}
		// Wait for stopped state
		for i := 0; i < 30; i++ {
			time.Sleep(time.Second)
			ct, err = p.deps.ROS.GetContainer(ctx, name)
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
	if err := p.deps.ROS.RemoveContainer(ctx, ct.ID); err != nil {
		return fmt.Errorf("removing container %s: %w", name, err)
	}

	// Recreate with new tag, preserving config
	spec := routeros.ContainerSpec{
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
	if err := p.deps.ROS.CreateContainer(ctx, spec); err != nil {
		return fmt.Errorf("creating container %s: %w", name, err)
	}

	// Wait for extraction then start
	var newCt *routeros.Container
	for i := 0; i < 60; i++ {
		time.Sleep(time.Second)
		newCt, err = p.deps.ROS.GetContainer(ctx, name)
		if err == nil {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("waiting for container %s after create: %w", name, err)
	}

	log.Infow("starting container", "name", name)
	if err := p.deps.ROS.StartContainer(ctx, newCt.ID); err != nil {
		return fmt.Errorf("starting container %s: %w", name, err)
	}

	return nil
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
