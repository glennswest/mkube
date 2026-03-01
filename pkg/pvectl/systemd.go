package pvectl

import (
	"fmt"
	"sort"
	"strings"

	"go.uber.org/zap"
)

// ServiceConfig describes a systemd service to install in the container.
type ServiceConfig struct {
	Name        string            // unit name (without .service)
	Description string            // unit description
	ExecStart   string            // full command line
	EnvVars     map[string]string // environment variables
	WorkingDir  string            // optional working directory
	Restart     string            // restart policy (default "always")
	RestartSec  int               // seconds between restarts (default 5)
}

// generateUnit produces a systemd unit file from the config.
func generateUnit(cfg ServiceConfig) string {
	if cfg.Restart == "" {
		cfg.Restart = "always"
	}
	if cfg.RestartSec == 0 {
		cfg.RestartSec = 5
	}
	if cfg.Description == "" {
		cfg.Description = cfg.Name
	}

	var b strings.Builder
	b.WriteString("[Unit]\n")
	fmt.Fprintf(&b, "Description=%s\n", cfg.Description)
	b.WriteString("After=network-online.target\n")
	b.WriteString("Wants=network-online.target\n")
	b.WriteString("\n[Service]\n")
	b.WriteString("Type=simple\n")

	if cfg.WorkingDir != "" {
		fmt.Fprintf(&b, "WorkingDirectory=%s\n", cfg.WorkingDir)
	}

	// Sort env vars for deterministic output
	if len(cfg.EnvVars) > 0 {
		keys := make([]string, 0, len(cfg.EnvVars))
		for k := range cfg.EnvVars {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "Environment=%s=%s\n", k, cfg.EnvVars[k])
		}
	}

	fmt.Fprintf(&b, "ExecStart=%s\n", cfg.ExecStart)
	fmt.Fprintf(&b, "Restart=%s\n", cfg.Restart)
	fmt.Fprintf(&b, "RestartSec=%d\n", cfg.RestartSec)
	b.WriteString("StandardOutput=journal\n")
	b.WriteString("StandardError=journal\n")
	b.WriteString("\n[Install]\n")
	b.WriteString("WantedBy=multi-user.target\n")

	return b.String()
}

// InstallService generates a systemd unit file and installs it in the container.
func InstallService(host, user string, vmid int, cfg ServiceConfig, log *zap.SugaredLogger) error {
	unitContent := generateUnit(cfg)
	unitPath := fmt.Sprintf("/etc/systemd/system/%s.service", cfg.Name)

	log.Infow("installing systemd service", "vmid", vmid, "service", cfg.Name)

	// Push the unit file into the container
	if err := PctPushContent(host, user, vmid, unitContent, unitPath, "0644", log); err != nil {
		return fmt.Errorf("pushing unit file: %w", err)
	}

	// Reload systemd, enable and start the service
	commands := fmt.Sprintf("systemctl daemon-reload && systemctl enable %s && systemctl start %s",
		cfg.Name, cfg.Name)
	if _, err := PctExec(host, user, vmid, commands, log); err != nil {
		return fmt.Errorf("enabling service %s: %w", cfg.Name, err)
	}

	log.Infow("service installed and started", "service", cfg.Name)
	return nil
}
