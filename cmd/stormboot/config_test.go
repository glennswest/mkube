package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadAppliesDefaults(t *testing.T) {
	c, err := Load(write(t, `
stages:
  - name: storage
    container: infra_stormblockmk_stormblockmk
    readyURL: http://192.168.200.21:9090/mk/v1/ready
    readyContains: '"ready":true'
  - container: kube.gt.lo
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := c.Stages[0].ReadyStatus; got != 200 {
		t.Errorf("default ReadyStatus = %d, want 200", got)
	}
	if got := c.Stages[0].Timeout; got != DefaultStageTimeout {
		t.Errorf("default Timeout = %s, want %s", got, DefaultStageTimeout)
	}
	// A stage with no name is identified by its container rather than being
	// reported as an empty string in the log.
	if got := c.Stages[1].Name; got != "kube.gt.lo" {
		t.Errorf("Name defaulted to %q, want the container name", got)
	}
}

func TestLoadRejectsNothingToDo(t *testing.T) {
	if _, err := Load(write(t, "stages: []\n")); err == nil {
		t.Fatal("expected an error for a config with no stages")
	}
}

func TestLoadRejectsMissingContainer(t *testing.T) {
	if _, err := Load(write(t, "stages:\n  - name: storage\n")); err == nil {
		t.Fatal("expected an error for a stage with no container")
	}
}

// The ordering is the whole point, so a container listed twice is a config
// that cannot express what its author meant.
func TestLoadRejectsDuplicateContainer(t *testing.T) {
	_, err := Load(write(t, `
stages:
  - container: kube.gt.lo
  - container: kube.gt.lo
`))
	if err == nil {
		t.Fatal("expected an error for a container listed twice")
	}
}

func TestExplicitTimeoutSurvives(t *testing.T) {
	c, err := Load(write(t, "stages:\n  - container: a\n    timeout: 90s\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := c.Stages[0].Timeout; got != 90*time.Second {
		t.Errorf("Timeout = %s, want 90s", got)
	}
}
