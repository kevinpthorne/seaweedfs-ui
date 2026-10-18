package weedhttp

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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

func TestGetClusterStatus(t *testing.T) {
	netw := newTestNetwork()
	_, cleanup := netw.NewServer("master1:9333", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cluster/status" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"IsLeader":true,"Leader":"master1:9333","Peers":["master1:9333","master2:9333"]}`)
	}))
	defer cleanup()

	client := NewClient(5 * time.Second)
	client.SetDialContext(netw.DialContext)

	target := cluster.ClusterTarget{
		ID:      "test-cluster",
		Masters: []string{"master1:9333"},
	}

	status, err := client.GetClusterStatus(context.Background(), target)
	if err != nil {
		t.Fatalf("GetClusterStatus failed: %v", err)
	}
	if !status.IsLeader {
		t.Errorf("expected IsLeader true, got %v", status.IsLeader)
	}
	if status.Leader != "master1:9333" {
		t.Errorf("expected Leader master1:9333, got %q", status.Leader)
	}
	if len(status.Peers) != 2 {
		t.Errorf("expected 2 peers, got %d", len(status.Peers))
	}
}

func TestGetDirStatusAndTopology(t *testing.T) {
	fixture := `{
		"Topology": {
			"Max": 100,
			"Free": 95,
			"DataCenters": [
				{
					"Id": "dc1",
					"Racks": [
						{
							"Id": "rack1",
							"DataNodes": [
								{
									"Url": "10.0.0.1:8080",
									"PublicUrl": "10.0.0.1:8080",
									"Volumes": 5,
									"Max": 100,
									"Free": 95
								}
							]
						}
					]
				}
			],
			"Layouts": [
				{
					"collection": "photos",
					"replication": "001",
					"writables": [1, 2]
				}
			]
		},
		"Version": "3.80"
	}`

	netw := newTestNetwork()
	_, cleanup := netw.NewServer("master1:9333", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/dir/status" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, fixture)
	}))
	defer cleanup()

	client := NewClient(5 * time.Second)
	client.SetDialContext(netw.DialContext)

	target := cluster.ClusterTarget{
		ID:      "test-cluster",
		Masters: []string{"master1:9333"},
	}

	dirResp, err := client.GetDirStatus(context.Background(), target)
	if err != nil {
		t.Fatalf("GetDirStatus failed: %v", err)
	}
	if dirResp.Topology.Max != 100 || dirResp.Topology.Free != 95 {
		t.Errorf("expected Max=100, Free=95; got %d, %d", dirResp.Topology.Max, dirResp.Topology.Free)
	}

	topo, err := client.GetTopology(context.Background(), target)
	if err != nil {
		t.Fatalf("GetTopology failed: %v", err)
	}
	if topo.MaxVolumeSlots != 100 || topo.FreeVolumeSlots != 95 {
		t.Errorf("topology slot mismatch: max=%d free=%d", topo.MaxVolumeSlots, topo.FreeVolumeSlots)
	}
	if len(topo.DataCenters) != 1 || topo.DataCenters[0].Id != "dc1" {
		t.Errorf("datacenter mismatch: %+v", topo.DataCenters)
	}
}

func TestGetVolumeStatus(t *testing.T) {
	fixture := `{
		"Version": "3.80",
		"Volumes": [
			{
				"Id": 1,
				"Size": 1000000,
				"ReplicaPlacement": 1,
				"Ttl": "",
				"Collection": "metrics",
				"Version": 2,
				"FileCount": 500,
				"DeleteCount": 150,
				"DeletedByteCount": 250000,
				"ReadOnly": false
			},
			{
				"Id": 2,
				"Size": 500000,
				"ReplicaPlacement": 0,
				"Ttl": "",
				"Collection": "",
				"Version": 2,
				"FileCount": 100,
				"DeleteCount": 0,
				"DeletedByteCount": 0,
				"ReadOnly": true
			}
		]
	}`

	netw := newTestNetwork()
	_, cleanup := netw.NewServer("master1:9333", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/vol/status" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, fixture)
	}))
	defer cleanup()

	client := NewClient(5 * time.Second)
	client.SetDialContext(netw.DialContext)

	target := cluster.ClusterTarget{
		ID:      "test-cluster",
		Masters: []string{"master1:9333"},
	}

	volumes, err := client.GetVolumes(context.Background(), target)
	if err != nil {
		t.Fatalf("GetVolumes failed: %v", err)
	}
	if len(volumes) != 2 {
		t.Fatalf("expected 2 volumes, got %d", len(volumes))
	}

	// Verify volume 1 GarbageRatio calculation
	v1 := volumes[0]
	if v1.ID != 1 || v1.Size != 1000000 {
		t.Errorf("v1 mismatch: %+v", v1)
	}
	expectedRatio := 250000.0 / 1000000.0
	if v1.GarbageRatio != expectedRatio {
		t.Errorf("expected garbage ratio %f, got %f", expectedRatio, v1.GarbageRatio)
	}
	if v1.ReplicaPlacement != "001" {
		t.Errorf("expected replica placement 001, got %s", v1.ReplicaPlacement)
	}

	// Verify volume 2
	v2 := volumes[1]
	if !v2.ReadOnly || v2.GarbageRatio != 0.0 {
		t.Errorf("v2 mismatch: %+v", v2)
	}
}

