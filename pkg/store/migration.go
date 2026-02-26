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

	// Export Deployments
	if s.Deployments != nil {
		deployKeys, err := s.Deployments.Keys(ctx, "")
		if err != nil {
			return nil, fmt.Errorf("listing deployments: %w", err)
		}
		for _, key := range deployKeys {
			raw, _, err := s.Deployments.Get(ctx, key)
			if err != nil {
				continue
			}
			// Re-marshal with TypeMeta set
			var doc map[string]interface{}
			if err := json.Unmarshal(raw, &doc); err != nil {
				continue
			}
			doc["apiVersion"] = "apps/v1"
			doc["kind"] = "Deployment"
			// Clear runtime status for export
			delete(doc, "status")
			data, err := json.MarshalIndent(doc, "", "  ")
			if err != nil {
				continue
			}
			buf.WriteString("---\n")
			buf.Write(data)
			buf.WriteString("\n")
		}
	}

	// Export Networks
	if s.Networks != nil {
		netKeys, err := s.Networks.Keys(ctx, "")
		if err != nil {
			return nil, fmt.Errorf("listing networks: %w", err)
		}
		for _, key := range netKeys {
			raw, _, err := s.Networks.Get(ctx, key)
			if err != nil {
				continue
			}
			var doc map[string]interface{}
			if err := json.Unmarshal(raw, &doc); err != nil {
				continue
			}
			doc["apiVersion"] = "v1"
			doc["kind"] = "Network"
			delete(doc, "status")
			data, err := json.MarshalIndent(doc, "", "  ")
			if err != nil {
				continue
			}
			buf.WriteString("---\n")
			buf.Write(data)
			buf.WriteString("\n")
		}
	}

	// Export Registries
	if s.Registries != nil {
		regKeys, err := s.Registries.Keys(ctx, "")
		if err != nil {
			return nil, fmt.Errorf("listing registries: %w", err)
		}
		for _, key := range regKeys {
			raw, _, err := s.Registries.Get(ctx, key)
			if err != nil {
				continue
			}
			var doc map[string]interface{}
			if err := json.Unmarshal(raw, &doc); err != nil {
				continue
			}
			doc["apiVersion"] = "v1"
			doc["kind"] = "Registry"
			delete(doc, "status")
			data, err := json.MarshalIndent(doc, "", "  ")
			if err != nil {
				continue
			}
			buf.WriteString("---\n")
			buf.Write(data)
			buf.WriteString("\n")
		}
	}

	// Export PVCs
	if s.PersistentVolumeClaims != nil {
		pvcKeys, err := s.PersistentVolumeClaims.Keys(ctx, "")
		if err != nil {
			return nil, fmt.Errorf("listing PVCs: %w", err)
		}
		for _, key := range pvcKeys {
			var pvc corev1.PersistentVolumeClaim
			if _, err := s.PersistentVolumeClaims.GetJSON(ctx, key, &pvc); err != nil {
				continue
			}
			pvc.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"}
			// Clear runtime status for export
			pvc.Status = corev1.PersistentVolumeClaimStatus{}
			data, err := json.MarshalIndent(&pvc, "", "  ")
			if err != nil {
				continue
			}
			buf.WriteString("---\n")
			buf.Write(data)
			buf.WriteString("\n")
		}
	}

	return buf.Bytes(), nil
}

// ImportYAML imports pods, configmaps, and deployments from a multi-document YAML body.
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

	// Import Deployments
	if s.Deployments != nil {
		deploys, err := parseDeployments(data)
		if err == nil {
			for _, deploy := range deploys {
				key := deploy.Namespace + "." + deploy.Name
				if _, err := s.Deployments.PutJSON(ctx, key, &deploy); err != nil {
					continue
				}
			}
		}
	}

	// Import PVCs
	if s.PersistentVolumeClaims != nil {
		pvcs, err := parsePVCs(data)
		if err == nil {
			for _, pvc := range pvcs {
				key := pvc.Namespace + "." + pvc.Name
				if _, err := s.PersistentVolumeClaims.PutJSON(ctx, key, &pvc); err != nil {
					continue
				}
			}
		}
	}

	// Import Registries
	if s.Registries != nil {
		regs, err := parseRegistries(data)
		if err == nil {
			for _, reg := range regs {
				if _, err := s.Registries.PutJSON(ctx, reg.Name, &reg); err != nil {
					continue
				}
			}
		}
	}

	// Import Networks
	if s.Networks != nil {
		nets, err := parseNetworks(data)
		if err == nil {
			for _, net := range nets {
				if _, err := s.Networks.PutJSON(ctx, net.Name, &net); err != nil {
					continue
				}
			}
		}
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

// deploymentDoc is a lightweight Deployment representation for import/export.
// Kept in the store package to avoid circular imports with provider.
type deploymentDoc struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Spec              json.RawMessage `json:"spec"`
	Status            json.RawMessage `json:"status,omitempty"`
}

