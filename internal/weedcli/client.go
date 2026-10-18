package weedcli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"seaweedfs-ui/internal/cluster"
	"seaweedfs-ui/internal/config"
	"seaweedfs-ui/internal/pki"
)

// ShellResult represents the outcome of a weed shell execution.
type ShellResult struct {
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	ExitCode   int    `json:"exitCode"`
	DurationMs int64  `json:"durationMs"`
}

// Client is the primary interface for authoritative weed shell CLI operations.
type Client interface {
	Vacuum(ctx context.Context, target cluster.ClusterTarget, collection string) (*ShellResult, error)
	Balance(ctx context.Context, target cluster.ClusterTarget) (*ShellResult, error)
	FixReplication(ctx context.Context, target cluster.ClusterTarget) (*ShellResult, error)
	Grow(ctx context.Context, target cluster.ClusterTarget, count int, collection, replication, dataCenter string) (*ShellResult, error)

	ListBuckets(ctx context.Context, target cluster.ClusterTarget) ([]string, error)
	CreateBucket(ctx context.Context, target cluster.ClusterTarget, name string) (*ShellResult, error)
	DeleteBucket(ctx context.Context, target cluster.ClusterTarget, name string) (*ShellResult, error)
	ListUsers(ctx context.Context, target cluster.ClusterTarget) ([]S3User, error)

	MetaCat(ctx context.Context, target cluster.ClusterTarget, path string) (string, error)

	Execute(ctx context.Context, target cluster.ClusterTarget, command string, isRaw bool) (*ShellResult, error)

	VolumeList(ctx context.Context, target cluster.ClusterTarget, collection string) (*VolumeTopology, error)
}

// CmdRunner abstracts OS subprocess execution to allow deterministic mock testing.
type CmdRunner interface {
	Run(ctx context.Context, binaryPath string, env []string, stdin io.Reader, args ...string) (stdout string, stderr string, exitCode int, err error)
}

// DefaultCmdRunner runs real processes using os/exec.
type DefaultCmdRunner struct{}

func (r *DefaultCmdRunner) Run(ctx context.Context, binaryPath string, env []string, stdin io.Reader, args ...string) (string, string, int, error) {
	cmd := exec.CommandContext(ctx, binaryPath, args...)
	cmd.Env = env
	cmd.Stdin = stdin

	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	err := cmd.Run()
	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}
	return stdoutBuf.String(), stderrBuf.String(), exitCode, err
}

// DefaultClient implements Client with guardrails, stdin framing, and audit logging.
type DefaultClient struct {
	cfg         *config.Config
	runner      CmdRunner
	actionMutex *ClusterActionMutex
	confirmMgr  *ConfirmationManager
	auditLogger *AuditLogger
}

// NewClient constructs a new DefaultClient.
func NewClient(cfg *config.Config, runner CmdRunner, actionMutex *ClusterActionMutex, confirmMgr *ConfirmationManager, auditLogger *AuditLogger) *DefaultClient {
	if cfg == nil {
		cfg = &config.Config{
			WeedBinaryPath: "/usr/local/bin/weed",
			QueryTimeout:   15 * time.Second,
			ActionTimeout:  120 * time.Second,
		}
	}
	if runner == nil {
		runner = &DefaultCmdRunner{}
	}
	if actionMutex == nil {
		actionMutex = NewClusterActionMutex()
	}
	if confirmMgr == nil {
		confirmMgr = NewConfirmationManager(nil)
	}
	if auditLogger == nil {
		auditLogger = NewAuditLogger(os.Stdout)
	}

	return &DefaultClient{
		cfg:         cfg,
		runner:      runner,
		actionMutex: actionMutex,
		confirmMgr:  confirmMgr,
		auditLogger: auditLogger,
	}
}

// ConfirmationManager returns the configured ConfirmationManager.
func (c *DefaultClient) ConfirmationManager() *ConfirmationManager {
	return c.confirmMgr
}

// ActionMutex returns the configured ClusterActionMutex.
func (c *DefaultClient) ActionMutex() *ClusterActionMutex {
	return c.actionMutex
}

