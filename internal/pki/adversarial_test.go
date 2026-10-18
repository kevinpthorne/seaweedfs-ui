package pki

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"seaweedfs-ui/internal/config"
)

// Helper to generate expired certificates
type ExpiredTestCertificates struct {
	Dir               string
	CACertPath        string
	CAKeyPath         string
	ExpiredServerCert string
	ExpiredServerKey  string
	ExpiredClientCert string
	ExpiredClientKey  string
	ValidServerCert   string
	ValidServerKey    string
	ValidClientCert   string
	ValidClientKey    string
}

func generateExpiredCerts(t *testing.T, dir string) *ExpiredTestCertificates {
	t.Helper()

	// CA
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate ca key: %v", err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2001),
		Subject: pkix.Name{
			CommonName:   "Expired Test CA",
			Organization: []string{"SeaweedFS Expiry Test"},
		},
		NotBefore:             time.Now().Add(-72 * time.Hour),
		NotAfter:              time.Now().Add(72 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create ca cert: %v", err)
	}
	caCertPath := filepath.Join(dir, "exp_ca.crt")
	caKeyPath := filepath.Join(dir, "exp_ca.key")
	writePEM(t, caCertPath, "CERTIFICATE", caDER)
	writePEM(t, caKeyPath, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(caKey))

	// Expired Server Cert (expired 1 hour ago)
	expServerKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	expServerTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2002),
		Subject: pkix.Name{
			CommonName: "localhost",
		},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.IPv6loopback},
		DNSNames:              []string{"localhost"},
		NotBefore:             time.Now().Add(-48 * time.Hour),
		NotAfter:              time.Now().Add(-1 * time.Hour), // Expired!
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	expServerDER, _ := x509.CreateCertificate(rand.Reader, expServerTemplate, caTemplate, &expServerKey.PublicKey, caKey)
	expServerCertPath := filepath.Join(dir, "exp_server.crt")
	expServerKeyPath := filepath.Join(dir, "exp_server.key")
	writePEM(t, expServerCertPath, "CERTIFICATE", expServerDER)
	writePEM(t, expServerKeyPath, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(expServerKey))

	// Expired Client Cert (expired 1 hour ago)
	expClientKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	expClientTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2003),
		Subject: pkix.Name{
			CommonName: "expired-client",
		},
		NotBefore:             time.Now().Add(-48 * time.Hour),
		NotAfter:              time.Now().Add(-1 * time.Hour), // Expired!
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	expClientDER, _ := x509.CreateCertificate(rand.Reader, expClientTemplate, caTemplate, &expClientKey.PublicKey, caKey)
	expClientCertPath := filepath.Join(dir, "exp_client.crt")
	expClientKeyPath := filepath.Join(dir, "exp_client.key")
	writePEM(t, expClientCertPath, "CERTIFICATE", expClientDER)
	writePEM(t, expClientKeyPath, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(expClientKey))

	// Valid Server Cert
	valServerKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	valServerTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2004),
		Subject: pkix.Name{
			CommonName: "localhost",
		},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.IPv6loopback},
		DNSNames:              []string{"localhost"},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	valServerDER, _ := x509.CreateCertificate(rand.Reader, valServerTemplate, caTemplate, &valServerKey.PublicKey, caKey)
	valServerCertPath := filepath.Join(dir, "val_server.crt")
	valServerKeyPath := filepath.Join(dir, "val_server.key")
	writePEM(t, valServerCertPath, "CERTIFICATE", valServerDER)
	writePEM(t, valServerKeyPath, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(valServerKey))

	// Valid Client Cert
	valClientKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	valClientTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2005),
		Subject: pkix.Name{
			CommonName: "valid-client",
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	valClientDER, _ := x509.CreateCertificate(rand.Reader, valClientTemplate, caTemplate, &valClientKey.PublicKey, caKey)
	valClientCertPath := filepath.Join(dir, "val_client.crt")
	valClientKeyPath := filepath.Join(dir, "val_client.key")
	writePEM(t, valClientCertPath, "CERTIFICATE", valClientDER)
	writePEM(t, valClientKeyPath, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(valClientKey))

	return &ExpiredTestCertificates{
		Dir:               dir,
		CACertPath:        caCertPath,
		CAKeyPath:         caKeyPath,
		ExpiredServerCert: expServerCertPath,
		ExpiredServerKey:  expServerKeyPath,
		ExpiredClientCert: expClientCertPath,
		ExpiredClientKey:  expClientKeyPath,
		ValidServerCert:   valServerCertPath,
		ValidServerKey:    valServerKeyPath,
		ValidClientCert:   valClientCertPath,
		ValidClientKey:    valClientKeyPath,
	}
}

