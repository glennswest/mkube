// mkube-installer: Local CLI tool that bootstraps a fresh RouterOS device.
//
// Runs on your Mac/Linux workstation. Connects to the device via REST API
// and SSH. Creates the registry, seeds images, and starts mkube-update.
// No tarballs — all containers use OCI image pulls via tag=.
//
// Usage:
//   mkube-installer --device 192.168.1.88
//   mkube-installer --device rose1.gw.lo --registry-ip 192.168.200.3

package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/glennswest/mkube/pkg/dockersave"
)

var version = "dev"

func main() {
	rootCmd := &cobra.Command{
		Use:     "mkube-installer",
		Short:   "Bootstrap a fresh RouterOS device with mkube infrastructure",
		Version: version,
		RunE:    run,
	}

	f := rootCmd.Flags()
	f.String("device", "", "RouterOS device address (IP or hostname, required)")
	f.String("ssh-user", "admin", "SSH username")
	f.String("ros-user", "mkube", "RouterOS REST API username")
	f.String("ros-password", "mkube-rest", "RouterOS REST API password")
	f.String("registry-ip", "192.168.200.3", "Static IP for the registry container")
	f.String("registry-image", "ghcr.io/glennswest/mkube-registry:edge", "Registry container image on GHCR")
	f.String("bridge", "bridge-gt", "RouterOS bridge name for container network")
	f.String("gateway", "192.168.200.1", "Gateway IP for container network")
	f.String("dns", "192.168.200.199", "DNS server for containers")
	f.String("mkube-ip", "192.168.200.2", "Static IP for the mkube container")
	f.String("mkube-update-ip", "192.168.200.5", "Static IP for the mkube-update container")
	f.StringSlice("seed", []string{
		"ghcr.io/glennswest/mkube:edge",
		"ghcr.io/glennswest/mkube-update:edge",
		"ghcr.io/glennswest/mkube-registry:edge",
		"ghcr.io/glennswest/microdns:edge",
		"ghcr.io/glennswest/nats:edge",
		"ghcr.io/glennswest/micrologs:edge",
		"ghcr.io/glennswest/micrologs-collector-routeros:edge",
		"ghcr.io/glennswest/ipmiserial:edge",
		"ghcr.io/glennswest/mkube-console:edge",
		"ghcr.io/glennswest/agent-monitor:edge",
		"ghcr.io/glennswest/fastregistry:edge",
	}, "Images to seed from GHCR into local registry")

	_ = rootCmd.MarkFlagRequired("device")

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func run(cmd *cobra.Command, args []string) error {
	logger, _ := zap.NewProduction()
	defer func() { _ = logger.Sync() }()
	log := logger.Sugar()

	device, _ := cmd.Flags().GetString("device")
	sshUser, _ := cmd.Flags().GetString("ssh-user")
	rosUser, _ := cmd.Flags().GetString("ros-user")
	rosPass, _ := cmd.Flags().GetString("ros-password")
	registryIP, _ := cmd.Flags().GetString("registry-ip")
	registryImage, _ := cmd.Flags().GetString("registry-image")
	bridge, _ := cmd.Flags().GetString("bridge")
	gateway, _ := cmd.Flags().GetString("gateway")
	dns, _ := cmd.Flags().GetString("dns")
	mkubeIP, _ := cmd.Flags().GetString("mkube-ip")
	mkubeUpdateIP, _ := cmd.Flags().GetString("mkube-update-ip")
	seedImages, _ := cmd.Flags().GetStringSlice("seed")

	log.Infow("mkube-installer", "version", version, "device", device)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	ins := &Installer{
		device:        device,
		sshUser:       sshUser,
		rosURL:        fmt.Sprintf("http://%s/rest", device),
		rosUser:       rosUser,
		rosPassword:   rosPass,
		registryIP:    registryIP,
		registryImage: registryImage,
		bridge:        bridge,
		gateway:       gateway,
		dns:           dns,
		mkubeIP:       mkubeIP,
		mkubeUpdateIP: mkubeUpdateIP,
		seedImages:    seedImages,
		log:           log,
		http: &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
			Timeout: 60 * time.Second,
		},
	}

	return ins.Run(ctx)
}

