package pki

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"sync"
	"time"

	"seaweedfs-ui/internal/config"
)

// CertReloader dynamically monitors and reloads client TLS certificates from disk
// to support Kubernetes cert-manager certificate rotation without application restarts.
type CertReloader struct {
	mu          sync.RWMutex
	certFile    string
	keyFile     string
	certModTime time.Time
	keyModTime  time.Time
	cert        *tls.Certificate
}

// NewCertReloader creates a new CertReloader and performs an initial keypair load.
func NewCertReloader(certFile, keyFile string) (*CertReloader, error) {
	if certFile == "" || keyFile == "" {
		return nil, fmt.Errorf("both certFile and keyFile must be provided")
	}

	reloader := &CertReloader{
		certFile: certFile,
		keyFile:  keyFile,
	}

	if err := reloader.Reload(); err != nil {
		return nil, err
	}

	return reloader, nil
}

// Reload re-reads the certificate and private key from disk and updates the in-memory cache.
func (r *CertReloader) Reload() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	certStat, err := os.Stat(r.certFile)
	if err != nil {
		return fmt.Errorf("stat cert file %q: %w", r.certFile, err)
	}

	keyStat, err := os.Stat(r.keyFile)
	if err != nil {
		return fmt.Errorf("stat key file %q: %w", r.keyFile, err)
	}

	cert, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		return fmt.Errorf("load x509 keypair (%q, %q): %w", r.certFile, r.keyFile, err)
	}

	r.cert = &cert
	r.certModTime = certStat.ModTime()
	r.keyModTime = keyStat.ModTime()
	return nil
}

// Certificate returns the currently cached client certificate.
func (r *CertReloader) Certificate() *tls.Certificate {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cert
}

// GetClientCertificate implements the tls.Config.GetClientCertificate callback.
func (r *CertReloader) GetClientCertificate(cri *tls.CertificateRequestInfo) (*tls.Certificate, error) {
	// Check if files on disk have been updated
	certStat, err1 := os.Stat(r.certFile)
	keyStat, err2 := os.Stat(r.keyFile)
	if err1 == nil && err2 == nil {
		r.mu.RLock()
		needReload := certStat.ModTime().After(r.certModTime) || keyStat.ModTime().After(r.keyModTime)
		r.mu.RUnlock()

		if needReload {
			// If reload fails (e.g. intermediate symlink state in Kubernetes Secret mount),
			// retain previous certificate to avoid transient handshake failure.
			_ = r.Reload()
		}
	}

	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cert, nil
}

// BuildTLSConfig constructs a standard crypto/tls.Config for HTTPS REST probes.
// If TLS is disabled (cfg.Enabled == false), returns nil, nil.
func BuildTLSConfig(cfg config.TLSConfig) (*tls.Config, error) {
	if !cfg.Enabled {
		return nil, nil
	}

	tlsConf := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: cfg.InsecureSkipVerify,
	}

	// 1. Load Root CA bundle if provided
	if cfg.CAFile != "" {
		caBytes, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read CA file %q: %w", cfg.CAFile, err)
		}
		if len(caBytes) == 0 {
			return nil, fmt.Errorf("CA file %q is empty", cfg.CAFile)
		}

		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caBytes) {
			return nil, fmt.Errorf("failed to parse valid PEM certificates from %q", cfg.CAFile)
		}
		tlsConf.RootCAs = pool
	}

	// 2. Load Client Keypair for mTLS if provided
	if cfg.CertFile != "" || cfg.KeyFile != "" {
		if cfg.CertFile == "" || cfg.KeyFile == "" {
			return nil, fmt.Errorf("both certFile and keyFile must be provided for client certificate authentication")
		}

		reloader, err := NewCertReloader(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("initialize client certificate reloader: %w", err)
		}

		tlsConf.GetClientCertificate = reloader.GetClientCertificate
		if initialCert := reloader.Certificate(); initialCert != nil {
			tlsConf.Certificates = []tls.Certificate{*initialCert}
		}
	}

	return tlsConf, nil
}
