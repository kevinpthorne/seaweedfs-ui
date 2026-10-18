package weedcli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"seaweedfs-ui/internal/cluster"
	"seaweedfs-ui/internal/config"
)

type MockCall struct {
	BinaryPath string
	Env        []string
	Args       []string
	Stdin      string
}

type MockCmdRunner struct {
	mu       sync.Mutex
	Calls    []MockCall
	Response struct {
		Stdout   string
		Stderr   string
		ExitCode int
		Err      error
	}
	Handler func(ctx context.Context, binaryPath string, env []string, stdin io.Reader, args ...string) (string, string, int, error)
}

func (m *MockCmdRunner) Run(ctx context.Context, binaryPath string, env []string, stdin io.Reader, args ...string) (string, string, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var stdinStr string
	if stdin != nil {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(stdin)
		stdinStr = buf.String()
	}

	m.Calls = append(m.Calls, MockCall{
		BinaryPath: binaryPath,
		Env:        env,
		Args:       args,
		Stdin:      stdinStr,
	})

	if m.Handler != nil {
		return m.Handler(ctx, binaryPath, env, strings.NewReader(stdinStr), args...)
	}

	return m.Response.Stdout, m.Response.Stderr, m.Response.ExitCode, m.Response.Err
}

func testTarget() cluster.ClusterTarget {
	return cluster.ClusterTarget{
		ID:      "c1",
		Name:    "Cluster One",
		Masters: []string{"10.0.0.1:9333", "10.0.0.2:9333"},
		Filer:   "10.0.0.1:8888",
	}
}

func TestStdinFramingAndLocking(t *testing.T) {
	runner := &MockCmdRunner{}
	runner.Response.Stdout = "ok"

	cfg := &config.Config{
		AllowAdminActions: true,
		WeedBinaryPath:    "/bin/weed",
		QueryTimeout:      5 * time.Second,
		ActionTimeout:     10 * time.Second,
	}

	client := NewClient(cfg, runner, nil, nil, nil)
	target := testTarget()

	// 1. Mutating operation requires lock
	_, err := client.Vacuum(context.Background(), target, "")
	if err != nil {
		t.Fatalf("Vacuum failed: %v", err)
	}

	if len(runner.Calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(runner.Calls))
	}

	call := runner.Calls[0]
	expectedStdin := "lock\nvolume.vacuum\nunlock\nexit\n"
	if call.Stdin != expectedStdin {
		t.Errorf("stdin mismatch:\nexpected:\n%s\ngot:\n%s", expectedStdin, call.Stdin)
	}

	// Verify command line arguments
	if call.BinaryPath != "/bin/weed" {
		t.Errorf("expected binary /bin/weed, got %s", call.BinaryPath)
	}
	argsStr := strings.Join(call.Args, " ")
	if !strings.Contains(argsStr, "-master=10.0.0.1:9333,10.0.0.2:9333") || !strings.Contains(argsStr, "-filer=10.0.0.1:8888") {
		t.Errorf("args mismatch: %s", argsStr)
	}

	// Verify HOME=/tmp/seaweedfs in env
	foundHome := false
	for _, env := range call.Env {
		if env == "HOME=/tmp/seaweedfs" {
			foundHome = true
			break
		}
	}
	if !foundHome {
		t.Errorf("HOME=/tmp/seaweedfs not found in environment: %v", call.Env)
	}

	// 2. Read-only operation does NOT require lock
	_, err = client.ListBuckets(context.Background(), target)
	if err != nil {
		t.Fatalf("ListBuckets failed: %v", err)
	}

	call2 := runner.Calls[1]
	expectedStdin2 := "s3.bucket.list\nexit\n"
	if call2.Stdin != expectedStdin2 {
		t.Errorf("stdin mismatch for read-only:\nexpected:\n%s\ngot:\n%s", expectedStdin2, call2.Stdin)
	}
}