// AuditLogger returns the configured AuditLogger.
func (c *DefaultClient) AuditLogger() *AuditLogger {
	return c.auditLogger
}

// executeCommands runs one or more shell commands inside a single weed shell session.
func (c *DefaultClient) executeCommands(ctx context.Context, target cluster.ClusterTarget, commands []string, requiresLock bool, isMutating bool, tier int) (*ShellResult, error) {
	start := time.Now()
	cmdStr := strings.Join(commands, " ; ")

	// 1. Validate master endpoints
	if len(target.Masters) == 0 {
		err := fmt.Errorf("cluster %q has no master endpoints configured", target.ID)
		c.logAudit(target.ID, cmdStr, tier, isMutating, 0, "FAILED", -1, err.Error(), 0)
		return nil, err
	}

	// 2. Enforce AllowAdminActions guardrail
	if isMutating && !c.cfg.AllowAdminActions {
		c.logAudit(target.ID, cmdStr, tier, isMutating, 0, "REJECTED_FORBIDDEN", 0, ErrAdminActionsDisabled.Error(), 0)
		return nil, ErrAdminActionsDisabled
	}

	// 3. Enforce concurrency mutex for locking maintenance actions
	if requiresLock {
		if !c.actionMutex.TryLock(target.ID) {
			c.logAudit(target.ID, cmdStr, tier, isMutating, 0, "CONFLICT", 0, ErrClusterBusy.Error(), 0)
			return nil, ErrClusterBusy
		}
		defer c.actionMutex.Unlock(target.ID)
	}

	// 4. Configure context timeout
	timeout := c.cfg.QueryTimeout
	if requiresLock || isMutating {
		timeout = c.cfg.ActionTimeout
	}
	if timeout <= 0 {
		if requiresLock || isMutating {
			timeout = 120 * time.Second
		} else {
			timeout = 15 * time.Second
		}
	}
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// 5. Build weed shell command arguments
	binaryPath := c.cfg.WeedBinaryPath
	if binaryPath == "" {
		binaryPath = "/usr/local/bin/weed"
	}

	masters := strings.Join(target.Masters, ",")
	args := []string{"shell", "-master=" + masters}
	if target.Filer != "" {
		args = append(args, "-filer="+target.Filer)
	}

	// 6. Setup subprocess environment and mTLS configuration
	env := os.Environ()
	env = append(env, "HOME=/tmp/seaweedfs")

	if target.TLS.Enabled {
		tlsCfg := target.TLS
		if tlsCfg.SecurityTomlPath == "" {
			tlsCfg.SecurityTomlPath = "/tmp/seaweedfs/.seaweedfs/security.toml"
		}
		if _, err := pki.EnsureSecurityToml(tlsCfg); err != nil {
			err = fmt.Errorf("ensure security.toml for cluster %q: %w", target.ID, err)
			c.logAudit(target.ID, cmdStr, tier, isMutating, 0, "FAILED", -1, err.Error(), 0)
			return nil, err
		}
	}

	// 7. Format batch stdin payload
	var stdinBuf bytes.Buffer
	if requiresLock {
		stdinBuf.WriteString("lock\n")
	}
	for _, cmd := range commands {
		stdinBuf.WriteString(strings.TrimSpace(cmd))
		stdinBuf.WriteString("\n")
	}
	if requiresLock {
		stdinBuf.WriteString("unlock\n")
	}
	stdinBuf.WriteString("exit\n")

	// 8. Execute via runner
	stdout, stderr, exitCode, err := c.runner.Run(execCtx, binaryPath, env, &stdinBuf, args...)
	durationMs := time.Since(start).Milliseconds()

	// Check context timeout
	if execCtx.Err() == context.DeadlineExceeded {
		c.logAudit(target.ID, cmdStr, tier, isMutating, durationMs, "FAILED", exitCode, ErrActionTimeout.Error(), 0)
		return nil, ErrActionTimeout
	}

	// 9. Detect in-band errors
	if scanErr := DetectInBandError(stdout, stderr, exitCode); scanErr != nil {
		c.logAudit(target.ID, cmdStr, tier, isMutating, durationMs, "FAILED", exitCode, scanErr.Error(), 0)
		return &ShellResult{
			Stdout:     stdout,
			Stderr:     stderr,
			ExitCode:   exitCode,
			DurationMs: durationMs,
		}, scanErr
	}

	if err != nil && exitCode == 0 {
		c.logAudit(target.ID, cmdStr, tier, isMutating, durationMs, "FAILED", -1, err.Error(), 0)
		return nil, err
	}

	c.logAudit(target.ID, cmdStr, tier, isMutating, durationMs, "SUCCESS", exitCode, "", 0)
	return &ShellResult{
		Stdout:     stdout,
		Stderr:     stderr,
		ExitCode:   exitCode,
		DurationMs: durationMs,
	}, nil
}