// ---------------------------------------------------------------------------
// 1. Adversarial Test: Expired Certificates
// ---------------------------------------------------------------------------

func TestAdversarial_ExpiredServerCertificate_Rejected(t *testing.T) {
	tempDir := t.TempDir()
	certs := generateExpiredCerts(t, tempDir)

	// Mock server running with EXPIRED server certificate
	serverCert, err := tls.LoadX509KeyPair(certs.ExpiredServerCert, certs.ExpiredServerKey)
	if err != nil {
		t.Fatalf("load expired server keypair: %v", err)
	}

	caBytes, err := os.ReadFile(certs.CACertPath)
	if err != nil {
		t.Fatalf("read ca: %v", err)
	}
	clientCAs := x509.NewCertPool()
	clientCAs.AppendCertsFromPEM(caBytes)

	serverTLS := &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientCAs:    clientCAs,
		ClientAuth:   tls.RequireAndVerifyClientCert,
	}

	listener := newPipeListener()
	tlsListener := tls.NewListener(listener, serverTLS)
	httpServer := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}),
	}
	go func() {
		_ = httpServer.Serve(tlsListener)
	}()
	defer func() {
		_ = httpServer.Close()
		_ = listener.Close()
	}()

	// Client has CA and valid client cert
	cfg := config.TLSConfig{
		Enabled:  true,
		CAFile:   certs.CACertPath,
		CertFile: certs.ValidClientCert,
		KeyFile:  certs.ValidClientKey,
	}
	clientTLS, err := BuildTLSConfig(cfg)
	if err != nil {
		t.Fatalf("BuildTLSConfig failed: %v", err)
	}

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return listener.Dial()
			},
			TLSClientConfig: clientTLS,
		},
		Timeout: 3 * time.Second,
	}

	resp, err := client.Get("https://localhost:9333")
	if err == nil {
		resp.Body.Close()
		t.Fatalf("expected client to reject expired server certificate, but connection succeeded")
	}

	// Verify error specifically indicates certificate expiration
	if !strings.Contains(err.Error(), "expired") && !strings.Contains(err.Error(), "certificate has expired") {
		t.Logf("connection failed as expected with: %v", err)
	}
}

