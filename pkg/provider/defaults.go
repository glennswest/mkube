package provider

import (
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/glennswest/mkube/pkg/config"
)

// generateDefaultConfigMaps creates built-in ConfigMaps derived from the
// running mkube configuration. These are loaded at startup and can be
// overridden by user-supplied ConfigMaps from the boot manifest.
func generateDefaultConfigMaps(cfg *config.Config) []*corev1.ConfigMap {
	gateway := cfg.DefaultNetwork().Gateway

	// Derive mkube container IP: gateway .1 → mkube .2 (deploy convention)
	mkubeIP := gateway
	if parts := strings.Split(gateway, "."); len(parts) == 4 {
		if n, err := strconv.Atoi(parts[3]); err == nil {
			parts[3] = strconv.Itoa(n + 1)
			mkubeIP = strings.Join(parts, ".")
		}
	}

	consoleConfig := fmt.Sprintf(`cluster_name: %s
listen_port: 9090

mkube:
  base_url: "http://%s:8082"

registry:
  base_url: "http://%s:5000"
`, cfg.NodeName, mkubeIP, mkubeIP)

	return []*corev1.ConfigMap{
		{
			TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
			ObjectMeta: metav1.ObjectMeta{
				Name:      "mkube-console-config",
				Namespace: "infra",
			},
			Data: map[string]string{
				"config.yaml": consoleConfig,
			},
		},
	}
}
