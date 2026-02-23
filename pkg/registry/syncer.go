package registry

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	"go.uber.org/zap"

	"github.com/glennswest/mkube/pkg/config"
)

// UpstreamSyncer watches for local registry pushes and mirrors them to
// the upstream registry (GHCR) as backup. It listens on the Registry's
// SyncEvents channel, which only receives events from external manifest
// PUTs (not from the ImageWatcher), preventing sync loops.
type UpstreamSyncer struct {
	cfg    config.RegistryConfig
	store  *BlobStore
	events <-chan PushEvent
	log    *zap.SugaredLogger
	token  string
}

// NewUpstreamSyncer creates a syncer that mirrors locally-pushed images
// to upstream registries. Returns nil if no token is available.
func NewUpstreamSyncer(cfg config.RegistryConfig, store *BlobStore, events <-chan PushEvent, log *zap.SugaredLogger) *UpstreamSyncer {
	token := cfg.UpstreamToken
	if token == "" && cfg.UpstreamTokenFile != "" {
		data, err := os.ReadFile(cfg.UpstreamTokenFile)
		if err != nil {
			log.Warnw("failed to read upstream token file", "path", cfg.UpstreamTokenFile, "error", err)
			return nil
		}
		token = strings.TrimSpace(string(data))
	}
	if token == "" {
		log.Info("upstream sync: no token configured, syncer disabled")
		return nil
	}

	return &UpstreamSyncer{
		cfg:    cfg,
		store:  store,
		events: events,
		log:    log.Named("upstream-sync"),
		token:  token,
	}
}

// Run processes sync events until ctx is cancelled.
func (s *UpstreamSyncer) Run(ctx context.Context) {
	s.log.Info("upstream syncer started")

	for {
		select {
		case <-ctx.Done():
			s.log.Info("upstream syncer shutting down")
			return
		case evt, ok := <-s.events:
			if !ok {
				return
			}
			s.syncImage(ctx, evt)
		}
	}
}

// syncImage pushes a locally-stored image to the upstream registry.
func (s *UpstreamSyncer) syncImage(ctx context.Context, evt PushEvent) {
	// Look up the reverse mapping: localRepo → upstream ref
	upHost, upRepo, _ := s.resolveUpstream(evt.Repo)
	if upHost == "" || upRepo == "" {
		s.log.Debugw("no upstream mapping for local repo, skipping sync", "repo", evt.Repo)
		return
	}

	upstreamRef := fmt.Sprintf("%s/%s:%s", upHost, upRepo, evt.Reference)
	s.log.Infow("syncing to upstream", "local", evt.Repo+":"+evt.Reference, "upstream", upstreamRef)

	// Pull image from local registry via crane
	localRef := fmt.Sprintf("%s/%s:%s", s.cfg.LocalAddresses[0], evt.Repo, evt.Reference)

	img, err := crane.Pull(localRef, crane.WithContext(ctx), crane.Insecure)
	if err != nil {
		s.log.Warnw("failed to pull from local registry for sync", "ref", localRef, "error", err)
		return
	}

	// Push to upstream with auth
	auth := &authn.Basic{Username: "glennswest", Password: s.token}
	dst, err := name.ParseReference(upstreamRef)
	if err != nil {
		s.log.Warnw("failed to parse upstream ref", "ref", upstreamRef, "error", err)
		return
	}

	if err := crane.Push(img, dst.String(), crane.WithContext(ctx), crane.WithAuth(auth)); err != nil {
		s.log.Warnw("failed to push to upstream", "upstream", upstreamRef, "error", err)
		return
	}

	s.log.Infow("synced to upstream", "local", evt.Repo+":"+evt.Reference, "upstream", upstreamRef)
}

// resolveUpstream finds the upstream host/repo for a local repo name
// by looking up the watchImages config (reverse mapping).
func (s *UpstreamSyncer) resolveUpstream(localRepo string) (host, repo, ref string) {
	for _, wi := range s.cfg.WatchImages {
		if wi.LocalRepo == localRepo {
			return parseUpstreamRef(wi.Upstream)
		}
	}
	return "", "", ""
}