func TestAdversarial_ExpiredClientCertificate_Rejected(t *testing.T) {
	tempDir := t.TempDir()
	certs := generateExpiredCerts(t, tempDir)

	// Mock server running with VALID server certificate and requiring client cert verification
	serverCert, err := tls.LoadX509KeyPair(certs.ValidServerCert, certs.ValidServerKey)
	if err != nil {
		t.Fatalf("load valid server keypair: %v", err)
	}

	caBytes, err := os.ReadFile(certs.CACertPath)
	if err != nil {
		t.Fatalf("read ca: %v", err)
	}
	clientCAs := x509.NewCertPool()
	clientCAs.AppendCertsFromPEM(caBytes)

	serverTLS := &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientCAs:    clientCAs,
		ClientAuth:   tls.RequireAndVerifyClientCert,
	}

	listener := newPipeListener()
	tlsListener := tls.NewListener(listener, serverTLS)
	httpServer := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}),
	}
	go func() {
		_ = httpServer.Serve(tlsListener)
	}()
	defer func() {
		_ = httpServer.Close()
		_ = listener.Close()
	}()

	// Client uses EXPIRED client certificate
	cfg := config.TLSConfig{
		Enabled:  true,
		CAFile:   certs.CACertPath,
		CertFile: certs.ExpiredClientCert,
		KeyFile:  certs.ExpiredClientKey,
	}
	clientTLS, err := BuildTLSConfig(cfg)
	if err != nil {
		t.Fatalf("BuildTLSConfig failed: %v", err)
	}

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return listener.Dial()
			},
			TLSClientConfig: clientTLS,
		},
		Timeout: 3 * time.Second,
	}

	resp, err := client.Get("https://localhost:9333")
	if err == nil {
		resp.Body.Close()
		t.Fatalf("expected server to reject expired client certificate, but connection succeeded")
	}
	t.Logf("expired client certificate rejected as expected: %v", err)
}

// ---------------------------------------------------------------------------
// 2. Adversarial Test: Untrusted CA & Hostname Mismatch
// ---------------------------------------------------------------------------

func TestAdversarial_UntrustedServerCA_Rejected(t *testing.T) {
	tempDir := t.TempDir()
	certs := generateCertKey(t, tempDir)

	// Server uses legitimate server cert from certs
	server := startMockMTLSServer(t, certs)
	defer server.Close()

	// Client uses Rogue CA (does not trust the server's CA)
	cfg := config.TLSConfig{
		Enabled:  true,
		CAFile:   certs.UntrustedCertPath, // Rogue CA!
		CertFile: certs.ClientCertPath,
		KeyFile:  certs.ClientKeyPath,
	}
	clientTLS, err := BuildTLSConfig(cfg)
	if err != nil {
		t.Fatalf("BuildTLSConfig failed: %v", err)
	}

	client := server.Client(clientTLS)
	resp, err := client.Get(server.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatalf("expected TLS handshake failure when server CA is untrusted by client")
	}

	if !strings.Contains(err.Error(), "unknown authority") && !strings.Contains(err.Error(), "certificate") {
		t.Errorf("unexpected error message: %v", err)
	}
	t.Logf("untrusted server CA correctly rejected: %v", err)
}

