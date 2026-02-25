package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// annotationOwnerDeployment marks a pod as owned by a deployment.
	annotationOwnerDeployment = "vkube.io/owner-deployment"
)

// ─── Types ──────────────────────────────────────────────────────────────────

// Deployment is a lightweight deployment controller that maintains a desired
// number of pod replicas. Unlike k8s Deployments, it manages pods directly
// without ReplicaSets (single-node architecture).
type Deployment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Spec              DeploymentSpec   `json:"spec"`
	Status            DeploymentStatus `json:"status,omitempty"`
}

// DeploymentSpec defines the desired state for a deployment.
type DeploymentSpec struct {
	Replicas int32                  `json:"replicas"`
	Template corev1.PodTemplateSpec `json:"template"`
}

// DeploymentStatus tracks the observed state of a deployment.
type DeploymentStatus struct {
	Replicas      int32  `json:"replicas"`
	ReadyReplicas int32  `json:"readyReplicas"`
	UpdatedAt     string `json:"updatedAt,omitempty"`
}

// DeploymentList is a list of deployments.
type DeploymentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`
	Items           []Deployment `json:"items"`
}

// DeepCopy returns a deep copy of a Deployment.
func (d *Deployment) DeepCopy() *Deployment {
	out := *d
	out.ObjectMeta = *d.ObjectMeta.DeepCopy()
	// Deep copy template annotations and labels
	if d.Spec.Template.Annotations != nil {
		out.Spec.Template.Annotations = make(map[string]string, len(d.Spec.Template.Annotations))
		for k, v := range d.Spec.Template.Annotations {
			out.Spec.Template.Annotations[k] = v
		}
	}
	if d.Spec.Template.Labels != nil {
		out.Spec.Template.Labels = make(map[string]string, len(d.Spec.Template.Labels))
		for k, v := range d.Spec.Template.Labels {
			out.Spec.Template.Labels[k] = v
		}
	}
	// Deep copy containers
	if len(d.Spec.Template.Spec.Containers) > 0 {
		out.Spec.Template.Spec.Containers = make([]corev1.Container, len(d.Spec.Template.Spec.Containers))
		for i, c := range d.Spec.Template.Spec.Containers {
			out.Spec.Template.Spec.Containers[i] = *c.DeepCopy()
		}
	}
	if len(d.Spec.Template.Spec.Volumes) > 0 {
		out.Spec.Template.Spec.Volumes = make([]corev1.Volume, len(d.Spec.Template.Spec.Volumes))
		for i, v := range d.Spec.Template.Spec.Volumes {
			out.Spec.Template.Spec.Volumes[i] = *v.DeepCopy()
		}
	}
	return &out
}

// ─── CRUD Handlers ──────────────────────────────────────────────────────────

func (p *MicroKubeProvider) handleCreateDeployment(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")

	var deploy Deployment
	if err := json.NewDecoder(r.Body).Decode(&deploy); err != nil {
		http.Error(w, fmt.Sprintf("invalid Deployment JSON: %v", err), http.StatusBadRequest)
		return
	}
	deploy.Namespace = ns
	if deploy.Name == "" {
		http.Error(w, "Deployment name is required", http.StatusBadRequest)
		return
	}
	deploy.TypeMeta = metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"}
	if deploy.CreationTimestamp.IsZero() {
		deploy.CreationTimestamp = metav1.Now()
	}
	if deploy.Spec.Replicas <= 0 {
		deploy.Spec.Replicas = 1
	}

	key := ns + "/" + deploy.Name
	if _, exists := p.deployments[key]; exists {
		http.Error(w, fmt.Sprintf("Deployment %s already exists", key), http.StatusConflict)
		return
	}

	// Persist to NATS
	if p.deps.Store != nil && p.deps.Store.Deployments != nil {
		storeKey := ns + "." + deploy.Name
		if _, err := p.deps.Store.Deployments.PutJSON(r.Context(), storeKey, &deploy); err != nil {
			http.Error(w, fmt.Sprintf("persisting deployment: %v", err), http.StatusInternalServerError)
			return
		}
	}

	deploy.Status = DeploymentStatus{
		Replicas:  0,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	p.deployments[key] = &deploy
	p.deps.Logger.Infow("deployment created", "deployment", key, "replicas", deploy.Spec.Replicas)

	podWriteJSON(w, http.StatusCreated, &deploy)
}

func (p *MicroKubeProvider) handleGetDeployment(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name")
	key := ns + "/" + name

	deploy, ok := p.deployments[key]
	if !ok {
		http.Error(w, fmt.Sprintf("Deployment %s not found", key), http.StatusNotFound)
		return
	}

	out := deploy.DeepCopy()
	p.enrichDeploymentStatus(out)
	podWriteJSON(w, http.StatusOK, out)
}

func (p *MicroKubeProvider) handleListAllDeployments(w http.ResponseWriter, r *http.Request) {
	items := make([]Deployment, 0, len(p.deployments))
	for _, deploy := range p.deployments {
		out := deploy.DeepCopy()
		p.enrichDeploymentStatus(out)
		items = append(items, *out)
	}

	podWriteJSON(w, http.StatusOK, DeploymentList{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DeploymentList"},
		Items:    items,
	})
}

func (p *MicroKubeProvider) handleListNamespacedDeployments(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	items := make([]Deployment, 0)
	for _, deploy := range p.deployments {
		if deploy.Namespace != ns {
			continue
		}
		out := deploy.DeepCopy()
		p.enrichDeploymentStatus(out)
		items = append(items, *out)
	}

	podWriteJSON(w, http.StatusOK, DeploymentList{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DeploymentList"},
		Items:    items,
	})
}

func (p *MicroKubeProvider) handleUpdateDeployment(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name")
	key := ns + "/" + name

	if _, ok := p.deployments[key]; !ok {
		http.Error(w, fmt.Sprintf("Deployment %s not found", key), http.StatusNotFound)
		return
	}

	var deploy Deployment
	if err := json.NewDecoder(r.Body).Decode(&deploy); err != nil {
		http.Error(w, fmt.Sprintf("invalid Deployment JSON: %v", err), http.StatusBadRequest)
		return
	}
	deploy.Namespace = ns
	deploy.Name = name
	deploy.TypeMeta = metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"}
	if deploy.Spec.Replicas <= 0 {
		deploy.Spec.Replicas = 1
	}

	// Persist to NATS
	if p.deps.Store != nil && p.deps.Store.Deployments != nil {
		storeKey := ns + "." + name
		if _, err := p.deps.Store.Deployments.PutJSON(r.Context(), storeKey, &deploy); err != nil {
			http.Error(w, fmt.Sprintf("persisting deployment update: %v", err), http.StatusInternalServerError)
			return
		}
	}

	deploy.Status.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	p.deployments[key] = &deploy
	p.deps.Logger.Infow("deployment updated", "deployment", key, "replicas", deploy.Spec.Replicas)

	p.enrichDeploymentStatus(&deploy)
	podWriteJSON(w, http.StatusOK, &deploy)
}

func (p *MicroKubeProvider) handleDeleteDeployment(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name")
	key := ns + "/" + name

	deploy, ok := p.deployments[key]
	if !ok {
		http.Error(w, fmt.Sprintf("Deployment %s not found", key), http.StatusNotFound)
		return
	}

	// Delete all owned pods
	ownedPods := p.deploymentPods(deploy)
	for _, pod := range ownedPods {
		podKey := pod.Namespace + "/" + pod.Name
		p.deps.Logger.Infow("deleting deployment-owned pod", "deployment", key, "pod", podKey)
		if err := p.DeletePod(r.Context(), pod); err != nil {
			p.deps.Logger.Warnw("failed to delete deployment-owned pod", "pod", podKey, "error", err)
		}
		// Remove from NATS Pods bucket
		if p.deps.Store != nil {
			storeKey := pod.Namespace + "." + pod.Name
			if err := p.deps.Store.Pods.Delete(r.Context(), storeKey); err != nil {
				p.deps.Logger.Warnw("failed to delete pod from store", "key", storeKey, "error", err)
			}
		}
	}

	// Remove deployment from NATS
	if p.deps.Store != nil && p.deps.Store.Deployments != nil {
		storeKey := ns + "." + name
		if err := p.deps.Store.Deployments.Delete(r.Context(), storeKey); err != nil {
			p.deps.Logger.Warnw("failed to delete deployment from store", "key", storeKey, "error", err)
		}
	}

	delete(p.deployments, key)
	p.deps.Logger.Infow("deployment deleted", "deployment", key, "podsRemoved", len(ownedPods))

	podWriteJSON(w, http.StatusOK, metav1.Status{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
		Status:   "Success",
		Message:  fmt.Sprintf("deployment %q deleted (%d pods removed)", name, len(ownedPods)),
	})
}

// ─── Reconciliation ─────────────────────────────────────────────────────────

// reconcileDeployments ensures each deployment has the correct number of running pods.
// Called at the start of every reconcile cycle.
func (p *MicroKubeProvider) reconcileDeployments(ctx context.Context) {
	log := p.deps.Logger

	// Load deployments from store if we haven't yet
	if p.deps.Store != nil && p.deps.Store.Connected() && p.deps.Store.Deployments != nil {
		keys, err := p.deps.Store.Deployments.Keys(ctx, "")
		if err != nil {
			log.Warnw("failed to list deployments from store", "error", err)
		} else {
			for _, key := range keys {
				var deploy Deployment
				if _, err := p.deps.Store.Deployments.GetJSON(ctx, key, &deploy); err != nil {
					log.Warnw("failed to read deployment from store", "key", key, "error", err)
					continue
				}
				mapKey := deploy.Namespace + "/" + deploy.Name
				if _, exists := p.deployments[mapKey]; !exists {
					p.deployments[mapKey] = &deploy
					log.Infow("loaded deployment from store", "deployment", mapKey)
				}
			}
		}
	}

	for _, deploy := range p.deployments {
		p.reconcileOneDeployment(ctx, deploy)
	}
}

func (p *MicroKubeProvider) reconcileOneDeployment(ctx context.Context, deploy *Deployment) {
	log := p.deps.Logger
	deployKey := deploy.Namespace + "/" + deploy.Name
	replicas := deploy.Spec.Replicas
	if replicas <= 0 {
		replicas = 1
	}

	// Compute desired pod names: {deploy.Name}-0 .. {deploy.Name}-(replicas-1)
	desiredNames := make(map[string]bool, replicas)
	for i := int32(0); i < replicas; i++ {
		desiredNames[fmt.Sprintf("%s-%d", deploy.Name, i)] = true
	}

	// Find existing owned pods
	ownedPods := p.deploymentPods(deploy)
	ownedByName := make(map[string]*corev1.Pod, len(ownedPods))
	for _, pod := range ownedPods {
		ownedByName[pod.Name] = pod
	}

	// Create missing pods
	for name := range desiredNames {
		if _, exists := ownedByName[name]; exists {
			continue
		}
		log.Infow("creating deployment pod", "deployment", deployKey, "pod", name)
		pod := p.podFromDeployment(deploy, name)

		// Persist to NATS Pods bucket
		if p.deps.Store != nil {
			storeKey := pod.Namespace + "." + pod.Name
			if _, err := p.deps.Store.Pods.PutJSON(ctx, storeKey, pod); err != nil {
				log.Errorw("failed to persist deployment pod to store", "pod", name, "error", err)
				continue
			}
		}

		if err := p.CreatePod(ctx, pod); err != nil {
			log.Errorw("failed to create deployment pod", "deployment", deployKey, "pod", name, "error", err)
			p.recordEvent(pod, "CreateFailed", fmt.Sprintf("Failed to create deployment pod: %v", err), "Warning")
		}
	}

	// Delete excess pods (scale down) — remove highest index first
	var extras []*corev1.Pod
	for _, pod := range ownedPods {
		if !desiredNames[pod.Name] {
			extras = append(extras, pod)
		}
	}
	// Sort by name descending to remove highest index first
	sort.Slice(extras, func(i, j int) bool {
		return extras[i].Name > extras[j].Name
	})
	for _, pod := range extras {
		podKey := pod.Namespace + "/" + pod.Name
		log.Infow("removing excess deployment pod", "deployment", deployKey, "pod", podKey)
		if err := p.DeletePod(ctx, pod); err != nil {
			log.Errorw("failed to delete excess pod", "pod", podKey, "error", err)
		}
		if p.deps.Store != nil {
			storeKey := pod.Namespace + "." + pod.Name
			_ = p.deps.Store.Pods.Delete(ctx, storeKey)
		}
	}

	// Image update check (rolling update) for auto-update deployments
	if deploy.Spec.Template.Annotations[annotationImagePolicy] == "auto" {
		for _, c := range deploy.Spec.Template.Spec.Containers {
			_, changed, err := p.deps.StorageMgr.RefreshImage(ctx, c.Image)
			if err != nil {
				log.Debugw("deployment image check failed", "deployment", deployKey, "image", c.Image, "error", err)
			} else if changed {
				log.Infow("new image detected for deployment, rolling update", "deployment", deployKey, "image", c.Image)
				p.rollingUpdateDeployment(ctx, deploy)
				break
			}
		}
	}

	// Update status
	deploy.Status.Replicas = int32(len(p.deploymentPods(deploy)))
	deploy.Status.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
}

// rollingUpdateDeployment performs a rolling update of deployment pods one at a time.
// Each pod is verified alive before proceeding to the next.
func (p *MicroKubeProvider) rollingUpdateDeployment(ctx context.Context, deploy *Deployment) {
	log := p.deps.Logger
	deployKey := deploy.Namespace + "/" + deploy.Name

	ownedPods := p.deploymentPods(deploy)
	for i, pod := range ownedPods {
		podKey := pod.Namespace + "/" + pod.Name
		log.Infow("rolling update pod",
			"deployment", deployKey, "pod", podKey,
			"index", i+1, "total", len(ownedPods))

		// Rebuild pod from current template to pick up new image
		newPod := p.podFromDeployment(deploy, pod.Name)
		if err := p.UpdatePod(ctx, newPod); err != nil {
			log.Errorw("rolling update failed for pod", "pod", podKey, "error", err)
			return // stop rolling on first failure
		}
		// Wait for liveness before proceeding to next replica
		if i < len(ownedPods)-1 {
			if !p.waitForPodLiveness(ctx, newPod, 60*time.Second) {
				log.Errorw("pod failed liveness during rolling update, halting",
					"deployment", deployKey, "pod", podKey)
				return
			}
		}
	}
}

// podFromDeployment generates a pod spec from a deployment template.
func (p *MicroKubeProvider) podFromDeployment(deploy *Deployment, podName string) *corev1.Pod {
	pod := &corev1.Pod{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{
			Name:              podName,
			Namespace:         deploy.Namespace,
			CreationTimestamp: metav1.Now(),
			Annotations:       make(map[string]string),
			Labels:            make(map[string]string),
		},
		Spec: *deploy.Spec.Template.Spec.DeepCopy(),
	}

	// Copy template annotations
	for k, v := range deploy.Spec.Template.Annotations {
		pod.Annotations[k] = v
	}
	// Copy template labels
	for k, v := range deploy.Spec.Template.Labels {
		pod.Labels[k] = v
	}

	// Set owner annotation
	pod.Annotations[annotationOwnerDeployment] = deploy.Name

	// Remove static-ip annotation for multi-replica deployments —
	// each replica needs a unique IP from IPAM.
	if deploy.Spec.Replicas > 1 {
		delete(pod.Annotations, annotationStaticIP)
	}

	return pod
}

// deploymentPods returns all pods owned by the given deployment.
func (p *MicroKubeProvider) deploymentPods(deploy *Deployment) []*corev1.Pod {
	var pods []*corev1.Pod
	for _, pod := range p.pods {
		if pod.Namespace != deploy.Namespace {
			continue
		}
		if pod.Annotations[annotationOwnerDeployment] == deploy.Name {
			pods = append(pods, pod)
		}
	}
	// Sort by name for deterministic ordering
	sort.Slice(pods, func(i, j int) bool {
		return pods[i].Name < pods[j].Name
	})
	return pods
}

// isOwnedByDeployment checks if a pod is owned by a deployment.
func isOwnedByDeployment(pod *corev1.Pod) bool {
	if pod.Annotations == nil {
		return false
	}
	return pod.Annotations[annotationOwnerDeployment] != ""
}

// enrichDeploymentStatus updates the deployment status with live pod counts.
func (p *MicroKubeProvider) enrichDeploymentStatus(deploy *Deployment) {
	ownedPods := p.deploymentPods(deploy)
	deploy.Status.Replicas = int32(len(ownedPods))

	var ready int32
	for _, pod := range ownedPods {
		// Check if all containers in the pod exist on RouterOS
		allRunning := true
		for _, c := range pod.Spec.Containers {
			name := sanitizeName(pod, c.Name)
			_ = name
			// Simple check: if the pod is tracked, count it as ready
			if _, ok := p.pods[pod.Namespace+"/"+pod.Name]; !ok {
				allRunning = false
				break
			}
		}
		if allRunning {
			ready++
		}
	}
	deploy.Status.ReadyReplicas = ready
}

// deploymentExpectedContainers returns all container names that should exist
// for all deployments. Used by orphan detection to avoid cleaning up
// deployment-owned containers.
func (p *MicroKubeProvider) deploymentExpectedContainers() map[string]bool {
	expected := make(map[string]bool)
	for _, deploy := range p.deployments {
		replicas := deploy.Spec.Replicas
		if replicas <= 0 {
			replicas = 1
		}
		for i := int32(0); i < replicas; i++ {
			podName := fmt.Sprintf("%s-%d", deploy.Name, i)
			fakePod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      podName,
					Namespace: deploy.Namespace,
				},
			}
			for _, c := range deploy.Spec.Template.Spec.Containers {
				expected[sanitizeName(fakePod, c.Name)] = true
			}
		}
	}
	return expected
}

// LoadDeploymentsFromStore loads all deployments from the NATS store into memory.
func (p *MicroKubeProvider) LoadDeploymentsFromStore(ctx context.Context) {
	if p.deps.Store == nil || p.deps.Store.Deployments == nil {
		return
	}

	keys, err := p.deps.Store.Deployments.Keys(ctx, "")
	if err != nil {
		p.deps.Logger.Warnw("failed to list deployments from store", "error", err)
		return
	}

	for _, key := range keys {
		var deploy Deployment
		if _, err := p.deps.Store.Deployments.GetJSON(ctx, key, &deploy); err != nil {
			p.deps.Logger.Warnw("failed to read deployment from store", "key", key, "error", err)
			continue
		}
		parts := strings.SplitN(key, ".", 2)
		if len(parts) == 2 {
			deploy.Namespace = parts[0]
			deploy.Name = parts[1]
		}
		mapKey := deploy.Namespace + "/" + deploy.Name
		p.deployments[mapKey] = &deploy
		p.deps.Logger.Infow("loaded deployment from store", "deployment", mapKey)
	}
}