func (c *DefaultClient) logAudit(clusterID, cmd string, tier int, isMutating bool, durationMs int64, status string, exitCode int, errMsg string, _ int) {
	if c.auditLogger == nil {
		return
	}
	c.auditLogger.Log(AuditRecord{
		Timestamp:  time.Now().UTC(),
		ClusterID:  clusterID,
		Action:     "weed shell",
		Command:    cmd,
		Tier:       tier,
		IsMutating: isMutating,
		DurationMs: durationMs,
		Status:     status,
		ExitCode:   exitCode,
		Error:      errMsg,
	})
}

// Vacuum executes volume.vacuum wrapped in a lock block.
func (c *DefaultClient) Vacuum(ctx context.Context, target cluster.ClusterTarget, collection string) (*ShellResult, error) {
	if err := SanitizeArg(collection); err != nil {
		return nil, err
	}
	cmd := "volume.vacuum"
	if collection != "" {
		cmd += fmt.Sprintf(" -collection=%s", collection)
	}
	return c.executeCommands(ctx, target, []string{cmd}, true, true, Tier1LowImpact)
}

// Balance executes volume.balance wrapped in a lock block.
func (c *DefaultClient) Balance(ctx context.Context, target cluster.ClusterTarget) (*ShellResult, error) {
	cmd := "volume.balance -force"
	return c.executeCommands(ctx, target, []string{cmd}, true, true, Tier2Rebalance)
}

// FixReplication executes volume.fix.replication wrapped in a lock block.
func (c *DefaultClient) FixReplication(ctx context.Context, target cluster.ClusterTarget) (*ShellResult, error) {
	cmd := "volume.fix.replication"
	return c.executeCommands(ctx, target, []string{cmd}, true, true, Tier2Rebalance)
}

// Grow executes volume.grow wrapped in a lock block.
func (c *DefaultClient) Grow(ctx context.Context, target cluster.ClusterTarget, count int, collection, replication, dataCenter string) (*ShellResult, error) {
	if count <= 0 {
		count = 1
	}
	if err := SanitizeArg(collection); err != nil {
		return nil, err
	}
	if err := SanitizeReplication(replication); err != nil {
		return nil, err
	}
	if err := SanitizeArg(dataCenter); err != nil {
		return nil, err
	}

	cmd := fmt.Sprintf("volume.grow -count=%d", count)
	if collection != "" {
		cmd += fmt.Sprintf(" -collection=%s", collection)
	}
	if replication != "" {
		cmd += fmt.Sprintf(" -replication=%s", replication)
	}
	if dataCenter != "" {
		cmd += fmt.Sprintf(" -dataCenter=%s", dataCenter)
	}

	return c.executeCommands(ctx, target, []string{cmd}, true, true, Tier2Rebalance)
}

// ListBuckets queries weed shell s3.bucket.list and returns parsed bucket names.
func (c *DefaultClient) ListBuckets(ctx context.Context, target cluster.ClusterTarget) ([]string, error) {
	cmd := "s3.bucket.list"
	res, err := c.executeCommands(ctx, target, []string{cmd}, false, false, Tier0ReadOnly)
	if err != nil {
		return nil, err
	}
	names, _, err := ParseS3BucketList(res.Stdout)
	return names, err
}