type Installer struct {
	device        string
	sshUser       string
	rosURL        string
	rosUser       string
	rosPassword   string
	registryIP    string
	registryImage string
	bridge        string
	gateway       string
	dns           string
	mkubeIP       string
	mkubeUpdateIP string
	seedImages    []string
	log           *zap.SugaredLogger
	http          *http.Client

	// TLS: CA cert + registry server cert generated during install
	caCertPEM     string // PEM-encoded CA certificate (distributed to clients)
	serverCertPEM string // PEM-encoded server certificate (for registry)
	serverKeyPEM  string // PEM-encoded server private key (for registry)
}

func (ins *Installer) Run(ctx context.Context) error {
	ins.log.Info("step 1/8: verifying device connectivity")
	if err := ins.verifyDevice(ctx); err != nil {
		return fmt.Errorf("device not reachable: %w", err)
	}

	ins.log.Info("step 2/8: generating TLS certificates")
	if err := ins.generateCerts(); err != nil {
		return fmt.Errorf("generating TLS certs: %w", err)
	}

	ins.log.Info("step 3/8: creating network interfaces and mounts")
	if err := ins.setupInfra(ctx); err != nil {
		return fmt.Errorf("infra setup: %w", err)
	}

	ins.log.Info("step 4/8: uploading config files")
	if err := ins.uploadConfigs(); err != nil {
		return fmt.Errorf("uploading configs: %w", err)
	}

	ins.log.Info("step 5/8: creating and starting registry")
	if err := ins.ensureRegistry(ctx); err != nil {
		return fmt.Errorf("registry: %w", err)
	}

	ins.log.Info("step 6/8: seeding images from GHCR")
	if err := ins.seedAllImages(ctx); err != nil {
		return fmt.Errorf("seeding: %w", err)
	}

	ins.log.Info("step 7/8: cleaning up old containers")
	ins.cleanupOldContainers(ctx)

	ins.log.Info("step 8/8: creating mkube-update")
	if err := ins.ensureMkubeUpdate(ctx); err != nil {
		return fmt.Errorf("mkube-update: %w", err)
	}

	ins.log.Info("bootstrap complete — mkube-update will now start mkube")
	return nil
}

// ── Step 1: Verify device ───────────────────────────────────────────────────

func (ins *Installer) verifyDevice(ctx context.Context) error {
	var result map[string]interface{}
	if err := ins.rosGET(ctx, "/system/resource", &result); err != nil {
		return fmt.Errorf("REST API: %w", err)
	}
	arch, _ := result["architecture-name"].(string)
	board, _ := result["board-name"].(string)
	ins.log.Infow("device info", "arch", arch, "board", board)
	return nil
}

// ── Step 2: Generate TLS certificates ────────────────────────────────────────

func (ins *Installer) generateCerts() error {
	// Generate CA key + cert
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generating CA key: %w", err)
	}

	caSerial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	caTmpl := &x509.Certificate{
		SerialNumber:          caSerial,
		Subject:               pkix.Name{CommonName: "mkube-registry-ca", Organization: []string{"mkube"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}

	caCertDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return fmt.Errorf("creating CA cert: %w", err)
	}
	caCert, _ := x509.ParseCertificate(caCertDER)

	// Generate server key + cert signed by CA
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generating server key: %w", err)
	}

	serverSerial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	serverTmpl := &x509.Certificate{
		SerialNumber: serverSerial,
		Subject:      pkix.Name{CommonName: "registry.gt.lo"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP(ins.registryIP), net.IPv4(127, 0, 0, 1)},
		DNSNames:     []string{"registry.gt.lo", "localhost"},
	}

	serverCertDER, err := x509.CreateCertificate(rand.Reader, serverTmpl, caCert, &serverKey.PublicKey, caKey)
	if err != nil {
		return fmt.Errorf("creating server cert: %w", err)
	}

	// Encode to PEM
	ins.caCertPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caCertDER}))

	ins.serverCertPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverCertDER}))

	serverKeyBytes, _ := x509.MarshalECPrivateKey(serverKey)
	ins.serverKeyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: serverKeyBytes}))

	ins.log.Info("TLS certificates generated (CA + server cert for registry)")

	// Update HTTP client to trust our CA for registry health checks
	caPool := x509.NewCertPool()
	caPool.AddCert(caCert)
	ins.http.Transport = &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: caPool},
	}

	return nil
}

