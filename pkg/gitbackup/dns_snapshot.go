package gitbackup

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/glennswest/mkube/pkg/config"
	"github.com/glennswest/mkube/pkg/dns"
)

// DNSSnapshotter manages debounced git-backed snapshots of microdns state.
// On each DNS/DHCP mutation, it debounces by network and pushes a full
// config snapshot to a per-network git repo (e.g. microdns-gt, microdns-gw).
type DNSSnapshotter struct {
	cfg       config.GitBackupConfig
	dnsClient *dns.Client
	client    *rust4gitClient
	log       *zap.SugaredLogger

	mu       sync.Mutex
	timers   map[string]*time.Timer // network -> pending debounce timer
	inflight map[string]bool        // network -> snapshot currently running
	pending  map[string]*dnsChange  // network -> change queued during inflight
	closed   bool
}

// dnsChange captures the parameters needed to snapshot a network.
type dnsChange struct {
	endpoint string
	zone     string
}

// NewDNSSnapshotter creates a new DNS config snapshotter.
// It reuses the rust4git connection settings from GitBackupConfig.
func NewDNSSnapshotter(cfg config.GitBackupConfig, dnsClient *dns.Client, log *zap.SugaredLogger) *DNSSnapshotter {
	password := resolvePassword(cfg)

	client := newClient(
		cfg.RepoURL, "", cfg.Branch,
		cfg.CommitAuthor, cfg.CommitEmail,
		cfg.Username, password, cfg.PasswordFile,
		cfg.InsecureTLS,
	)

	return &DNSSnapshotter{
		cfg:       cfg,
		dnsClient: dnsClient,
		client:    client,
		log:       log.Named("dns-snapshot"),
		timers:    make(map[string]*time.Timer),
		inflight:  make(map[string]bool),
		pending:   make(map[string]*dnsChange),
	}
}

// NotifyChange signals that a DNS/DHCP mutation occurred on the given network.
// It resets a per-network debounce timer. When the timer fires, a full
// snapshot of the network's microdns state is pushed to git.
func (s *DNSSnapshotter) NotifyChange(networkName, endpoint, zone string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return
	}

	debounce := time.Duration(s.cfg.DNSSnapshotDebounce) * time.Second
	if debounce <= 0 {
		debounce = 5 * time.Second
	}

	// If a snapshot is already running for this network, queue a re-run
	if s.inflight[networkName] {
		s.pending[networkName] = &dnsChange{endpoint: endpoint, zone: zone}
		return
	}

	// Reset or create debounce timer
	if t, ok := s.timers[networkName]; ok {
		t.Stop()
	}

	change := &dnsChange{endpoint: endpoint, zone: zone}
	s.timers[networkName] = time.AfterFunc(debounce, func() {
		s.fireSnapshot(networkName, change)
	})
}

// fireSnapshot starts a snapshot goroutine with inflight tracking.
func (s *DNSSnapshotter) fireSnapshot(networkName string, change *dnsChange) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.inflight[networkName] = true
	delete(s.timers, networkName)
	s.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		s.snapshot(ctx, networkName, change.endpoint, change.zone)

		s.mu.Lock()
		delete(s.inflight, networkName)

		// If a change arrived during snapshot, re-arm
		if queued, ok := s.pending[networkName]; ok {
			delete(s.pending, networkName)
			s.mu.Unlock()
			s.NotifyChange(networkName, queued.endpoint, queued.zone)
		} else {
			s.mu.Unlock()
		}
	}()
}

// Close stops all pending timers and prevents new snapshots.
func (s *DNSSnapshotter) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.closed = true
	for _, t := range s.timers {
		t.Stop()
	}
	s.timers = nil
}

