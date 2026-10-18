package weedcli

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"seaweedfs-ui/internal/cluster"
	"seaweedfs-ui/internal/config"
)

// ConcurrentMockRunner implements CmdRunner without an artificial mutex,
// allowing genuine parallel subprocess testing.
type ConcurrentMockRunner struct {
	Handler func(ctx context.Context, binaryPath string, env []string, stdin io.Reader, args ...string) (string, string, int, error)
}

func (r *ConcurrentMockRunner) Run(ctx context.Context, binaryPath string, env []string, stdin io.Reader, args ...string) (string, string, int, error) {
	if r.Handler != nil {
		return r.Handler(ctx, binaryPath, env, stdin, args...)
	}
	return "ok", "", 0, nil
}

// -----------------------------------------------------------------------------
// 1. Concurrency Stress Testing
// -----------------------------------------------------------------------------

func TestAdversarial_Concurrency_SameClusterSerialization(t *testing.T) {
	var activeWorkers atomic.Int32
	var maxActiveWorkers atomic.Int32

	runner := &ConcurrentMockRunner{
		Handler: func(ctx context.Context, binaryPath string, env []string, stdin io.Reader, args ...string) (string, string, int, error) {
			curr := activeWorkers.Add(1)
			defer activeWorkers.Add(-1)

			for {
				oldMax := maxActiveWorkers.Load()
				if curr <= oldMax || maxActiveWorkers.CompareAndSwap(oldMax, curr) {
					break
				}
			}

			// Simulate maintenance work
			time.Sleep(20 * time.Millisecond)
			return "compacted 1 volumes, 0 errors, 1024 bytes reclaimed", "", 0, nil
		},
	}

	cfg := &config.Config{
		AllowAdminActions: true,
		ActionTimeout:     5 * time.Second,
	}
	client := NewClient(cfg, runner, nil, nil, nil)
	target := testTarget()

	concurrency := 50
	var wg sync.WaitGroup
	var successCount atomic.Int32
	var busyCount atomic.Int32
	var errorCount atomic.Int32

	startCh := make(chan struct{})

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-startCh // Align all goroutines for simultaneous burst

			var err error
			switch idx % 4 {
			case 0:
				_, err = client.Vacuum(context.Background(), target, "")
			case 1:
				_, err = client.Balance(context.Background(), target)
			case 2:
				_, err = client.FixReplication(context.Background(), target)
			case 3:
				_, err = client.Grow(context.Background(), target, 1, "", "", "")
			}

			if err == nil {
				successCount.Add(1)
			} else if errors.Is(err, ErrClusterBusy) {
				busyCount.Add(1)
			} else {
				errorCount.Add(1)
				t.Errorf("goroutine %d unexpected error: %v", idx, err)
			}
		}(i)
	}

	close(startCh)
	wg.Wait()

	if maxActiveWorkers.Load() > 1 {
		t.Fatalf("VIOLATION: ClusterActionMutex allowed %d concurrent workers on same cluster, expected at most 1", maxActiveWorkers.Load())
	}

	if successCount.Load() == 0 {
		t.Fatalf("expected at least 1 successful maintenance action, got 0")
	}

	if busyCount.Load() == 0 {
		t.Fatalf("expected rapid concurrent actions to trigger ErrClusterBusy, got 0 busy conflicts")
	}

	total := successCount.Load() + busyCount.Load() + errorCount.Load()
	if total != int32(concurrency) {
		t.Fatalf("total outcomes (%d) != concurrency (%d)", total, concurrency)
	}

	// Verify mutex was cleanly released after all concurrent attempts finished
	res, err := client.Balance(context.Background(), target)
	if err != nil {
		t.Fatalf("subsequent action failed after concurrency storm: %v", err)
	}
	if res == nil {
		t.Fatalf("expected non-nil result from subsequent action")
	}
}