// ── Step 3: Create veths, bridge ports, mounts, directories ─────────────────

func (ins *Installer) setupInfra(ctx context.Context) error {
	// Veths
	veths := []struct {
		name, ip string
	}{
		{"veth-registry", ins.registryIP + "/24"},
		{"veth-mkube", ins.mkubeIP + "/24"},
		{"veth-mkube-update", ins.mkubeUpdateIP + "/24"},
	}
	for _, v := range veths {
		ins.rosPost(ctx, "/interface/veth/add", map[string]string{
			"name": v.name, "address": v.ip, "gateway": ins.gateway,
		}) // ignore error if exists
		ins.rosPost(ctx, "/interface/bridge/port/add", map[string]string{
			"bridge": ins.bridge, "interface": v.name,
		}) // ignore error if exists
		ins.log.Infow("veth ready", "name", v.name, "ip", v.ip)
	}

	// Mounts (idempotent — only created if not already present)
	mounts := []struct {
		list, src, dst string
	}{
		{"registry.gt.lo.config", "/raid1/volumes/registry.gt.lo/config", "/etc/registry"},
		{"registry.gt.lo.data", "/raid1/registry", "/raid1/registry"},
		{"kube.gt.lo.config", "/raid1/volumes/kube.gt.lo/config", "/etc/mkube"},
		{"kube.gt.lo.registry", "/raid1/registry", "/raid1/registry"},
		{"kube.gt.lo.cache", "/raid1/cache", "/raid1/cache"},
		{"kube.gt.lo.data", "/raid1/volumes/kube.gt.lo/data", "/data"},
		{"mkube-update-updater.config", "/raid1/volumes/mkube-update-updater/config", "/etc/mkube-update"},
		{"mkube-update-updater.data", "/raid1/volumes/mkube-update-updater/data", "/data"},
	}
	for _, m := range mounts {
		ins.rosPost(ctx, "/container/mounts/add", map[string]string{
			"list": m.list, "src": m.src, "dst": m.dst,
		}) // ignore error if exists
	}
	ins.log.Info("mounts ready")

	// Directories via SFTP
	dirs := []string{
		"/raid1/volumes/registry.gt.lo/config",
		"/raid1/volumes/registry.gt.lo/data",
		"/raid1/volumes/kube.gt.lo/config",
		"/raid1/volumes/kube.gt.lo/data",
		"/raid1/volumes/kube.gt.lo/data/configmaps",
		"/raid1/volumes/mkube-update-updater/config",
		"/raid1/volumes/mkube-update-updater/data",
		"/raid1/registry",
		"/raid1/cache",
		"/raid1/tarballs",
	}
	var sftpCmds strings.Builder
	for _, d := range dirs {
		fmt.Fprintf(&sftpCmds, "-mkdir %s\n", d)
	}
	ins.sftp(sftpCmds.String())
	ins.log.Info("directories ready")

	return nil
}

// ── Step 3: Upload config files ─────────────────────────────────────────────

