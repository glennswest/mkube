package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/glennswest/mkube/pkg/store"
)

// K8sWatchEvent is a Kubernetes-compatible watch event.
type K8sWatchEvent struct {
	Type   string      `json:"type"`   // ADDED, MODIFIED, DELETED
	Object interface{} `json:"object"` // the resource
}

// handleWatchPods streams pod events as newline-delimited JSON (K8s watch format).
func (p *MicroKubeProvider) handleWatchPods(w http.ResponseWriter, r *http.Request, nsFilter string) {
	if p.deps.Store == nil {
		http.Error(w, "watch requires NATS store", http.StatusServiceUnavailable)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Transfer-Encoding", "chunked")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ctx := r.Context()

	// Send existing pods as ADDED events first
	pods, _ := p.GetPods(ctx)
	enc := json.NewEncoder(w)
	for _, pod := range pods {
		if nsFilter != "" && pod.Namespace != nsFilter {
			continue
		}
		enriched := pod.DeepCopy()
		enriched.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}
		if status, err := p.GetPodStatus(ctx, pod.Namespace, pod.Name); err == nil {
			enriched.Status = *status
		}
		evt := K8sWatchEvent{Type: "ADDED", Object: enriched}
		if err := enc.Encode(evt); err != nil {
			return
		}
		flusher.Flush()
	}

	// Open watch on the NATS KV store
	events, err := p.deps.Store.Pods.WatchAll(ctx)
	if err != nil {
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-events:
			if !ok {
				return
			}

			var pod corev1.Pod
			if evt.Type == store.EventDelete {
				// For deletes, construct a minimal pod from the key
				ns, name := parseStoreKey(evt.Key)
				if nsFilter != "" && ns != nsFilter {
					continue
				}
				pod = corev1.Pod{
					TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
					ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
				}
			} else {
				if err := json.Unmarshal(evt.Value, &pod); err != nil {
					continue
				}
				if nsFilter != "" && pod.Namespace != nsFilter {
					continue
				}
				pod.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}
			}

			pod.ResourceVersion = fmt.Sprintf("%d", evt.Revision)

			watchEvt := K8sWatchEvent{
				Type:   string(evt.Type),
				Object: &pod,
			}

			if err := enc.Encode(watchEvt); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// handleWatchConfigMaps streams configmap events as newline-delimited JSON.
func (p *MicroKubeProvider) handleWatchConfigMaps(w http.ResponseWriter, r *http.Request, nsFilter string) {
	if p.deps.Store == nil {
		http.Error(w, "watch requires NATS store", http.StatusServiceUnavailable)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Transfer-Encoding", "chunked")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ctx := r.Context()
	enc := json.NewEncoder(w)

	events, err := p.deps.Store.ConfigMaps.WatchAll(ctx)
	if err != nil {
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-events:
			if !ok {
				return
			}

			var cm corev1.ConfigMap
			if evt.Type == store.EventDelete {
				ns, name := parseStoreKey(evt.Key)
				if nsFilter != "" && ns != nsFilter {
					continue
				}
				cm = corev1.ConfigMap{
					TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
					ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
				}
			} else {
				if err := json.Unmarshal(evt.Value, &cm); err != nil {
					continue
				}
				if nsFilter != "" && cm.Namespace != nsFilter {
					continue
				}
				cm.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}
			}

			cm.ResourceVersion = fmt.Sprintf("%d", evt.Revision)

			watchEvt := K8sWatchEvent{
				Type:   string(evt.Type),
				Object: &cm,
			}

			if err := enc.Encode(watchEvt); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// parseStoreKey splits a "namespace.name" key into its components.
func parseStoreKey(key string) (ns, name string) {
	for i := 0; i < len(key); i++ {
		if key[i] == '.' {
			return key[:i], key[i+1:]
		}
	}
	return "", key
}

// RunWatchReconciler uses NATS watch events to drive pod reconciliation
// instead of periodic polling. Falls back to polling on disconnect.
func (p *MicroKubeProvider) RunWatchReconciler(ctx context.Context) error {
	log := p.deps.Logger

	if p.deps.Store == nil {
		log.Info("no NATS store, falling back to polling reconciler")
		return p.RunStandaloneReconciler(ctx)
	}

	log.Info("watch-driven reconciler starting")

	for {
		if err := p.runWatchLoop(ctx); err != nil {
			log.Warnw("watch loop error, retrying in 5s", "error", err)
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(5 * time.Second):
			// Retry watch connection
		}
	}
}

func (p *MicroKubeProvider) runWatchLoop(ctx context.Context) error {
	log := p.deps.Logger

	events, err := p.deps.Store.Pods.WatchAll(ctx)
	if err != nil {
		return fmt.Errorf("opening watch: %w", err)
	}

	// Do an initial full reconcile
	if err := p.reconcile(ctx); err != nil {
		log.Warnw("initial reconcile failed", "error", err)
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case evt, ok := <-events:
			if !ok {
				return fmt.Errorf("watch channel closed")
			}

			switch evt.Type {
			case store.EventPut, store.EventUpdate:
				var pod corev1.Pod
				if err := json.Unmarshal(evt.Value, &pod); err != nil {
					log.Warnw("failed to unmarshal watch event", "error", err)
					continue
				}
				key := podKey(&pod)
				if _, tracked := p.pods[key]; !tracked {
					log.Infow("watch: creating pod", "pod", key)
					if err := p.CreatePod(ctx, &pod); err != nil {
						log.Errorw("watch: failed to create pod", "pod", key, "error", err)
					}
				}

			case store.EventDelete:
				ns, name := parseStoreKey(evt.Key)
				key := ns + "/" + name
				if pod, tracked := p.pods[key]; tracked {
					log.Infow("watch: deleting pod", "pod", key)
					if err := p.DeletePod(ctx, pod); err != nil {
						log.Errorw("watch: failed to delete pod", "pod", key, "error", err)
					}
				}
			}

		case pushEvt, ok := <-p.pushEventsChan():
			if !ok {
				continue
			}
			log.Infow("registry push event, clearing tarball cache and reconciling",
				"repo", pushEvt.Repo, "ref", pushEvt.Reference)
			// Clear stale tarballs for this repo — registry is source of truth.
			p.deps.StorageMgr.ClearImageDigestByRepo(pushEvt.Repo)
			if err := p.reconcile(ctx); err != nil {
				log.Errorw("reconciliation error (push-triggered)", "error", err)
			}

		case pushEvt := <-p.pushNotify:
			log.Infow("push-notify received, clearing tarball cache and reconciling",
				"repo", pushEvt.Repo, "ref", pushEvt.Reference)
			// Clear stale tarballs for this repo — registry is source of truth.
			p.deps.StorageMgr.ClearImageDigestByRepo(pushEvt.Repo)
			if err := p.reconcile(ctx); err != nil {
				log.Errorw("reconciliation error (push-notify)", "error", err)
			}
		}
	}
}