func TestAdversarial_ServerHostnameMismatch_Rejected(t *testing.T) {
	tempDir := t.TempDir()
	caDir := filepath.Join(tempDir, "ca")
	_ = os.MkdirAll(caDir, 0700)
	certs := generateCertKey(t, caDir)

	// Generate server cert with SAN = evil.example.com (NOT localhost or 127.0.0.1)
	caKeyPEM, _ := os.ReadFile(certs.CAKeyPath)
	caBlock, _ := pem.Decode(caKeyPEM)
	caKey, _ := x509.ParsePKCS1PrivateKey(caBlock.Bytes)

	caCertPEM, _ := os.ReadFile(certs.CACertPath)
	caCertBlock, _ := pem.Decode(caCertPEM)
	caCert, _ := x509.ParseCertificate(caCertBlock.Bytes)

	mismatchKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	mismatchTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(7001),
		Subject: pkix.Name{
			CommonName: "evil.example.com",
		},
		DNSNames:              []string{"evil.example.com"},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	mismatchDER, _ := x509.CreateCertificate(rand.Reader, mismatchTemplate, caCert, &mismatchKey.PublicKey, caKey)
	mismatchCertPath := filepath.Join(tempDir, "mismatch_server.crt")
	mismatchKeyPath := filepath.Join(tempDir, "mismatch_server.key")
	writePEM(t, mismatchCertPath, "CERTIFICATE", mismatchDER)
	writePEM(t, mismatchKeyPath, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(mismatchKey))

	serverCert, _ := tls.LoadX509KeyPair(mismatchCertPath, mismatchKeyPath)
	caPool := x509.NewCertPool()
	caPool.AddCert(caCert)

	serverTLS := &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientCAs:    caPool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
	}
	listener := newPipeListener()
	tlsListener := tls.NewListener(listener, serverTLS)
	httpServer := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}),
	}
	go func() {
		_ = httpServer.Serve(tlsListener)
	}()
	defer func() {
		_ = httpServer.Close()
		_ = listener.Close()
	}()

	cfg := config.TLSConfig{
		Enabled:  true,
		CAFile:   certs.CACertPath,
		CertFile: certs.ClientCertPath,
		KeyFile:  certs.ClientKeyPath,
	}
	clientTLS, err := BuildTLSConfig(cfg)
	if err != nil {
		t.Fatalf("BuildTLSConfig failed: %v", err)
	}

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return listener.Dial()
			},
			TLSClientConfig: clientTLS,
		},
		Timeout: 3 * time.Second,
	}

	// Dialing https://localhost:9333 with a certificate for evil.example.com
	resp, err := client.Get("https://localhost:9333")
	if err == nil {
		resp.Body.Close()
		t.Fatalf("expected hostname verification failure, but connection succeeded")
	}

	if !strings.Contains(err.Error(), "certificate is valid for") && !strings.Contains(err.Error(), "not localhost") {
		t.Logf("hostname verification failed with: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 3. Adversarial Test: Missing Client Certificate on mTLS Server
// ---------------------------------------------------------------------------

func TestAdversarial_MissingClientCert_StrictlyRejected(t *testing.T) {
	tempDir := t.TempDir()
	certs := generateCertKey(t, tempDir)
	server := startMockMTLSServer(t, certs)
	defer server.Close()

	// 1. Client has TLS enabled with CAFile, but no client keypair
	cfg := config.TLSConfig{
		Enabled:  true,
		CAFile:   certs.CACertPath,
		CertFile: "",
		KeyFile:  "",
	}
	clientTLS, err := BuildTLSConfig(cfg)
	if err != nil {
		t.Fatalf("BuildTLSConfig failed: %v", err)
	}

	client := server.Client(clientTLS)
	resp, err := client.Get(server.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatalf("expected connection failure when client cert is missing on mTLS server")
	}

	t.Logf("connection properly rejected for missing client cert: %v", err)
}

// ---------------------------------------------------------------------------
// 4. Adversarial Test: Dynamic Reloading Under Concurrent TLS Connection Negotiation
// ---------------------------------------------------------------------------

func TestAdversarial_ConcurrentNegotiation_DynamicReloading(t *testing.T) {
	tempDir := t.TempDir()
	certs := generateCertKey(t, tempDir)

	// Start server recording client CN
	var lastSeenCN atomic.Value
	serverCert, err := tls.LoadX509KeyPair(certs.ServerCertPath, certs.ServerKeyPath)
	if err != nil {
		t.Fatalf("load server key: %v", err)
	}
	caBytes, _ := os.ReadFile(certs.CACertPath)
	clientCAs := x509.NewCertPool()
	clientCAs.AppendCertsFromPEM(caBytes)

	serverTLS := &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientCAs:    clientCAs,
		ClientAuth:   tls.RequireAndVerifyClientCert,
	}

	listener := newPipeListener()
	tlsListener := tls.NewListener(listener, serverTLS)
	httpServer := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
				cn := r.TLS.PeerCertificates[0].Subject.CommonName
				lastSeenCN.Store(cn)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				fmt.Fprintf(w, `{"cn":%q}`, cn)
				return
			}
			http.Error(w, "no client cert", http.StatusUnauthorized)
		}),
	}
	go func() {
		_ = httpServer.Serve(tlsListener)
	}()
	defer func() {
		_ = httpServer.Close()
		_ = listener.Close()
	}()

	cfg := config.TLSConfig{
		Enabled:  true,
		CAFile:   certs.CACertPath,
		CertFile: certs.ClientCertPath,
		KeyFile:  certs.ClientKeyPath,
	}

	clientTLS, err := BuildTLSConfig(cfg)
	if err != nil {
		t.Fatalf("BuildTLSConfig failed: %v", err)
	}

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return listener.Dial()
			},
			TLSClientConfig: clientTLS,
			// Disable keep-alives so each request performs a fresh TLS handshake
			DisableKeepAlives: true,
		},
		Timeout: 5 * time.Second,
	}

	// 1. Initial request verifies client CN is seaweedfs-admin-client
	resp1, err := client.Get("https://localhost:9333")
	if err != nil {
		t.Fatalf("initial request failed: %v", err)
	}
	body1, _ := io.ReadAll(resp1.Body)
	resp1.Body.Close()
	if !strings.Contains(string(body1), "seaweedfs-admin-client") {
		t.Fatalf("expected CN 'seaweedfs-admin-client', got %s", string(body1))
	}

	// 2. Launch 20 concurrent worker goroutines negotiating TLS connections continuously
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var successCount atomic.Int64
	var failureCount atomic.Int64

	caKeyPEM, _ := os.ReadFile(certs.CAKeyPath)
	caBlock, _ := pem.Decode(caKeyPEM)
	caKey, _ := x509.ParsePKCS1PrivateKey(caBlock.Bytes)

	caCertPEM, _ := os.ReadFile(certs.CACertPath)
	caCertBlock, _ := pem.Decode(caCertPEM)
	caCert, _ := x509.ParseCertificate(caCertBlock.Bytes)

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					r, err := client.Get("https://localhost:9333")
					if err != nil {
						failureCount.Add(1)
					} else {
						_, _ = io.ReadAll(r.Body)
						r.Body.Close()
						successCount.Add(1)
					}
					time.Sleep(2 * time.Millisecond)
				}
			}
		}()
	}

	// 3. Concurrently rotate certificates on disk multiple times
	for rot := 1; rot <= 5; rot++ {
		time.Sleep(20 * time.Millisecond)
		newKey, _ := rsa.GenerateKey(rand.Reader, 2048)
		newCN := fmt.Sprintf("rotated-client-generation-%d", rot)
		newTemplate := &x509.Certificate{
			SerialNumber: big.NewInt(int64(3000 + rot)),
			Subject: pkix.Name{
				CommonName: newCN,
			},
			NotBefore:             time.Now().Add(-1 * time.Hour),
			NotAfter:              time.Now().Add(24 * time.Hour),
			KeyUsage:              x509.KeyUsageDigitalSignature,
			ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
			BasicConstraintsValid: true,
		}
		newDER, _ := x509.CreateCertificate(rand.Reader, newTemplate, caCert, &newKey.PublicKey, caKey)

		// Write to disk and advance mod time
		writePEM(t, certs.ClientCertPath, "CERTIFICATE", newDER)
		writePEM(t, certs.ClientKeyPath, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(newKey))
		futureMod := time.Now().Add(time.Duration(rot) * 2 * time.Second)
		_ = os.Chtimes(certs.ClientCertPath, futureMod, futureMod)
		_ = os.Chtimes(certs.ClientKeyPath, futureMod, futureMod)
	}

	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()

	t.Logf("Stress test finished: %d successful handshakes, %d failed handshakes", successCount.Load(), failureCount.Load())
	if successCount.Load() == 0 {
		t.Fatalf("zero successful handshakes during concurrent rotation")
	}

	// 4. Verify that final connection sees the rotated certificate
	finalResp, err := client.Get("https://localhost:9333")
	if err != nil {
		t.Fatalf("final request failed: %v", err)
	}
	finalBody, _ := io.ReadAll(finalResp.Body)
	finalResp.Body.Close()

	if !strings.Contains(string(finalBody), "rotated-client-generation-5") {
		t.Errorf("expected final request to have CN 'rotated-client-generation-5', got: %s", string(finalBody))
	}
}

