package weedcli

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Sentinel Errors for weedcli and safety guardrails
var (
	ErrAdminActionsDisabled   = errors.New("admin actions are disabled by configuration")
	ErrRawShellDisabled       = errors.New("raw shell execution is disabled by configuration")
	ErrInvalidCommand         = errors.New("command rejected by security allowlist")
	ErrInvalidArg             = errors.New("command argument contains invalid characters")
	ErrActionTimeout          = errors.New("maintenance action timed out")
	ErrLockFailed             = errors.New("failed to acquire master lock")
	ErrCommandExecutionFailed = errors.New("command execution failed")
	ErrClusterBusy            = errors.New("another maintenance action is currently executing on this cluster")
	ErrTokenExpired           = errors.New("confirmation token has expired")
	ErrTokenInvalid           = errors.New("confirmation token is invalid or signature mismatch")
	ErrTokenAlreadyUsed       = errors.New("confirmation token has already been consumed")
	ErrChallengeMismatch      = errors.New("confirmation challenge response mismatch")
)

// 5-Tier Operation Classification
const (
	Tier0ReadOnly    = 0 // Pure Read-Only (cluster.status, volume.list, etc.)
	Tier1LowImpact   = 1 // Low-Impact Maintenance (volume.vacuum)
	Tier2Rebalance   = 2 // Replication & Topology Rebalance (volume.balance, fix.replication, grow)
	Tier3Destructive = 3 // Destructive Operations (s3.bucket.delete, recursive delete)
	Tier4RawShell    = 4 // Raw Shell Execution
)

var (
	safeArgRegex         = regexp.MustCompile(`^[a-zA-Z0-9._/-]+$`)
	safeBucketRegex      = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)
	safeReplicationRegex = regexp.MustCompile(`^[0-9]{3}$`)
)

// SanitizeArg validates that an argument contains only safe characters.
// Prevents command injection, newline injection, and shell expansion.
func SanitizeArg(arg string) error {
	if arg == "" {
		return nil
	}
	if !safeArgRegex.MatchString(arg) {
		return fmt.Errorf("%w: argument %q contains invalid characters", ErrInvalidArg, arg)
	}
	return nil
}

// SanitizeBucketName validates bucket names strictly (no slashes permitted).
func SanitizeBucketName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("%w: bucket name cannot be empty", ErrInvalidArg)
	}
	if !safeBucketRegex.MatchString(name) {
		return fmt.Errorf("%w: bucket name %q contains invalid characters", ErrInvalidArg, name)
	}
	return nil
}

// SanitizeReplication validates 3-digit replication strings (e.g. 000, 001, 010).
func SanitizeReplication(rep string) error {
	if rep == "" {
		return nil
	}
	if !safeReplicationRegex.MatchString(rep) {
		return fmt.Errorf("%w: replication %q must be a 3-digit number (e.g. 000, 001)", ErrInvalidArg, rep)
	}
	return nil
}

// Read-only preset commands allowed when EnableRawShell is false
var readOnlyPresets = []string{
	"cluster.status",
	"volume.list",
	"s3.bucket.list",
	"s3.configure",
	"fs.ls",
}

// Read-only command prefixes allowed when EnableRawShell is true but AllowAdminActions is false
var readOnlyPrefixes = []string{
	"cluster.status",
	"volume.list",
	"s3.bucket.list",
	"s3.configure",
	"fs.ls",
	"fs.meta.cat",
	"ec.shard.list",
}

// IsPresetCommand returns true if the command starts with one of the curated presets.
func IsPresetCommand(cmd string) bool {
	fields := strings.Fields(strings.TrimSpace(cmd))
	if len(fields) == 0 {
		return false
	}
	base := strings.ToLower(fields[0])
	for _, p := range readOnlyPresets {
		if base == p {
			return true
		}
	}
	return false
}

// IsReadOnlyCommand returns true if the command is classified as read-only.
func IsReadOnlyCommand(cmd string) bool {
	fields := strings.Fields(strings.TrimSpace(cmd))
	if len(fields) == 0 {
		return false
	}
	base := strings.ToLower(fields[0])
	for _, prefix := range readOnlyPrefixes {
		if base == prefix {
			return true
		}
	}
	return false
}

// ClusterActionMutex provides in-process per-cluster mutual exclusion for maintenance operations.
type ClusterActionMutex struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// NewClusterActionMutex constructs a new ClusterActionMutex.
func NewClusterActionMutex() *ClusterActionMutex {
	return &ClusterActionMutex{
		locks: make(map[string]*sync.Mutex),
	}
}

func (m *ClusterActionMutex) getLock(clusterID string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, exists := m.locks[clusterID]
	if !exists {
		l = &sync.Mutex{}
		m.locks[clusterID] = l
	}
	return l
}

// TryLock attempts to acquire the lock for a given cluster. Returns false if already locked.
func (m *ClusterActionMutex) TryLock(clusterID string) bool {
	return m.getLock(clusterID).TryLock()
}

// Unlock releases the lock for a given cluster.
func (m *ClusterActionMutex) Unlock(clusterID string) {
	m.getLock(clusterID).Unlock()
}

// ConfirmationClaims defines the payload inside a double-confirmation token.
type ConfirmationClaims struct {
	Action    string `json:"act"`
	Target    string `json:"tgt"`
	ClusterID string `json:"cid"`
	IssuedAt  int64  `json:"iat"` // Unix timestamp in seconds
	ExpiresAt int64  `json:"exp"` // iat + 60s
	Nonce     string `json:"nonce"`
}

// ConfirmationManager manages HMAC-SHA256 double-confirmation tokens with 60s TTL and single-use nonces.
type ConfirmationManager struct {
	mu        sync.Mutex
	secretKey []byte
	usedNonce map[string]int64 // nonce -> expiresAt
}

