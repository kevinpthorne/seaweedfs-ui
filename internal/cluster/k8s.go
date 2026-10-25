package cluster

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"seaweedfs-ui/internal/config"
)

const (
	defaultTokenPath     = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	defaultCAPath        = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	defaultNamespacePath = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

	defaultServiceSelector  = "app.kubernetes.io/name=seaweedfs"
	defaultFilerSyncSelector = "app.kubernetes.io/component=filer-sync"
)

// K8sDiscoveryOption configures the K8sDiscoveryProvider.
type K8sDiscoveryOption func(*K8sDiscoveryProvider)

// WithAPIBaseURL sets the base URL for the Kubernetes API server (e.g. for testing).
func WithAPIBaseURL(baseURL string) K8sDiscoveryOption {
	return func(p *K8sDiscoveryProvider) {
		p.apiBaseURL = strings.TrimSuffix(baseURL, "/")
		p.isK8sEnv = true
	}
}

// WithHTTPClient sets a custom HTTP client for Kubernetes API requests.
func WithHTTPClient(client *http.Client) K8sDiscoveryOption {
	return func(p *K8sDiscoveryProvider) {
		p.httpClient = client
	}
}

// WithToken sets the bearer token for authenticating to the Kubernetes API.
func WithToken(token string) K8sDiscoveryOption {
	return func(p *K8sDiscoveryProvider) {
		p.token = token
	}
}

// WithInClusterNamespace sets the local in-cluster namespace.
func WithInClusterNamespace(namespace string) K8sDiscoveryOption {
	return func(p *K8sDiscoveryProvider) {
		p.inClusterNS = namespace
	}
}

// K8sDiscoveryProvider discovers SeaweedFS clusters from Kubernetes Services, Endpoints, and Pods.
type K8sDiscoveryProvider struct {
	cfg         config.K8sDiscoveryConfig
	globalTLS   config.TLSConfig
	apiBaseURL  string
	token       string
	httpClient  *http.Client
	isK8sEnv    bool
	inClusterNS string
}

// NewK8sDiscoveryProvider constructs a new K8sDiscoveryProvider.
func NewK8sDiscoveryProvider(cfg config.K8sDiscoveryConfig, globalTLS config.TLSConfig, opts ...K8sDiscoveryOption) *K8sDiscoveryProvider {
	p := &K8sDiscoveryProvider{
		cfg:       cfg,
		globalTLS: globalTLS,
	}

	for _, opt := range opts {
		opt(p)
	}

	if p.apiBaseURL == "" {
		p.initInClusterConfig()
	}

	if p.httpClient == nil {
		p.httpClient = &http.Client{Timeout: 10 * time.Second}
	}

	return p
}

func (p *K8sDiscoveryProvider) initInClusterConfig() {
	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	port := os.Getenv("KUBERNETES_SERVICE_PORT")

	tokenBytes, err := os.ReadFile(defaultTokenPath)
	if err != nil || host == "" || port == "" {
		p.isK8sEnv = false
		log.Println("[INFO] Kubernetes in-cluster environment not detected; K8s discovery will run in inactive fallback mode")
		return
	}

	p.token = strings.TrimSpace(string(tokenBytes))
	p.apiBaseURL = fmt.Sprintf("https://%s:%s", host, port)
	p.isK8sEnv = true

	// Read in-cluster namespace if available
	if nsBytes, err := os.ReadFile(defaultNamespacePath); err == nil {
		p.inClusterNS = strings.TrimSpace(string(nsBytes))
	}

	// Build CA pool for K8s API server
	pool := x509.NewCertPool()
	if caBytes, err := os.ReadFile(defaultCAPath); err == nil {
		pool.AppendCertsFromPEM(caBytes)
	}

	p.httpClient = &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs: pool,
			},
		},
	}
}