func TestAllowAdminActionsEnforcement(t *testing.T) {
	runner := &MockCmdRunner{}
	cfg := &config.Config{
		AllowAdminActions: false, // Disabled!
	}
	client := NewClient(cfg, runner, nil, nil, nil)
	target := testTarget()

	mutatingActions := []struct {
		name string
		fn   func() error
	}{
		{"Vacuum", func() error { _, err := client.Vacuum(context.Background(), target, ""); return err }},
		{"Balance", func() error { _, err := client.Balance(context.Background(), target); return err }},
		{"FixReplication", func() error { _, err := client.FixReplication(context.Background(), target); return err }},
		{"Grow", func() error { _, err := client.Grow(context.Background(), target, 1, "", "", ""); return err }},
		{"CreateBucket", func() error { _, err := client.CreateBucket(context.Background(), target, "b1"); return err }},
		{"DeleteBucket", func() error { _, err := client.DeleteBucket(context.Background(), target, "b1"); return err }},
	}

	for _, tc := range mutatingActions {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.fn()
			if !errors.Is(err, ErrAdminActionsDisabled) {
				t.Fatalf("expected ErrAdminActionsDisabled, got %v", err)
			}
		})
	}

	// Subprocess must never have been called
	if len(runner.Calls) != 0 {
		t.Errorf("expected 0 subprocess calls, got %d", len(runner.Calls))
	}
}

func TestEnableRawShellEnforcement(t *testing.T) {
	runner := &MockCmdRunner{}
	runner.Response.Stdout = "ok"
	target := testTarget()

	// 1. EnableRawShell = false
	cfg := &config.Config{
		EnableRawShell:    false,
		AllowAdminActions: false,
	}
	client := NewClient(cfg, runner, nil, nil, nil)

	// Presets should succeed
	presets := []string{"cluster.status", "volume.list", "s3.bucket.list", "s3.configure", "fs.ls"}
	for _, p := range presets {
		_, err := client.Execute(context.Background(), target, p, true)
		if err != nil {
			t.Errorf("preset %q failed unexpectedly: %v", p, err)
		}
	}

	// Arbitrary command must be rejected
	_, err := client.Execute(context.Background(), target, "volume.tier.upload", true)
	if !errors.Is(err, ErrRawShellDisabled) {
		t.Errorf("expected ErrRawShellDisabled, got %v", err)
	}

	// 2. EnableRawShell = true, but AllowAdminActions = false
	cfg2 := &config.Config{
		EnableRawShell:    true,
		AllowAdminActions: false,
	}
	client2 := NewClient(cfg2, runner, nil, nil, nil)

	// Read-only arbitrary permitted
	_, err = client2.Execute(context.Background(), target, "fs.meta.cat /dir", true)
	if err != nil {
		t.Errorf("read-only command failed: %v", err)
	}

	// Mutating arbitrary rejected with ErrAdminActionsDisabled
	_, err = client2.Execute(context.Background(), target, "volume.balance -force", true)
	if !errors.Is(err, ErrAdminActionsDisabled) {
		t.Errorf("expected ErrAdminActionsDisabled, got %v", err)
	}

	// 3. EnableRawShell = true AND AllowAdminActions = true
	cfg3 := &config.Config{
		EnableRawShell:    true,
		AllowAdminActions: true,
	}
	client3 := NewClient(cfg3, runner, nil, nil, nil)
	_, err = client3.Execute(context.Background(), target, "volume.balance -force", true)
	if err != nil {
		t.Errorf("mutating command should succeed with admin allowed: %v", err)
	}
}

func TestArgumentSanitization(t *testing.T) {
	cfg := &config.Config{AllowAdminActions: true}
	client := NewClient(cfg, &MockCmdRunner{}, nil, nil, nil)
	target := testTarget()

	// 1. Valid arguments pass
	validArgs := []string{"my-bucket", "col_123", "data/dir/file.txt", "10.0.0.1"}
	for _, arg := range validArgs {
		if err := SanitizeArg(arg); err != nil {
			t.Errorf("valid arg %q rejected: %v", arg, err)
		}
	}

	// 2. Injection attempts fail
	injections := []string{
		"test; rm -rf /",
		"bucket\nlock\n",
		"foo$(whoami)",
		"a | b",
		"cmd`id`",
		"test&echo",
		"name with spaces",
		"arg'quote",
		"arg\"double",
	}

	for _, inj := range injections {
		if err := SanitizeArg(inj); err == nil {
			t.Errorf("injection %q should have been rejected", inj)
		}
	}

	// 3. Bucket name sanitization (no slashes)
	if err := SanitizeBucketName("path/with/slash"); err == nil {
		t.Errorf("bucket name with slash should be rejected")
	}
	if err := SanitizeBucketName("valid-bucket-name.1"); err != nil {
		t.Errorf("valid bucket name rejected: %v", err)
	}

	// 4. Client call with invalid argument returns ErrInvalidArg
	_, err := client.Vacuum(context.Background(), target, "col;drop")
	if !errors.Is(err, ErrInvalidArg) {
		t.Errorf("expected ErrInvalidArg, got %v", err)
	}
}

