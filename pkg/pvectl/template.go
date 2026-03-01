package pvectl

import (
	"fmt"
	"strings"

	"go.uber.org/zap"
)

// EnsureBaseTemplate downloads the Debian 12 standard template if not already present.
// Returns the template reference (e.g. "local:vztmpl/debian-12-standard_12.7-1_amd64.tar.zst").
func EnsureBaseTemplate(host, user, node, storage string, log *zap.SugaredLogger) (string, error) {
	log.Infow("checking for Debian 12 base template", "storage", storage)

	// List existing templates
	out, err := RunOnHost(host, user,
		fmt.Sprintf("pveam list %s 2>/dev/null | grep debian-12-standard || true", storage), log)
	if err != nil {
		return "", fmt.Errorf("listing templates: %w", err)
	}

	if out != "" {
		// Parse the template name from the listing
		// Format: "local:vztmpl/debian-12-standard_12.7-1_amd64.tar.zst  225.45MB"
		lines := strings.Split(strings.TrimSpace(out), "\n")
		for _, line := range lines {
			fields := strings.Fields(line)
			if len(fields) > 0 && strings.Contains(fields[0], "debian-12-standard") {
				log.Infow("template already available", "template", fields[0])
				return fields[0], nil
			}
		}
	}

	// Update available template list
	log.Info("updating template catalog")
	if _, err := RunOnHost(host, user, "pveam update", log); err != nil {
		return "", fmt.Errorf("pveam update: %w", err)
	}

	// Find the latest Debian 12 standard template
	out, err = RunOnHost(host, user,
		"pveam available --section system 2>/dev/null | grep debian-12-standard | tail -1", log)
	if err != nil || strings.TrimSpace(out) == "" {
		return "", fmt.Errorf("no debian-12-standard template found in catalog")
	}

	// Parse template name from available list
	// Format: "system    debian-12-standard_12.7-1_amd64.tar.zst"
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) < 2 {
		return "", fmt.Errorf("unexpected pveam available output: %s", out)
	}
	templateName := fields[len(fields)-1]

	// Download the template
	log.Infow("downloading template", "template", templateName)
	if _, err := RunOnHost(host, user,
		fmt.Sprintf("pveam download %s %s", storage, templateName), log); err != nil {
		return "", fmt.Errorf("downloading template: %w", err)
	}

	templateRef := fmt.Sprintf("%s:vztmpl/%s", storage, templateName)
	log.Infow("template ready", "ref", templateRef)
	return templateRef, nil
}