// CreateBucket creates an S3 bucket via s3.bucket.create.
func (c *DefaultClient) CreateBucket(ctx context.Context, target cluster.ClusterTarget, name string) (*ShellResult, error) {
	if err := SanitizeBucketName(name); err != nil {
		return nil, err
	}
	cmd := fmt.Sprintf("s3.bucket.create -name %s", name)
	return c.executeCommands(ctx, target, []string{cmd}, true, true, Tier2Rebalance)
}

// DeleteBucket deletes an S3 bucket via s3.bucket.delete (Tier 3 Destructive).
func (c *DefaultClient) DeleteBucket(ctx context.Context, target cluster.ClusterTarget, name string) (*ShellResult, error) {
	if err := SanitizeBucketName(name); err != nil {
		return nil, err
	}
	cmd := fmt.Sprintf("s3.bucket.delete -name %s", name)
	return c.executeCommands(ctx, target, []string{cmd}, true, true, Tier3Destructive)
}

// ListUsers queries weed shell s3.configure and parses IAM users.
func (c *DefaultClient) ListUsers(ctx context.Context, target cluster.ClusterTarget) ([]S3User, error) {
	cmd := "s3.configure"
	res, err := c.executeCommands(ctx, target, []string{cmd}, false, false, Tier0ReadOnly)
	if err != nil {
		return nil, err
	}
	users, _, err := ParseS3Configure(res.Stdout)
	return users, err
}

// MetaCat queries metadata for a filer path via fs.meta.cat.
func (c *DefaultClient) MetaCat(ctx context.Context, target cluster.ClusterTarget, path string) (string, error) {
	if err := SanitizeArg(path); err != nil {
		return "", err
	}
	cmd := fmt.Sprintf("fs.meta.cat %s", path)
	res, err := c.executeCommands(ctx, target, []string{cmd}, false, false, Tier0ReadOnly)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(res.Stdout), nil
}

// VolumeList queries weed shell volume.list and parses the topology tree.
func (c *DefaultClient) VolumeList(ctx context.Context, target cluster.ClusterTarget, collection string) (*VolumeTopology, error) {
	if err := SanitizeArg(collection); err != nil {
		return nil, err
	}
	cmd := "volume.list"
	if collection != "" {
		cmd += fmt.Sprintf(" -collection=%s", collection)
	}
	res, err := c.executeCommands(ctx, target, []string{cmd}, false, false, Tier0ReadOnly)
	if err != nil {
		return nil, err
	}
	return ParseVolumeList(res.Stdout)
}

// Execute handles direct shell command execution, checking raw shell permissions and argument safety.
func (c *DefaultClient) Execute(ctx context.Context, target cluster.ClusterTarget, command string, isRaw bool) (*ShellResult, error) {
	trimmed := strings.TrimSpace(command)
	if trimmed == "" {
		return nil, fmt.Errorf("%w: empty command", ErrInvalidCommand)
	}

	// Prohibit newlines to prevent multi-command injection into stdin
	if strings.Contains(trimmed, "\n") || strings.Contains(trimmed, "\r") {
		return nil, fmt.Errorf("%w: command contains newline characters", ErrInvalidArg)
	}

	isReadOnly := IsReadOnlyCommand(trimmed)

	if isRaw {
		if !c.cfg.EnableRawShell {
			if !IsPresetCommand(trimmed) {
				return nil, ErrRawShellDisabled
			}
		} else {
			if !c.cfg.AllowAdminActions && !isReadOnly {
				return nil, ErrAdminActionsDisabled
			}
		}
	} else {
		if !IsPresetCommand(trimmed) {
			return nil, ErrInvalidCommand
		}
	}

	requiresLock := !isReadOnly
	isMutating := !isReadOnly
	tier := Tier0ReadOnly
	if isMutating {
		tier = Tier4RawShell
	}

	return c.executeCommands(ctx, target, []string{trimmed}, requiresLock, isMutating, tier)
}
