package filersync

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"seaweedfs-ui/internal/cluster"
	"seaweedfs-ui/internal/config"
)

// In-memory pipe network to support real HTTP/HTTPS servers under macOS sandbox restrictions
type noopDeadlineConn struct {
	net.Conn
}

func (c *noopDeadlineConn) SetDeadline(t time.Time) error      { return nil }
func (c *noopDeadlineConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *noopDeadlineConn) SetWriteDeadline(t time.Time) error { return nil }

type pipeAddr struct {
	addr string
}

func (a *pipeAddr) Network() string { return "tcp" }
func (a *pipeAddr) String() string  { return a.addr }

type pipeListener struct {
	conns     chan net.Conn
	closed    chan struct{}
	closeOnce sync.Once
	addr      string
}

func newPipeListener(addr string) *pipeListener {
	return &pipeListener{
		conns:  make(chan net.Conn),
		closed: make(chan struct{}),
		addr:   addr,
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
	return &pipeAddr{addr: l.addr}
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

type testNetwork struct {
	mu        sync.Mutex
	listeners map[string]*pipeListener
}

func newTestNetwork() *testNetwork {
	return &testNetwork{
		listeners: make(map[string]*pipeListener),
	}
}

func (n *testNetwork) NewServer(addr string, handler http.Handler) (*http.Server, func()) {
	l := newPipeListener(addr)
	n.mu.Lock()
	n.listeners[addr] = l
	n.mu.Unlock()

	srv := &http.Server{Handler: handler}
	go func() {
		_ = srv.Serve(l)
	}()

	cleanup := func() {
		_ = srv.Close()
		_ = l.Close()
		n.mu.Lock()
		delete(n.listeners, addr)
		n.mu.Unlock()
	}
	return srv, cleanup
}

func (n *testNetwork) NewTLSServer(addr string, handler http.Handler, tlsConfig *tls.Config) (*http.Server, func()) {
	l := newPipeListener(addr)
	n.mu.Lock()
	n.listeners[addr] = l
	n.mu.Unlock()

	tlsListener := tls.NewListener(l, tlsConfig)
	srv := &http.Server{Handler: handler, TLSConfig: tlsConfig}
	go func() {
		_ = srv.Serve(tlsListener)
	}()

	cleanup := func() {
		_ = srv.Close()
		_ = l.Close()
		n.mu.Lock()
		delete(n.listeners, addr)
		n.mu.Unlock()
	}
	return srv, cleanup
}

func (n *testNetwork) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	n.mu.Lock()
	l, exists := n.listeners[addr]
	n.mu.Unlock()
	if !exists {
		return nil, fmt.Errorf("connection refused: %s", addr)
	}
	return l.Dial()
}

func TestInspectSyncJobs_Healthy(t *testing.T) {
	fixedNow := time.Unix(1729180000, 0).UTC()
	// Offset 2 seconds prior to now (nanoseconds)
	offsetNs := fixedNow.Add(-2 * time.Second).UnixNano()

	metricsText := fmt.Sprintf(`
# HELP SeaweedFS_filerSync_sync_offset replication offset
# TYPE SeaweedFS_filerSync_sync_offset gauge
SeaweedFS_filerSync_sync_offset{source="f1",target="f2"} %d
SeaweedFS_filerSync_synced_files_total 14800
SeaweedFS_filerSync_bytes_per_second 1048576.5
SeaweedFS_filerSync_sync_errors_total 0
`, offsetNs)

	netw := newTestNetwork()
	_, cleanup := netw.NewServer("sync-pod:9327", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, metricsText)
	}))
	defer cleanup()

	insp := NewInspector(10*time.Second, 60*time.Second, 3*time.Second)
	insp.SetDialContext(netw.DialContext)
	insp.SetNowFunc(func() time.Time { return fixedNow })

	target := cluster.ClusterTarget{
		ID: "c1",
		FilerSyncs: []cluster.FilerSyncTarget{
			{
				ID:          "sync-1",
				PodName:     "sync-pod",
				MetricsPort: 9327,
				SourceFiler: "filer1:8888",
				TargetFiler: "filer2:8888",
			},
		},
	}

	results, err := insp.InspectSyncJobs(context.Background(), target)
	if err != nil {
		t.Fatalf("InspectSyncJobs failed: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	res := results[0]
	if res.Status != StatusHealthy {
		t.Errorf("expected HEALTHY, got %s", res.Status)
	}
	if res.ReplicationLagSeconds < 1.9 || res.ReplicationLagSeconds > 2.1 {
		t.Errorf("expected ~2.0s lag, got %f", res.ReplicationLagSeconds)
	}
	if res.SyncedFileCount != 14800 {
		t.Errorf("expected 14800 files, got %d", res.SyncedFileCount)
	}
	if res.BytesPerSecond != 1048576.5 {
		t.Errorf("expected 1048576.5 Bps, got %f", res.BytesPerSecond)
	}
}

func TestInspectSyncJobs_Lagging(t *testing.T) {
	fixedNow := time.Unix(1729180000, 0).UTC()
	// Offset 45 seconds prior to now (nanoseconds)
	offsetNs := fixedNow.Add(-45 * time.Second).UnixNano()

	metricsText := fmt.Sprintf(`
SeaweedFS_filerSync_sync_offset %d
SeaweedFS_filerSync_synced_files_total 500
SeaweedFS_filerSync_sync_errors_total 0
`, offsetNs)

	netw := newTestNetwork()
	_, cleanup := netw.NewServer("sync-pod:9327", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, metricsText)
	}))
	defer cleanup()

	insp := NewInspector(10*time.Second, 60*time.Second, 3*time.Second)
	insp.SetDialContext(netw.DialContext)
	insp.SetNowFunc(func() time.Time { return fixedNow })

	target := cluster.ClusterTarget{
		ID: "c1",
		FilerSyncs: []cluster.FilerSyncTarget{
			{
				ID:          "sync-1",
				PodName:     "sync-pod",
				MetricsPort: 9327,
			},
		},
	}

	results, err := insp.InspectSyncJobs(context.Background(), target)
	if err != nil {
		t.Fatalf("InspectSyncJobs failed: %v", err)
	}

	if results[0].Status != StatusLagging {
		t.Errorf("expected LAGGING, got %s", results[0].Status)
	}
	if results[0].ReplicationLagSeconds < 44.9 || results[0].ReplicationLagSeconds > 45.1 {
		t.Errorf("expected ~45.0s lag, got %f", results[0].ReplicationLagSeconds)
	}
}

