package stormbase

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"os"
	"strconv"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/glennswest/mkube/pkg/runtime"
	stormdpb "github.com/glennswest/mkube/pkg/stormbase/proto"
)

// ClientConfig holds connection settings for a stormd gRPC endpoint.
type ClientConfig struct {
	Address    string // "host:port"
	CACert     string // path to CA cert PEM
	ClientCert string // path to client cert PEM
	ClientKey  string // path to client key PEM
	Insecure   bool   // skip TLS entirely
}

// Client wraps a gRPC connection to a stormd node and implements
// the runtime.ContainerRuntime interface.
type Client struct {
	conn   *grpc.ClientConn
	daemon stormdpb.StormDaemonClient
}

// NewClient dials a stormd gRPC endpoint and returns a Client.
func NewClient(cfg ClientConfig) (*Client, error) {
	var opts []grpc.DialOption

	if cfg.Insecure {
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	} else {
		tlsCfg := &tls.Config{}

		if cfg.CACert != "" {
			caPEM, err := os.ReadFile(cfg.CACert)
			if err != nil {
				return nil, fmt.Errorf("reading CA cert: %w", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(caPEM) {
				return nil, fmt.Errorf("failed to parse CA cert")
			}
			tlsCfg.RootCAs = pool
		}

		if cfg.ClientCert != "" && cfg.ClientKey != "" {
			cert, err := tls.LoadX509KeyPair(cfg.ClientCert, cfg.ClientKey)
			if err != nil {
				return nil, fmt.Errorf("loading client cert: %w", err)
			}
			tlsCfg.Certificates = []tls.Certificate{cert}
		}

		opts = append(opts, grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)))
	}

	conn, err := grpc.NewClient(cfg.Address, opts...)
	if err != nil {
		return nil, fmt.Errorf("dialing stormd at %s: %w", cfg.Address, err)
	}

	return &Client{
		conn:   conn,
		daemon: stormdpb.NewStormDaemonClient(conn),
	}, nil
}

// ── ContainerRuntime implementation ──────────────────────────────────────────

func (c *Client) CreateContainer(ctx context.Context, spec runtime.ContainerSpec) error {
	req := &stormdpb.WorkloadCreateRequest{
		Name:          spec.Name,
		Kind:          "container",
		Image:         spec.Image,
		RestartPolicy: spec.RestartPolicy,
		CpuLimit:      spec.CPULimit,
		MemoryLimit:   spec.MemoryLimit,
		Command:       spec.Command,
		Env:           spec.Env,
	}

	for _, p := range spec.Ports {
		req.Ports = append(req.Ports, &stormdpb.PortMapping{
			HostPort:      p.HostPort,
			ContainerPort: p.ContainerPort,
			Protocol:      p.Protocol,
		})
	}
	for _, v := range spec.Volumes {
		req.Volumes = append(req.Volumes, &stormdpb.VolumeMount{
			Source:   v.Source,
			Target:   v.Target,
			Readonly: v.ReadOnly,
		})
	}

	resp, err := c.daemon.WorkloadCreate(ctx, req)
	if err != nil {
		return fmt.Errorf("WorkloadCreate: %w", err)
	}
	if !resp.Success {
		return fmt.Errorf("WorkloadCreate failed: %s", resp.Message)
	}
	return nil
}

func (c *Client) StartContainer(ctx context.Context, name string) error {
	resp, err := c.daemon.WorkloadStart(ctx, &stormdpb.WorkloadActionRequest{Name: name})
	if err != nil {
		return fmt.Errorf("WorkloadStart: %w", err)
	}
	if !resp.Success {
		return fmt.Errorf("WorkloadStart failed: %s", resp.Message)
	}
	return nil
}

func (c *Client) StopContainer(ctx context.Context, name string) error {
	resp, err := c.daemon.WorkloadStop(ctx, &stormdpb.WorkloadActionRequest{Name: name})
	if err != nil {
		return fmt.Errorf("WorkloadStop: %w", err)
	}
	if !resp.Success {
		return fmt.Errorf("WorkloadStop failed: %s", resp.Message)
	}
	return nil
}

func (c *Client) RemoveContainer(ctx context.Context, name string) error {
	resp, err := c.daemon.WorkloadRemove(ctx, &stormdpb.WorkloadActionRequest{Name: name})
	if err != nil {
		return fmt.Errorf("WorkloadRemove: %w", err)
	}
	if !resp.Success {
		return fmt.Errorf("WorkloadRemove failed: %s", resp.Message)
	}
	return nil
}