func TestInBandErrorDetection(t *testing.T) {
	// Test exit code 0 with in-band error messages
	errCases := []string{
		"Error: collection test not found",
		"failed to acquire master lock: timed out",
		"rpc error: code = Unavailable desc = connection refused",
		"cannot find volume 42",
		"unknown command foo",
		"panic: runtime error",
	}

	for _, stdout := range errCases {
		err := DetectInBandError(stdout, "", 0)
		if !errors.Is(err, ErrCommandExecutionFailed) {
			t.Errorf("expected ErrCommandExecutionFailed for %q, got %v", stdout, err)
		}
	}

	// Non-zero exit code
	err := DetectInBandError("", "fatal crash", 1)
	if !errors.Is(err, ErrCommandExecutionFailed) {
		t.Errorf("expected ErrCommandExecutionFailed on non-zero exit code, got %v", err)
	}

	// False positive filtering
	cleanCases := []string{
		"compacted 1 volumes, 0 errors, 1024 bytes reclaimed",
		"volume balance completed with no error",
		"all 5 volumes balanced (0 error)",
	}

	for _, clean := range cleanCases {
		if err := DetectInBandError(clean, "", 0); err != nil {
			t.Errorf("unexpected error for %q: %v", clean, err)
		}
	}
}

func TestParseVolumeList(t *testing.T) {
	fixture := `
Topology volumeSizeLimit:30000 MB
  DC: dc1
    Rack: rack1
      DataNode: 10.244.1.20:8080 (Volumes: 2/100, Free: 98)
        volume id: 1 size: 100000 replica placement: 000 collection: "benchmark" version: 3 file count: 120 delete count: 20 deleted byte count: 25000 read only: false
        volume id: 2 size: 50000 replica placement: 001 collection: "" version: 2 file count: 50 delete count: 0 deleted byte count: 0 read only: true
  DC: dc2
    Rack: rack2
      DataNode: 10.244.2.20:8080 (Volumes: 1/100, Free: 99)
        volume id: 3 size: 200000 replica placement: 010 collection: "photos" version: 3 file count: 300 delete count: 50 deleted byte count: 50000 read only: false
`

	topo, err := ParseVolumeList(fixture)
	if err != nil {
		t.Fatalf("ParseVolumeList failed: %v", err)
	}

	if topo.VolumeSizeLimitMB != 30000 {
		t.Errorf("expected limit 30000, got %d", topo.VolumeSizeLimitMB)
	}
	if len(topo.DataCenters) != 2 {
		t.Fatalf("expected 2 DCs, got %d", len(topo.DataCenters))
	}
	if topo.TotalVolumes != 3 {
		t.Errorf("expected 3 total volumes, got %d", topo.TotalVolumes)
	}
	if topo.ActiveVolumes != 2 {
		t.Errorf("expected 2 active volumes, got %d", topo.ActiveVolumes)
	}
	if topo.FreeVolumes != 197 {
		t.Errorf("expected 197 free volumes, got %d", topo.FreeVolumes)
	}

	// Verify volume 1
	v1 := topo.DataCenters[0].Racks[0].DataNodes[0].Volumes[0]
	if v1.ID != 1 || v1.Size != 100000 || v1.Collection != "benchmark" {
		t.Errorf("v1 mismatch: %+v", v1)
	}
	expectedRatio := 25000.0 / 100000.0
	if v1.GarbageRatio != expectedRatio {
		t.Errorf("v1 garbage ratio expected %f, got %f", expectedRatio, v1.GarbageRatio)
	}

	// Verify volume 2 (read-only)
	v2 := topo.DataCenters[0].Racks[0].DataNodes[0].Volumes[1]
	if !v2.ReadOnly {
		t.Errorf("expected v2 to be read only")
	}
}

