package cluster

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"seaweedfs-ui/internal/config"
)

func TestK8sDiscoveryProviderRefreshesProjectedToken(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("initial-token"), 0600); err != nil {
		t.Fatal(err)
	}

	authorization := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization <- r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	provider := NewK8sDiscoveryProvider(
		config.K8sDiscoveryConfig{Enabled: true},
		config.TLSConfig{},
		WithAPIBaseURL(server.URL),
		WithHTTPClient(server.Client()),
	)
	provider.tokenPath = tokenPath

	request := func() string {
		t.Helper()
		var result map[string]bool
		if err := provider.get(context.Background(), "/probe", &result); err != nil {
			t.Fatalf("request failed: %v", err)
		}
		return <-authorization
	}

	if got := request(); got != "Bearer initial-token" {
		t.Fatalf("initial authorization = %q", got)
	}
	if err := os.WriteFile(tokenPath, []byte("rotated-token"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := request(); got != "Bearer rotated-token" {
		t.Fatalf("authorization after token rotation = %q", got)
	}
}