func TestAdversarial_Concurrency_DifferentClustersIndependent(t *testing.T) {
	var activeTotal atomic.Int32
	var maxActiveTotal atomic.Int32

	alphaEntered := make(chan struct{})
	releaseAlpha := make(chan struct{})

	betaEntered := make(chan struct{})
	releaseBeta := make(chan struct{})

	runner := &ConcurrentMockRunner{
		Handler: func(ctx context.Context, binaryPath string, env []string, stdin io.Reader, args ...string) (string, string, int, error) {
			curr := activeTotal.Add(1)
			defer activeTotal.Add(-1)

			for {
				old := maxActiveTotal.Load()
				if curr <= old || maxActiveTotal.CompareAndSwap(old, curr) {
					break
				}
			}

			isAlpha := false
			for _, a := range args {
				if strings.Contains(a, "10.0.1.1") {
					isAlpha = true
					break
				}
			}

			if isAlpha {
				close(alphaEntered)
				<-releaseAlpha
			} else {
				close(betaEntered)
				<-releaseBeta
			}

			return "ok", "", 0, nil
		},
	}

	cfg := &config.Config{
		AllowAdminActions: true,
		ActionTimeout:     5 * time.Second,
	}
	client := NewClient(cfg, runner, nil, nil, nil)

	targetAlpha := cluster.ClusterTarget{
		ID:      "cluster-alpha",
		Masters: []string{"10.0.1.1:9333"},
	}
	targetBeta := cluster.ClusterTarget{
		ID:      "cluster-beta",
		Masters: []string{"10.0.2.1:9333"},
	}

	var wg sync.WaitGroup
	var errAlpha, errBeta error

	wg.Add(2)
	go func() {
		defer wg.Done()
		_, errAlpha = client.Balance(context.Background(), targetAlpha)
	}()

	go func() {
		defer wg.Done()
		_, errBeta = client.Balance(context.Background(), targetBeta)
	}()

	// Wait until BOTH clusters have entered their execution handlers simultaneously
	<-alphaEntered
	<-betaEntered

	if maxActiveTotal.Load() != 2 {
		t.Fatalf("expected exactly 2 active operations concurrently across different clusters, got %d", maxActiveTotal.Load())
	}

	// Release both
	close(releaseAlpha)
	close(releaseBeta)
	wg.Wait()

	if errAlpha != nil {
		t.Errorf("cluster-alpha returned unexpected error: %v", errAlpha)
	}
	if errBeta != nil {
		t.Errorf("cluster-beta returned unexpected error: %v", errBeta)
	}
}

// -----------------------------------------------------------------------------
// 2. Guardrail Stress & Security: Injection & Sanitization
// -----------------------------------------------------------------------------

func TestAdversarial_CommandInjectionAttacks(t *testing.T) {
	cfg := &config.Config{
		AllowAdminActions: true,
		EnableRawShell:    true,
	}
	runner := &ConcurrentMockRunner{
		Handler: func(ctx context.Context, binaryPath string, env []string, stdin io.Reader, args ...string) (string, string, int, error) {
			t.Fatalf("CRITICAL SECURITY VULNERABILITY: runner invoked with unsanitized argument!")
			return "", "", 0, nil
		},
	}
	client := NewClient(cfg, runner, nil, nil, nil)
	target := testTarget()

	injectionVectors := []string{
		"volume.balance; rm -rf /",
		"col; rm -rf /",
		"test; cat /etc/passwd",
		"col\nrm -rf /",
		"col\rreboot",
		"col\r\nreboot",
		"\nlock\n",
		"`whoami`",
		"col`id`",
		"`rm -rf /`",
		"col$(id)",
		"col$(whoami)",
		"col${PATH}",
		"$USER",
		"col | sh",
		"col || echo pwned",
		"col|cat",
		"col & echo background",
		"col && cat /etc/shadow",
		"> /tmp/pwn",
		"col >> /tmp/pwn",
		"< /etc/passwd",
		"col' OR 1=1 --",
		"col\" OR \"1\"=\"1",
		"col with spaces",
		"col\twith\ttabs",
		"col\\backslash",
		"col\x00nullbyte",
		"col;drop",
		"col(paren)",
		"col{brace}",
	}

	for _, payload := range injectionVectors {
		t.Run("SanitizeArg_"+payload, func(t *testing.T) {
			err := SanitizeArg(payload)
			if err == nil {
				t.Errorf("SanitizeArg accepted malicious payload: %q", payload)
			}
			if !errors.Is(err, ErrInvalidArg) {
				t.Errorf("expected ErrInvalidArg, got %v", err)
			}
		})

		t.Run("Vacuum_"+payload, func(t *testing.T) {
			_, err := client.Vacuum(context.Background(), target, payload)
			if err == nil {
				t.Errorf("Vacuum accepted malicious payload: %q", payload)
			}
			if !errors.Is(err, ErrInvalidArg) {
				t.Errorf("expected ErrInvalidArg, got %v", err)
			}
		})

		t.Run("MetaCat_"+payload, func(t *testing.T) {
			_, err := client.MetaCat(context.Background(), target, payload)
			if err == nil {
				t.Errorf("MetaCat accepted malicious payload: %q", payload)
			}
			if !errors.Is(err, ErrInvalidArg) {
				t.Errorf("expected ErrInvalidArg, got %v", err)
			}
		})

		t.Run("VolumeList_"+payload, func(t *testing.T) {
			_, err := client.VolumeList(context.Background(), target, payload)
			if err == nil {
				t.Errorf("VolumeList accepted malicious payload: %q", payload)
			}
			if !errors.Is(err, ErrInvalidArg) {
				t.Errorf("expected ErrInvalidArg, got %v", err)
			}
		})
	}
}

