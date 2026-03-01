// mkube-boot is the Proxmox bootstrap binary.
//
// It runs inside an LXC container on Proxmox and deploys the mkube
// infrastructure: registry, mkube controller, and dependencies.
// Once mkube is running, mkube-boot enters watchdog mode.
//
// Usage:
//
//	mkube-boot --config /etc/mkube-boot/config.yaml
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"

	"github.com/glennswest/mkube/pkg/pvectl"
)

var version = "dev"

// BootConfig describes the bootstrap sequence.
type BootConfig struct {
	// Deploy steps in order
	Steps []BootStep `yaml:"steps"`

	// Watchdog config
	Watchdog WatchdogConfig `yaml:"watchdog"`
}

// BootStep is a single deployment step.
type BootStep struct {
	Name       string `yaml:"name"`       // human-readable step name
	ConfigFile string `yaml:"configFile"` // path to pvectl DeployConfig YAML
	HealthURL  string `yaml:"healthURL"`  // optional health check URL
	HealthWait int    `yaml:"healthWait"` // seconds to wait for health (default 120)
	Skip       bool   `yaml:"skip"`       // skip this step
}

// WatchdogConfig describes the watchdog behavior after bootstrap.
type WatchdogConfig struct {
	Enabled  bool   `yaml:"enabled"`  // enter watchdog mode after bootstrap
	Interval int    `yaml:"interval"` // check interval in seconds (default 30)
	Target   string `yaml:"target"`   // health URL to watch (usually mkube API)
}

func main() {
	rootCmd := &cobra.Command{
		Use:     "mkube-boot",
		Short:   "Bootstrap mkube infrastructure on Proxmox",
		Version: version,
		RunE:    run,
	}

	rootCmd.Flags().StringP("config", "c", "/etc/mkube-boot/config.yaml", "Boot config file")

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func run(cmd *cobra.Command, args []string) error {
	logger, _ := zap.NewProduction()
	defer func() { _ = logger.Sync() }()
	log := logger.Sugar()

	log.Infow("mkube-boot starting", "version", version)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	configPath, _ := cmd.Flags().GetString("config")
	bootCfg, err := loadBootConfig(configPath)
	if err != nil {
		return fmt.Errorf("loading boot config: %w", err)
	}

	// Execute bootstrap steps
	for i, step := range bootCfg.Steps {
		if step.Skip {
			log.Infow("skipping step", "step", i+1, "name", step.Name)
			continue
		}

		log.Infow("executing bootstrap step", "step", i+1, "name", step.Name)

		deployCfg, err := pvectl.LoadConfig(step.ConfigFile)
		if err != nil {
			return fmt.Errorf("step %d (%s): loading config: %w", i+1, step.Name, err)
		}

		if err := pvectl.Deploy(ctx, deployCfg, log); err != nil {
			return fmt.Errorf("step %d (%s): deploy failed: %w", i+1, step.Name, err)
		}

		// Health check if URL provided
		if step.HealthURL != "" {
			wait := step.HealthWait
			if wait == 0 {
				wait = 120
			}
			if err := waitForHealth(ctx, step.HealthURL, time.Duration(wait)*time.Second, log); err != nil {
				return fmt.Errorf("step %d (%s): health check failed: %w", i+1, step.Name, err)
			}
		}

		log.Infow("bootstrap step complete", "step", i+1, "name", step.Name)
	}

	log.Info("bootstrap complete")

	// Watchdog mode
	if bootCfg.Watchdog.Enabled && bootCfg.Watchdog.Target != "" {
		return runWatchdog(ctx, bootCfg.Watchdog, log)
	}

	return nil
}

func loadBootConfig(path string) (*BootConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	expanded := os.ExpandEnv(string(data))

	var cfg BootConfig
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func waitForHealth(ctx context.Context, url string, timeout time.Duration, log *zap.SugaredLogger) error {
	log.Infow("waiting for health", "url", url, "timeout", timeout)

	client := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(timeout)

	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("health check timed out after %v: %s", timeout, url)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 400 {
				log.Infow("health check passed", "url", url)
				return nil
			}
		}

		time.Sleep(3 * time.Second)
	}
}

func runWatchdog(ctx context.Context, cfg WatchdogConfig, log *zap.SugaredLogger) error {
	interval := time.Duration(cfg.Interval) * time.Second
	if interval == 0 {
		interval = 30 * time.Second
	}

	log.Infow("entering watchdog mode", "target", cfg.Target, "interval", interval)

	client := &http.Client{Timeout: 5 * time.Second}
	failures := 0

	for {
		select {
		case <-ctx.Done():
			log.Info("watchdog shutting down")
			return nil
		case <-time.After(interval):
		}

		req, _ := http.NewRequestWithContext(ctx, "GET", cfg.Target, nil)
		resp, err := client.Do(req)
		if err != nil || resp.StatusCode >= 500 {
			failures++
			log.Warnw("watchdog health check failed", "target", cfg.Target, "failures", failures, "err", err)
			if resp != nil {
				resp.Body.Close()
			}
		} else {
			resp.Body.Close()
			if failures > 0 {
				log.Infow("watchdog target recovered", "target", cfg.Target)
			}
			failures = 0
		}

		if failures >= 3 {
			log.Errorw("watchdog target unhealthy — manual intervention required",
				"target", cfg.Target, "consecutiveFailures", failures)
			// Future: auto-redeploy mkube via pvectl.Deploy()
		}
	}
}