func TestMultiMasterFailover_307Redirect(t *testing.T) {
	netw := newTestNetwork()

	// Leader server
	_, cleanupLeader := netw.NewServer("master-leader:9333", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"IsLeader":true,"Leader":"master-leader:9333","Peers":["master-leader:9333","master-follower:9333"]}`)
	}))
	defer cleanupLeader()

	// Follower server returning 307
	_, cleanupFollower := netw.NewServer("master-follower:9333", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://master-leader:9333/cluster/status", http.StatusTemporaryRedirect)
	}))
	defer cleanupFollower()

	client := NewClient(5 * time.Second)
	client.SetDialContext(netw.DialContext)

	target := cluster.ClusterTarget{
		ID:      "multi-master-cluster",
		Masters: []string{"master-follower:9333", "master-leader:9333"},
	}

	status, err := client.GetClusterStatus(context.Background(), target)
	if err != nil {
		t.Fatalf("failed through 307 redirect: %v", err)
	}
	if !status.IsLeader {
		t.Errorf("expected IsLeader true from redirected leader")
	}

	// Verify cached leader was updated
	client.mu.RLock()
	cachedLeader := client.leaderCache[target.ID]
	client.mu.RUnlock()
	if cachedLeader != "master-leader:9333" {
		t.Errorf("expected cached leader master-leader:9333, got %s", cachedLeader)
	}
}

func TestMultiMasterFailover_NetworkError(t *testing.T) {
	netw := newTestNetwork()

	// Healthy server only (master-dead is not in netw listeners, so dial will fail)
	_, cleanupHealthy := netw.NewServer("master-healthy:9333", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"IsLeader":true,"Leader":"master-healthy:9333"}`)
	}))
	defer cleanupHealthy()

	client := NewClient(5 * time.Second)
	client.SetDialContext(netw.DialContext)

	target := cluster.ClusterTarget{
		ID:      "failover-cluster",
		Masters: []string{"master-dead:9333", "master-healthy:9333"},
	}

	status, err := client.GetClusterStatus(context.Background(), target)
	if err != nil {
		t.Fatalf("failover to healthy master failed: %v", err)
	}
	if status.Leader != "master-healthy:9333" {
		t.Errorf("expected Leader master-healthy:9333, got %s", status.Leader)
	}
}

func TestAllMastersUnreachable(t *testing.T) {
	netw := newTestNetwork()
	client := NewClient(1 * time.Second)
	client.SetDialContext(netw.DialContext)

	target := cluster.ClusterTarget{
		ID:      "dead-cluster",
		Masters: []string{"dead1:9333", "dead2:9333"},
	}

	_, err := client.GetClusterStatus(context.Background(), target)
	if !errors.Is(err, ErrAllMastersUnreachable) {
		t.Fatalf("expected ErrAllMastersUnreachable, got %v", err)
	}
}