// ListClusters discovers running SeaweedFS clusters and filer.sync jobs from Kubernetes.
func (p *K8sDiscoveryProvider) ListClusters(ctx context.Context) ([]ClusterTarget, error) {
	if !p.isK8sEnv || !p.cfg.Enabled {
		return []ClusterTarget{}, nil
	}

	svcSelector := p.cfg.ServiceSelector
	if svcSelector == "" {
		svcSelector = defaultServiceSelector
	}

	// 1. Fetch Services
	services, err := p.fetchServices(ctx, svcSelector)
	if err != nil {
		log.Printf("[WARN] Failed to fetch K8s services: %v", err)
		return []ClusterTarget{}, nil
	}

	// 2. Fetch FilerSync Pods
	syncSelector := p.cfg.FilerSyncPodSelector
	if syncSelector == "" {
		syncSelector = defaultFilerSyncSelector
	}
	syncPods, err := p.fetchFilerSyncPods(ctx, syncSelector)
	if err != nil {
		log.Printf("[WARN] Failed to fetch filer.sync pods: %v", err)
	}

	// 3. Group services by (namespace, instance)
	type clusterGroup struct {
		namespace string
		instance  string
		masters   []string
		filer     string
		s3        string
		labels    map[string]string
	}

	groups := make(map[string]*clusterGroup)

	for _, svc := range services {
		ns := svc.Metadata.Namespace
		instance := svc.Metadata.Labels["app.kubernetes.io/instance"]
		if instance == "" {
			instance = extractInstancePrefix(svc.Metadata.Name)
		}

		key := fmt.Sprintf("%s/%s", ns, instance)
		grp, exists := groups[key]
		if !exists {
			grp = &clusterGroup{
				namespace: ns,
				instance:  instance,
				labels:    svc.Metadata.Labels,
			}
			groups[key] = grp
		}

		// Inspect ports
		for _, port := range svc.Spec.Ports {
			portName := strings.ToLower(port.Name)
			portNum := port.Port

			// Skip internal gRPC ports for REST HTTP discovery
			if strings.Contains(portName, "grpc") {
				continue
			}

			// Master port: 9333 or name contains 'master'
			if portNum == 9333 || strings.Contains(portName, "master") {
				if svc.Spec.ClusterIP == "None" {
					// Headless service: resolve individual endpoints
					podMasters := p.resolveHeadlessEndpoints(ctx, ns, svc.Metadata.Name, portNum)
					grp.masters = append(grp.masters, podMasters...)
				} else {
					addr := fmt.Sprintf("%s.%s.svc.cluster.local:%d", svc.Metadata.Name, ns, portNum)
					grp.masters = append(grp.masters, addr)
				}
			}

			// Filer port: 8888 or name contains 'filer'
			if portNum == 8888 || strings.Contains(portName, "filer") {
				// Prefer ClusterIP service over headless peer service
				if grp.filer == "" || (!strings.Contains(svc.Metadata.Name, "-peer") && svc.Spec.ClusterIP != "None") {
					grp.filer = fmt.Sprintf("%s.%s.svc.cluster.local:%d", svc.Metadata.Name, ns, portNum)
				}
			}

			// S3 port: 8333 or name contains 's3'
			if portNum == 8333 || strings.Contains(portName, "s3") {
				if grp.s3 == "" || strings.Contains(svc.Metadata.Name, "-https") || (!strings.Contains(svc.Metadata.Name, "-peer") && svc.Spec.ClusterIP != "None") {
					grp.s3 = fmt.Sprintf("%s.%s.svc.cluster.local:%d", svc.Metadata.Name, ns, portNum)
				}
			}
		}
	}

	// 4. Construct ClusterTargets
	targets := make([]ClusterTarget, 0, len(groups))
	for _, grp := range groups {
		clusterID := fmt.Sprintf("k8s-%s-%s", grp.namespace, grp.instance)
		clusterName := fmt.Sprintf("%s (%s)", grp.instance, grp.namespace)

		// Attach matching FilerSync targets
		matchingSyncs := make([]FilerSyncTarget, 0)
		for _, sync := range syncPods {
			if sync.Namespace == grp.namespace || strings.Contains(sync.SourceFiler, grp.instance) || strings.Contains(sync.TargetFiler, grp.instance) {
				matchingSyncs = append(matchingSyncs, sync)
			}
		}

		targets = append(targets, ClusterTarget{
			ID:         clusterID,
			Name:       clusterName,
			Namespace:  grp.namespace,
			Source:     SourceKubernetes,
			Masters:    dedupStrings(grp.masters),
			Filer:      grp.filer,
			S3:         grp.s3,
			TLS:        p.globalTLS,
			TLSEnabled: p.globalTLS.Enabled,
			FilerSyncs: matchingSyncs,
			Labels:     grp.labels,
		})
	}

	return targets, nil
}

