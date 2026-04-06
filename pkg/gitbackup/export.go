package gitbackup

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/glennswest/mkube/pkg/store"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// bucketExport maps bucket names to directory names and export config.
type bucketExport struct {
	bucketName string
	dirName    string
	skip       bool // true = never export (e.g. Secrets, ephemeral)
}

var bucketExports = []bucketExport{
	{"CONFIGMAPS", "configmaps", false},
	{"PODS", "pods", false},
	{"DEPLOYMENTS", "deployments", false},
	{"BAREMETALHOSTS", "baremetalhosts", false},
	{"NETWORKS", "networks", false},
	{"REGISTRIES", "registries", false},
	{"PVCS", "pvcs", false},
	{"ISCSICDROMS", "iscsicdroms", false},
	{"ISCSIDISKS", "iscsidisks", false},
	{"BOOTCONFIGS", "bootconfigs", false},
	{"HOSTRESERVATIONS", "hostreservations", false},
	{"JOBRUNNERS", "jobrunners", false},
	{"STORAGEPOOLS", "storagepools", false},
	{"JOBS", "jobs", false},
	// Skipped buckets:
	{"SECRETS", "secrets", true},     // never export secrets to git
	{"NAMESPACES", "namespaces", true}, // runtime-only
	{"NODE_STATUS", "node_status", true}, // ephemeral
	{"JOBLOGS", "joblogs", true},     // ephemeral, TTL-based
}

// exportedFile represents a file to push to git.
type exportedFile struct {
	Path    string // e.g. "pods/default.nats.yaml"
	Content []byte
}

// exportAll exports all store buckets as individual YAML files.
// Returns a slice of files, a manifest of current resource keys, and diagnostics.
func exportAll(ctx context.Context, s *store.Store) ([]exportedFile, []string, error) {
	var files []exportedFile
	var manifest []string

	if s == nil {
		return nil, nil, fmt.Errorf("store is nil")
	}

	for _, be := range bucketExports {
		if be.skip {
			continue
		}

		bucket := s.BucketByName(be.bucketName)
		if bucket == nil {
			continue
		}

		keys, err := bucket.Keys(ctx, "")
		if err != nil {
			continue
		}

		// Sort keys for deterministic output
		sort.Strings(keys)

		for _, key := range keys {
			path := be.dirName + "/" + key + ".yaml"
			manifest = append(manifest, path)

			content, err := exportResource(ctx, bucket, key, be.bucketName)
			if err != nil {
				continue
			}

			files = append(files, exportedFile{Path: path, Content: content})
		}
	}

	return files, manifest, nil
}

// exportResource exports a single resource as pretty-printed JSON with TypeMeta.
func exportResource(ctx context.Context, bucket *store.Bucket, key, bucketName string) ([]byte, error) {
	raw, _, err := bucket.Get(ctx, key)
	if err != nil {
		return nil, err
	}

	// For typed resources (Pods, ConfigMaps, PVCs), use proper structs
	switch bucketName {
	case "PODS":
		var pod corev1.Pod
		if err := json.Unmarshal(raw, &pod); err != nil {
			return nil, err
		}
		pod.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}
		pod.Status = corev1.PodStatus{}
		return json.MarshalIndent(&pod, "", "  ")

	case "CONFIGMAPS":
		var cm corev1.ConfigMap
		if err := json.Unmarshal(raw, &cm); err != nil {
			return nil, err
		}
		cm.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}
		return json.MarshalIndent(&cm, "", "  ")

	case "PVCS":
		var pvc corev1.PersistentVolumeClaim
		if err := json.Unmarshal(raw, &pvc); err != nil {
			return nil, err
		}
		pvc.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"}
		pvc.Status = corev1.PersistentVolumeClaimStatus{}
		return json.MarshalIndent(&pvc, "", "  ")

	default:
		// Generic: unmarshal to map, inject TypeMeta, strip status
		var doc map[string]interface{}
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, err
		}
		doc["apiVersion"] = apiVersionForBucket(bucketName)
		doc["kind"] = kindForBucket(bucketName)
		delete(doc, "status")
		return json.MarshalIndent(doc, "", "  ")
	}
}

func apiVersionForBucket(name string) string {
	switch name {
	case "DEPLOYMENTS":
		return "apps/v1"
	default:
		return "v1"
	}
}

func kindForBucket(name string) string {
	switch name {
	case "PODS":
		return "Pod"
	case "CONFIGMAPS":
		return "ConfigMap"
	case "PVCS":
		return "PersistentVolumeClaim"
	case "DEPLOYMENTS":
		return "Deployment"
	case "BAREMETALHOSTS":
		return "BareMetalHost"
	case "NETWORKS":
		return "Network"
	case "REGISTRIES":
		return "Registry"
	case "ISCSICDROMS":
		return "ISCSICdrom"
	case "ISCSIDISKS":
		return "ISCSIDisk"
	case "BOOTCONFIGS":
		return "BootConfig"
	case "HOSTRESERVATIONS":
		return "HostReservation"
	case "JOBRUNNERS":
		return "JobRunner"
	case "STORAGEPOOLS":
		return "StoragePool"
	case "JOBS":
		return "Job"
	default:
		return name
	}
}