func TestInspectSyncJobs_SyncErrors(t *testing.T) {
	metricsText := `
SeaweedFS_filerSync_sync_offset 1729180000000000000
SeaweedFS_filerSync_sync_errors_total 5
`
	netw := newTestNetwork()
	_, cleanup := netw.NewServer("sync-pod:9327", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, metricsText)
	}))
	defer cleanup()

	insp := NewInspector(10*time.Second, 60*time.Second, 3*time.Second)
	insp.SetDialContext(netw.DialContext)

	target := cluster.ClusterTarget{
		ID: "c1",
		FilerSyncs: []cluster.FilerSyncTarget{
			{
				ID:          "sync-1",
				PodName:     "sync-pod",
				MetricsPort: 9327,
			},
		},
	}

	results, err := insp.InspectSyncJobs(context.Background(), target)
	if err != nil {
		t.Fatalf("InspectSyncJobs failed: %v", err)
	}

	if results[0].Status != StatusError {
		t.Errorf("expected ERROR on error count > 0, got %s", results[0].Status)
	}
}

func TestInspectSyncJobs_UnreachablePort(t *testing.T) {
	netw := newTestNetwork()
	// Do not start any server on dead-pod:9327

	insp := NewInspector(10*time.Second, 60*time.Second, 1*time.Second)
	insp.SetDialContext(netw.DialContext)

	target := cluster.ClusterTarget{
		ID: "c1",
		FilerSyncs: []cluster.FilerSyncTarget{
			{
				ID:          "sync-dead",
				PodName:     "dead-pod",
				MetricsPort: 9327,
			},
		},
	}

	results, err := insp.InspectSyncJobs(context.Background(), target)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if results[0].Status != StatusDegraded {
		t.Errorf("expected DEGRADED for unreachable metrics port, got %s", results[0].Status)
	}
}

func TestInspectSyncJobs_HTTPError(t *testing.T) {
	netw := newTestNetwork()
	_, cleanup := netw.NewServer("sync-pod:9327", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}))
	defer cleanup()

	insp := NewInspector(10*time.Second, 60*time.Second, 3*time.Second)
	insp.SetDialContext(netw.DialContext)

	target := cluster.ClusterTarget{
		ID: "c1",
		FilerSyncs: []cluster.FilerSyncTarget{
			{
				ID:          "sync-1",
				PodName:     "sync-pod",
				MetricsPort: 9327,
			},
		},
	}

	results, err := insp.InspectSyncJobs(context.Background(), target)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if results[0].Status != StatusDegraded {
		t.Errorf("expected DEGRADED for HTTP 500, got %s", results[0].Status)
	}
}

func TestInspectSyncJobs_MalformedPrometheus(t *testing.T) {
	netw := newTestNetwork()
	_, cleanup := netw.NewServer("sync-pod:9327", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<html><body>not prometheus text</body></html>")
	}))
	defer cleanup()

	insp := NewInspector(10*time.Second, 60*time.Second, 3*time.Second)
	insp.SetDialContext(netw.DialContext)

	target := cluster.ClusterTarget{
		ID: "c1",
		FilerSyncs: []cluster.FilerSyncTarget{
			{
				ID:          "sync-1",
				PodName:     "sync-pod",
				MetricsPort: 9327,
			},
		},
	}

	results, err := insp.InspectSyncJobs(context.Background(), target)
	if err != nil {
		t.Fatalf("InspectSyncJobs should handle malformed text gracefully: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result")
	}
}

