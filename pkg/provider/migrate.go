package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// migrateRequest is the JSON body for POST /api/v1/namespaces/{namespace}/pods/{name}/migrate.
type migrateRequest struct {
	TargetNode string `json:"targetNode"`
}

// handleMigratePod moves a pod from this node to a target node in the cluster.
// The pod is deleted locally and its vkube.io/node annotation is updated in NATS.
// The target node will pick it up on its next reconcile cycle (~10s).
func (p *MicroKubeProvider) handleMigratePod(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name")
	ctx := r.Context()

	var req migrateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON: %v", err), http.StatusBadRequest)
		return
	}
	if req.TargetNode == "" {
		http.Error(w, `"targetNode" is required`, http.StatusBadRequest)
		return
	}

	// Clustering must be enabled
	if p.clusterMgr == nil {
		http.Error(w, "clustering is not enabled", http.StatusBadRequest)
		return
	}

	// Can't migrate to self
	if req.TargetNode == p.nodeName {
		http.Error(w, "cannot migrate pod to the same node", http.StatusBadRequest)
		return
	}

	// Pod must exist and be local
	pod, err := p.GetPod(ctx, ns, name)
	if err != nil {
		http.Error(w, fmt.Sprintf("pod %s/%s not found", ns, name), http.StatusNotFound)
		return
	}
	if !p.isLocalPod(pod) {
		http.Error(w, fmt.Sprintf("pod %s/%s is not on this node", ns, name), http.StatusBadRequest)
		return
	}

	// Target node must be healthy
	if !p.clusterMgr.IsPeerHealthy(req.TargetNode) {
		http.Error(w, fmt.Sprintf("target node %q is not healthy", req.TargetNode), http.StatusBadRequest)
		return
	}

	// Architecture must match
	localArch := p.clusterMgr.Architecture()
	peerArch := p.clusterMgr.PeerArchitecture(req.TargetNode)
	if peerArch == "" {
		http.Error(w, fmt.Sprintf("target node %q architecture is unknown", req.TargetNode), http.StatusBadRequest)
		return
	}
	if localArch != peerArch {
		http.Error(w, fmt.Sprintf("architecture mismatch: source=%s, target=%s", localArch, peerArch), http.StatusBadRequest)
		return
	}

	// Delete pod locally (stops containers, releases veths, cleans up DNS)
	if err := p.DeletePod(ctx, pod); err != nil {
		http.Error(w, fmt.Sprintf("failed to delete pod locally: %v", err), http.StatusInternalServerError)
		return
	}

	// Update the node annotation in NATS so the target node picks it up
	if pod.Annotations == nil {
		pod.Annotations = make(map[string]string)
	}
	pod.Annotations[annotationNode] = req.TargetNode

	if p.deps.Store != nil {
		storeKey := ns + "." + name
		if _, err := p.deps.Store.Pods.PutJSON(ctx, storeKey, pod); err != nil {
			http.Error(w, fmt.Sprintf("failed to update pod in store: %v", err), http.StatusInternalServerError)
			return
		}
	}

	p.deps.Logger.Infow("pod migrated",
		"pod", ns+"/"+name,
		"from", p.nodeName,
		"to", req.TargetNode)

	podWriteJSON(w, http.StatusOK, map[string]string{
		"message": fmt.Sprintf("pod migrating to %s", req.TargetNode),
		"pod":     ns + "/" + name,
		"from":    p.nodeName,
		"to":      req.TargetNode,
	})
}