func (p *K8sDiscoveryProvider) fetchServices(ctx context.Context, selector string) ([]k8sService, error) {
	var paths []string
	if len(p.cfg.Namespaces) > 0 {
		for _, ns := range p.cfg.Namespaces {
			paths = append(paths, fmt.Sprintf("/api/v1/namespaces/%s/services?labelSelector=%s", ns, url.QueryEscape(selector)))
		}
	} else {
		paths = append(paths, fmt.Sprintf("/api/v1/services?labelSelector=%s", url.QueryEscape(selector)))
	}

	var allServices []k8sService
	for _, path := range paths {
		var list k8sServiceList
		if err := p.get(ctx, path, &list); err != nil {
			// If cluster-wide listing returned 403 Forbidden and we have an in-cluster namespace, fallback to that namespace
			if strings.Contains(err.Error(), "returned 403") && p.inClusterNS != "" && len(p.cfg.Namespaces) == 0 {
				log.Printf("[INFO] Cluster-wide service query forbidden; falling back to in-cluster namespace %q", p.inClusterNS)
				fallbackPath := fmt.Sprintf("/api/v1/namespaces/%s/services?labelSelector=%s", p.inClusterNS, url.QueryEscape(selector))
				var fallbackList k8sServiceList
				if fallbackErr := p.get(ctx, fallbackPath, &fallbackList); fallbackErr == nil {
					allServices = append(allServices, fallbackList.Items...)
					continue
				}
			}
			return nil, err
		}
		allServices = append(allServices, list.Items...)
	}

	return allServices, nil
}

func (p *K8sDiscoveryProvider) resolveHeadlessEndpoints(ctx context.Context, namespace, serviceName string, port int) []string {
	endpointPath := fmt.Sprintf("/api/v1/namespaces/%s/endpoints/%s", namespace, serviceName)
	var ep k8sEndpoints
	if err := p.get(ctx, endpointPath, &ep); err != nil {
		// Fallback to service FQDN
		return []string{fmt.Sprintf("%s.%s.svc.cluster.local:%d", serviceName, namespace, port)}
	}

	var addrs []string
	for _, subset := range ep.Subsets {
		for _, addr := range subset.Addresses {
			if addr.Hostname != "" {
				addrs = append(addrs, fmt.Sprintf("%s.%s.%s.svc.cluster.local:%d", addr.Hostname, serviceName, namespace, port))
			} else if addr.TargetRef != nil && addr.TargetRef.Name != "" {
				addrs = append(addrs, fmt.Sprintf("%s.%s.%s.svc.cluster.local:%d", addr.TargetRef.Name, serviceName, namespace, port))
			} else if addr.IP != "" {
				addrs = append(addrs, fmt.Sprintf("%s:%d", addr.IP, port))
			}
		}
	}

	if len(addrs) == 0 {
		addrs = append(addrs, fmt.Sprintf("%s.%s.svc.cluster.local:%d", serviceName, namespace, port))
	}

	return addrs
}