func TestParseS3BucketList(t *testing.T) {
	fixture := `
> s3.bucket.list
Name       Size
bucket-alpha 1048576
bucket-beta  2097152
bucket-gamma 0
exit
`
	names, buckets, err := ParseS3BucketList(fixture)
	if err != nil {
		t.Fatalf("ParseS3BucketList failed: %v", err)
	}
	if len(names) != 3 {
		t.Fatalf("expected 3 buckets, got %d", len(names))
	}
	if names[0] != "bucket-alpha" || names[1] != "bucket-beta" || names[2] != "bucket-gamma" {
		t.Errorf("names mismatch: %v", names)
	}
	if buckets[0].Size != 1048576 {
		t.Errorf("bucket 0 size mismatch: %d", buckets[0].Size)
	}
}

func TestParseS3Configure(t *testing.T) {
	fixture := `
weed shell s3.configure
{
  "identities": [
    {
      "name": "admin-user",
      "credentials": [
        {
          "accessKey": "AKIA123",
          "secretKey": "SECRET456"
        }
      ],
      "actions": ["Read", "Write", "Admin"]
    }
  ]
}
`
	users, config, err := ParseS3Configure(fixture)
	if err != nil {
		t.Fatalf("ParseS3Configure failed: %v", err)
	}
	if len(users) != 1 {
		t.Fatalf("expected 1 user, got %d", len(users))
	}
	if users[0].Name != "admin-user" {
		t.Errorf("user name mismatch: %s", users[0].Name)
	}
	if len(users[0].Credentials) != 1 || users[0].Credentials[0].AccessKey != "AKIA123" {
		t.Errorf("credentials mismatch: %+v", users[0].Credentials)
	}
	if len(config.Identities) != 1 {
		t.Errorf("identities mismatch: %+v", config.Identities)
	}
}

func TestParseMetaCat(t *testing.T) {
	fixture := `
{
  "FullPath": "/buckets/b1/file.txt",
  "Mtime": "2026-10-17T12:00:00Z",
  "Mode": 2147484141,
  "chunks": [
    {
      "fileId": "3,0123456789",
      "size": 1024,
      "modifiedTsNs": 1729180000000000000,
      "eTag": "abc"
    }
  ]
}
`
	meta, err := ParseMetaCat(fixture)
	if err != nil {
		t.Fatalf("ParseMetaCat failed: %v", err)
	}
	if len(meta.Chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(meta.Chunks))
	}
	if meta.Chunks[0].FileID != "3,0123456789" {
		t.Errorf("chunk fileId mismatch: %s", meta.Chunks[0].FileID)
	}
}

func TestClusterActionMutex(t *testing.T) {
	mutex := NewClusterActionMutex()

	// Single cluster lock
	if !mutex.TryLock("cluster-1") {
		t.Fatalf("initial TryLock failed")
	}
	if mutex.TryLock("cluster-1") {
		t.Fatalf("second TryLock on same cluster should fail")
	}

	// Different cluster lock should succeed concurrently
	if !mutex.TryLock("cluster-2") {
		t.Fatalf("TryLock on cluster-2 should succeed")
	}

	mutex.Unlock("cluster-1")
	// Now cluster-1 can be locked again
	if !mutex.TryLock("cluster-1") {
		t.Fatalf("TryLock after unlock failed")
	}
	mutex.Unlock("cluster-1")
	mutex.Unlock("cluster-2")

	// High concurrency race test
	var wg sync.WaitGroup
	acquired := 0
	var mu sync.Mutex

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if mutex.TryLock("contended-cluster") {
				mu.Lock()
				acquired++
				mu.Unlock()
				time.Sleep(10 * time.Millisecond)
				mutex.Unlock("contended-cluster")
			}
		}()
	}
	wg.Wait()

	if acquired == 0 {
		t.Errorf("expected at least 1 lock acquisition")
	}
}