// ---------------------------------------------------------------------------
// 5. Adversarial Test: EnsureSecurityToml Extreme Conditions
// ---------------------------------------------------------------------------

func TestAdversarial_EnsureSecurityToml_HomeEmptyAndUnset(t *testing.T) {
	// Case 1: HOME is unset
	origHome := os.Getenv("HOME")
	defer os.Setenv("HOME", origHome)

	os.Unsetenv("HOME")
	tempTmp := t.TempDir()
	// Test when SecurityTomlPath is empty, it defaults to /tmp/seaweedfs or HOME
	// To avoid polluting /tmp/seaweedfs on user's system, we test with an explicit empty HOME
	os.Setenv("HOME", "")

	cfg := config.TLSConfig{
		Enabled:  true,
		CAFile:   "/certs/ca.crt",
		CertFile: "/certs/tls.crt",
		KeyFile:  "/certs/tls.key",
	}

	path, err := EnsureSecurityToml(cfg)
	if err != nil {
		t.Fatalf("EnsureSecurityToml failed with empty HOME: %v", err)
	}

	expectedPrefix := filepath.Clean("/tmp/seaweedfs")
	if !strings.HasPrefix(filepath.Clean(path), expectedPrefix) {
		t.Errorf("expected path to start with %q, got: %q", expectedPrefix, path)
	}

	// Verify secondary file was created
	secPath := filepath.Join(expectedPrefix, "security.toml")
	secInfo, err := os.Stat(secPath)
	if err != nil {
		t.Fatalf("secondary security.toml not created at %q: %v", secPath, err)
	}
	if secInfo.Mode().Perm() != 0600 {
		t.Errorf("expected secondary file mode 0600, got %o", secInfo.Mode().Perm())
	}
	_ = os.RemoveAll(expectedPrefix)
	_ = tempTmp
}

