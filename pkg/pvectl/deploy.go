package pvectl

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"go.uber.org/zap"

	"github.com/glennswest/mkube/pkg/proxmox"
)

// Deploy creates an LXC container on Proxmox, extracts a binary from an OCI
// image, pushes it into the container, and installs a systemd service.
func Deploy(ctx context.Context, cfg *DeployConfig, log *zap.SugaredLogger) error {
	log.Infow("starting deployment",
		"hostname", cfg.Hostname,
		"vmid", cfg.VMID,
		"image", cfg.ImageRef,
	)

	// Step 1: Connect to PVE API to check if container already exists
	pveClient, err := proxmox.NewClient(proxmox.ClientConfig{
		URL:            fmt.Sprintf("https://%s:8006", cfg.PVEHost),
		TokenID:        cfg.PVEAPIUser,
		TokenSecret:    cfg.PVEAPIToken,
		Node:           cfg.PVENode,
		InsecureVerify: true,
		VMIDRange:      fmt.Sprintf("%d-%d", cfg.VMID, cfg.VMID),
	})
	if err != nil {
		log.Warnw("PVE API client init failed, proceeding with SSH only", "err", err)
		pveClient = nil
	}

	// Step 2: Check if container already exists
	if pveClient != nil {
		status, err := pveClient.GetContainerStatus(ctx, cfg.VMID)
		if err == nil {
			if status.Status == "running" {
				log.Infow("container already running, stopping for redeploy", "vmid", cfg.VMID)
				if err := pveClient.StopContainer(ctx, cfg.VMID); err != nil {
					return fmt.Errorf("stopping existing container: %w", err)
				}
				time.Sleep(3 * time.Second)
			}
			log.Infow("removing existing container", "vmid", cfg.VMID)
			if err := pveClient.RemoveContainer(ctx, cfg.VMID); err != nil {
				return fmt.Errorf("removing existing container: %w", err)
			}
			time.Sleep(2 * time.Second)
		}
	}

	// Step 3: Ensure Debian 12 base template is available
	templateRef := cfg.Template
	if templateRef == "" {
		storage := "local"
		templateRef, err = EnsureBaseTemplate(cfg.PVEHost, cfg.PVEUser, cfg.PVENode, storage, log)
		if err != nil {
			return fmt.Errorf("ensuring base template: %w", err)
		}
	}

	// Step 4: Create LXC container
	log.Infow("creating LXC container", "vmid", cfg.VMID, "template", templateRef)
	if pveClient != nil {
		err = pveClient.CreateContainer(ctx, proxmox.LXCCreateSpec{
			VMID:         cfg.VMID,
			Hostname:     cfg.Hostname,
			OSTemplate:   templateRef,
			Net0:         cfg.Net0Spec(),
			Memory:       cfg.Memory,
			Swap:         256,
			Cores:        cfg.Cores,
			RootFS:       cfg.Storage + ":" + cfg.DiskSizeGB(),
			Unprivileged: true,
			Features:     "nesting=1",
			Nameserver:   cfg.DNS,
			OnBoot:       true,
			Start:        false,
		})
	} else {
		// Fallback to SSH-based creation
		createCmd := fmt.Sprintf(
			"pct create %d %s --hostname %s --net0 %s --memory %d --swap 256 --cores %d --rootfs %s:%s --unprivileged 1 --features nesting=1 --onboot 1",
			cfg.VMID, templateRef, cfg.Hostname, cfg.Net0Spec(),
			cfg.Memory, cfg.Cores, cfg.Storage, cfg.DiskSizeGB(),
		)
		if cfg.DNS != "" {
			createCmd += " --nameserver " + cfg.DNS
		}
		_, err = RunOnHost(cfg.PVEHost, cfg.PVEUser, createCmd, log)
	}
	if err != nil {
		return fmt.Errorf("creating container: %w", err)
	}

	// Step 5: Start the container
	log.Infow("starting container", "vmid", cfg.VMID)
	if pveClient != nil {
		err = pveClient.StartContainer(ctx, cfg.VMID)
	} else {
		_, err = RunOnHost(cfg.PVEHost, cfg.PVEUser,
			fmt.Sprintf("pct start %d", cfg.VMID), log)
	}
	if err != nil {
		return fmt.Errorf("starting container: %w", err)
	}

	// Step 6: Wait for boot
	if err := WaitForBoot(cfg.PVEHost, cfg.PVEUser, cfg.VMID, 60*time.Second, log); err != nil {
		return fmt.Errorf("waiting for boot: %w", err)
	}

	// Step 7: Create directories inside the container
	for remotePath := range cfg.ConfigFiles {
		dir := dirOf(remotePath)
		PctExec(cfg.PVEHost, cfg.PVEUser, cfg.VMID,
			fmt.Sprintf("mkdir -p %s", dir), log)
	}

	// Step 8: Extract binary from OCI image
	binaryPath, err := ExtractBinary(ctx, cfg.ImageRef, cfg.BinaryName, log)
	if err != nil {
		return fmt.Errorf("extracting binary: %w", err)
	}
	defer os.Remove(binaryPath)

	// Step 9: Push binary into container
	log.Infow("pushing binary into container", "binary", cfg.BinaryName, "path", cfg.BinaryPath)
	// Ensure target directory exists
	PctExec(cfg.PVEHost, cfg.PVEUser, cfg.VMID,
		fmt.Sprintf("mkdir -p %s", dirOf(cfg.BinaryPath)), log)

	if err := PctPush(cfg.PVEHost, cfg.PVEUser, cfg.VMID, binaryPath, cfg.BinaryPath, log); err != nil {
		return fmt.Errorf("pushing binary: %w", err)
	}

	// Step 10: Push config files
	for remotePath, content := range cfg.ConfigFiles {
		log.Infow("pushing config file", "path", remotePath)
		if err := PctPushContent(cfg.PVEHost, cfg.PVEUser, cfg.VMID, content, remotePath, "0644", log); err != nil {
			return fmt.Errorf("pushing config %s: %w", remotePath, err)
		}
	}

	// Step 11: Install systemd service
	execStart := cfg.BinaryPath
	if cfg.ExecArgs != "" {
		execStart += " " + cfg.ExecArgs
	}

	if err := InstallService(cfg.PVEHost, cfg.PVEUser, cfg.VMID, ServiceConfig{
		Name:        cfg.ServiceName,
		Description: fmt.Sprintf("%s service", cfg.Hostname),
		ExecStart:   execStart,
		EnvVars:     cfg.EnvVars,
	}, log); err != nil {
		return fmt.Errorf("installing service: %w", err)
	}

	// Step 12: Health check
	log.Infow("verifying service status", "service", cfg.ServiceName)
	time.Sleep(3 * time.Second)
	out, err := PctExec(cfg.PVEHost, cfg.PVEUser, cfg.VMID,
		fmt.Sprintf("systemctl is-active %s", cfg.ServiceName), log)
	if err != nil || out != "active" {
		log.Warnw("service may not be fully healthy yet", "status", out, "err", err)
		// Show journal output for debugging
		journal, _ := PctExec(cfg.PVEHost, cfg.PVEUser, cfg.VMID,
			fmt.Sprintf("journalctl -u %s --no-pager -n 20", cfg.ServiceName), log)
		if journal != "" {
			log.Infow("service journal", "output", journal)
		}
	} else {
		log.Infow("service healthy", "service", cfg.ServiceName, "status", "active")
	}

	log.Infow("deployment complete",
		"hostname", cfg.Hostname,
		"vmid", cfg.VMID,
		"ip", cfg.IP,
		"service", cfg.ServiceName,
	)
	return nil
}