func TestInspectSyncJobs_TimestampScales(t *testing.T) {
	fixedNow := time.Unix(1729180000, 0).UTC()
	targetTime := fixedNow.Add(-10 * time.Second)

	// 1. Nanoseconds scale
	lagNs, tsNs := computeLag(float64(targetTime.UnixNano()), fixedNow)
	if math.Abs(lagNs-10.0) > 0.01 || tsNs != targetTime.Unix() {
		t.Errorf("ns scale mismatch: lag=%f ts=%d", lagNs, tsNs)
	}

	// 2. Milliseconds scale
	lagMs, tsMs := computeLag(float64(targetTime.UnixMilli()), fixedNow)
	if math.Abs(lagMs-10.0) > 0.01 || tsMs != targetTime.Unix() {
		t.Errorf("ms scale mismatch: lag=%f ts=%d", lagMs, tsMs)
	}

	// 3. Seconds scale
	lagSec, tsSec := computeLag(float64(targetTime.Unix()), fixedNow)
	if math.Abs(lagSec-10.0) > 0.01 || tsSec != targetTime.Unix() {
		t.Errorf("sec scale mismatch: lag=%f ts=%d", lagSec, tsSec)
	}
}

func TestInspectSyncJobs_NegativeLagClockSkew(t *testing.T) {
	fixedNow := time.Unix(1729180000, 0).UTC()
	futureTime := fixedNow.Add(5 * time.Second)

	lag, _ := computeLag(float64(futureTime.Unix()), fixedNow)
	if lag != 0.0 {
		t.Errorf("expected clamped 0.0 lag for future clock drift, got %f", lag)
	}
}

func TestInspectSyncJobs_NoMetricsPort(t *testing.T) {
	insp := NewInspector(10*time.Second, 60*time.Second, 3*time.Second)

	target := cluster.ClusterTarget{
		ID: "c1",
		FilerSyncs: []cluster.FilerSyncTarget{
			{
				ID:          "sync-no-port",
				MetricsPort: 0,
				MetricsURL:  "",
				SourceFiler: "f1",
				TargetFiler: "f2",
			},
		},
	}

	results, err := insp.InspectSyncJobs(context.Background(), target)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if results[0].Status != StatusDegraded {
		t.Errorf("expected DEGRADED when no metrics port configured, got %s", results[0].Status)
	}
}

func TestInspectSyncJobs_ModeMapping(t *testing.T) {
	insp := NewInspector(10*time.Second, 60*time.Second, 3*time.Second)

	target := cluster.ClusterTarget{
		ID: "c1",
		FilerSyncs: []cluster.FilerSyncTarget{
			{ID: "s1", IsActivePassive: true},
			{ID: "s2", IsActivePassive: false},
		},
	}

	results, err := insp.InspectSyncJobs(context.Background(), target)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if results[0].Mode != ModeActivePassive {
		t.Errorf("expected Active-Passive, got %s", results[0].Mode)
	}
	if results[1].Mode != ModeActiveActive {
		t.Errorf("expected Active-Active, got %s", results[1].Mode)
	}
}

func TestInspectSyncJobs_ConcurrentScraping(t *testing.T) {
	netw := newTestNetwork()

	// Register 5 mock servers
	for i := 1; i <= 5; i++ {
		addr := fmt.Sprintf("pod-%d:9327", i)
		count := i * 100
		netw.NewServer(addr, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "SeaweedFS_filerSync_synced_files_total %d\n", count)
		}))
	}

	insp := NewInspector(10*time.Second, 60*time.Second, 2*time.Second)
	insp.SetDialContext(netw.DialContext)

	var targets []cluster.FilerSyncTarget
	for i := 1; i <= 5; i++ {
		targets = append(targets, cluster.FilerSyncTarget{
			ID:          fmt.Sprintf("sync-%d", i),
			PodName:     fmt.Sprintf("pod-%d", i),
			MetricsPort: 9327,
		})
	}

	target := cluster.ClusterTarget{
		ID:         "c1",
		FilerSyncs: targets,
	}

	results, err := insp.InspectSyncJobs(context.Background(), target)
	if err != nil {
		t.Fatalf("InspectSyncJobs failed: %v", err)
	}

	if len(results) != 5 {
		t.Fatalf("expected 5 results, got %d", len(results))
	}
}

