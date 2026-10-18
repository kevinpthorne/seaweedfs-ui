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
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"seaweedfs-ui/internal/config"
)

type TestCertificates struct {
	Dir               string
	CACertPath        string
	CAKeyPath         string
	ServerCertPath    string
	ServerKeyPath     string
	ClientCertPath    string
	ClientKeyPath     string
	UntrustedCertPath string
	UntrustedKeyPath  string
}

func generateCertKey(t *testing.T, dir string) *TestCertificates {
	t.Helper()

	// 1. Root CA
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate ca key: %v", err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1001),
		Subject: pkix.Name{
			CommonName:   "SeaweedFS Test CA",
			Organization: []string{"SeaweedFS Test Org"},
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create ca cert: %v", err)
	}

	caCertPath := filepath.Join(dir, "ca.crt")
	caKeyPath := filepath.Join(dir, "ca.key")
	writePEM(t, caCertPath, "CERTIFICATE", caDER)
	writePEM(t, caKeyPath, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(caKey))

	// 2. Server Certificate
	serverKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate server key: %v", err)
	}
	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1002),
		Subject: pkix.Name{
			CommonName:   "localhost",
			Organization: []string{"SeaweedFS Test Org"},
		},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.IPv6loopback},
		DNSNames:              []string{"localhost"},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caTemplate, &serverKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create server cert: %v", err)
	}

	serverCertPath := filepath.Join(dir, "server.crt")
	serverKeyPath := filepath.Join(dir, "server.key")
	writePEM(t, serverCertPath, "CERTIFICATE", serverDER)
	writePEM(t, serverKeyPath, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(serverKey))

	// 3. Client Certificate (Trusted)
	clientKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate client key: %v", err)
	}
	clientTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1003),
		Subject: pkix.Name{
			CommonName:   "seaweedfs-admin-client",
			Organization: []string{"SeaweedFS Test Org"},
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	clientDER, err := x509.CreateCertificate(rand.Reader, clientTemplate, caTemplate, &clientKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create client cert: %v", err)
	}

	clientCertPath := filepath.Join(dir, "client.crt")
	clientKeyPath := filepath.Join(dir, "client.key")
	writePEM(t, clientCertPath, "CERTIFICATE", clientDER)
	writePEM(t, clientKeyPath, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(clientKey))

	// 4. Untrusted Client Certificate (Self-signed by independent CA)
	untrustedCAKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	untrustedCATemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(9999),
		Subject:               pkix.Name{CommonName: "Untrusted Rogue CA"},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}
	_, _ = x509.CreateCertificate(rand.Reader, untrustedCATemplate, untrustedCATemplate, &untrustedCAKey.PublicKey, untrustedCAKey)

	untrustedClientKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	untrustedClientTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(9998),
		Subject:      pkix.Name{CommonName: "untrusted-client"},
		NotBefore:    time.Now().Add(-1 * time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	untrustedClientDER, _ := x509.CreateCertificate(rand.Reader, untrustedClientTemplate, untrustedCATemplate, &untrustedClientKey.PublicKey, untrustedCAKey)

	untrustedCertPath := filepath.Join(dir, "untrusted.crt")
	untrustedKeyPath := filepath.Join(dir, "untrusted.key")
	writePEM(t, untrustedCertPath, "CERTIFICATE", untrustedClientDER)
	writePEM(t, untrustedKeyPath, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(untrustedClientKey))

	return &TestCertificates{
		Dir:               dir,
		CACertPath:        caCertPath,
		CAKeyPath:         caKeyPath,
		ServerCertPath:    serverCertPath,
		ServerKeyPath:     serverKeyPath,
		ClientCertPath:    clientCertPath,
		ClientKeyPath:     clientKeyPath,
		UntrustedCertPath: untrustedCertPath,
		UntrustedKeyPath:  untrustedKeyPath,
	}
}