func (ins *Installer) uploadConfigs() error {
	registryConfig := ins.generateRegistryConfig()
	mkubeUpdateConfig := ins.generateMkubeUpdateConfig()

	uploads := []struct {
		content, remotePath string
	}{
		// Registry config + TLS certs
		{registryConfig, "/raid1/volumes/registry.gt.lo/config/config.yaml"},
		{ins.serverCertPEM, "/raid1/volumes/registry.gt.lo/config/tls.crt"},
		{ins.serverKeyPEM, "/raid1/volumes/registry.gt.lo/config/tls.key"},

		// mkube-update config + CA cert
		{mkubeUpdateConfig, "/raid1/volumes/mkube-update-updater/config/config.yaml"},
		{ins.caCertPEM, "/raid1/volumes/mkube-update-updater/config/registry-ca.crt"},

		// mkube CA cert (so mkube can verify registry)
		{ins.caCertPEM, "/raid1/volumes/kube.gt.lo/config/registry-ca.crt"},
	}

	// Also upload rose1-config.yaml and boot-order.yaml if they exist locally
	for _, local := range []struct {
		path, remote string
	}{
		{"deploy/rose1-config.yaml", "/raid1/volumes/kube.gt.lo/config/config.yaml"},
		{"deploy/boot-order.yaml", "/raid1/volumes/kube.gt.lo/config/boot-order.yaml"},
	} {
		data, err := os.ReadFile(local.path)
		if err == nil {
			uploads = append(uploads, struct{ content, remotePath string }{string(data), local.remote})
		}
	}

	for _, u := range uploads {
		if err := ins.sftpWriteFile(u.content, u.remotePath); err != nil {
			return fmt.Errorf("uploading %s: %w", u.remotePath, err)
		}
		ins.log.Infow("uploaded", "path", u.remotePath)
	}

	return nil
}

func (ins *Installer) generateRegistryConfig() string {
	return fmt.Sprintf(`listenAddr: ":5000"
storePath: /raid1/registry
notifyURL: "http://%s:8082/api/v1/registry/push-notify"
pullThrough: true
upstreamRegistries:
  - "ghcr.io"
watchPollSeconds: 120
watchImages:
  - upstream: "ghcr.io/glennswest/mkube:edge"
    localRepo: "mkube"
  - upstream: "ghcr.io/glennswest/mkube-update:edge"
    localRepo: "mkube-update"
  - upstream: "ghcr.io/glennswest/mkube-registry:edge"
    localRepo: "mkube-registry"
  - upstream: "ghcr.io/glennswest/microdns:edge"
    localRepo: "microdns"
  - upstream: "ghcr.io/glennswest/micrologs:edge"
    localRepo: "micrologs"
  - upstream: "ghcr.io/glennswest/micrologs-collector-routeros:edge"
    localRepo: "micrologs-collector-routeros"
  - upstream: "ghcr.io/glennswest/fastregistry:edge"
    localRepo: "fastregistry"
  - upstream: "ghcr.io/glennswest/agent-monitor:edge"
    localRepo: "agent-monitor"
  - upstream: "ghcr.io/glennswest/ipmiserial:edge"
    localRepo: "ipmiserial"
  - upstream: "ghcr.io/glennswest/mkube-console:edge"
    localRepo: "mkube-console"
  - upstream: "ghcr.io/glennswest/nats:edge"
    localRepo: "nats"
`, ins.mkubeIP)
}

func (ins *Installer) generateMkubeUpdateConfig() string {
	return fmt.Sprintf(`registryURL: "https://%s:5000"
routerosURL: "http://%s/rest"
routerosUser: "%s"
routerosPassword: "%s"
mkubeAPI: "http://%s:8080"
pollSeconds: 60

bootstrap:
  enabled: true
  image: "ghcr.io/glennswest/mkube:edge"
  selfRootDir: "raid1/images/mkube-update-updater"
  tarballDir: "/data"
  container:
    name: "kube.gt.lo"
    interface: "veth-mkube"
    rootDir: "/raid1/images/kube.gt.lo"
    hostname: "kube.gt.lo"
    dns: "%s"
    logging: "yes"
    startOnBoot: "yes"
    mountLists: "kube.gt.lo.config,kube.gt.lo.registry,kube.gt.lo.cache,kube.gt.lo.data"

watches:
  - repo: mkube
    tags: [edge, latest]
    container: kube.gt.lo
  - repo: mkube-update
    tags: [edge, latest]
    container: mkube-update-updater
    selfUpdate: true
  - repo: mkube-registry
    tags: [edge, latest]
    container: registry.gt.lo
  - repo: microdns
    tags: [edge, latest]
    containers:
      - gw_dns_microdns
      - g10_dns_microdns
      - g11_dns_microdns
      - gt_dns_microdns
    rolling: true
    rollingDelay: 5s
`, ins.registryIP, ins.gateway, ins.rosUser, ins.rosPassword, ins.mkubeIP, ins.dns)
}