func (p *K8sDiscoveryProvider) fetchFilerSyncPods(ctx context.Context, selector string) ([]FilerSyncTarget, error) {
	var paths []string
	if len(p.cfg.Namespaces) > 0 {
		for _, ns := range p.cfg.Namespaces {
			paths = append(paths, fmt.Sprintf("/api/v1/namespaces/%s/pods?labelSelector=%s", ns, url.QueryEscape(selector)))
		}
	} else {
		paths = append(paths, fmt.Sprintf("/api/v1/pods?labelSelector=%s", url.QueryEscape(selector)))
	}

	var syncTargets []FilerSyncTarget
	for _, path := range paths {
		var list k8sPodList
		if err := p.get(ctx, path, &list); err != nil {
			// If cluster-wide listing returned 403 Forbidden and we have an in-cluster namespace, fallback to that namespace
			if strings.Contains(err.Error(), "returned 403") && p.inClusterNS != "" && len(p.cfg.Namespaces) == 0 {
				log.Printf("[INFO] Cluster-wide pods query forbidden; falling back to in-cluster namespace %q", p.inClusterNS)
				fallbackPath := fmt.Sprintf("/api/v1/namespaces/%s/pods?labelSelector=%s", p.inClusterNS, url.QueryEscape(selector))
				var fallbackList k8sPodList
				if fallbackErr := p.get(ctx, fallbackPath, &fallbackList); fallbackErr == nil {
					for _, pod := range fallbackList.Items {
						for _, container := range pod.Spec.Containers {
							target := parseFilerSyncArgs(pod.Metadata.Namespace, pod.Metadata.Name, pod.Status.PodIP, container.Command, container.Args, pod.Metadata.Labels)
							if target != nil {
								syncTargets = append(syncTargets, *target)
							}
						}
					}
					continue
				}
			}
			return nil, err
		}

		for _, pod := range list.Items {
			for _, container := range pod.Spec.Containers {
				target := parseFilerSyncArgs(pod.Metadata.Namespace, pod.Metadata.Name, pod.Status.PodIP, container.Command, container.Args, pod.Metadata.Labels)
				if target != nil {
					syncTargets = append(syncTargets, *target)
				}
			}
		}
	}

	return syncTargets, nil
}

func parseFilerSyncArgs(namespace, podName, podIP string, cmd, args []string, labels map[string]string) *FilerSyncTarget {
	allTokens := append(append([]string{}, cmd...), args...)

	var sourceFiler, targetFiler string
	sourcePath := "/"
	targetPath := "/"
	isActivePassive := false
	metricsPort := 0

	for i := 0; i < len(allTokens); i++ {
		token := allTokens[i]

		// Handle -a / -filerA
		if strings.HasPrefix(token, "-a=") || strings.HasPrefix(token, "--a=") {
			sourceFiler = strings.SplitN(token, "=", 2)[1]
		} else if strings.HasPrefix(token, "-filerA=") || strings.HasPrefix(token, "--filerA=") {
			sourceFiler = strings.SplitN(token, "=", 2)[1]
		} else if (token == "-a" || token == "--a" || token == "-filerA" || token == "--filerA") && i+1 < len(allTokens) {
			sourceFiler = allTokens[i+1]
			i++
		}

		// Handle -b / -filerB
		if strings.HasPrefix(token, "-b=") || strings.HasPrefix(token, "--b=") {
			targetFiler = strings.SplitN(token, "=", 2)[1]
		} else if strings.HasPrefix(token, "-filerB=") || strings.HasPrefix(token, "--filerB=") {
			targetFiler = strings.SplitN(token, "=", 2)[1]
		} else if (token == "-b" || token == "--b" || token == "-filerB" || token == "--filerB") && i+1 < len(allTokens) {
			targetFiler = allTokens[i+1]
			i++
		}

		// Handle -a.path
		if strings.HasPrefix(token, "-a.path=") || strings.HasPrefix(token, "--a.path=") {
			sourcePath = strings.SplitN(token, "=", 2)[1]
		} else if (token == "-a.path" || token == "--a.path") && i+1 < len(allTokens) {
			sourcePath = allTokens[i+1]
			i++
		}

		// Handle -b.path
		if strings.HasPrefix(token, "-b.path=") || strings.HasPrefix(token, "--b.path=") {
			targetPath = strings.SplitN(token, "=", 2)[1]
		} else if (token == "-b.path" || token == "--b.path") && i+1 < len(allTokens) {
			targetPath = allTokens[i+1]
			i++
		}

		// Handle -isActivePassive
		if strings.HasPrefix(token, "-isActivePassive=") || strings.HasPrefix(token, "--isActivePassive=") {
			val := strings.SplitN(token, "=", 2)[1]
			isActivePassive = (val == "true" || val == "1")
		} else if token == "-isActivePassive" || token == "--isActivePassive" {
			isActivePassive = true
		}

		// Handle -metricsPort
		if strings.HasPrefix(token, "-metricsPort=") || strings.HasPrefix(token, "--metricsPort=") {
			val := strings.SplitN(token, "=", 2)[1]
			metricsPort, _ = strconv.Atoi(val)
		} else if (token == "-metricsPort" || token == "--metricsPort") && i+1 < len(allTokens) {
			metricsPort, _ = strconv.Atoi(allTokens[i+1])
			i++
		}
	}

	if sourceFiler == "" && targetFiler == "" {
		return nil
	}

	metricsURL := ""
	if metricsPort > 0 && podIP != "" {
		metricsURL = fmt.Sprintf("http://%s:%d/metrics", podIP, metricsPort)
	}

	return &FilerSyncTarget{
		ID:              fmt.Sprintf("%s/%s", namespace, podName),
		Name:            podName,
		DiscoverySource: SourceKubernetes,
		PodName:         podName,
		Namespace:       namespace,
		MetricsURL:      metricsURL,
		MetricsPort:     metricsPort,
		SourceFiler:     sourceFiler,
		SourcePath:      sourcePath,
		TargetFiler:     targetFiler,
		TargetPath:      targetPath,
		IsActivePassive: isActivePassive,
		Labels:          labels,
	}
}

