package main

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/glennswest/mkube/pkg/config"
)

// Config is the whole of what stormboot needs to bring a device up.
//
// Deliberately a list of stages rather than hard-coded knowledge of
// stormblockmk, sbregistry and mkube: the ordering is the point, and the
// ordering is policy. A device that grows a fourth thing that has to be up
// before the controller adds a stage, it does not add a code path.
type Config struct {
	RouterOS config.RouterOSConfig `yaml:"routeros"`
	Stages   []Stage               `yaml:"stages"`
}

// Stage is one component that must be up, and how to know that it is.
type Stage struct {
	// Name is what appears in the log. Free-form.
	Name string `yaml:"name"`

	// Container is the RouterOS container name, exactly as `/container print`
	// reports it (e.g. `infra_stormblockmk_stormblockmk`).
	Container string `yaml:"container"`

	// ReadyURL is polled until it answers acceptably. This is the difference
	// between "the container is running" and "the thing inside it is serving",
	// and the ordering is only meaningful in terms of the second — starting
	// sbregistry the instant stormblockmk's container exists, rather than when
	// its API answers, just moves the failure later.
	ReadyURL string `yaml:"readyURL"`

	// ReadyStatus is the HTTP status that counts as ready. Default 200.
	//
	// Needed because these services disagree: stormblockmk's /mk/v1/ready
	// answers 503 with a JSON body listing blockers until every export is
	// wired, while sbregistry's /readyz answers 200/503 the usual way.
	ReadyStatus int `yaml:"readyStatus"`

	// ReadyContains, when set, must appear in the response body as well. For
	// an endpoint that answers 200 whatever its state, the body is the only
	// signal — mkube's /healthz says "ok" and then reports its commit.
	ReadyContains string `yaml:"readyContains"`

	// Timeout bounds the wait for this stage. Zero means DefaultStageTimeout.
	Timeout time.Duration `yaml:"timeout"`

	// Optional stages are waited for but do not stop the sequence. For
	// something a device can run without, this is how to say so.
	Optional bool `yaml:"optional"`
}

const (
	// DefaultStageTimeout is how long a stage may take to become ready.
	// Generous on purpose: stormblockmk restores its volume metadata and
	// re-wires every export before it reports ready, which is not fast on a
	// device with a lot of volumes.
	DefaultStageTimeout = 5 * time.Minute

	// pollInterval is how often a stage's readiness is re-checked.
	pollInterval = 3 * time.Second
)

// Load reads and validates a config file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var c Config
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

func (c *Config) validate() error {
	if len(c.Stages) == 0 {
		return fmt.Errorf("no stages: stormboot has nothing to bring up")
	}
	seen := make(map[string]bool, len(c.Stages))
	for i := range c.Stages {
		s := &c.Stages[i]
		if s.Name == "" {
			s.Name = s.Container
		}
		if s.Container == "" {
			return fmt.Errorf("stage %q: container is required", s.Name)
		}
		if seen[s.Container] {
			return fmt.Errorf("stage %q: container %s appears twice", s.Name, s.Container)
		}
		seen[s.Container] = true
		if s.ReadyStatus == 0 {
			s.ReadyStatus = 200
		}
		if s.Timeout == 0 {
			s.Timeout = DefaultStageTimeout
		}
	}
	return nil
}