// ── Step 4: Registry ────────────────────────────────────────────────────────

func (ins *Installer) ensureRegistry(ctx context.Context) error {
	name := "registry.gt.lo"

	ct, err := ins.rosGetContainer(ctx, name)
	if err == nil {
		if ct.isRunning() {
			ins.log.Info("registry already running")
			return ins.waitForRegistryHealth(ctx)
		}
		ins.log.Info("registry exists but stopped, starting")
		if err := ins.rosPost(ctx, "/container/start", map[string]string{".id": ct.ID}); err != nil {
			return err
		}
		if err := ins.waitForContainerRunning(ctx, name); err != nil {
			return err
		}
		return ins.waitForRegistryHealth(ctx)
	}

	// Create container — RouterOS pulls the OCI image from GHCR
	ins.log.Infow("creating registry container", "image", ins.registryImage)
	if err := ins.rosPost(ctx, "/container/add", map[string]string{
		"name":              name,
		"remote-image":      ins.registryImage,
		"interface":         "veth-registry",
		"root-dir":          "/raid1/images/registry.gt.lo",
		"logging":           "yes",
		"start-on-boot":     "yes",
		"hostname":          name,
		"dns":               ins.dns,
		"mountlists":        "registry.gt.lo.config,registry.gt.lo.data",
		"check-certificate": "no",
	}); err != nil {
		return fmt.Errorf("creating container: %w", err)
	}

	ins.log.Info("waiting for image pull and extraction (this may take a minute)...")
	if err := ins.waitForExtraction(ctx, name, 300); err != nil {
		return err
	}

	ct, err = ins.rosGetContainer(ctx, name)
	if err != nil {
		return err
	}

	ins.log.Info("starting registry")
	if err := ins.rosPost(ctx, "/container/start", map[string]string{".id": ct.ID}); err != nil {
		return err
	}

	if err := ins.waitForContainerRunning(ctx, name); err != nil {
		return err
	}

	return ins.waitForRegistryHealth(ctx)
}

func (ins *Installer) waitForRegistryHealth(ctx context.Context) error {
	url := fmt.Sprintf("https://%s:5000/v2/", ins.registryIP)
	ins.log.Infow("waiting for registry health", "url", url)

	for i := 0; i < 60; i++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
		resp, err := ins.http.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				ins.log.Info("registry healthy")
				return nil
			}
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("registry not healthy at %s after 120s", url)
}

// ── Step 5: Seed images ─────────────────────────────────────────────────────

func (ins *Installer) seedAllImages(ctx context.Context) error {
	registryAddr := ins.registryIP + ":5000"

	// Use the installer's HTTP transport (trusts our CA) for local registry checks
	registryTransport := ins.http.Transport

	for _, upstream := range ins.seedImages {
		local := extractLocal(upstream)
		localRef := registryAddr + "/" + local

		// Check if image already exists in local registry first
		_, headErr := crane.Head(localRef, crane.WithTransport(registryTransport))
		if headErr == nil {
			ins.log.Infow("already in registry, skipping", "image", local)
			continue
		}

		ins.log.Infow("seeding", "from", upstream, "to", localRef)

		// Pull from GHCR (default transport) then push to local registry (our CA transport).
		// crane.Copy uses a single transport, so we pull first then push.
		img, err := crane.Pull(upstream,
			crane.WithContext(ctx),
			crane.WithPlatform(&v1.Platform{OS: "linux", Architecture: "arm64"}),
			crane.WithAuthFromKeychain(
				authn.NewMultiKeychain(authn.DefaultKeychain, dockersave.AnonymousKeychain{}),
			),
		)
		if err != nil {
			return fmt.Errorf("pulling %s: %w", upstream, err)
		}

		if err := crane.Push(img, localRef,
			crane.WithContext(ctx),
			crane.WithTransport(registryTransport),
		); err != nil {
			return fmt.Errorf("pushing %s to local registry: %w", local, err)
		}

		ins.log.Infow("seeded", "image", local)
	}

	return nil
}