func (p *K8sDiscoveryProvider) get(ctx context.Context, path string, out interface{}) error {
	fullURL := p.apiBaseURL + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return fmt.Errorf("create k8s request: %w", err)
	}

	if p.token != "" {
		req.Header.Set("Authorization", "Bearer "+p.token)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("do k8s request (%s): %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("k8s api returned %d for %s: %s", resp.StatusCode, path, string(body))
	}

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode k8s response (%s): %w", path, err)
	}

	return nil
}

func extractInstancePrefix(name string) string {
	for _, suffix := range []string{"-master-peer", "-filer-peer", "-volume-peer", "-master", "-filer", "-s3", "-volume"} {
		if strings.HasSuffix(name, suffix) {
			return strings.TrimSuffix(name, suffix)
		}
	}
	return name
}

func dedupStrings(in []string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// Minimal Kubernetes API JSON structures
type k8sServiceList struct {
	Items []k8sService `json:"items"`
}

type k8sService struct {
	Metadata k8sMetadata   `json:"metadata"`
	Spec     k8sServiceSpec `json:"spec"`
}

type k8sMetadata struct {
	Name      string            `json:"name"`
	Namespace string            `json:"namespace"`
	Labels    map[string]string `json:"labels"`
}

type k8sServiceSpec struct {
	ClusterIP string            `json:"clusterIP"`
	Selector  map[string]string `json:"selector"`
	Ports     []k8sPort         `json:"ports"`
}

type k8sPort struct {
	Name string `json:"name"`
	Port int    `json:"port"`
}

type k8sEndpoints struct {
	Metadata k8sMetadata `json:"metadata"`
	Subsets  []k8sSubset `json:"subsets"`
}

type k8sSubset struct {
	Addresses []k8sAddress `json:"addresses"`
	Ports     []k8sPort    `json:"ports"`
}

type k8sAddress struct {
	IP        string        `json:"ip"`
	Hostname  string        `json:"hostname"`
	TargetRef *k8sTargetRef `json:"targetRef"`
}

type k8sTargetRef struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

type k8sPodList struct {
	Items []k8sPod `json:"items"`
}

type k8sPod struct {
	Metadata k8sMetadata  `json:"metadata"`
	Spec     k8sPodSpec   `json:"spec"`
	Status   k8sPodStatus `json:"status"`
}

type k8sPodSpec struct {
	Containers []k8sContainer `json:"containers"`
}

type k8sContainer struct {
	Name    string   `json:"name"`
	Command []string `json:"command"`
	Args    []string `json:"args"`
}

type k8sPodStatus struct {
	Phase string `json:"phase"`
	PodIP string `json:"podIP"`
}
