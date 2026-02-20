package runtime

import (
	"context"
	"errors"
	"io"
)

// ErrNotFound is returned when a container does not exist.
var ErrNotFound = errors.New("container not found")

// ContainerRuntime abstracts container lifecycle operations across
// different backends (RouterOS REST API, StormBase gRPC, etc.).
// The provider uses this interface instead of a specific client.
type ContainerRuntime interface {
	// Container lifecycle
	CreateContainer(ctx context.Context, spec ContainerSpec) error
	StartContainer(ctx context.Context, id string) error
	StopContainer(ctx context.Context, id string) error
	RemoveContainer(ctx context.Context, id string) error
	GetContainer(ctx context.Context, name string) (*Container, error)
	ListContainers(ctx context.Context) ([]Container, error)

	// Observability
	GetLogs(ctx context.Context, name string) ([]LogEntry, error)
	GetSystemResource(ctx context.Context) (*SystemResource, error)

	// File operations for image storage
	UploadFile(ctx context.Context, path string, data io.Reader) error
	RemoveFile(ctx context.Context, path string) error

	// Mount operations (RouterOS-specific, no-op on other backends)
	CreateMount(ctx context.Context, name, src, dst string) error
	RemoveMountsByList(ctx context.Context, listName string) error

	// Backend identification
	Backend() string // "routeros" or "stormbase"

	Close() error
}

// Container represents a running or stopped container.
type Container struct {
	ID        string
	Name      string
	Image     string
	Interface string
	RootDir   string
	Status    string // "running", "stopped", "pending", "failed"
	Hostname  string
	DNS       string

	// RouterOS-specific fields (populated by RouterOS adapter)
	Tag         string
	MountLists  string
	Cmd         string
	Entrypoint  string
	WorkDir     string
	Logging     string
	StartOnBoot string
}

// IsRunning returns true if the container is currently running.
func (c Container) IsRunning() bool {
	return c.Status == "running"
}

// IsStopped returns true if the container is stopped.
func (c Container) IsStopped() bool {
	return c.Status == "stopped"
}

// ContainerSpec describes a container to create.
type ContainerSpec struct {
	Name        string
	Image       string // tarball path (RouterOS) or image reference (StormBase)
	Tag         string // registry image ref (RouterOS alternative to Image)
	Interface   string
	RootDir     string
	MountLists  string
	Cmd         string
	Entrypoint  string
	WorkDir     string
	Hostname    string
	DNS         string
	Logging     string
	StartOnBoot string

	// StormBase-specific fields
	RestartPolicy string
	CPULimit      string
	MemoryLimit   string
	Command       []string
	Env           []string
	Ports         []PortMapping
	Volumes       []VolumeMount
}

// PortMapping maps a host port to a container port.
type PortMapping struct {
	HostPort      uint32
	ContainerPort uint32
	Protocol      string // "tcp" or "udp"
}

// VolumeMount describes a bind mount.
type VolumeMount struct {
	Source   string
	Target   string
	ReadOnly bool
}

// LogEntry represents a log line from a container.
type LogEntry struct {
	Timestamp string
	Stream    string
	Message   string
}

// SystemResource represents system resource information.
type SystemResource struct {
	Hostname      string
	Uptime        string
	CPUCount      string
	CPULoad       string
	FreeMemory    string
	TotalMemory   string
	Architecture  string
	BoardName     string
	Version       string
	Platform      string
	DiskTotal     uint64
	DiskAvailable uint64
	WorkloadCount uint32
	RunningCount  uint32
}
