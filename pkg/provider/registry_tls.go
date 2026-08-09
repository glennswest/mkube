package provider

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"time"
)

// Self-signed TLS material for a managed registry.
//
// The registry serves HTTP and HTTPS on one port by peeking at the first byte
// of each connection (0x16 = TLS ClientHello), but only when a certificate is
// present — otherwise it logs "TLS cert not found, falling back to HTTP" and
// serves plaintext only. A plaintext registry then fails every mkube pull with
// "server gave HTTP response to HTTPS client", because the pull path uses a
// TLS transport.
//
// Certificates cannot be signed by the original registry CA: mkube-installer
// generates that CA key in memory and never persists it (only registry-ca.crt
// is written out). Since mkube is the only client that verifies — podman
// pushes with --tls-verify=false — a self-signed certificate per registry is
// sufficient, provided mkube adds it to its own trust pool. See
// Manager.SetRegistryCAs.

const registryCertValidity = 10 * 365 * 24 * time.Hour

// generateRegistryTLS returns a PEM certificate and key for a registry
// reachable as `hostname` and/or `ip`. Both are included as SANs so a pull by
// either form verifies.
func generateRegistryTLS(hostname, ip string) (certPEM, keyPEM string, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", fmt.Errorf("generating key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return "", "", fmt.Errorf("generating serial: %w", err)
	}

	cn := hostname
	if cn == "" {
		cn = ip
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   cn,
			Organization: []string{"mkube"},
		},
		NotBefore: time.Now().Add(-time.Hour),
		NotAfter:  time.Now().Add(registryCertValidity),
		KeyUsage:  x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
		},
		BasicConstraintsValid: true,
		// Self-signed and used directly as a trust anchor by mkube, so it must
		// be a valid CA as well as a leaf.
		IsCA: true,
	}
	if hostname != "" {
		tmpl.DNSNames = append(tmpl.DNSNames, hostname)
	}
	if ip != "" {
		if parsed := net.ParseIP(ip); parsed != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, parsed)
		}
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return "", "", fmt.Errorf("creating certificate: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return "", "", fmt.Errorf("marshalling key: %w", err)
	}

	certPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	keyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	return certPEM, keyPEM, nil
}