func TestInspectSyncJobs_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // canceled immediately

	insp := NewInspector(10*time.Second, 60*time.Second, 1*time.Second)

	target := cluster.ClusterTarget{
		ID: "c1",
		FilerSyncs: []cluster.FilerSyncTarget{
			{
				ID:          "s1",
				PodName:     "pod1",
				MetricsPort: 9327,
			},
		},
	}

	results, err := insp.InspectSyncJobs(ctx, target)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result")
	}
}

func TestInspectSyncJobs_mTLS(t *testing.T) {
	// Generate Ephemeral CA, Server, and Client Certificates
	caKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Test CA"},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	caCert, _ := x509.ParseCertificate(caDER)

	// Server cert for "metrics-pod"
	serverKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "metrics-pod"},
		DNSNames:     []string{"metrics-pod"},
		NotBefore:    time.Now().Add(-1 * time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	serverDER, _ := x509.CreateCertificate(rand.Reader, serverTemplate, caCert, &serverKey.PublicKey, caKey)
	serverTLSCert := tls.Certificate{
		Certificate: [][]byte{serverDER},
		PrivateKey:  serverKey,
	}

	// Client cert
	clientKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	clientTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(3),
		Subject:      pkix.Name{CommonName: "client"},
		NotBefore:    time.Now().Add(-1 * time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	clientDER, _ := x509.CreateCertificate(rand.Reader, clientTemplate, caCert, &clientKey.PublicKey, caKey)

	tmpDir := t.TempDir()
	caFile := filepath.Join(tmpDir, "ca.crt")
	certFile := filepath.Join(tmpDir, "client.crt")
	keyFile := filepath.Join(tmpDir, "client.key")

	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: clientDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(clientKey)})

	_ = os.WriteFile(caFile, caPEM, 0600)
	_ = os.WriteFile(certFile, certPEM, 0600)
	_ = os.WriteFile(keyFile, keyPEM, 0600)

	clientCAPool := x509.NewCertPool()
	clientCAPool.AddCert(caCert)

	serverTLS := &tls.Config{
		Certificates: []tls.Certificate{serverTLSCert},
		ClientCAs:    clientCAPool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
	}

	netw := newTestNetwork()
	_, cleanup := netw.NewTLSServer("metrics-pod:9327", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintln(w, "SeaweedFS_filerSync_synced_files_total 9999")
	}), serverTLS)
	defer cleanup()

	insp := NewInspector(10*time.Second, 60*time.Second, 3*time.Second)
	insp.SetDialContext(netw.DialContext)

	target := cluster.ClusterTarget{
		ID: "mtls-cluster",
		TLS: config.TLSConfig{
			Enabled:  true,
			CAFile:   caFile,
			CertFile: certFile,
			KeyFile:  keyFile,
		},
		FilerSyncs: []cluster.FilerSyncTarget{
			{
				ID:          "sync-mtls",
				PodName:     "metrics-pod",
				MetricsPort: 9327,
			},
		},
	}

	results, err := insp.InspectSyncJobs(context.Background(), target)
	if err != nil {
		t.Fatalf("InspectSyncJobs failed over mTLS: %v", err)
	}
	if len(results) != 1 || results[0].SyncedFileCount != 9999 {
		t.Errorf("mTLS sync metrics mismatch: %+v", results)
	}
}

func TestToFilerSyncStatus(t *testing.T) {
	s := SyncStatus{
		SourceFiler:           "f1:8888",
		TargetFiler:           "f2:8888",
		SourcePath:            "/a",
		TargetPath:            "/b",
		Mode:                  ModeActivePassive,
		PodName:               "pod-1",
		Namespace:             "seaweedfs",
		ReplicationLagSeconds: 1.5,
		SyncedFileCount:       1234,
		BytesPerSecond:        567.8,
		LastSyncTimestamp:     1729180000,
		Status:                StatusHealthy,
	}

	apiModel := s.ToFilerSyncStatus("s1", "prod-to-dr", "kubernetes")
	if apiModel.ID != "s1" || apiModel.Name != "prod-to-dr" {
		t.Errorf("identity mismatch: %s %s", apiModel.ID, apiModel.Name)
	}
	if !apiModel.Ready {
		t.Errorf("expected Ready true for StatusHealthy")
	}
	if apiModel.Mode != ModeActivePassive {
		t.Errorf("mode mismatch: %s", apiModel.Mode)
	}
	if apiModel.Source.Filer != "f1:8888" || apiModel.Source.Path != "/a" {
		t.Errorf("source mismatch: %+v", apiModel.Source)
	}
	if apiModel.Metrics.SyncedFilesTotal != 1234 {
		t.Errorf("synced files mismatch: %d", apiModel.Metrics.SyncedFilesTotal)
	}
	if apiModel.Checkpoint.ReplicationLagSeconds != 1.5 {
		t.Errorf("lag mismatch: %f", apiModel.Checkpoint.ReplicationLagSeconds)
	}
}
