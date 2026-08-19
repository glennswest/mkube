// stormboot: bring a device's control plane up in the order it depends on.
//
// # Why this exists
//
// mkube-update grew out of a single problem — the registry cannot pull its
// own update — and ended up owning image polling, tarball staging, container
// creation and self-update. mkube-installer owns the same knowledge again for
// a fresh device, from a workstation. Neither knows about ordering, because
// when they were written there was nothing to order: mkube ran from a tarball
// on the hardware disk and depended on nothing.
//
// That changed when the control plane grew storage. stormblockmk serves the
// volumes, sbregistry builds the goldens, mkube clones them. A pod whose root
// filesystem is a clone cannot start before the thing serving the clone, and
// nothing on the device knows that. Boot order is currently an emergent
// property of RouterOS `start-on-boot` and a `boot-priority` annotation mkube
// itself reads — which is no use for bringing up mkube.
//
// stormboot is the sequencer that makes the dependency explicit:
//
//	/raid1 → stormblockmk → sbregistry → mkube → the fleet
//
// It is the floor. It runs from a plain tarball on the hardware disk and
// depends on nothing it starts, which is the one property that lets everything
// above it live on storage that did not exist yet when the device powered on.
//
// # What it does today
//
// Converges: for each stage in order, make sure the container is running, then
// wait until the service inside it actually answers. Idempotent — running it
// against a healthy device does nothing and says so — so it is equally a boot
// sequencer and a health check for the dependency chain.
//
// # What it grows into
//
// Launching mkube from a CoW clone rather than a tarball, and replacing
// mkube-update's polling with sbregistry's push notification. Both need the
// ordering this establishes, which is why the ordering comes first.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/glennswest/mkube/pkg/routeros"
)

var version = "dev"

// Outcome is what happened to one stage.
type Outcome int

const (
	// AlreadyUp: the container was running and the service answered. Nothing
	// was done, which is the expected result on a healthy device.
	AlreadyUp Outcome = iota
	// Started: the container was not running and stormboot started it.
	Started
	// Failed: the stage never became ready inside its timeout.
	Failed
	// Missing: the container does not exist on the device.
	Missing
)

func (o Outcome) String() string {
	switch o {
	case AlreadyUp:
		return "already up"
	case Started:
		return "started"
	case Failed:
		return "FAILED"
	case Missing:
		return "MISSING"
	}
	return "unknown"
}

func main() {
	var (
		configPath = flag.String("config", "/etc/stormboot/config.yaml", "config file")
		dryRun     = flag.Bool("dry-run", false, "report what each stage is doing, change nothing")
		showVer    = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVer {
		fmt.Println(version)
		return
	}

	cfg, err := Load(*configPath)
	if err != nil {
		fatal("config: %v", err)
	}

	ros, err := routeros.NewClient(cfg.RouterOS)
	if err != nil {
		fatal("connecting to RouterOS: %v", err)
	}
	defer ros.Close()

	// A boot sequencer that ignores SIGTERM is a boot sequencer that has to be
	// killed. Cancelling mid-wait leaves the device exactly as it was: the
	// stages already up stay up, and the next run picks up from there.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if *dryRun {
		fmt.Println("dry run — no containers will be started")
	}

	failed := run(ctx, ros, cfg, *dryRun)
	if failed > 0 {
		os.Exit(1)
	}
}

// run walks the stages in order and returns how many required ones failed.
func run(ctx context.Context, ros *routeros.Client, cfg *Config, dryRun bool) int {
	failed := 0
	for i := range cfg.Stages {
		s := &cfg.Stages[i]
		outcome, err := ensureStage(ctx, ros, s, dryRun)

		label := fmt.Sprintf("[%d/%d] %s", i+1, len(cfg.Stages), s.Name)
		switch {
		case err != nil && s.Optional:
			fmt.Printf("%s: %s (optional) — %v\n", label, outcome, err)
		case err != nil:
			fmt.Printf("%s: %s — %v\n", label, outcome, err)
			failed++
			// Stop at the first required failure. Starting a stage whose
			// dependency is down produces a second, misleading failure and
			// buries the one that matters.
			fmt.Printf("stopping: %s is required by everything after it\n", s.Name)
			return failed
		default:
			fmt.Printf("%s: %s\n", label, outcome)
		}
		if ctx.Err() != nil {
			fmt.Println("interrupted")
			return failed
		}
	}
	return failed
}

// ensureStage makes one stage true: container running, service answering.
func ensureStage(ctx context.Context, ros *routeros.Client, s *Stage, dryRun bool) (Outcome, error) {
	ct, err := ros.GetContainer(ctx, s.Container)
	if err != nil || ct == nil {
		return Missing, fmt.Errorf("container %s not found on the device", s.Container)
	}

	outcome := AlreadyUp
	if ct.Running != "true" {
		if dryRun {
			return Started, nil
		}
		if err := ros.StartContainer(ctx, ct.ID); err != nil {
			return Failed, fmt.Errorf("starting %s: %w", s.Container, err)
		}
		outcome = Started
	}

	// No readiness probe means the container running is all we can check. Say
	// nothing more than that rather than implying the service is up.
	if s.ReadyURL == "" {
		return outcome, nil
	}
	if dryRun {
		return outcome, nil
	}

	if err := waitReady(ctx, s); err != nil {
		return Failed, err
	}
	return outcome, nil
}

// waitReady polls a stage's readiness endpoint until it answers acceptably.
func waitReady(ctx context.Context, s *Stage) error {
	deadline := time.Now().Add(s.Timeout)
	client := &http.Client{Timeout: 10 * time.Second}
	var last string

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		status, body, err := probe(ctx, client, s.ReadyURL)
		switch {
		case err != nil:
			last = err.Error()
		case status != s.ReadyStatus:
			last = fmt.Sprintf("status %d, want %d: %s", status, s.ReadyStatus, firstLine(body))
		case s.ReadyContains != "" && !strings.Contains(body, s.ReadyContains):
			last = fmt.Sprintf("body does not contain %q: %s", s.ReadyContains, firstLine(body))
		default:
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("not ready after %s (%s)", s.Timeout, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

func probe(ctx context.Context, client *http.Client, url string) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	// Bounded: a readiness endpoint that answers with a stream is a readiness
	// endpoint we should not hang on.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	return resp.StatusCode, string(body), nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "stormboot: "+format+"\n", args...)
	os.Exit(1)
}