func writePEM(t *testing.T, path, blockType string, bytes []byte) {
	t.Helper()
	block := &pem.Block{Type: blockType, Bytes: bytes}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		t.Fatalf("open file %q: %v", path, err)
	}
	defer f.Close()
	if err := pem.Encode(f, block); err != nil {
		t.Fatalf("encode pem: %v", err)
	}
}

// In-memory pipe listener to avoid sandbox socket restrictions
type pipeListener struct {
	conns     chan net.Conn
	closed    chan struct{}
	closeOnce sync.Once
}

type noopDeadlineConn struct {
	net.Conn
}

func (c *noopDeadlineConn) SetDeadline(t time.Time) error      { return nil }
func (c *noopDeadlineConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *noopDeadlineConn) SetWriteDeadline(t time.Time) error { return nil }

func newPipeListener() *pipeListener {
	return &pipeListener{
		conns:  make(chan net.Conn),
		closed: make(chan struct{}),
	}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case <-l.closed:
		return nil, net.ErrClosed
	case c := <-l.conns:
		return c, nil
	}
}

func (l *pipeListener) Close() error {
	l.closeOnce.Do(func() {
		close(l.closed)
	})
	return nil
}

func (l *pipeListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 9333}
}

func (l *pipeListener) Dial() (net.Conn, error) {
	select {
	case <-l.closed:
		return nil, net.ErrClosed
	default:
	}
	s, c := net.Pipe()
	select {
	case <-l.closed:
		s.Close()
		c.Close()
		return nil, net.ErrClosed
	case l.conns <- &noopDeadlineConn{s}:
		return &noopDeadlineConn{c}, nil
	}
}

type mockMTLSServer struct {
	listener *pipeListener
	server   *http.Server
	URL      string
}