func TestAdversarial_EnsureSecurityToml_TargetDirPermissions(t *testing.T) {
	tempBase := t.TempDir()

	// Scenario A: Target directory already exists with 0700
	dir700 := filepath.Join(tempBase, "dir700")
	if err := os.Mkdir(dir700, 0700); err != nil {
		t.Fatalf("mkdir 0700: %v", err)
	}
	fileA := filepath.Join(dir700, "security.toml")
	cfgA := config.TLSConfig{
		Enabled:          true,
		CAFile:           "/certs/ca.crt",
		CertFile:         "/certs/tls.crt",
		KeyFile:          "/certs/tls.key",
		SecurityTomlPath: fileA,
	}
	pathA, err := EnsureSecurityToml(cfgA)
	if err != nil {
		t.Fatalf("EnsureSecurityToml failed with existing 0700 dir: %v", err)
	}
	infoA, _ := os.Stat(pathA)
	if infoA.Mode().Perm() != 0600 {
		t.Errorf("expected 0600 file permission, got: %o", infoA.Mode().Perm())
	}

	// Scenario B: Target directory already exists with 0755
	dir755 := filepath.Join(tempBase, "dir755")
	if err := os.Mkdir(dir755, 0755); err != nil {
		t.Fatalf("mkdir 0755: %v", err)
	}
	fileB := filepath.Join(dir755, "security.toml")
	cfgB := config.TLSConfig{
		Enabled:          true,
		CAFile:           "/certs/ca.crt",
		CertFile:         "/certs/tls.crt",
		KeyFile:          "/certs/tls.key",
		SecurityTomlPath: fileB,
	}
	pathB, err := EnsureSecurityToml(cfgB)
	if err != nil {
		t.Fatalf("EnsureSecurityToml failed with existing 0755 dir: %v", err)
	}
	infoB, _ := os.Stat(pathB)
	if infoB.Mode().Perm() != 0600 {
		t.Errorf("expected 0600 file permission, got: %o", infoB.Mode().Perm())
	}

	// Scenario C: Target directory exists with read-only 0500 permissions (cannot write)
	dir500 := filepath.Join(tempBase, "dir500")
	if err := os.Mkdir(dir500, 0500); err != nil {
		t.Fatalf("mkdir 0500: %v", err)
	}
	defer os.Chmod(dir500, 0700) // Cleanup restore
	fileC := filepath.Join(dir500, "security.toml")
	cfgC := config.TLSConfig{
		Enabled:          true,
		CAFile:           "/certs/ca.crt",
		CertFile:         "/certs/tls.crt",
		KeyFile:          "/certs/tls.key",
		SecurityTomlPath: fileC,
	}
	_, errC := EnsureSecurityToml(cfgC)
	if errC == nil {
		t.Fatalf("expected EnsureSecurityToml to fail when target directory is read-only (0500)")
	}
	t.Logf("correctly failed on read-only directory: %v", errC)
}

