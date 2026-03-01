package pvectl

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"go.uber.org/zap"
)

// RunOnHost executes a command on the Proxmox host via SSH.
func RunOnHost(host, user, command string, log *zap.SugaredLogger) (string, error) {
	log.Debugw("ssh", "host", host, "cmd", command)
	cmd := exec.Command("ssh",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "ConnectTimeout=10",
		"-o", "BatchMode=yes",
		fmt.Sprintf("%s@%s", user, host),
		command,
	)
	out, err := cmd.CombinedOutput()
	result := strings.TrimSpace(string(out))
	if err != nil {
		return result, fmt.Errorf("ssh %s@%s %q: %w: %s", user, host, command, err, result)
	}
	return result, nil
}

// PctExec runs a command inside an LXC container via pct exec.
func PctExec(host, user string, vmid int, command string, log *zap.SugaredLogger) (string, error) {
	sshCmd := fmt.Sprintf("pct exec %d -- bash -c %q", vmid, command)
	return RunOnHost(host, user, sshCmd, log)
}

// PctPush copies a local file into an LXC container via pct push.
func PctPush(host, user string, vmid int, localPath, remotePath string, log *zap.SugaredLogger) error {
	log.Debugw("pct push", "vmid", vmid, "local", localPath, "remote", remotePath)

	// First SCP the file to the PVE host
	tmpRemote := fmt.Sprintf("/tmp/pve-deploy-%d-%d", vmid, time.Now().UnixNano())

	scpCmd := exec.Command("scp",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "ConnectTimeout=10",
		localPath,
		fmt.Sprintf("%s@%s:%s", user, host, tmpRemote),
	)
	if out, err := scpCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("scp to host: %w: %s", err, string(out))
	}

	// Then pct push from PVE host into container
	pushCmd := fmt.Sprintf("pct push %d %s %s --perms 0755 && rm -f %s", vmid, tmpRemote, remotePath, tmpRemote)
	if _, err := RunOnHost(host, user, pushCmd, log); err != nil {
		// Cleanup temp file on failure
		RunOnHost(host, user, "rm -f "+tmpRemote, log)
		return fmt.Errorf("pct push: %w", err)
	}

	return nil
}

// PctPushContent writes content to a temp file then pushes it into the container.
func PctPushContent(host, user string, vmid int, content, remotePath string, perms string, log *zap.SugaredLogger) error {
	tmp, err := os.CreateTemp("", "pvectl-*")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()

	// SCP to host, then pct push into container
	tmpRemote := fmt.Sprintf("/tmp/pve-deploy-%d-%d", vmid, time.Now().UnixNano())

	scpCmd := exec.Command("scp",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "ConnectTimeout=10",
		tmp.Name(),
		fmt.Sprintf("%s@%s:%s", user, host, tmpRemote),
	)
	if out, err := scpCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("scp content to host: %w: %s", err, string(out))
	}

	if perms == "" {
		perms = "0644"
	}

	pushCmd := fmt.Sprintf("pct push %d %s %s --perms %s && rm -f %s", vmid, tmpRemote, remotePath, perms, tmpRemote)
	if _, err := RunOnHost(host, user, pushCmd, log); err != nil {
		RunOnHost(host, user, "rm -f "+tmpRemote, log)
		return fmt.Errorf("pct push content: %w", err)
	}

	return nil
}

// WaitForBoot polls the container until it's running and systemd is ready.
func WaitForBoot(host, user string, vmid int, timeout time.Duration, log *zap.SugaredLogger) error {
	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("container %d not ready after %v", vmid, timeout)
		}

		// Check if systemd finished booting
		out, err := PctExec(host, user, vmid, "systemctl is-system-running 2>/dev/null || true", log)
		if err == nil && (out == "running" || out == "degraded") {
			log.Infow("container booted", "vmid", vmid, "state", out)
			return nil
		}

		time.Sleep(2 * time.Second)
	}
}