func startMockMTLSServer(t *testing.T, certs *TestCertificates) *mockMTLSServer {
	t.Helper()

	serverCert, err := tls.LoadX509KeyPair(certs.ServerCertPath, certs.ServerKeyPath)
	if err != nil {
		t.Fatalf("load server keypair: %v", err)
	}

	caBytes, err := os.ReadFile(certs.CACertPath)
	if err != nil {
		t.Fatalf("read ca cert: %v", err)
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
			if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
				http.Error(w, "missing peer certificate", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"status":"ok","cn":%q}`, r.TLS.PeerCertificates[0].Subject.CommonName)
		}),
	}

	go func() {
		_ = httpServer.Serve(tlsListener)
	}()

	return &mockMTLSServer{
		listener: listener,
		server:   httpServer,
		URL:      "https://localhost:9333",
	}
}

func (s *mockMTLSServer) Close() {
	_ = s.server.Close()
	_ = s.listener.Close()
}

func (s *mockMTLSServer) Client(clientTLS *tls.Config) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return s.listener.Dial()
			},
			TLSClientConfig: clientTLS,
		},
		Timeout: 5 * time.Second,
	}
}

// -------------------------------------------------------------
// Test Cases
// -------------------------------------------------------------

func TestBuildTLSConfig_Disabled(t *testing.T) {
	cfg := config.TLSConfig{Enabled: false}
	tlsConf, err := BuildTLSConfig(cfg)
	if err != nil {
		t.Fatalf("expected nil err, got: %v", err)
	}
	if tlsConf != nil {
		t.Fatalf("expected nil tlsConf when disabled, got: %+v", tlsConf)
	}
}

func TestBuildTLSConfig_ValidMTLS_ConnectionSuccess(t *testing.T) {
	tempDir := t.TempDir()
	certs := generateCertKey(t, tempDir)
	server := startMockMTLSServer(t, certs)
	defer server.Close()

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

	client := server.Client(clientTLS)

	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatalf("expected successful GET request over mTLS, got error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected HTTP 200 OK, got: %d", resp.StatusCode)
	}
}

func TestBuildTLSConfig_UntrustedClient_Rejected(t *testing.T) {
	tempDir := t.TempDir()
	certs := generateCertKey(t, tempDir)
	server := startMockMTLSServer(t, certs)
	defer server.Close()

	cfg := config.TLSConfig{
		Enabled:  true,
		CAFile:   certs.CACertPath,
		CertFile: certs.UntrustedCertPath,
		KeyFile:  certs.UntrustedKeyPath,
	}

	clientTLS, err := BuildTLSConfig(cfg)
	if err != nil {
		t.Fatalf("BuildTLSConfig failed: %v", err)
	}

	client := server.Client(clientTLS)

	resp, err := client.Get(server.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatalf("expected handshake failure for untrusted client cert, got HTTP 200 OK")
	}
}

func TestBuildTLSConfig_MissingClientCert_Rejected(t *testing.T) {
	tempDir := t.TempDir()
	certs := generateCertKey(t, tempDir)
	server := startMockMTLSServer(t, certs)
	defer server.Close()

	cfg := config.TLSConfig{
		Enabled: true,
		CAFile:  certs.CACertPath,
		// CertFile and KeyFile omitted
	}

	clientTLS, err := BuildTLSConfig(cfg)
	if err != nil {
		t.Fatalf("BuildTLSConfig failed: %v", err)
	}

	client := server.Client(clientTLS)

	resp, err := client.Get(server.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatalf("expected failure when client cert is missing on server requiring client cert")
	}
}

func TestBuildTLSConfig_InsecureSkipVerify(t *testing.T) {
	tempDir := t.TempDir()
	certs := generateCertKey(t, tempDir)
	server := startMockMTLSServer(t, certs)
	defer server.Close()

	cfg := config.TLSConfig{
		Enabled:            true,
		InsecureSkipVerify: true,
		CertFile:           certs.ClientCertPath,
		KeyFile:            certs.ClientKeyPath,
		// CAFile omitted, but InsecureSkipVerify is true
	}

	clientTLS, err := BuildTLSConfig(cfg)
	if err != nil {
		t.Fatalf("BuildTLSConfig failed: %v", err)
	}

	if !clientTLS.InsecureSkipVerify {
		t.Errorf("expected InsecureSkipVerify to be true")
	}

	client := server.Client(clientTLS)

	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatalf("expected connection success with InsecureSkipVerify and client cert, got: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got: %d", resp.StatusCode)
	}
}

func TestBuildTLSConfig_Errors(t *testing.T) {
	tempDir := t.TempDir()
	certs := generateCertKey(t, tempDir)

	tests := []struct {
		name      string
		cfg       config.TLSConfig
		errSubstr string
	}{
		{
			name: "missing CA file",
			cfg: config.TLSConfig{
				Enabled: true,
				CAFile:  filepath.Join(tempDir, "non_existent_ca.crt"),
			},
			errSubstr: "read CA file",
		},
		{
			name: "empty CA file",
			cfg: func() config.TLSConfig {
				p := filepath.Join(tempDir, "empty.crt")
				_ = os.WriteFile(p, []byte(""), 0600)
				return config.TLSConfig{Enabled: true, CAFile: p}
			}(),
			errSubstr: "is empty",
		},
		{
			name: "invalid PEM in CA file",
			cfg: func() config.TLSConfig {
				p := filepath.Join(tempDir, "invalid.crt")
				_ = os.WriteFile(p, []byte("NOT PEM DATA"), 0600)
				return config.TLSConfig{Enabled: true, CAFile: p}
			}(),
			errSubstr: "failed to parse valid PEM",
		},
		{
			name: "cert without key",
			cfg: config.TLSConfig{
				Enabled:  true,
				CertFile: certs.ClientCertPath,
			},
			errSubstr: "both certFile and keyFile must be provided",
		},
		{
			name: "key without cert",
			cfg: config.TLSConfig{
				Enabled: true,
				KeyFile: certs.ClientKeyPath,
			},
			errSubstr: "both certFile and keyFile must be provided",
		},
		{
			name: "mismatched keypair",
			cfg: config.TLSConfig{
				Enabled:  true,
				CertFile: certs.ClientCertPath,
				KeyFile:  certs.UntrustedKeyPath,
			},
			errSubstr: "initialize client certificate reloader",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := BuildTLSConfig(tc.cfg)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.errSubstr)
			}
			if !strings.Contains(err.Error(), tc.errSubstr) {
				t.Errorf("expected error %q to contain %q", err.Error(), tc.errSubstr)
			}
		})
	}
}

func TestCertReloader_DynamicReload(t *testing.T) {
	tempDir := t.TempDir()
	certs := generateCertKey(t, tempDir)

	reloader, err := NewCertReloader(certs.ClientCertPath, certs.ClientKeyPath)
	if err != nil {
		t.Fatalf("NewCertReloader failed: %v", err)
	}

	initialCert := reloader.Certificate()
	if initialCert == nil {
		t.Fatal("expected non-nil initial certificate")
	}

	// Wait 10ms and overwrite cert file with a newly generated cert
	time.Sleep(10 * time.Millisecond)
	newKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	caKeyPEM, _ := os.ReadFile(certs.CAKeyPath)
	caBlock, _ := pem.Decode(caKeyPEM)
	caKey, _ := x509.ParsePKCS1PrivateKey(caBlock.Bytes)

	caCertPEM, _ := os.ReadFile(certs.CACertPath)
	caCertBlock, _ := pem.Decode(caCertPEM)
	caCert, _ := x509.ParseCertificate(caCertBlock.Bytes)

	newTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(8888),
		Subject:      pkix.Name{CommonName: "rotated-client-cert"},
		NotBefore:    time.Now().Add(-1 * time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	newDER, _ := x509.CreateCertificate(rand.Reader, newTemplate, caCert, &newKey.PublicKey, caKey)
	writePEM(t, certs.ClientCertPath, "CERTIFICATE", newDER)
	writePEM(t, certs.ClientKeyPath, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(newKey))

	// Touch modification time in the future
	futureTime := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(certs.ClientCertPath, futureTime, futureTime)
	_ = os.Chtimes(certs.ClientKeyPath, futureTime, futureTime)

	// Invoke GetClientCertificate which triggers dynamic reload
	newCert, err := reloader.GetClientCertificate(nil)
	if err != nil {
		t.Fatalf("GetClientCertificate failed: %v", err)
	}

	parsed, err := x509.ParseCertificate(newCert.Certificate[0])
	if err != nil {
		t.Fatalf("parse reloaded cert: %v", err)
	}

	if parsed.Subject.CommonName != "rotated-client-cert" {
		t.Errorf("expected common name 'rotated-client-cert', got: %s", parsed.Subject.CommonName)
	}
}

func TestCertReloader_ConcurrencyRace(t *testing.T) {
	tempDir := t.TempDir()
	certs := generateCertKey(t, tempDir)

	reloader, err := NewCertReloader(certs.ClientCertPath, certs.ClientKeyPath)
	if err != nil {
		t.Fatalf("NewCertReloader failed: %v", err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// 10 concurrent readers
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = reloader.Certificate()
					_, _ = reloader.GetClientCertificate(nil)
				}
			}
		}()
	}

	// 1 writer calling Reload
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			_ = reloader.Reload()
			time.Sleep(2 * time.Millisecond)
		}
	}()

	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()
}

func TestEnsureSecurityToml_Success(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)

	cfg := config.TLSConfig{
		Enabled:  true,
		CAFile:   "/etc/seaweedfs-ui/tls/ca.crt",
		CertFile: "/etc/seaweedfs-ui/tls/tls.crt",
		KeyFile:  "/etc/seaweedfs-ui/tls/tls.key",
	}

	path, err := EnsureSecurityToml(cfg)
	if err != nil {
		t.Fatalf("EnsureSecurityToml failed: %v", err)
	}

	expectedPath := filepath.Join(tempHome, ".seaweedfs", "security.toml")
	if path != expectedPath {
		t.Fatalf("expected path %q, got: %q", expectedPath, path)
	}

	// 1. Verify directory permissions 0700
	dotDir := filepath.Dir(expectedPath)
	dirInfo, err := os.Stat(dotDir)
	if err != nil {
		t.Fatalf("stat dotDir: %v", err)
	}
	if dirInfo.Mode().Perm() != 0700 {
		t.Errorf("expected directory mode 0700, got: %o", dirInfo.Mode().Perm())
	}

	// 2. Verify file permissions 0600
	fileInfo, err := os.Stat(expectedPath)
	if err != nil {
		t.Fatalf("stat file: %v", err)
	}
	if fileInfo.Mode().Perm() != 0600 {
		t.Errorf("expected file mode 0600, got: %o", fileInfo.Mode().Perm())
	}

	// 3. Verify file content syntax
	content, err := os.ReadFile(expectedPath)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	text := string(content)

	if !strings.Contains(text, "[grpc]") {
		t.Errorf("missing [grpc] section")
	}
	if !strings.Contains(text, `ca = "/etc/seaweedfs-ui/tls/ca.crt"`) {
		t.Errorf("missing grpc ca config")
	}
	if !strings.Contains(text, "[grpc.client]") {
		t.Errorf("missing [grpc.client] section")
	}
	if !strings.Contains(text, `cert = "/etc/seaweedfs-ui/tls/tls.crt"`) {
		t.Errorf("missing grpc.client cert")
	}
	if !strings.Contains(text, `key  = "/etc/seaweedfs-ui/tls/tls.key"`) {
		t.Errorf("missing grpc.client key")
	}
	if !strings.Contains(text, "[https.client]") {
		t.Errorf("missing [https.client] section")
	}
	if !strings.Contains(text, "enabled = true") {
		t.Errorf("missing https.client enabled")
	}

	// 4. Verify secondary file exists at $HOME/security.toml
	secPath := filepath.Join(tempHome, "security.toml")
	secInfo, err := os.Stat(secPath)
	if err != nil {
		t.Fatalf("stat secondary file: %v", err)
	}
	if secInfo.Mode().Perm() != 0600 {
		t.Errorf("expected secondary file mode 0600, got: %o", secInfo.Mode().Perm())
	}
}

func TestEnsureSecurityToml_Disabled(t *testing.T) {
	cfg := config.TLSConfig{Enabled: false}
	path, err := EnsureSecurityToml(cfg)
	if err != nil {
		t.Fatalf("expected nil err, got: %v", err)
	}
	if path != "" {
		t.Fatalf("expected empty path when TLS disabled, got: %q", path)
	}
}

func TestEnsureSecurityToml_CustomPath(t *testing.T) {
	tempDir := t.TempDir()
	customPath := filepath.Join(tempDir, "custom", "security.toml")

	cfg := config.TLSConfig{
		Enabled:          true,
		CAFile:           "/certs/ca.crt",
		SecurityTomlPath: customPath,
	}

	path, err := EnsureSecurityToml(cfg)
	if err != nil {
		t.Fatalf("EnsureSecurityToml failed: %v", err)
	}
	if path != customPath {
		t.Errorf("expected custom path %q, got: %q", customPath, path)
	}

	info, err := os.Stat(customPath)
	if err != nil {
		t.Fatalf("stat custom file: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("expected mode 0600, got: %o", info.Mode().Perm())
	}
}

func TestEnsureSecurityToml_ServerOnlyTLS(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)

	cfg := config.TLSConfig{
		Enabled: true,
		CAFile:  "/certs/ca.crt",
	}

	path, err := EnsureSecurityToml(cfg)
	if err != nil {
		t.Fatalf("EnsureSecurityToml failed: %v", err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	text := string(content)

	if !strings.Contains(text, "[grpc]") {
		t.Errorf("missing [grpc]")
	}
	if strings.Contains(text, "[grpc.client]") {
		t.Errorf("should not contain [grpc.client] when client cert/key are absent")
	}
	if !strings.Contains(text, "[https.client]") {
		t.Errorf("missing [https.client]")
	}
}