func TestListFilerDir_POSIXMode(t *testing.T) {
	fixture := `{
		"Path": "/buckets/data",
		"Entries": [
			{
				"FullPath": "/buckets/data/subdir",
				"Mtime": "2026-10-17T12:00:00Z",
				"Crtime": "2026-10-17T12:00:00Z",
				"Mode": 2147484141,
				"Uid": 1000,
				"Gid": 1000,
				"Mime": "",
				"FileSize": 0
			},
			{
				"FullPath": "/buckets/data/report.pdf",
				"Mtime": "2026-10-17T12:05:00Z",
				"Crtime": "2026-10-17T12:05:00Z",
				"Mode": 420,
				"Uid": 1000,
				"Gid": 1000,
				"Mime": "application/pdf",
				"FileSize": 2048,
				"chunks": [
					{"file_id": "1,123", "size": 2048, "mtime": 1729180000}
				]
			}
		],
		"Limit": 200,
		"LastFileName": "report.pdf",
		"ShouldDisplayLoadMore": false
	}`

	netw := newTestNetwork()
	_, cleanup := netw.NewServer("filer:8888", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/json" {
			t.Errorf("missing Accept: application/json header")
		}
		if !strings.Contains(r.URL.RawQuery, "limit=200") {
			t.Errorf("missing limit=200 query: %s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, fixture)
	}))
	defer cleanup()

	client := NewClient(5 * time.Second)
	client.SetDialContext(netw.DialContext)

	target := cluster.ClusterTarget{
		ID:    "filer-cluster",
		Filer: "filer:8888",
	}

	res, err := client.ListFilerDir(context.Background(), target, "/buckets/data", 200)
	if err != nil {
		t.Fatalf("ListFilerDir failed: %v", err)
	}

	if len(res.Entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(res.Entries))
	}

	// 1. Directory entry validation
	dirEntry := res.Entries[0]
	if !dirEntry.IsDir() {
		t.Errorf("expected isDir true for mode %d", dirEntry.Mode)
	}
	if dirEntry.Name() != "subdir" {
		t.Errorf("expected name 'subdir', got %q", dirEntry.Name())
	}
	if dirEntry.Permissions() != "drwxr-xr-x" {
		t.Errorf("expected permissions 'drwxr-xr-x', got %q", dirEntry.Permissions())
	}

	// 2. Regular file entry validation
	fileEntry := res.Entries[1]
	if fileEntry.IsDir() {
		t.Errorf("expected isDir false for regular file")
	}
	if fileEntry.Name() != "report.pdf" {
		t.Errorf("expected name 'report.pdf', got %q", fileEntry.Name())
	}
	if fileEntry.Permissions() != "-rw-r--r--" {
		t.Errorf("expected permissions '-rw-r--r--', got %q", fileEntry.Permissions())
	}
	if fileEntry.Size() != 2048 {
		t.Errorf("expected size 2048, got %d", fileEntry.Size())
	}
}

func TestStreamFileDownload(t *testing.T) {
	fileContent := "Hello, SeaweedFS stream payload!"
	netw := newTestNetwork()
	_, cleanup := netw.NewServer("filer:8888", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/files/hello.txt" {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(fileContent))
			return
		}
		http.NotFound(w, r)
	}))
	defer cleanup()

	client := NewClient(5 * time.Second)
	client.SetDialContext(netw.DialContext)

	target := cluster.ClusterTarget{
		ID:    "c1",
		Filer: "filer:8888",
	}

	// 1. Success download
	reader, contentType, err := client.StreamFile(context.Background(), target, "/files/hello.txt")
	if err != nil {
		t.Fatalf("StreamFile failed: %v", err)
	}
	defer reader.Close()

	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read stream body failed: %v", err)
	}
	if string(body) != fileContent {
		t.Errorf("content mismatch: %s", string(body))
	}
	if !strings.HasPrefix(contentType, "text/plain") {
		t.Errorf("contentType mismatch: %s", contentType)
	}

	// 2. 404 Not Found
	_, _, errNotFound := client.StreamFile(context.Background(), target, "/files/nonexistent.bin")
	if !errors.Is(errNotFound, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", errNotFound)
	}
}