// extractLocal turns "ghcr.io/glennswest/mkube:edge" into "mkube:edge"
func extractLocal(ref string) string {
	// Split off host
	parts := strings.SplitN(ref, "/", 3)
	var repoTag string
	if len(parts) == 3 {
		repoTag = parts[2] // "glennswest/mkube:edge"
	} else {
		repoTag = ref
	}
	// Take last path component
	slashParts := strings.Split(repoTag, "/")
	return slashParts[len(slashParts)-1] // "mkube:edge"
}

// ── Step 6: mkube-update ────────────────────────────────────────────────────

func (ins *Installer) cleanupOldContainers(ctx context.Context) {
	// Remove old mkube-installer container (no longer needed — installer runs locally)
	for _, name := range []string{"mkube-installer"} {
		ct, err := ins.rosGetContainer(ctx, name)
		if err != nil {
			continue
		}
		ins.log.Infow("removing old container", "name", name)
		_ = ins.rosPost(ctx, "/container/stop", map[string]string{".id": ct.ID})
		time.Sleep(2 * time.Second)
		_ = ins.rosPost(ctx, "/container/remove", map[string]string{".id": ct.ID})
		time.Sleep(2 * time.Second)
	}
}

// removeRootDir removes a container's root-dir via SSH so RouterOS
// re-extracts the image tarball on creation.
func (ins *Installer) removeRootDir(dir string) {
	ins.log.Infow("removing old root-dir", "dir", dir)
	_, _ = ins.ssh(fmt.Sprintf("/file/remove %s", dir))
	time.Sleep(time.Second)
}

func (ins *Installer) ensureMkubeUpdate(ctx context.Context) error {
	name := "mkube-update-updater"
	imageRef := fmt.Sprintf("%s:5000/mkube-update:edge", ins.registryIP)

	ct, err := ins.rosGetContainer(ctx, name)
	if err == nil {
		if ct.isRunning() {
			ins.log.Info("mkube-update already running")
			return nil
		}
		// Stopped container — likely stale/broken. Remove and recreate.
		ins.log.Infow("removing stale mkube-update container", "id", ct.ID)
		_ = ins.rosPost(ctx, "/container/stop", map[string]string{".id": ct.ID})
		time.Sleep(2 * time.Second)
		if err := ins.rosPost(ctx, "/container/remove", map[string]string{".id": ct.ID}); err != nil {
			ins.log.Warnw("failed to remove old container, proceeding anyway", "err", err)
		}
		time.Sleep(2 * time.Second)
	}

	ins.removeRootDir("/raid1/images/mkube-update-updater")

	ins.log.Infow("creating mkube-update", "image", imageRef)
	if err := ins.rosPost(ctx, "/container/add", map[string]string{
		"name":              name,
		"remote-image":      imageRef,
		"interface":         "veth-mkube-update",
		"root-dir":          "/raid1/images/mkube-update-updater",
		"logging":           "yes",
		"start-on-boot":     "yes",
		"hostname":          name,
		"dns":               ins.dns,
		"mountlists":        "mkube-update-updater.config,mkube-update-updater.data",
		"check-certificate": "no",
	}); err != nil {
		return fmt.Errorf("creating container: %w", err)
	}

	ins.log.Info("waiting for image pull and extraction...")
	if err := ins.waitForExtraction(ctx, name, 120); err != nil {
		return err
	}

	ct, err = ins.rosGetContainer(ctx, name)
	if err != nil {
		return err
	}

	ins.log.Info("starting mkube-update")
	if err := ins.rosPost(ctx, "/container/start", map[string]string{".id": ct.ID}); err != nil {
		return err
	}

	return ins.waitForContainerRunning(ctx, name)
}

// ── RouterOS REST helpers ───────────────────────────────────────────────────