func TestAdversarial_BucketNameSanitizationAttacks(t *testing.T) {
	cfg := &config.Config{AllowAdminActions: true}
	runner := &ConcurrentMockRunner{}
	client := NewClient(cfg, runner, nil, nil, nil)
	target := testTarget()

	maliciousBuckets := []string{
		"",
		"   ",
		"path/with/slash",
		"path\\with\\backslash",
		"../../etc/passwd",
		"/etc/passwd",
		"bucket; rm -rf /",
		"bucket\nsecond_cmd",
		"bucket\rsecond_cmd",
		"bucket`id`",
		"bucket$(id)",
		"bucket|cat",
		"bucket&echo",
		"bucket with spaces",
		"bucket\x00null",
		"../escape",
		"bucket'singlequote",
		"bucket\"doublequote",
		"bucket;drop table",
	}

	for _, b := range maliciousBuckets {
		t.Run("SanitizeBucketName_"+b, func(t *testing.T) {
			err := SanitizeBucketName(b)
			if err == nil {
				t.Errorf("SanitizeBucketName accepted malicious bucket: %q", b)
			}
			if !errors.Is(err, ErrInvalidArg) {
				t.Errorf("expected ErrInvalidArg, got %v", err)
			}
		})

		t.Run("CreateBucket_"+b, func(t *testing.T) {
			_, err := client.CreateBucket(context.Background(), target, b)
			if err == nil {
				t.Errorf("CreateBucket accepted malicious bucket name %q", b)
			}
			if !errors.Is(err, ErrInvalidArg) {
				t.Errorf("expected ErrInvalidArg, got %v", err)
			}
		})

		t.Run("DeleteBucket_"+b, func(t *testing.T) {
			_, err := client.DeleteBucket(context.Background(), target, b)
			if err == nil {
				t.Errorf("DeleteBucket accepted malicious bucket name %q", b)
			}
			if !errors.Is(err, ErrInvalidArg) {
				t.Errorf("expected ErrInvalidArg, got %v", err)
			}
		})
	}
}

func TestAdversarial_ReplicationSanitizationAttacks(t *testing.T) {
	cfg := &config.Config{AllowAdminActions: true}
	runner := &ConcurrentMockRunner{}
	client := NewClient(cfg, runner, nil, nil, nil)
	target := testTarget()

	maliciousReplications := []string{
		"00",
		"0",
		"0000",
		"abc",
		"001; rm",
		"001\n",
		"001`id`",
		"001$(id)",
		"-100",
		"1.5",
	}

	for _, rep := range maliciousReplications {
		t.Run("GrowReplication_"+rep, func(t *testing.T) {
			_, err := client.Grow(context.Background(), target, 1, "", rep, "")
			if err == nil {
				t.Errorf("Grow accepted invalid replication %q", rep)
			}
			if !errors.Is(err, ErrInvalidArg) {
				t.Errorf("expected ErrInvalidArg, got %v", rep)
			}
		})
	}
}

