package gitbackup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/glennswest/mkube/pkg/config"
	"github.com/glennswest/mkube/pkg/store"
)

// Manager handles periodic and change-triggered git backups of store state.
type Manager struct {
	cfg       config.GitBackupConfig
	store     *store.Store
	log       *zap.SugaredLogger
	client    *rust4gitClient
	triggerCh chan struct{}

	// Content hashes for incremental pushes
	mu     sync.Mutex
	hashes map[string]string // path → sha256 of last pushed content

	// Status
	statusMu        sync.RWMutex
	lastBackup      time.Time
	lastCommitHash  string
	lastError       string
	lastExportCount int
	lastPushErr     string
	pendingChanges  int64
	totalPushed     int64
}

// New creates a new git backup manager.
func New(cfg config.GitBackupConfig, s *store.Store, log *zap.SugaredLogger) (*Manager, error) {
	// Apply defaults
	if cfg.Branch == "" {
		cfg.Branch = "main"
	}
	if cfg.IntervalSeconds <= 0 {
		cfg.IntervalSeconds = 300
	}
	if cfg.DebounceSeconds <= 0 {
		cfg.DebounceSeconds = 30
	}
	if cfg.CommitAuthor == "" {
		cfg.CommitAuthor = "mkube"
	}
	if cfg.CommitEmail == "" {
		cfg.CommitEmail = "mkube@gt.lo"
	}

	client := newClient(
		cfg.RepoURL, cfg.RepoName, cfg.Branch,
		cfg.CommitAuthor, cfg.CommitEmail,
		cfg.Username, cfg.Password, cfg.PasswordFile,
		cfg.InsecureTLS,
	)

	return &Manager{
		cfg:       cfg,
		store:     s,
		log:       log.Named("gitbackup"),
		client:    client,
		triggerCh: make(chan struct{}, 1),
		hashes:    make(map[string]string),
	}, nil
}

// OnStoreChange is the sync hook callback. It increments the pending counter
// and resets the debounce timer.
func (m *Manager) OnStoreChange(bucket, key, op string, value []byte) {
	atomic.AddInt64(&m.pendingChanges, 1)
	// Non-blocking send to trigger channel
	select {
	case m.triggerCh <- struct{}{}:
	default:
	}
}

// TriggerNow forces an immediate backup snapshot.
func (m *Manager) TriggerNow() {
	select {
	case m.triggerCh <- struct{}{}:
	default:
	}
}

// Run starts the backup loop. It runs until ctx is cancelled.
func (m *Manager) Run(ctx context.Context) {
	// Ensure repo exists on first run
	if err := m.client.ensureRepo(); err != nil {
		m.log.Warnw("failed to ensure git repo exists", "error", err)
	}

	interval := time.Duration(m.cfg.IntervalSeconds) * time.Second
	debounce := time.Duration(m.cfg.DebounceSeconds) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Token renewal every 30 days
	renewTicker := time.NewTicker(30 * 24 * time.Hour)
	defer renewTicker.Stop()

	var debounceTimer *time.Timer

	m.log.Infow("git backup started",
		"repo", m.cfg.RepoName,
		"interval", interval,
		"debounce", debounce,
	)

	// Initial backup after a short delay
	time.AfterFunc(10*time.Second, func() {
		m.TriggerNow()
	})

	for {
		select {
		case <-ctx.Done():
			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			m.log.Info("git backup stopped")
			return

		case <-renewTicker.C:
			if err := m.client.RenewToken(); err != nil {
				m.log.Warnw("token renewal failed", "error", err)
			} else {
				m.log.Infow("token renewed successfully")
			}

		case <-ticker.C:
			m.doSnapshot(ctx)

		case <-m.triggerCh:
			// Reset debounce timer — coalesce rapid changes
			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			debounceTimer = time.AfterFunc(debounce, func() {
				m.doSnapshot(ctx)
			})
		}
	}
}