func (c *Client) GetContainer(ctx context.Context, name string) (*runtime.Container, error) {
	resp, err := c.daemon.WorkloadList(ctx, &stormdpb.WorkloadListRequest{})
	if err != nil {
		return nil, fmt.Errorf("WorkloadList: %w", err)
	}
	for _, w := range resp.Workloads {
		if w.Name == name {
			return workloadToContainer(w), nil
		}
	}
	return nil, fmt.Errorf("container %q not found", name)
}

func (c *Client) ListContainers(ctx context.Context) ([]runtime.Container, error) {
	resp, err := c.daemon.WorkloadList(ctx, &stormdpb.WorkloadListRequest{})
	if err != nil {
		return nil, fmt.Errorf("WorkloadList: %w", err)
	}
	out := make([]runtime.Container, 0, len(resp.Workloads))
	for _, w := range resp.Workloads {
		if w.Kind == "container" || w.Kind == "" {
			out = append(out, *workloadToContainer(w))
		}
	}
	return out, nil
}

func (c *Client) GetLogs(ctx context.Context, name string) ([]runtime.LogEntry, error) {
	stream, err := c.daemon.WorkloadLogs(ctx, &stormdpb.WorkloadLogsRequest{
		Name: name,
		Tail: 100,
	})
	if err != nil {
		return nil, fmt.Errorf("WorkloadLogs: %w", err)
	}

	var entries []runtime.LogEntry
	for {
		entry, err := stream.Recv()
		if err != nil {
			break
		}
		entries = append(entries, runtime.LogEntry{
			Timestamp: entry.Timestamp,
			Stream:    entry.Stream,
			Message:   entry.Message,
		})
	}
	return entries, nil
}

func (c *Client) GetSystemResource(ctx context.Context) (*runtime.SystemResource, error) {
	resp, err := c.daemon.NodeStatus(ctx, &stormdpb.NodeStatusRequest{})
	if err != nil {
		return nil, fmt.Errorf("NodeStatus: %w", err)
	}
	return &runtime.SystemResource{
		Hostname:      resp.Hostname,
		Uptime:        strconv.FormatUint(resp.UptimeSecs, 10) + "s",
		TotalMemory:   strconv.FormatUint(resp.MemoryTotal, 10),
		FreeMemory:    strconv.FormatUint(resp.MemoryAvailable, 10),
		DiskTotal:     resp.DiskTotal,
		DiskAvailable: resp.DiskAvailable,
		WorkloadCount: resp.WorkloadCount,
		RunningCount:  resp.RunningCount,
		Platform:      "stormbase",
	}, nil
}

func (c *Client) UploadFile(ctx context.Context, path string, _ io.Reader) error {
	// StormBase manages images via ImagePull/ImageEnsure RPCs, not file upload.
	// Ensure the image is available on the node.
	resp, err := c.daemon.ImageEnsure(ctx, &stormdpb.ImageEnsureRequest{
		Reference: path,
	})
	if err != nil {
		return fmt.Errorf("ImageEnsure: %w", err)
	}
	if !resp.Available {
		return fmt.Errorf("ImageEnsure: image not available: %s", resp.Message)
	}
	return nil
}

func (c *Client) RemoveFile(_ context.Context, _ string) error {
	// No-op for StormBase — image GC is handled internally by stormd.
	return nil
}

func (c *Client) CreateMount(_ context.Context, _, _, _ string) error {
	return nil // volumes are passed inline via CreateContainer spec
}

func (c *Client) RemoveMountsByList(_ context.Context, _ string) error {
	return nil // stormd manages volume cleanup
}

func (c *Client) Backend() string {
	return "stormbase"
}

func (c *Client) Close() error {
	return c.conn.Close()
}

// GRPCClient returns the underlying gRPC client for operations not covered
// by ContainerRuntime (mesh updates, service discovery, network policy, etc.).
func (c *Client) GRPCClient() stormdpb.StormDaemonClient {
	return c.daemon
}

// IsNodeCordoned returns whether the remote stormd node is cordoned
// and the reason string. Returns (false, "") on error.
func (c *Client) IsNodeCordoned(ctx context.Context) (bool, string) {
	resp, err := c.daemon.NodeStatus(ctx, &stormdpb.NodeStatusRequest{})
	if err != nil {
		return false, ""
	}
	return resp.Cordoned, resp.CordonReason
}

func workloadToContainer(w *stormdpb.WorkloadInfo) *runtime.Container {
	return &runtime.Container{
		ID:          w.ContainerId,
		Name:        w.Name,
		Status:      w.Status,
		StartOnBoot: boolStr(w.RestartPolicy == "always"),
	}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// Ensure Client implements ContainerRuntime at compile time.
var _ runtime.ContainerRuntime = (*Client)(nil)