func TestAdversarial_NewlineInjectionInExecute(t *testing.T) {
	cfg := &config.Config{
		EnableRawShell:    true,
		AllowAdminActions: true,
	}
	runner := &ConcurrentMockRunner{}
	client := NewClient(cfg, runner, nil, nil, nil)
	target := testTarget()

	newlinePayloads := []string{
		"volume.list\nvolume.balance",
		"cluster.status\rvolume.balance",
		"s3.bucket.list\r\ns3.bucket.delete -name b1",
		"volume.list\nrm -rf /",
	}

	for _, cmd := range newlinePayloads {
		_, err := client.Execute(context.Background(), target, cmd, true)
		if err == nil {
			t.Errorf("Execute accepted command with embedded newline: %q", cmd)
		}
		if !errors.Is(err, ErrInvalidArg) {
			t.Errorf("expected ErrInvalidArg, got %v", err)
		}
	}
}

// -----------------------------------------------------------------------------
// 3. Double-Confirmation Token State Machine Stress Testing
// -----------------------------------------------------------------------------

func TestAdversarial_ConfirmationManager_ExpiredTokens(t *testing.T) {
	secretKey := []byte("secret-key-32-bytes-long-1234567")
	mgr := NewConfirmationManager(secretKey)

	// Forge an expired token (issued 100s ago, expired 40s ago)
	now := time.Now().Unix()
	claims := ConfirmationClaims{
		Action:    "s3.bucket.delete",
		Target:    "prod-data",
		ClusterID: "prod-cluster",
		IssuedAt:  now - 100,
		ExpiresAt: now - 40,
		Nonce:     "deadbeef0123456789abcdef",
	}

	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}

	payloadB64 := base64.RawURLEncoding.EncodeToString(claimsJSON)
	mac := hmac.New(sha256.New, secretKey)
	mac.Write([]byte(payloadB64))
	sigHex := hex.EncodeToString(mac.Sum(nil))

	expiredToken := fmt.Sprintf("ctk_%s.%s", payloadB64, sigHex)

	err = mgr.ValidateAndConsumeToken(expiredToken, "s3.bucket.delete", "prod-data", "prod-cluster", "prod-data")
	if !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("expected ErrTokenExpired for expired token, got %v", err)
	}
}

func TestAdversarial_ConfirmationManager_ReplayProtection(t *testing.T) {
	mgr := NewConfirmationManager(nil)

	token, challenge, err := mgr.GenerateToken("s3.bucket.delete", "critical-bucket", "c1")
	if err != nil {
		t.Fatalf("GenerateToken failed: %v", err)
	}

	// 1st consumption MUST succeed
	err = mgr.ValidateAndConsumeToken(token, "s3.bucket.delete", "critical-bucket", "c1", challenge)
	if err != nil {
		t.Fatalf("first ValidateAndConsumeToken failed: %v", err)
	}

	// 2nd consumption MUST fail with ErrTokenAlreadyUsed (single-use enforced)
	errReplay := mgr.ValidateAndConsumeToken(token, "s3.bucket.delete", "critical-bucket", "c1", challenge)
	if !errors.Is(errReplay, ErrTokenAlreadyUsed) {
		t.Fatalf("expected ErrTokenAlreadyUsed on replay, got %v", errReplay)
	}

	// 3rd consumption MUST also fail
	errReplay3 := mgr.ValidateAndConsumeToken(token, "s3.bucket.delete", "critical-bucket", "c1", challenge)
	if !errors.Is(errReplay3, ErrTokenAlreadyUsed) {
		t.Fatalf("expected ErrTokenAlreadyUsed on 3rd attempt, got %v", errReplay3)
	}
}

