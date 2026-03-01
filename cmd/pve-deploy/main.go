// pve-deploy deploys OCI container images as LXC containers on Proxmox VE.
//
// It creates a Debian 12 LXC, extracts the Go binary from the OCI image,
// pushes it into the container, and installs a systemd service.
//
// Usage:
//
//	pve-deploy --config deploy/pvex-registry.yaml
//	pve-deploy --image ghcr.io/glennswest/mkube-registry:latest \
//	           --vmid 119 --hostname registry \
//	           --ip 192.168.1.161/24 --gateway 192.168.1.1 \
//	           --pve-host pvex.gw.lo
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/glennswest/mkube/pkg/pvectl"
)

var version = "dev"

func main() {
	rootCmd := &cobra.Command{
		Use:     "pve-deploy",
		Short:   "Deploy OCI images as LXC containers on Proxmox VE",
		Version: version,
		RunE:    run,
	}

	f := rootCmd.Flags()
	f.StringP("config", "c", "", "YAML config file path")

	// CLI overrides (used when --config is not set, or to override config values)
	f.String("pve-host", "", "Proxmox host (SSH)")
	f.String("pve-user", "root", "Proxmox SSH user")
	f.String("pve-api-user", "", "PVE API token ID")
	f.String("pve-api-token", "", "PVE API token secret")
	f.String("pve-node", "", "PVE node name")
	f.Int("vmid", 0, "Container VMID")
	f.String("hostname", "", "Container hostname")
	f.String("image", "", "OCI image reference")
	f.String("binary", "", "Binary name in image")
	f.String("binary-path", "", "Install path for binary")
	f.Int("memory", 512, "Memory in MB")
	f.Int("cores", 2, "CPU cores")
	f.String("disk", "4G", "Disk size")
	f.String("storage", "local-lvm", "PVE storage")
	f.String("bridge", "vmbr0", "Network bridge")
	f.String("ip", "", "Container IP with CIDR")
	f.String("gateway", "", "Gateway IP")
	f.String("dns", "", "DNS server")
	f.String("service", "", "Systemd service name")
	f.String("exec-args", "", "Extra args for binary")
	f.Bool("destroy", false, "Destroy existing container instead of deploying")

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func run(cmd *cobra.Command, args []string) error {
	logger, _ := zap.NewProduction()
	defer func() { _ = logger.Sync() }()
	log := logger.Sugar()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	configPath, _ := cmd.Flags().GetString("config")
	destroyMode, _ := cmd.Flags().GetBool("destroy")

	var cfg *pvectl.DeployConfig

	if configPath != "" {
		var err error
		cfg, err = pvectl.LoadConfig(configPath)
		if err != nil {
			return fmt.Errorf("loading config: %w", err)
		}
		log.Infow("loaded config", "path", configPath, "hostname", cfg.Hostname, "vmid", cfg.VMID)
	} else {
		cfg = &pvectl.DeployConfig{}
	}

	// Apply CLI flag overrides
	applyOverrides(cmd, cfg)

	if destroyMode {
		log.Infow("destroying container", "vmid", cfg.VMID)
		return pvectl.Destroy(ctx, cfg.PVEHost, cfg.PVEUser, cfg.PVENode, cfg.VMID,
			cfg.PVEAPIUser, cfg.PVEAPIToken, log)
	}

	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}

	return pvectl.Deploy(ctx, cfg, log)
}

func applyOverrides(cmd *cobra.Command, cfg *pvectl.DeployConfig) {
	if v, _ := cmd.Flags().GetString("pve-host"); v != "" {
		cfg.PVEHost = v
	}
	if v, _ := cmd.Flags().GetString("pve-user"); v != "" {
		cfg.PVEUser = v
	}
	if v, _ := cmd.Flags().GetString("pve-api-user"); v != "" {
		cfg.PVEAPIUser = v
	}
	if v, _ := cmd.Flags().GetString("pve-api-token"); v != "" {
		cfg.PVEAPIToken = v
	}
	if v, _ := cmd.Flags().GetString("pve-node"); v != "" {
		cfg.PVENode = v
	}
	if v, _ := cmd.Flags().GetInt("vmid"); v != 0 {
		cfg.VMID = v
	}
	if v, _ := cmd.Flags().GetString("hostname"); v != "" {
		cfg.Hostname = v
	}
	if v, _ := cmd.Flags().GetString("image"); v != "" {
		cfg.ImageRef = v
	}
	if v, _ := cmd.Flags().GetString("binary"); v != "" {
		cfg.BinaryName = v
	}
	if v, _ := cmd.Flags().GetString("binary-path"); v != "" {
		cfg.BinaryPath = v
	}
	if cmd.Flags().Changed("memory") {
		cfg.Memory, _ = cmd.Flags().GetInt("memory")
	}
	if cmd.Flags().Changed("cores") {
		cfg.Cores, _ = cmd.Flags().GetInt("cores")
	}
	if v, _ := cmd.Flags().GetString("disk"); cmd.Flags().Changed("disk") {
		cfg.DiskSize = v
	}
	if v, _ := cmd.Flags().GetString("storage"); cmd.Flags().Changed("storage") {
		cfg.Storage = v
	}
	if v, _ := cmd.Flags().GetString("bridge"); cmd.Flags().Changed("bridge") {
		cfg.Bridge = v
	}
	if v, _ := cmd.Flags().GetString("ip"); v != "" {
		cfg.IP = v
	}
	if v, _ := cmd.Flags().GetString("gateway"); v != "" {
		cfg.Gateway = v
	}
	if v, _ := cmd.Flags().GetString("dns"); v != "" {
		cfg.DNS = v
	}
	if v, _ := cmd.Flags().GetString("service"); v != "" {
		cfg.ServiceName = v
	}
	if v, _ := cmd.Flags().GetString("exec-args"); v != "" {
		cfg.ExecArgs = v
	}
}