// Destroy stops and removes an LXC container.
func Destroy(ctx context.Context, host, user, node string, vmid int, apiUser, apiToken string, log *zap.SugaredLogger) error {
	pveClient, err := proxmox.NewClient(proxmox.ClientConfig{
		URL:            fmt.Sprintf("https://%s:8006", host),
		TokenID:        apiUser,
		TokenSecret:    apiToken,
		Node:           node,
		InsecureVerify: true,
		VMIDRange:      strconv.Itoa(vmid) + "-" + strconv.Itoa(vmid),
	})
	if err != nil {
		// Fallback to SSH
		log.Warnw("PVE API unavailable, using SSH", "err", err)
		RunOnHost(host, user, fmt.Sprintf("pct stop %d 2>/dev/null; pct destroy %d", vmid, vmid), log)
		return nil
	}

	status, err := pveClient.GetContainerStatus(ctx, vmid)
	if err != nil {
		return fmt.Errorf("container %d not found: %w", vmid, err)
	}

	if status.Status == "running" {
		log.Infow("stopping container", "vmid", vmid)
		if err := pveClient.StopContainer(ctx, vmid); err != nil {
			return fmt.Errorf("stopping: %w", err)
		}
		time.Sleep(3 * time.Second)
	}

	log.Infow("removing container", "vmid", vmid)
	return pveClient.RemoveContainer(ctx, vmid)
}

// dirOf returns the directory portion of a path.
func dirOf(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[:i]
		}
	}
	return "/"
}