func TestAdversarial_ConfirmationManager_TamperedSignatures(t *testing.T) {
	mgr := NewConfirmationManager(nil)
	token, challenge, _ := mgr.GenerateToken("s3.bucket.delete", "data", "c1")

	// 1. Flip last character of signature
	tamperedSig := token[:len(token)-1] + "0"
	if token[len(token)-1] == '0' {
		tamperedSig = token[:len(token)-1] + "1"
	}

	err := mgr.ValidateAndConsumeToken(tamperedSig, "s3.bucket.delete", "data", "c1", challenge)
	if !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("expected ErrTokenInvalid for flipped signature, got %v", err)
	}

	// 2. Truncate token
	err = mgr.ValidateAndConsumeToken(token[:20], "s3.bucket.delete", "data", "c1", challenge)
	if !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("expected ErrTokenInvalid for truncated token, got %v", err)
	}

	// 3. Invalid prefix
	err = mgr.ValidateAndConsumeToken("bad_"+token[4:], "s3.bucket.delete", "data", "c1", challenge)
	if !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("expected ErrTokenInvalid for invalid prefix, got %v", err)
	}

	// 4. Malformed tokens
	malformed := []string{"", "ctk_", "ctk_part1", "ctk_p1.p2.p3", "ctk_notbase64!@#.sig"}
	for _, m := range malformed {
		err := mgr.ValidateAndConsumeToken(m, "s3.bucket.delete", "data", "c1", challenge)
		if !errors.Is(err, ErrTokenInvalid) {
			t.Errorf("expected ErrTokenInvalid for %q, got %v", m, err)
		}
	}
}

func TestAdversarial_ConfirmationManager_ParameterTampering(t *testing.T) {
	mgr := NewConfirmationManager(nil)
	token, challenge, _ := mgr.GenerateToken("s3.bucket.delete", "bucket-a", "cluster-1")

	// Action mismatch
	err := mgr.ValidateAndConsumeToken(token, "volume.vacuum", "bucket-a", "cluster-1", challenge)
	if !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("expected ErrTokenInvalid on action mismatch, got %v", err)
	}

	// Target mismatch
	err = mgr.ValidateAndConsumeToken(token, "s3.bucket.delete", "bucket-b", "cluster-1", challenge)
	if !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("expected ErrTokenInvalid on target mismatch, got %v", err)
	}

	// ClusterID mismatch
	err = mgr.ValidateAndConsumeToken(token, "s3.bucket.delete", "bucket-a", "cluster-2", challenge)
	if !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("expected ErrTokenInvalid on cluster mismatch, got %v", err)
	}

	// Challenge response mismatch
	err = mgr.ValidateAndConsumeToken(token, "s3.bucket.delete", "bucket-a", "cluster-1", "wrong-challenge")
	if !errors.Is(err, ErrChallengeMismatch) {
		t.Errorf("expected ErrChallengeMismatch, got %v", err)
	}
}

func TestAdversarial_ConfirmationManager_ConcurrentRace(t *testing.T) {
	mgr := NewConfirmationManager(nil)
	token, challenge, _ := mgr.GenerateToken("s3.bucket.delete", "race-bucket", "c1")

	concurrency := 60
	var wg sync.WaitGroup
	var successCount atomic.Int32
	var alreadyUsedCount atomic.Int32
	var errorCount atomic.Int32

	startCh := make(chan struct{})

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-startCh

			err := mgr.ValidateAndConsumeToken(token, "s3.bucket.delete", "race-bucket", "c1", challenge)
			if err == nil {
				successCount.Add(1)
			} else if errors.Is(err, ErrTokenAlreadyUsed) {
				alreadyUsedCount.Add(1)
			} else {
				errorCount.Add(1)
			}
		}()
	}

	close(startCh)
	wg.Wait()

	if successCount.Load() != 1 {
		t.Fatalf("VIOLATION: token consumed %d times, expected exactly 1", successCount.Load())
	}
	if alreadyUsedCount.Load() != int32(concurrency-1) {
		t.Fatalf("expected %d ErrTokenAlreadyUsed, got %d", concurrency-1, alreadyUsedCount.Load())
	}
	if errorCount.Load() != 0 {
		t.Fatalf("unexpected errors during concurrent token consumption: %d", errorCount.Load())
	}
}

// -----------------------------------------------------------------------------
// 4. AllowAdminActions=false Gating Stress Testing
// -----------------------------------------------------------------------------