func TestUploadFileStreaming(t *testing.T) {
	uploadedName := ""
	var uploadedBytes []byte

	netw := newTestNetwork()
	_, cleanup := netw.NewServer("filer:8888", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mr, err := r.MultipartReader()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if part.FormName() == "file" {
				uploadedName = part.FileName()
				uploadedBytes, _ = io.ReadAll(part)
			}
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer cleanup()

	client := NewClient(5 * time.Second)
	client.SetDialContext(netw.DialContext)

	target := cluster.ClusterTarget{
		ID:    "c1",
		Filer: "filer:8888",
	}

	content := "multipart stream test content 123456789"
	err := client.UploadFile(context.Background(), target, "/upload", "test.txt", strings.NewReader(content), "text/plain")
	if err != nil {
		t.Fatalf("UploadFile failed: %v", err)
	}

	if uploadedName != "test.txt" {
		t.Errorf("expected filename 'test.txt', got %q", uploadedName)
	}
	if string(uploadedBytes) != content {
		t.Errorf("uploaded content mismatch: %s", string(uploadedBytes))
	}
}

func TestDeleteEntry(t *testing.T) {
	deletedURL := ""
	netw := newTestNetwork()
	_, cleanup := netw.NewServer("filer:8888", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deletedURL = r.URL.RequestURI()
			if strings.Contains(r.URL.Path, "missing") {
				http.NotFound(w, r)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.Error(w, "bad method", http.StatusMethodNotAllowed)
	}))
	defer cleanup()

	client := NewClient(5 * time.Second)
	client.SetDialContext(netw.DialContext)

	target := cluster.ClusterTarget{
		ID:    "c1",
		Filer: "filer:8888",
	}

	// 1. Delete file
	err := client.DeleteEntry(context.Background(), target, "/files/old.log", false)
	if err != nil {
		t.Fatalf("DeleteEntry file failed: %v", err)
	}
	if deletedURL != "/files/old.log" {
		t.Errorf("delete file URL mismatch: %s", deletedURL)
	}

	// 2. Delete directory (recursive query)
	err = client.DeleteEntry(context.Background(), target, "/files/tempdir", true)
	if err != nil {
		t.Fatalf("DeleteEntry dir failed: %v", err)
	}
	if !strings.Contains(deletedURL, "recursive=true") {
		t.Errorf("expected recursive=true in query: %s", deletedURL)
	}

	// 3. Delete 404
	err404 := client.DeleteEntry(context.Background(), target, "/files/missing", false)
	if !errors.Is(err404, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err404)
	}
}

func TestCheckS3Status(t *testing.T) {
	netw := newTestNetwork()

	// S3 daemon alive returning 403 AccessDenied
	_, cleanupAlive := netw.NewServer("s3-alive:8333", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer cleanupAlive()

	client := NewClient(5 * time.Second)
	client.SetDialContext(netw.DialContext)

	target := cluster.ClusterTarget{
		ID: "c1",
		S3: "s3-alive:8333",
	}

	status, err := client.CheckS3Status(context.Background(), target)
	if err != nil {
		t.Fatalf("CheckS3Status failed: %v", err)
	}
	if !status.Available {
		t.Errorf("expected S3 daemon available on HTTP 403")
	}

	// Dead S3 listener (not registered in netw, will simulate connection refused)
	targetDead := cluster.ClusterTarget{
		ID: "c2",
		S3: "s3-dead:8333",
	}
	statusDead, err := client.CheckS3Status(context.Background(), targetDead)
	if err != nil {
		t.Fatalf("unexpected probe error: %v", err)
	}
	if statusDead.Available {
		t.Errorf("expected dead S3 to be unavailable")
	}
}

func TestMTLSConnection(t *testing.T) {
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

	// Server cert for "localhost" and "master-mtls"
	serverKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "master-mtls"},
		DNSNames:     []string{"master-mtls", "localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
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
	_, cleanup := netw.NewTLSServer("master-mtls:9333", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"IsLeader":true,"Leader":"master-mtls:9333"}`)
	}), serverTLS)
	defer cleanup()

	target := cluster.ClusterTarget{
		ID:      "mtls-cluster",
		Masters: []string{"master-mtls:9333"},
		TLS: config.TLSConfig{
			Enabled:  true,
			CAFile:   caFile,
			CertFile: certFile,
			KeyFile:  keyFile,
		},
	}

	client := NewClient(5 * time.Second)
	client.SetDialContext(netw.DialContext)

	status, err := client.GetClusterStatus(context.Background(), target)
	if err != nil {
		t.Fatalf("mTLS GetClusterStatus failed: %v", err)
	}
	if status.Leader != "master-mtls:9333" {
		t.Errorf("expected Leader master-mtls:9333, got %s", status.Leader)
	}

	// Untrusted target without client certificate is rejected by server
	untrustedTarget := cluster.ClusterTarget{
		ID:      "untrusted-cluster",
		Masters: []string{"master-mtls:9333"},
		TLS: config.TLSConfig{
			Enabled:  true,
			CAFile:   caFile,
			CertFile: "", // Missing client cert
			KeyFile:  "",
		},
	}
	_, errUntrusted := client.GetClusterStatus(context.Background(), untrustedTarget)
	if errUntrusted == nil {
		t.Errorf("expected mTLS connection to fail without client certificate")
	}
}