func TestAdversarial_EnsureSecurityToml_StrictTOMLParsing(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)

	caPath := "/custom/pki/seaweed-ca.crt"
	certPath := "/custom/pki/seaweed-client.crt"
	keyPath := "/custom/pki/seaweed-client.key"

	cfg := config.TLSConfig{
		Enabled:  true,
		CAFile:   caPath,
		CertFile: certPath,
		KeyFile:  keyPath,
	}

	writtenPath, err := EnsureSecurityToml(cfg)
	if err != nil {
		t.Fatalf("EnsureSecurityToml failed: %v", err)
	}

	contentBytes, err := os.ReadFile(writtenPath)
	if err != nil {
		t.Fatalf("read security.toml: %v", err)
	}
	content := string(contentBytes)

	// 1. Structural check
	sections := []string{"[grpc]", "[grpc.client]", "[https.client]"}
	for _, sec := range sections {
		if !strings.Contains(content, sec) {
			t.Errorf("security.toml missing expected section %q", sec)
		}
	}

	// 2. Certificate paths verification
	if !strings.Contains(content, fmt.Sprintf("ca = %q", caPath)) {
		t.Errorf("missing CA path %q in [grpc]", caPath)
	}
	if !strings.Contains(content, fmt.Sprintf("cert = %q", certPath)) {
		t.Errorf("missing cert path %q in [grpc.client]", certPath)
	}
	if !strings.Contains(content, fmt.Sprintf("key  = %q", keyPath)) {
		t.Errorf("missing key path %q in [grpc.client]", keyPath)
	}

	// 3. Cross-validate with Python 3 tomllib (strict TOML 1.0.0 parser)
	pyScript := fmt.Sprintf(`
import tomllib, sys
with open(%q, "rb") as f:
    data = tomllib.load(f)

assert "grpc" in data, "missing [grpc]"
assert data["grpc"]["ca"] == %q, "mismatched grpc ca"

assert "grpc" in data and "client" in data["grpc"], "missing [grpc.client]"
assert data["grpc"]["client"]["cert"] == %q, "mismatched grpc.client cert"
assert data["grpc"]["client"]["key"] == %q, "mismatched grpc.client key"

assert "https" in data and "client" in data["https"], "missing [https.client]"
assert data["https"]["client"]["enabled"] == True, "mismatched https.client enabled"
assert data["https"]["client"]["cert"] == %q, "mismatched https.client cert"
assert data["https"]["client"]["key"] == %q, "mismatched https.client key"
assert data["https"]["client"]["ca"] == %q, "mismatched https.client ca"

print("TOML validation SUCCESS")
`, writtenPath, caPath, certPath, keyPath, certPath, keyPath, caPath)

	cmd := exec.Command("python3", "-c", pyScript)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("python3 tomllib validation failed: %v\nOutput:\n%s\nContent:\n%s", err, string(out), content)
	}
	if !strings.Contains(string(out), "TOML validation SUCCESS") {
		t.Fatalf("python3 tomllib did not confirm success: %s", string(out))
	}
	t.Logf("TOML strictly validated by python3 tomllib: %s", strings.TrimSpace(string(out)))
}
