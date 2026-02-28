package nats

import (
	"fmt"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"go.uber.org/zap"
)

// EmbeddedConfig configures the in-process NATS server.
type EmbeddedConfig struct {
	Host     string // listen address, default "0.0.0.0"
	Port     int    // listen port, default 4222
	StoreDir string // JetStream data directory
}

// EmbeddedServer wraps an in-process NATS server with JetStream enabled.
type EmbeddedServer struct {
	server *server.Server
	log    *zap.SugaredLogger
}

// NewEmbedded starts an in-process NATS server with JetStream.
func NewEmbedded(cfg EmbeddedConfig, log *zap.SugaredLogger) (*EmbeddedServer, error) {
	host := cfg.Host
	if host == "" {
		host = "0.0.0.0"
	}
	port := cfg.Port
	if port == 0 {
		port = 4222
	}
	if cfg.StoreDir == "" {
		return nil, fmt.Errorf("embedded NATS requires storeDir")
	}

	opts := &server.Options{
		Host:           host,
		Port:           port,
		JetStream:      true,
		StoreDir:       cfg.StoreDir,
		NoLog:          true, // we log via zap
		MaxPayload:     8 * 1024 * 1024,
		WriteDeadline:  10 * time.Second,
		MaxPending:     64 * 1024 * 1024,
		NoSystemAccount: true,
	}

	ns, err := server.NewServer(opts)
	if err != nil {
		return nil, fmt.Errorf("creating embedded NATS server: %w", err)
	}

	go ns.Start()

	if !ns.ReadyForConnections(10 * time.Second) {
		ns.Shutdown()
		return nil, fmt.Errorf("embedded NATS server failed to become ready")
	}

	log.Infow("embedded NATS server started",
		"host", host,
		"port", port,
		"storeDir", cfg.StoreDir,
		"clientURL", ns.ClientURL(),
	)

	return &EmbeddedServer{server: ns, log: log}, nil
}

// ClientURL returns the NATS client connection URL.
func (s *EmbeddedServer) ClientURL() string {
	return s.server.ClientURL()
}

// Shutdown gracefully stops the embedded NATS server.
func (s *EmbeddedServer) Shutdown() {
	s.log.Infow("shutting down embedded NATS server")
	s.server.Shutdown()
	s.server.WaitForShutdown()
}
