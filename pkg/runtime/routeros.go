package runtime

import (
	"context"
	"io"

	"github.com/glennswest/mkube/pkg/routeros"
)

// RouterOSRuntime adapts routeros.Client to the ContainerRuntime interface.
type RouterOSRuntime struct {
	client *routeros.Client
}

// NewRouterOSRuntime wraps an existing routeros.Client.
func NewRouterOSRuntime(client *routeros.Client) *RouterOSRuntime {
	return &RouterOSRuntime{client: client}
}

func (r *RouterOSRuntime) CreateContainer(ctx context.Context, spec ContainerSpec) error {
	return r.client.CreateContainer(ctx, routeros.ContainerSpec{
		Name:        spec.Name,
		File:        spec.Image,
		RemoteImage: spec.Tag,
		Interface:   spec.Interface,
		RootDir:     spec.RootDir,
		MountLists:  spec.MountLists,
		Cmd:         spec.Cmd,
		Entrypoint:  spec.Entrypoint,
		WorkDir:     spec.WorkDir,
		Hostname:    spec.Hostname,
		DNS:         spec.DNS,
		User:        spec.User,
		Logging:     spec.Logging,
		StartOnBoot: spec.StartOnBoot,
	})
}

func (r *RouterOSRuntime) StartContainer(ctx context.Context, id string) error {
	return r.client.StartContainer(ctx, id)
}

func (r *RouterOSRuntime) StopContainer(ctx context.Context, id string) error {
	return r.client.StopContainer(ctx, id)
}

func (r *RouterOSRuntime) RemoveContainer(ctx context.Context, id string) error {
	return r.client.RemoveContainer(ctx, id)
}

func (r *RouterOSRuntime) GetContainer(ctx context.Context, name string) (*Container, error) {
	ct, err := r.client.GetContainer(ctx, name)
	if err != nil {
		return nil, err
	}
	return rosContainerToRuntime(ct), nil
}

func (r *RouterOSRuntime) ListContainers(ctx context.Context) ([]Container, error) {
	containers, err := r.client.ListContainers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Container, len(containers))
	for i, ct := range containers {
		out[i] = *rosContainerToRuntime(&ct)
	}
	return out, nil
}

func (r *RouterOSRuntime) GetLogs(ctx context.Context, _ string) ([]LogEntry, error) {
	logs, err := r.client.GetLogs(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]LogEntry, len(logs))
	for i, l := range logs {
		out[i] = LogEntry{
			Timestamp: l.Time,
			Message:   l.Message,
		}
	}
	return out, nil
}

func (r *RouterOSRuntime) GetSystemResource(ctx context.Context) (*SystemResource, error) {
	sr, err := r.client.GetSystemResource(ctx)
	if err != nil {
		return nil, err
	}
	return &SystemResource{
		Uptime:       sr.Uptime,
		CPUCount:     sr.CPUCount,
		CPULoad:      sr.CPULoad,
		FreeMemory:   sr.FreeMemory,
		TotalMemory:  sr.TotalMemory,
		Architecture: sr.Architecture,
		BoardName:    sr.BoardName,
		Version:      sr.Version,
		Platform:     sr.Platform,
	}, nil
}

func (r *RouterOSRuntime) UploadFile(ctx context.Context, path string, data io.Reader) error {
	return r.client.UploadFile(ctx, path, data)
}

func (r *RouterOSRuntime) RemoveFile(ctx context.Context, path string) error {
	return r.client.RemoveFile(ctx, path)
}

func (r *RouterOSRuntime) CreateMount(ctx context.Context, name, src, dst string) error {
	return r.client.CreateMount(ctx, name, src, dst)
}

func (r *RouterOSRuntime) RemoveMountsByList(ctx context.Context, listName string) error {
	return r.client.RemoveMountsByList(ctx, listName)
}

// ─── iSCSI Operations (via ROSE /disk) ──────────────────────────────────────

func (r *RouterOSRuntime) CreateISCSITarget(ctx context.Context, name, filePath string) (string, error) {
	return r.client.CreateISCSITarget(ctx, name, filePath)
}

func (r *RouterOSRuntime) RemoveISCSITarget(ctx context.Context, id string) error {
	return r.client.RemoveISCSITarget(ctx, id)
}

func (r *RouterOSRuntime) GetISCSIDisk(ctx context.Context, id string) (*routeros.FileDisk, error) {
	return r.client.GetISCSIDisk(ctx, id)
}

func (r *RouterOSRuntime) Backend() string {
	return "routeros"
}

func (r *RouterOSRuntime) Close() error {
	return r.client.Close()
}

// Client returns the underlying routeros.Client for code that still needs
// direct access (discovery, storage, lifecycle during migration).
func (r *RouterOSRuntime) Client() *routeros.Client {
	return r.client
}

func rosContainerToRuntime(ct *routeros.Container) *Container {
	status := "stopped"
	if ct.IsRunning() {
		status = "running"
	}
	return &Container{
		ID:          ct.ID,
		Name:        ct.Name,
		Image:       ct.Image,
		Tag:         ct.Tag,
		Interface:   ct.Interface,
		RootDir:     ct.RootDir,
		Status:      status,
		Hostname:    ct.Hostname,
		DNS:         ct.DNS,
		MountLists:  ct.MountLists,
		Cmd:         ct.Cmd,
		Entrypoint:  ct.Entrypoint,
		WorkDir:     ct.WorkDir,
		Logging:     ct.Logging,
		StartOnBoot: ct.StartOnBoot,
	}
}

// Ensure RouterOSRuntime implements ContainerRuntime at compile time.
var _ ContainerRuntime = (*RouterOSRuntime)(nil)