// doSnapshot exports all resources and pushes changed files.
func (m *Manager) doSnapshot(_ context.Context) {
	m.log.Debugw("snapshot starting")
	m.mu.Lock()
	defer m.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	files, manifest, err := exportAll(ctx, m.store)
	if err != nil {
		m.setError(fmt.Sprintf("export failed: %v", err))
		m.log.Warnw("git backup export failed", "error", err)
		return
	}

	m.statusMu.Lock()
	m.lastExportCount = len(files)
	m.statusMu.Unlock()

	m.log.Infow("snapshot exported", "files", len(files), "manifest", len(manifest))

	// Determine which files changed
	var changed []exportedFile
	newHashes := make(map[string]string, len(files))

	for _, f := range files {
		hash := hashContent(f.Content)
		newHashes[f.Path] = hash
		if m.hashes[f.Path] != hash {
			changed = append(changed, f)
		}
	}

	if len(changed) == 0 {
		// No changes — still reset pending counter
		atomic.StoreInt64(&m.pendingChanges, 0)
		return
	}

	// Build a summary commit message
	message := buildCommitMessage(changed)

	// Push changed files
	pushCount := 0
	for _, f := range changed {
		if _, err := m.client.pushFile(f.Path, message, f.Content); err != nil {
			m.setError(fmt.Sprintf("push %s: %v", f.Path, err))
			m.log.Warnw("git backup push failed", "path", f.Path, "error", err)
			// Continue pushing other files
			continue
		}
		pushCount++
	}

	// Push manifest
	manifestJSON, _ := json.MarshalIndent(manifest, "", "  ")
	if _, err := m.client.pushFile("_manifest.json", "backup: update manifest", manifestJSON); err != nil {
		m.log.Warnw("failed to push manifest", "error", err)
	}

	// Update hashes for successfully pushed files
	for _, f := range changed {
		m.hashes[f.Path] = newHashes[f.Path]
	}

	atomic.StoreInt64(&m.pendingChanges, 0)
	atomic.AddInt64(&m.totalPushed, int64(pushCount))

	m.statusMu.Lock()
	m.lastBackup = time.Now()
	m.lastError = ""
	m.statusMu.Unlock()

	m.log.Infow("git backup complete",
		"changed", pushCount,
		"total_files", len(files),
	)
}

func (m *Manager) setError(msg string) {
	m.statusMu.Lock()
	m.lastError = msg
	m.statusMu.Unlock()
}

// Status returns the current backup status.
func (m *Manager) Status() map[string]interface{} {
	m.statusMu.RLock()
	defer m.statusMu.RUnlock()

	status := map[string]interface{}{
		"enabled":         m.cfg.Enabled,
		"repoURL":         m.cfg.RepoURL,
		"repoName":        m.cfg.RepoName,
		"branch":          m.cfg.Branch,
		"pendingChanges":  atomic.LoadInt64(&m.pendingChanges),
		"totalPushed":     atomic.LoadInt64(&m.totalPushed),
		"lastExportCount": m.lastExportCount,
	}
	if !m.lastBackup.IsZero() {
		status["lastBackup"] = m.lastBackup.Format(time.RFC3339)
	}
	if m.lastError != "" {
		status["lastError"] = m.lastError
	}
	if m.lastPushErr != "" {
		status["lastPushErr"] = m.lastPushErr
	}
	return status
}

func hashContent(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func buildCommitMessage(changed []exportedFile) string {
	// Count by directory (resource type)
	counts := make(map[string]int)
	for _, f := range changed {
		parts := strings.SplitN(f.Path, "/", 2)
		if len(parts) > 0 {
			counts[parts[0]]++
		}
	}

	var parts []string
	for dir, count := range counts {
		parts = append(parts, fmt.Sprintf("%d %s", count, dir))
	}
	return fmt.Sprintf("backup: %s", strings.Join(parts, ", "))
}
