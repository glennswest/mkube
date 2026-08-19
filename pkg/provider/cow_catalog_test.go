package provider

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// With the pivot in the stub the image's entrypoint must be handed over
// untouched. Rewriting argv[0] to /payload/... is the old behaviour and fixes
// only argv[0] — not a path the program opens for itself, not the loader a
// dynamic binary needs. That is why a CoW pod under it came up and exited
// immediately looking like a missing binary.
func TestRewriteEntrypointForCoWWithPivot(t *testing.T) {
	c := &corev1.Container{}
	img := &dockerSaveConfig{
		Entrypoint: []string{"/stormd"},
		Cmd:        []string{"--config", "/etc/stormd/config.toml"},
	}

	ep, cmd := rewriteEntrypointForCoW(nil, c, img, true)
	if ep != cowPivotPath {
		t.Errorf("entrypoint = %q, want the pivot %q", ep, cowPivotPath)
	}
	// The payload, then the image's own entrypoint and args, unmodified.
	want := cowPayloadDst + " /stormd --config /etc/stormd/config.toml"
	if cmd != want {
		t.Errorf("cmd = %q, want %q", cmd, want)
	}
	if strings.Contains(cmd, cowPayloadDst+"/stormd") {
		t.Error("the image's argv[0] was rewritten; the pivot exists so it should not be")
	}
}

// Without the pivot — an mkube image built before it existed — the old
// rewrite still applies, so an upgrade does not strand pods created by the
// previous build.
func TestRewriteEntrypointForCoWWithoutPivot(t *testing.T) {
	c := &corev1.Container{}
	img := &dockerSaveConfig{Entrypoint: []string{"/stormd"}, Cmd: []string{"--config", "/etc/x"}}

	ep, cmd := rewriteEntrypointForCoW(nil, c, img, false)
	if ep != cowPayloadDst+"/stormd" {
		t.Errorf("entrypoint = %q, want the old %s/stormd rewrite", ep, cowPayloadDst)
	}
	if cmd != "--config /etc/x" {
		t.Errorf("cmd = %q, want the args unchanged", cmd)
	}
}

// A pod command overrides the image, and must reach the pivot the same way.
func TestRewriteEntrypointForCoWPrefersPodCommand(t *testing.T) {
	c := &corev1.Container{Command: []string{"/bin/sh", "-c", "echo hi"}}
	ep, cmd := rewriteEntrypointForCoW(nil, c, nil, true)
	if ep != cowPivotPath {
		t.Errorf("entrypoint = %q, want the pivot", ep)
	}
	if cmd != cowPayloadDst+" /bin/sh -c echo hi" {
		t.Errorf("cmd = %q", cmd)
	}
}

// Nothing to run is a configuration error, with or without the pivot.
func TestRewriteEntrypointForCoWNoEntrypoint(t *testing.T) {
	for _, pivot := range []bool{true, false} {
		if ep, _ := rewriteEntrypointForCoW(nil, &corev1.Container{}, nil, pivot); ep != "" {
			t.Errorf("pivot=%v: expected no entrypoint, got %q", pivot, ep)
		}
	}
}
