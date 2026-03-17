package provider

import (
	"context"
	"encoding/json"
	"time"

	"github.com/glennswest/mkube/pkg/store"
)

// RunResourceWatchers starts NATS KV watchers for BMH, Deployment, Network,
// Job, and JobRunner buckets. Each watcher updates in-memory state and triggers
// the appropriate kick channel on changes from other nodes or API consumers.
func (p *MicroKubeProvider) RunResourceWatchers(ctx context.Context) {
	if p.deps.Store == nil {
		p.deps.Logger.Info("resource watchers: no NATS store, skipping")
		return
	}

	p.deps.Logger.Info("resource watchers starting")

	go p.watchDeploymentBucket(ctx)
	go p.watchBMHBucket(ctx)
	go p.watchNetworkBucket(ctx)
	go p.watchJobBucket(ctx)
	go p.watchJobRunnerBucket(ctx)
}

// watchDeploymentBucket watches NATS Deployments bucket and triggers reconcile on changes.
func (p *MicroKubeProvider) watchDeploymentBucket(ctx context.Context) {
	log := p.deps.Logger.Named("watch-deployments")
	for {
		if err := p.runDeploymentWatch(ctx); err != nil {
			log.Warnw("deployment watch error, retrying", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func (p *MicroKubeProvider) runDeploymentWatch(ctx context.Context) error {
	if p.deps.Store.Deployments == nil {
		return nil
	}
	events, err := p.deps.Store.Deployments.WatchAll(ctx)
	if err != nil {
		return err
	}
	p.deps.Logger.Info("deployment watcher started")
	for {
		select {
		case <-ctx.Done():
			return nil
		case evt, ok := <-events:
			if !ok {
				return nil
			}
			switch evt.Type {
			case store.EventPut, store.EventUpdate:
				var deploy Deployment
				if err := json.Unmarshal(evt.Value, &deploy); err != nil {
					continue
				}
				key := deploy.Namespace + "/" + deploy.Name
				p.mu.Lock()
				p.deployments[key] = &deploy
				p.mu.Unlock()
				p.triggerReconcile()
			case store.EventDelete:
				ns, name := parseStoreKey(evt.Key)
				key := ns + "/" + name
				p.mu.Lock()
				delete(p.deployments, key)
				p.mu.Unlock()
				p.triggerReconcile()
			}
		}
	}
}

// watchBMHBucket watches NATS BareMetalHosts bucket and triggers scheduler on changes.
func (p *MicroKubeProvider) watchBMHBucket(ctx context.Context) {
	log := p.deps.Logger.Named("watch-bmh")
	for {
		if err := p.runBMHWatch(ctx); err != nil {
			log.Warnw("BMH watch error, retrying", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func (p *MicroKubeProvider) runBMHWatch(ctx context.Context) error {
	if p.deps.Store.BareMetalHosts == nil {
		return nil
	}
	events, err := p.deps.Store.BareMetalHosts.WatchAll(ctx)
	if err != nil {
		return err
	}
	p.deps.Logger.Info("BMH watcher started")
	for {
		select {
		case <-ctx.Done():
			return nil
		case evt, ok := <-events:
			if !ok {
				return nil
			}
			switch evt.Type {
			case store.EventPut, store.EventUpdate:
				var bmh BareMetalHost
				if err := json.Unmarshal(evt.Value, &bmh); err != nil {
					continue
				}
				key := bmh.Namespace + "/" + bmh.Name
				p.mu.Lock()
				p.bareMetalHosts[key] = &bmh
				p.mu.Unlock()
				p.triggerScheduler()
			case store.EventDelete:
				ns, name := parseStoreKey(evt.Key)
				key := ns + "/" + name
				p.mu.Lock()
				delete(p.bareMetalHosts, key)
				p.mu.Unlock()
				p.triggerScheduler()
			}
		}
	}
}

// watchNetworkBucket watches NATS Networks bucket, rebuilds DHCP index,
// and re-seeds DNS config for managed networks.
func (p *MicroKubeProvider) watchNetworkBucket(ctx context.Context) {
	log := p.deps.Logger.Named("watch-networks")
	for {
		if err := p.runNetworkWatch(ctx); err != nil {
			log.Warnw("network watch error, retrying", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func (p *MicroKubeProvider) runNetworkWatch(ctx context.Context) error {
	if p.deps.Store.Networks == nil {
		return nil
	}
	events, err := p.deps.Store.Networks.WatchAll(ctx)
	if err != nil {
		return err
	}
	p.deps.Logger.Info("network watcher started")
	for {
		select {
		case <-ctx.Done():
			return nil
		case evt, ok := <-events:
			if !ok {
				return nil
			}
			switch evt.Type {
			case store.EventPut, store.EventUpdate:
				var net Network
				if err := json.Unmarshal(evt.Value, &net); err != nil {
					continue
				}
				p.mu.Lock()
				p.networks[net.Name] = &net
				p.rebuildDHCPIndex()
				p.mu.Unlock()
				p.triggerNetworkReseed(net.Name)
				p.triggerReconcile()
			case store.EventDelete:
				_, name := parseStoreKey(evt.Key)
				p.mu.Lock()
				delete(p.networks, name)
				p.rebuildDHCPIndex()
				p.mu.Unlock()
				p.triggerReconcile()
			}
		}
	}
}

// watchJobBucket watches NATS Jobs bucket and triggers the scheduler on changes.
func (p *MicroKubeProvider) watchJobBucket(ctx context.Context) {
	log := p.deps.Logger.Named("watch-jobs")
	for {
		if err := p.runJobWatch(ctx); err != nil {
			log.Warnw("job watch error, retrying", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func (p *MicroKubeProvider) runJobWatch(ctx context.Context) error {
	if p.deps.Store.Jobs == nil {
		return nil
	}
	events, err := p.deps.Store.Jobs.WatchAll(ctx)
	if err != nil {
		return err
	}
	p.deps.Logger.Info("job watcher started")
	for {
		select {
		case <-ctx.Done():
			return nil
		case evt, ok := <-events:
			if !ok {
				return nil
			}
			switch evt.Type {
			case store.EventPut, store.EventUpdate:
				var job Job
				if err := json.Unmarshal(evt.Value, &job); err != nil {
					continue
				}
				key := job.Namespace + "/" + job.Name
				p.mu.Lock()
				p.jobs[key] = &job
				p.mu.Unlock()
				p.triggerScheduler()
			case store.EventDelete:
				ns, name := parseStoreKey(evt.Key)
				key := ns + "/" + name
				p.mu.Lock()
				delete(p.jobs, key)
				p.mu.Unlock()
				p.triggerScheduler()
			}
		}
	}
}

// watchJobRunnerBucket watches NATS JobRunners bucket and triggers the scheduler.
func (p *MicroKubeProvider) watchJobRunnerBucket(ctx context.Context) {
	log := p.deps.Logger.Named("watch-jobrunners")
	for {
		if err := p.runJobRunnerWatch(ctx); err != nil {
			log.Warnw("jobrunner watch error, retrying", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func (p *MicroKubeProvider) runJobRunnerWatch(ctx context.Context) error {
	if p.deps.Store.JobRunners == nil {
		return nil
	}
	events, err := p.deps.Store.JobRunners.WatchAll(ctx)
	if err != nil {
		return err
	}
	p.deps.Logger.Info("jobrunner watcher started")
	for {
		select {
		case <-ctx.Done():
			return nil
		case evt, ok := <-events:
			if !ok {
				return nil
			}
			switch evt.Type {
			case store.EventPut, store.EventUpdate:
				var jr JobRunner
				if err := json.Unmarshal(evt.Value, &jr); err != nil {
					continue
				}
				p.mu.Lock()
				p.jobRunners[jr.Name] = &jr
				p.mu.Unlock()
				p.triggerScheduler()
			case store.EventDelete:
				_, name := parseStoreKey(evt.Key)
				p.mu.Lock()
				delete(p.jobRunners, name)
				p.mu.Unlock()
				p.triggerScheduler()
			}
		}
	}
}

// triggerNetworkReseed runs seedDNSConfig in the background for a managed network.
// Uses an atomic guard to prevent unbounded goroutine growth when events arrive
// faster than seeds complete.
func (p *MicroKubeProvider) triggerNetworkReseed(networkName string) {
	p.mu.RLock()
	net, ok := p.networks[networkName]
	p.mu.RUnlock()
	if !ok || !net.Spec.Managed || net.Spec.ExternalDNS {
		return
	}
	if !p.reseedRunning.CompareAndSwap(false, true) {
		p.deps.Logger.Debugw("network reseed already in progress, skipping", "network", networkName)
		return
	}
	go func() {
		defer p.reseedRunning.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		p.seedDNSConfig(ctx, net)
	}()
}