// NewConfirmationManager creates a ConfirmationManager with an optional secret key.
// If secretKey is nil or empty, a 32-byte cryptographically secure random key is generated.
func NewConfirmationManager(secretKey []byte) *ConfirmationManager {
	if len(secretKey) == 0 {
		secretKey = make([]byte, 32)
		if _, err := rand.Read(secretKey); err != nil {
			panic(fmt.Sprintf("failed to initialize cryptographically secure secret: %v", err))
		}
	}
	return &ConfirmationManager{
		secretKey: secretKey,
		usedNonce: make(map[string]int64),
	}
}

// GenerateToken generates a signed confirmation token with 60s TTL and challenge string.
func (m *ConfirmationManager) GenerateToken(action, target, clusterID string) (token string, challenge string, err error) {
	now := time.Now().Unix()
	nonceBytes := make([]byte, 16)
	if _, err := rand.Read(nonceBytes); err != nil {
		return "", "", fmt.Errorf("generate nonce: %w", err)
	}
	nonce := hex.EncodeToString(nonceBytes)

	claims := ConfirmationClaims{
		Action:    action,
		Target:    target,
		ClusterID: clusterID,
		IssuedAt:  now,
		ExpiresAt: now + 60, // 60 seconds TTL
		Nonce:     nonce,
	}

	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", "", fmt.Errorf("marshal claims: %w", err)
	}

	payloadB64 := base64.RawURLEncoding.EncodeToString(claimsJSON)

	mac := hmac.New(sha256.New, m.secretKey)
	mac.Write([]byte(payloadB64))
	sigHex := hex.EncodeToString(mac.Sum(nil))

	token = fmt.Sprintf("ctk_%s.%s", payloadB64, sigHex)
	return token, target, nil
}

// ValidateAndConsumeToken verifies token validity, signature, challenge response, and single-use nonce.
func (m *ConfirmationManager) ValidateAndConsumeToken(token, action, target, clusterID, challengeResponse string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Clean up expired nonces
	now := time.Now().Unix()
	for n, exp := range m.usedNonce {
		if now > exp {
			delete(m.usedNonce, n)
		}
	}

	if !strings.HasPrefix(token, "ctk_") {
		return fmt.Errorf("%w: invalid prefix", ErrTokenInvalid)
	}

	parts := strings.Split(strings.TrimPrefix(token, "ctk_"), ".")
	if len(parts) != 2 {
		return fmt.Errorf("%w: malformed token format", ErrTokenInvalid)
	}

	payloadB64 := parts[0]
	sigHex := parts[1]

	mac := hmac.New(sha256.New, m.secretKey)
	mac.Write([]byte(payloadB64))
	expectedSig := hex.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(sigHex), []byte(expectedSig)) {
		return fmt.Errorf("%w: signature mismatch", ErrTokenInvalid)
	}

	claimsJSON, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		return fmt.Errorf("%w: invalid base64 encoding", ErrTokenInvalid)
	}

	var claims ConfirmationClaims
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		return fmt.Errorf("%w: invalid json payload", ErrTokenInvalid)
	}

	if now > claims.ExpiresAt {
		return ErrTokenExpired
	}

	if claims.Action != action || claims.Target != target || claims.ClusterID != clusterID {
		return fmt.Errorf("%w: action/target/cluster mismatch", ErrTokenInvalid)
	}

	if challengeResponse != claims.Target {
		return fmt.Errorf("%w: expected %q, got %q", ErrChallengeMismatch, claims.Target, challengeResponse)
	}

	if _, used := m.usedNonce[claims.Nonce]; used {
		return ErrTokenAlreadyUsed
	}

	// Consume nonce
	m.usedNonce[claims.Nonce] = claims.ExpiresAt
	return nil
}

// AuditRecord models a structured JSON audit event.
type AuditRecord struct {
	Timestamp      time.Time `json:"timestamp"`
	EventID        string    `json:"eventId"`
	ClusterID      string    `json:"clusterId"`
	RemoteAddr     string    `json:"remoteAddr,omitempty"`
	ForwardedFor   string    `json:"forwardedFor,omitempty"`
	ClientCertSubj string    `json:"clientCertSubject,omitempty"`
	UserAgent      string    `json:"userAgent,omitempty"`
	Action         string    `json:"action"`
	Target         string    `json:"target,omitempty"`
	Tier           int       `json:"tier"`
	IsMutating     bool      `json:"isMutating"`
	Command        string    `json:"command,omitempty"`
	DurationMs     int64     `json:"durationMs"`
	Status         string    `json:"status"` // "SUCCESS", "REJECTED_FORBIDDEN", "FAILED", "CONFLICT", "INVALID_ARG"
	ExitCode       int       `json:"exitCode"`
	Error          string    `json:"error,omitempty"`
}

// AuditLogger writes structured JSON audit logs in a thread-safe manner.
type AuditLogger struct {
	mu     sync.Mutex
	writer io.Writer
}

// NewAuditLogger constructs an AuditLogger.
func NewAuditLogger(w io.Writer) *AuditLogger {
	return &AuditLogger{
		writer: w,
	}
}

// Log writes an AuditRecord as a single-line JSON object.
func (l *AuditLogger) Log(record AuditRecord) {
	if l == nil || l.writer == nil {
		return
	}
	if record.Timestamp.IsZero() {
		record.Timestamp = time.Now().UTC()
	}
	if record.EventID == "" {
		idBytes := make([]byte, 8)
		_, _ = rand.Read(idBytes)
		record.EventID = hex.EncodeToString(idBytes)
	}

	data, err := json.Marshal(record)
	if err != nil {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = l.writer.Write(append(data, '\n'))
}