type rosContainer struct {
	ID, Name, Running, Stopped string
}

func (c rosContainer) isRunning() bool { return c.Running == "true" }
func (c rosContainer) isStopped() bool { return c.Stopped == "true" }

func (ins *Installer) rosGetContainer(ctx context.Context, name string) (*rosContainer, error) {
	var containers []map[string]interface{}
	if err := ins.rosGET(ctx, "/container", &containers); err != nil {
		return nil, err
	}
	for _, c := range containers {
		n, _ := c["name"].(string)
		if n != name {
			continue
		}
		return &rosContainer{
			ID:      strVal(c, ".id"),
			Name:    n,
			Running: strVal(c, "running"),
			Stopped: strVal(c, "stopped"),
		}, nil
	}
	return nil, fmt.Errorf("container %q not found", name)
}

func strVal(m map[string]interface{}, key string) string {
	v, _ := m[key].(string)
	return v
}

func (ins *Installer) rosGET(ctx context.Context, path string, result interface{}) error {
	req, err := http.NewRequestWithContext(ctx, "GET", ins.rosURL+path, nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(ins.rosUser, ins.rosPassword)
	req.Header.Set("Accept", "application/json")

	resp, err := ins.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("GET %s: %d: %s", path, resp.StatusCode, string(b))
	}
	return json.NewDecoder(resp.Body).Decode(result)
}

func (ins *Installer) rosPost(ctx context.Context, path string, body interface{}) error {
	data, _ := json.Marshal(body)

	req, err := http.NewRequestWithContext(ctx, "POST", ins.rosURL+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.SetBasicAuth(ins.rosUser, ins.rosPassword)
	req.Header.Set("Content-Type", "application/json")

	resp, err := ins.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("POST %s: %d: %s", path, resp.StatusCode, string(b))
	}
	return nil
}

// ── SSH/SFTP helpers ────────────────────────────────────────────────────────

func (ins *Installer) ssh(command string) (string, error) {
	cmd := exec.Command("ssh",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "ConnectTimeout=10",
		fmt.Sprintf("%s@%s", ins.sshUser, ins.device),
		command,
	)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (ins *Installer) sftp(commands string) {
	cmd := exec.Command("sftp",
		"-o", "StrictHostKeyChecking=accept-new",
		fmt.Sprintf("%s@%s", ins.sshUser, ins.device),
	)
	cmd.Stdin = strings.NewReader(commands)
	_ = cmd.Run() // ignore errors for -mkdir (dirs may exist)
}

func (ins *Installer) sftpWriteFile(content, remotePath string) error {
	// Write content to a temp file, then sftp put it
	tmp, err := os.CreateTemp("", "mkube-installer-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()

	cmd := exec.Command("sftp",
		"-o", "StrictHostKeyChecking=accept-new",
		fmt.Sprintf("%s@%s", ins.sshUser, ins.device),
	)
	cmd.Stdin = strings.NewReader(fmt.Sprintf("put %s %s\n", tmp.Name(), remotePath))
	return cmd.Run()
}

// ── Wait helpers ────────────────────────────────────────────────────────────

func (ins *Installer) waitForExtraction(ctx context.Context, name string, timeoutSec int) error {
	for i := 0; i < timeoutSec; i++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		time.Sleep(time.Second)
		ct, err := ins.rosGetContainer(ctx, name)
		if err != nil {
			// During download phase, container might show as "extracting"
			// or not yet queryable — keep waiting
			continue
		}
		if ct.isStopped() {
			ins.log.Infow("container extracted", "name", name)
			return nil
		}
	}
	return fmt.Errorf("container %s not ready within %ds", name, timeoutSec)
}

func (ins *Installer) waitForContainerRunning(ctx context.Context, name string) error {
	for i := 0; i < 30; i++ {
		time.Sleep(time.Second)
		ct, err := ins.rosGetContainer(ctx, name)
		if err != nil {
			continue
		}
		if ct.isRunning() {
			ins.log.Infow("container running", "name", name)
			return nil
		}
	}
	return fmt.Errorf("container %s did not start within 30s", name)
}