// parseManifests parses multi-document YAML into pods, configmaps, and raw deployment docs.
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
		case "Deployment":
			// Deployments are handled separately via parseDeployments
			continue
		case "Network":
			// Networks are handled separately via parseNetworks
			continue
		case "Registry":
			// Registries are handled separately via parseRegistries
			continue
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

// parseDeployments extracts Deployment documents from a multi-document YAML.
func parseDeployments(data []byte) ([]deploymentDoc, error) {
	var deployments []deploymentDoc

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

		var meta metav1.TypeMeta
		if err := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(doc), 4096).Decode(&meta); err != nil {
			continue
		}

		if meta.Kind != "Deployment" {
			continue
		}

		var deploy deploymentDoc
		if err := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(doc), 4096).Decode(&deploy); err != nil {
			return nil, fmt.Errorf("decoding deployment: %w", err)
		}
		if deploy.Name == "" {
			continue
		}
		if deploy.Namespace == "" {
			deploy.Namespace = "default"
		}
		deployments = append(deployments, deploy)
	}

	return deployments, nil
}

// parsePVCs extracts PersistentVolumeClaim documents from a multi-document YAML.
func parsePVCs(data []byte) ([]corev1.PersistentVolumeClaim, error) {
	var pvcs []corev1.PersistentVolumeClaim

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

		var meta metav1.TypeMeta
		if err := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(doc), 4096).Decode(&meta); err != nil {
			continue
		}

		if meta.Kind != "PersistentVolumeClaim" {
			continue
		}

		var pvc corev1.PersistentVolumeClaim
		if err := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(doc), 4096).Decode(&pvc); err != nil {
			return nil, fmt.Errorf("decoding PVC: %w", err)
		}
		if pvc.Name == "" {
			continue
		}
		if pvc.Namespace == "" {
			pvc.Namespace = "default"
		}
		pvcs = append(pvcs, pvc)
	}

	return pvcs, nil
}

// networkDoc is a lightweight Network representation for import/export.
type networkDoc struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Spec              json.RawMessage `json:"spec"`
	Status            json.RawMessage `json:"status,omitempty"`
}

// parseNetworks extracts Network documents from a multi-document YAML.
func parseNetworks(data []byte) ([]networkDoc, error) {
	var networks []networkDoc

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

		var meta metav1.TypeMeta
		if err := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(doc), 4096).Decode(&meta); err != nil {
			continue
		}

		if meta.Kind != "Network" {
			continue
		}

		var net networkDoc
		if err := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(doc), 4096).Decode(&net); err != nil {
			return nil, fmt.Errorf("decoding network: %w", err)
		}
		if net.Name == "" {
			continue
		}
		networks = append(networks, net)
	}

	return networks, nil
}

// registryDoc is a lightweight Registry representation for import/export.
type registryDoc struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Spec              json.RawMessage `json:"spec"`
	Status            json.RawMessage `json:"status,omitempty"`
}

// parseRegistries extracts Registry documents from a multi-document YAML.
func parseRegistries(data []byte) ([]registryDoc, error) {
	var registries []registryDoc

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

		var meta metav1.TypeMeta
		if err := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(doc), 4096).Decode(&meta); err != nil {
			continue
		}

		if meta.Kind != "Registry" {
			continue
		}

		var reg registryDoc
		if err := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(doc), 4096).Decode(&reg); err != nil {
			return nil, fmt.Errorf("decoding registry: %w", err)
		}
		if reg.Name == "" {
			continue
		}
		registries = append(registries, reg)
	}

	return registries, nil
}