// snapshot fetches the full microdns state and pushes it to git.
func (s *DNSSnapshotter) snapshot(ctx context.Context, networkName, endpoint, zone string) {
	log := s.log.With("network", networkName)
	log.Debugw("starting DNS config snapshot")

	repoName := s.repoName(networkName)

	// Temporarily override client repoName for ensureRepo
	owner := s.cfg.DNSSnapshotOwner
	if owner == "" {
		owner = "mkube"
	}
	fullRepo := owner + "/" + repoName

	// Ensure repo exists
	saved := s.client.repoName
	s.client.repoName = fullRepo
	if err := s.client.ensureRepo(); err != nil {
		s.client.repoName = saved
		log.Warnw("failed to ensure DNS snapshot repo", "repo", fullRepo, "error", err)
		return
	}

	ts := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	message := fmt.Sprintf("snapshot: %s @ %s", networkName, ts)

	// Fetch and push each data type
	pushCount := 0
	pushErr := 0

	// 1. DHCP pools
	pools, err := s.dnsClient.ListDHCPPools(ctx, endpoint)
	if err != nil {
		log.Warnw("failed to list DHCP pools", "error", err)
	} else {
		sortDHCPPools(pools)
		data, _ := json.MarshalIndent(pools, "", "  ")
		if _, err := s.client.pushFile("dhcp-pools.json", message, data); err != nil {
			log.Warnw("failed to push dhcp-pools.json", "error", err)
			pushErr++
		} else {
			pushCount++
		}
	}

	// 2. DHCP reservations
	reservations, err := s.dnsClient.ListDHCPReservations(ctx, endpoint)
	if err != nil {
		log.Warnw("failed to list DHCP reservations", "error", err)
	} else {
		sortDHCPReservations(reservations)
		data, _ := json.MarshalIndent(reservations, "", "  ")
		if _, err := s.client.pushFile("dhcp-reservations.json", message, data); err != nil {
			log.Warnw("failed to push dhcp-reservations.json", "error", err)
			pushErr++
		} else {
			pushCount++
		}
	}

	// 3. DNS forwarders
	forwarders, err := s.dnsClient.ListDNSForwarders(ctx, endpoint)
	if err != nil {
		log.Warnw("failed to list DNS forwarders", "error", err)
	} else {
		sortDNSForwarders(forwarders)
		data, _ := json.MarshalIndent(forwarders, "", "  ")
		if _, err := s.client.pushFile("forwarders.json", message, data); err != nil {
			log.Warnw("failed to push forwarders.json", "error", err)
			pushErr++
		} else {
			pushCount++
		}
	}

	// 4. DNS records for the zone
	if zone != "" {
		zoneID, err := s.dnsClient.EnsureZone(ctx, endpoint, zone)
		if err != nil {
			log.Warnw("failed to resolve zone", "zone", zone, "error", err)
		} else {
			records, err := s.dnsClient.ListFullRecords(ctx, endpoint, zoneID)
			if err != nil {
				log.Warnw("failed to list DNS records", "zone", zone, "error", err)
			} else {
				sortFullRecords(records)
				data, _ := json.MarshalIndent(records, "", "  ")
				path := "dns-records/" + zone + ".json"
				if _, err := s.client.pushFile(path, message, data); err != nil {
					log.Warnw("failed to push DNS records", "path", path, "error", err)
					pushErr++
				} else {
					pushCount++
				}
			}
		}
	}

	s.client.repoName = saved

	if pushErr > 0 {
		log.Warnw("DNS config snapshot partial failure", "pushed", pushCount, "failed", pushErr)
	} else {
		log.Infow("DNS config snapshot pushed", "repo", fullRepo, "files", pushCount)
	}
}

// repoName returns the git repository name for a network.
func (s *DNSSnapshotter) repoName(networkName string) string {
	prefix := s.cfg.DNSSnapshotPrefix
	if prefix == "" {
		prefix = "microdns-"
	}
	return prefix + networkName
}

// resolvePassword reads the password from file if needed.
func resolvePassword(cfg config.GitBackupConfig) string {
	if cfg.Password != "" {
		return cfg.Password
	}
	if cfg.PasswordFile != "" {
		data, err := readFileContent(cfg.PasswordFile)
		if err == nil {
			return strings.TrimSpace(string(data))
		}
	}
	return ""
}

// Sort functions to ensure deterministic JSON output (Go map iteration order pitfall).

func sortDHCPPools(pools []dns.DHCPPool) {
	sort.Slice(pools, func(i, j int) bool {
		return pools[i].Subnet < pools[j].Subnet
	})
}

func sortDHCPReservations(reservations []dns.DHCPReservation) {
	sort.Slice(reservations, func(i, j int) bool {
		return reservations[i].MAC < reservations[j].MAC
	})
}

func sortDNSForwarders(forwarders []dns.DNSForwarder) {
	sort.Slice(forwarders, func(i, j int) bool {
		return forwarders[i].Zone < forwarders[j].Zone
	})
}

func sortFullRecords(records []dns.FullRecord) {
	sort.Slice(records, func(i, j int) bool {
		if records[i].Name != records[j].Name {
			return records[i].Name < records[j].Name
		}
		return records[i].Type < records[j].Type
	})
}

// readFileContent reads a file's contents.
func readFileContent(path string) ([]byte, error) {
	return os.ReadFile(path)
}
