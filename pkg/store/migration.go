package store

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
)

// ImportFromManifest reads a multi-document YAML file (boot-order.yaml format)
// and imports all Pods and ConfigMaps into the store. Existing keys are overwritten.
func (s *Store) ImportFromManifest(ctx context.Context, path string) (int, int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, fmt.Errorf("reading manifest %s: %w", path, err)
	}

	pods, cms, err := parseManifests(data)
	if err != nil {
		return 0, 0, err
	}

	podCount := 0
	for _, pod := range pods {
		key := pod.Namespace + "." + pod.Name
		if _, err := s.Pods.PutJSON(ctx, key, pod); err != nil {
			s.log.Warnw("failed to import pod", "key", key, "error", err)
			continue
		}
		podCount++
	}

	cmCount := 0
	for _, cm := range cms {
		key := cm.Namespace + "." + cm.Name
		if _, err := s.ConfigMaps.PutJSON(ctx, key, cm); err != nil {
			s.log.Warnw("failed to import configmap", "key", key, "error", err)
			continue
		}
		cmCount++
	}

	s.log.Infow("manifest import complete", "pods", podCount, "configmaps", cmCount)
	return podCount, cmCount, nil
}

// ExportYAML exports all pods and configmaps as multi-document YAML.
func (s *Store) ExportYAML(ctx context.Context) ([]byte, error) {
	var buf bytes.Buffer

	// Export ConfigMaps first (they may be referenced by pods)
	cmKeys, err := s.ConfigMaps.Keys(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("listing configmaps: %w", err)
	}
	for _, key := range cmKeys {
		var cm corev1.ConfigMap
		if _, err := s.ConfigMaps.GetJSON(ctx, key, &cm); err != nil {
			continue
		}
		cm.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}
		data, err := json.MarshalIndent(&cm, "", "  ")
		if err != nil {
			continue
		}
		buf.WriteString("---\n")
		buf.Write(data)
		buf.WriteString("\n")
	}

	// Export Pods
	podKeys, err := s.Pods.Keys(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("listing pods: %w", err)
	}
	for _, key := range podKeys {
		var pod corev1.Pod
		if _, err := s.Pods.GetJSON(ctx, key, &pod); err != nil {
			continue
		}
		pod.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}
		// Clear runtime status for export
		pod.Status = corev1.PodStatus{}
		data, err := json.MarshalIndent(&pod, "", "  ")
		if err != nil {
			continue
		}
		buf.WriteString("---\n")
		buf.Write(data)
		buf.WriteString("\n")
	}

	return buf.Bytes(), nil
}

// ImportYAML imports pods and configmaps from a multi-document YAML body.
func (s *Store) ImportYAML(ctx context.Context, data []byte) (int, int, error) {
	pods, cms, err := parseManifests(data)
	if err != nil {
		return 0, 0, err
	}

	podCount := 0
	for _, pod := range pods {
		key := pod.Namespace + "." + pod.Name
		if _, err := s.Pods.PutJSON(ctx, key, pod); err != nil {
			continue
		}
		podCount++
	}

	cmCount := 0
	for _, cm := range cms {
		key := cm.Namespace + "." + cm.Name
		if _, err := s.ConfigMaps.PutJSON(ctx, key, cm); err != nil {
			continue
		}
		cmCount++
	}

	return podCount, cmCount, nil
}

// MigrateIfEmpty checks if the PODS bucket is empty and imports from the boot
// manifest if so. Returns true if migration occurred.
func (s *Store) MigrateIfEmpty(ctx context.Context, manifestPath string, log *zap.SugaredLogger) (bool, error) {
	empty, err := s.Pods.IsEmpty(ctx)
	if err != nil {
		return false, fmt.Errorf("checking store state: %w", err)
	}

	if !empty {
		log.Info("NATS store has existing data, skipping migration")
		return false, nil
	}

	if _, err := os.Stat(manifestPath); os.IsNotExist(err) {
		log.Info("no boot manifest found, starting with empty store")
		return false, nil
	}

	log.Infow("migrating boot manifest to NATS store", "path", manifestPath)
	pods, cms, err := s.ImportFromManifest(ctx, manifestPath)
	if err != nil {
		return false, fmt.Errorf("migration failed: %w", err)
	}

	log.Infow("migration complete", "pods", pods, "configmaps", cms)
	return true, nil
}

// parseManifests parses multi-document YAML into pods and configmaps.
func parseManifests(data []byte) ([]*corev1.Pod, []*corev1.ConfigMap, error) {
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
