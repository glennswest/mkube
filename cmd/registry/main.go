// mkube-registry: Standalone OCI registry for mkube infrastructure.
//
// Runs the OCI Distribution v2 registry on :5000, polls GHCR for new image
// digests (ImageWatcher), and optionally syncs local pushes upstream
// (UpstreamSyncer). Push events are forwarded to mkube via HTTP webhook.
//
// This container boots before mkube in the boot order, solving the
// chicken-and-egg problem where mkube needs the registry to pull its
// own image.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"
	"gopkg.in/yaml.v3"

	"github.com/glennswest/mkube/pkg/config"
	"github.com/glennswest/mkube/pkg/registry"
)

var version = "dev"

func main() {
	logger, _ := zap.NewProduction()
	defer func() { _ = logger.Sync() }()
	log := logger.Sugar()

	log.Infow("starting mkube-registry", "version", version)

	configPath := "/etc/registry/config.yaml"
	if v := os.Getenv("REGISTRY_CONFIG"); v != "" {
		configPath = v
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		log.Fatalw("reading config", "path", configPath, "error", err)
	}

	var cfg config.RegistryConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		log.Fatalw("parsing config", "error", err)
	}

	// Apply defaults
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = ":5000"
	}
	if cfg.StorePath == "" {
		cfg.StorePath = "/raid1/registry"
	}
	if cfg.WatchPollSeconds == 0 {
		cfg.WatchPollSeconds = 120
	}
	cfg.Enabled = true

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Start registry (HTTP server + blob store)
	reg, err := registry.Start(ctx, cfg, log)
	if err != nil {
		log.Fatalw("starting registry", "error", err)
	}
	defer func() { _ = reg.Shutdown(ctx) }()
	log.Infow("registry started", "addr", cfg.ListenAddr, "store", cfg.StorePath)

	// Webhook: drain PushEvents and POST to mkube's push-notify endpoint
	if cfg.NotifyURL != "" {
		go webhookForwarder(ctx, reg.PushEvents, cfg.NotifyURL, log)
	} else {
		// Drain events to prevent channel backup
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-reg.PushEvents:
				}
			}
		}()
	}

	// Image Watcher: poll GHCR for new digests and mirror locally
	var watcher *registry.ImageWatcher
	if len(cfg.WatchImages) > 0 {
		watcher = registry.NewImageWatcher(cfg, reg.Store(), reg.PushEvents, log)
		go watcher.Run(ctx)
		log.Infow("image watcher started", "images", len(cfg.WatchImages))
	}

	// Upstream Syncer: mirror local pushes to GHCR
	if cfg.UpstreamSyncEnabled {
		syncer := registry.NewUpstreamSyncer(cfg, reg.Store(), reg.SyncEvents, log)
		if syncer != nil {
			go syncer.Run(ctx)
			log.Info("upstream syncer started")
		}
	}

	// Management API on a separate port
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "ok")
	})
	mux.HandleFunc("GET /healthz/watch", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher.Flush()

		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

		// Send initial heartbeat immediately
		fmt.Fprintf(w, "data: %d\n\n", time.Now().Unix())
		flusher.Flush()

		for {
			select {
			case <-r.Context().Done():
				return
			case t := <-ticker.C:
				_, err := fmt.Fprintf(w, "data: %d\n\n", t.Unix())
				if err != nil {
					return
				}
				flusher.Flush()
			}
		}
	})
	mux.HandleFunc("POST /poll", func(w http.ResponseWriter, r *http.Request) {
		if watcher == nil {
			http.NotFound(w, r)
			return
		}
		watcher.TriggerPoll()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}` + "\n"))
	})

	mgmtSrv := &http.Server{Addr: ":5001", Handler: mux}
	go func() {
		log.Info("management API listening on :5001")
		if err := mgmtSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Errorw("management server error", "error", err)
		}
	}()
	go func() {
		<-ctx.Done()
		shutdownCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = mgmtSrv.Shutdown(shutdownCtx)
	}()

	log.Info("registry ready, waiting for signal")
	<-ctx.Done()
	log.Info("shutting down")
}

// webhookForwarder drains PushEvents and POSTs each to the mkube push-notify endpoint.
func webhookForwarder(ctx context.Context, events <-chan registry.PushEvent, notifyURL string, log *zap.SugaredLogger) {
	client := &http.Client{Timeout: 10 * time.Second}

	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-events:
			if !ok {
				return
			}
			image := evt.Repo
			if evt.Reference != "" {
				image += ":" + evt.Reference
			}

			body, _ := json.Marshal(map[string]string{"image": image})

			req, err := http.NewRequestWithContext(ctx, "POST", notifyURL, bytes.NewReader(body))
			if err != nil {
				log.Warnw("webhook request build failed", "error", err)
				continue
			}
			req.Header.Set("Content-Type", "application/json")

			resp, err := client.Do(req)
			if err != nil {
				log.Warnw("webhook POST failed", "url", notifyURL, "image", image, "error", err)
				continue
			}
			resp.Body.Close()

			if resp.StatusCode >= 400 {
				log.Warnw("webhook returned error", "url", notifyURL, "image", image, "status", resp.StatusCode)
			} else {
				log.Infow("webhook delivered", "image", image)
			}
		}
	}
}