func TestConfirmationManager(t *testing.T) {
	mgr := NewConfirmationManager(nil)

	token, challenge, err := mgr.GenerateToken("s3.bucket.delete", "my-data", "cluster-prod")
	if err != nil {
		t.Fatalf("GenerateToken failed: %v", err)
	}
	if !strings.HasPrefix(token, "ctk_") {
		t.Errorf("expected ctk_ prefix, got %s", token)
	}
	if challenge != "my-data" {
		t.Errorf("expected challenge 'my-data', got %s", challenge)
	}

	// 1. Successful verification & single-use consumption
	err = mgr.ValidateAndConsumeToken(token, "s3.bucket.delete", "my-data", "cluster-prod", "my-data")
	if err != nil {
		t.Fatalf("ValidateAndConsumeToken failed: %v", err)
	}

	// 2. Replay of consumed token must fail
	errReplay := mgr.ValidateAndConsumeToken(token, "s3.bucket.delete", "my-data", "cluster-prod", "my-data")
	if !errors.Is(errReplay, ErrTokenAlreadyUsed) {
		t.Errorf("expected ErrTokenAlreadyUsed on replay, got %v", errReplay)
	}

	// 3. Challenge mismatch
	token2, _, _ := mgr.GenerateToken("s3.bucket.delete", "my-data", "cluster-prod")
	errMismatch := mgr.ValidateAndConsumeToken(token2, "s3.bucket.delete", "my-data", "cluster-prod", "wrong-data")
	if !errors.Is(errMismatch, ErrChallengeMismatch) {
		t.Errorf("expected ErrChallengeMismatch, got %v", errMismatch)
	}

	// 4. Invalid signature
	tamperedToken := token2 + "bad"
	errTampered := mgr.ValidateAndConsumeToken(tamperedToken, "s3.bucket.delete", "my-data", "cluster-prod", "my-data")
	if !errors.Is(errTampered, ErrTokenInvalid) {
		t.Errorf("expected ErrTokenInvalid for tampered token, got %v", errTampered)
	}
}

func TestAuditLogger(t *testing.T) {
	buf := new(bytes.Buffer)
	logger := NewAuditLogger(buf)

	record := AuditRecord{
		ClusterID:  "c1",
		Action:     "weed shell",
		Command:    "volume.balance",
		Tier:       Tier2Rebalance,
		IsMutating: true,
		DurationMs: 120,
		Status:     "SUCCESS",
		ExitCode:   0,
	}

	logger.Log(record)

	output := buf.String()
	if !strings.Contains(output, `"clusterId":"c1"`) || !strings.Contains(output, `"command":"volume.balance"`) {
		t.Errorf("audit log output mismatch: %s", output)
	}
}

func TestPKIAndEnvironmentWiring(t *testing.T) {
	runner := &MockCmdRunner{}
	runner.Response.Stdout = "ok"

	cfg := &config.Config{
		AllowAdminActions: true,
		WeedBinaryPath:    "/usr/bin/weed",
	}
	client := NewClient(cfg, runner, nil, nil, nil)

	target := testTarget()
	target.TLS = config.TLSConfig{
		Enabled:  true,
		CAFile:   "/etc/tls/ca.crt",
		CertFile: "/etc/tls/client.crt",
		KeyFile:  "/etc/tls/client.key",
	}

	_, err := client.Balance(context.Background(), target)
	if err != nil {
		t.Fatalf("Balance failed: %v", err)
	}

	if len(runner.Calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(runner.Calls))
	}

	foundHome := false
	for _, env := range runner.Calls[0].Env {
		if env == "HOME=/tmp/seaweedfs" {
			foundHome = true
			break
		}
	}
	if !foundHome {
		t.Errorf("expected HOME=/tmp/seaweedfs in environment")
	}
}

func TestSummaries(t *testing.T) {
	vacOut := "compacted 5 volumes, 10485760 bytes reclaimed"
	comp, recl := ParseVacuumSummary(vacOut)
	if comp != 5 || recl != 10485760 {
		t.Errorf("ParseVacuumSummary failed: %d, %d", comp, recl)
	}

	balOut := "moved 3 volumes to balance rack1"
	moved := ParseBalanceSummary(balOut)
	if moved != 3 {
		t.Errorf("ParseBalanceSummary failed: %d", moved)
	}

	fixOut := "fixed 2 volumes replication"
	fixed := ParseFixReplicationSummary(fixOut)
	if fixed != 2 {
		t.Errorf("ParseFixReplicationSummary failed: %d", fixed)
	}

	growOut := "created 4 volumes"
	grew, _ := ParseGrowSummary(growOut)
	if grew != 4 {
		t.Errorf("ParseGrowSummary failed: %d", grew)
	}
}