func TestAdversarial_AllowAdminActionsFalse_AllMutatingActionsBlocked(t *testing.T) {
	runner := &ConcurrentMockRunner{
		Handler: func(ctx context.Context, binaryPath string, env []string, stdin io.Reader, args ...string) (string, string, int, error) {
			t.Fatalf("CRITICAL SECURITY VIOLATION: Subprocess executed when AllowAdminActions=false!")
			return "", "", 0, nil
		},
	}

	cfg := &config.Config{
		AllowAdminActions: false,
		EnableRawShell:    true, // Even with raw shell enabled, mutating actions must be blocked!
	}
	client := NewClient(cfg, runner, nil, nil, nil)
	target := testTarget()

	actions := []struct {
		name string
		call func() error
	}{
		{"Vacuum", func() error { _, err := client.Vacuum(context.Background(), target, ""); return err }},
		{"Balance", func() error { _, err := client.Balance(context.Background(), target); return err }},
		{"FixReplication", func() error { _, err := client.FixReplication(context.Background(), target); return err }},
		{"Grow", func() error { _, err := client.Grow(context.Background(), target, 1, "", "", ""); return err }},
		{"CreateBucket", func() error { _, err := client.CreateBucket(context.Background(), target, "b1"); return err }},
		{"DeleteBucket", func() error { _, err := client.DeleteBucket(context.Background(), target, "b1"); return err }},
		{"RawMutating_VolumeBalance", func() error {
			_, err := client.Execute(context.Background(), target, "volume.balance -force", true)
			return err
		}},
		{"RawMutating_VolumeVacuum", func() error {
			_, err := client.Execute(context.Background(), target, "volume.vacuum", true)
			return err
		}},
		{"RawMutating_BucketDelete", func() error {
			_, err := client.Execute(context.Background(), target, "s3.bucket.delete -name b1", true)
			return err
		}},
	}

	for _, tc := range actions {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatalf("action %s succeeded when AllowAdminActions=false!", tc.name)
			}
			if !errors.Is(err, ErrAdminActionsDisabled) {
				t.Fatalf("action %s: expected ErrAdminActionsDisabled, got %v", tc.name, err)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// 5. EnableRawShell=false Gating Stress Testing
// -----------------------------------------------------------------------------

func TestAdversarial_EnableRawShellFalse_PresetVsNonPreset(t *testing.T) {
	runner := &ConcurrentMockRunner{}

	cfg := &config.Config{
		EnableRawShell:    false, // Disabled!
		AllowAdminActions: false,
	}
	client := NewClient(cfg, runner, nil, nil, nil)
	target := testTarget()

	// 1. All allowlisted presets MUST succeed
	presets := []string{
		"cluster.status",
		"volume.list",
		"s3.bucket.list",
		"s3.configure",
		"fs.ls",
		"fs.ls /buckets",
	}

	for _, p := range presets {
		t.Run("Preset_"+p, func(t *testing.T) {
			_, err := client.Execute(context.Background(), target, p, true)
			if err != nil {
				t.Errorf("preset %q failed unexpectedly: %v", p, err)
			}
		})
	}

	// 2. Non-preset shell commands MUST be rejected with ErrRawShellDisabled
	nonPresets := []string{
		"volume.balance",
		"volume.vacuum",
		"volume.tier.upload",
		"volume.configure.save",
		"ec.encode",
		"ec.rebuild",
		"volume.delete",
		"s3.bucket.delete",
		"arbitrary.cmd",
		"cat /etc/passwd",
		"help",
		"whoami",
	}

	for _, np := range nonPresets {
		t.Run("NonPreset_"+np, func(t *testing.T) {
			_, err := client.Execute(context.Background(), target, np, true)
			if err == nil {
				t.Errorf("non-preset command %q succeeded when EnableRawShell=false!", np)
			}
			if !errors.Is(err, ErrRawShellDisabled) {
				t.Errorf("expected ErrRawShellDisabled for %q, got %v", np, err)
			}
		})
	}

	// 3. Non-raw mode (isRaw = false) with non-presets returns ErrInvalidCommand
	for _, np := range nonPresets {
		t.Run("NonRawInvalid_"+np, func(t *testing.T) {
			_, err := client.Execute(context.Background(), target, np, false)
			if err == nil {
				t.Errorf("non-raw command %q succeeded with non-preset!", np)
			}
			if !errors.Is(err, ErrInvalidCommand) {
				t.Errorf("expected ErrInvalidCommand for %q, got %v", np, err)
			}
		})
	}
}
