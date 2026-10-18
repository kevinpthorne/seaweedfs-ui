// app.js - Application entry point, router, state, & auto-refresh ticker

import { apiFetch, showToast } from './api.js';
import { renderOverview } from './views/overview.js';
import { renderTopology } from './views/topology.js';
import { renderFilerSync } from './views/filersync.js';
import { renderS3 } from './views/s3.js';
import { renderFiler } from './views/filer.js';
import { renderShell } from './views/shell.js';

export const appState = {
  config: null,             // { allowAdminActions, enableRawShell, version, queryTimeout, actionTimeout }
  clusters: [],             // Array of ClusterTarget
  selectedClusterId: null,  // Active cluster ID
  currentTab: 'overview',   // Active route
  refreshIntervalMs: 15000, // 0 (Off), 5000, 15000, 30000
  refreshTimerId: null,
  isTabVisible: true
};

const viewRenderers = {
  overview: renderOverview,
  topology: renderTopology,
  filersync: renderFilerSync,
  s3: renderS3,
  filer: renderFiler,
  shell: renderShell
};

/**
 * Initializes and bootstraps the Single-Page Application.
 */
async function initApp() {
  initTheme();
  setupEventListeners();

  try {
    await loadConfig();
    await loadClusters();
  } catch (err) {
    console.error('Failed to initialize app data:', err);
  }

  handleRoute();
  setupAutoRefresh();
}

/**
 * Loads system configuration and feature flags from /api/v1/config.
 */
async function loadConfig() {
  try {
    const cfg = await apiFetch('/config');
    appState.config = cfg;

    // Update app version
    const versionPill = document.getElementById('app-version');
    if (versionPill && cfg.version) {
      versionPill.textContent = `v${cfg.version}`;
    }

    // Update admin mode badge
    const modeBadge = document.getElementById('admin-mode-badge');
    if (modeBadge) {
      if (cfg.allowAdminActions) {
        modeBadge.className = 'badge badge-admin';
        modeBadge.textContent = 'ADMIN WRITE ENABLED';
        modeBadge.title = 'Maintenance actions and write operations are permitted';
      } else {
        modeBadge.className = 'badge badge-readonly';
        modeBadge.textContent = 'READ-ONLY';
        modeBadge.title = 'All mutating operations are strictly forbidden';
      }
    }
  } catch (err) {
    console.error('Failed to load server configuration:', err);
  }
}

/**
 * Loads discovered clusters from /api/v1/clusters and populates the selector.
 */
async function loadClusters() {
  const clusterSelect = document.getElementById('cluster-select');
  if (!clusterSelect) return;

  try {
    const clusters = await apiFetch('/clusters');
    appState.clusters = Array.isArray(clusters) ? clusters : [];

    clusterSelect.innerHTML = '';

    if (appState.clusters.length === 0) {
      clusterSelect.innerHTML = '<option value="" disabled selected>No clusters found</option>';
      updateClusterBadges(null);
      return;
    }

    // Check saved cluster ID from localStorage
    const savedId = localStorage.getItem('seaweedfs_selected_cluster');
    let matchedId = null;

    appState.clusters.forEach(c => {
      const opt = document.createElement('option');
      opt.value = c.id;
      opt.textContent = `${c.name || c.id} (${c.source || 'static'})`;
      clusterSelect.appendChild(opt);

      if (savedId && c.id === savedId) {
        matchedId = c.id;
      }
    });

    if (!matchedId) {
      matchedId = appState.clusters[0].id;
    }

    clusterSelect.value = matchedId;
    appState.selectedClusterId = matchedId;
    localStorage.setItem('seaweedfs_selected_cluster', matchedId);

    const activeCluster = appState.clusters.find(c => c.id === matchedId);
    updateClusterBadges(activeCluster);
  } catch (err) {
    clusterSelect.innerHTML = '<option value="" disabled selected>Error loading clusters</option>';
    console.error('Failed to load clusters:', err);
  }
}

/**
 * Updates K8s/Static source and TLS badges in header.
 */
function updateClusterBadges(cluster) {
  const sourceBadge = document.getElementById('cluster-source-badge');
  const tlsBadge = document.getElementById('cluster-tls-badge');

  if (!cluster) {
    if (sourceBadge) sourceBadge.style.display = 'none';
    if (tlsBadge) tlsBadge.style.display = 'none';
    return;
  }

  if (sourceBadge) {
    sourceBadge.style.display = 'inline-flex';
    const isK8s = cluster.source === 'kubernetes';
    sourceBadge.textContent = isK8s ? 'K8s' : 'Static';
    sourceBadge.className = `badge ${isK8s ? 'badge-info' : 'badge-source'}`;
  }

  if (tlsBadge) {
    tlsBadge.style.display = cluster.tlsEnabled ? 'inline-flex' : 'none';
  }
}

/**
 * Router: handles hash navigation changes and mounts active view.
 */
function handleRoute() {
  let hash = window.location.hash.replace(/^#\/?/, '').trim();
  if (!hash || !viewRenderers[hash]) {
    hash = 'overview';
  }

  appState.currentTab = hash;

  // Update tab links active state
  document.querySelectorAll('.tabs-nav .tab-link').forEach(link => {
    const tabName = link.getAttribute('data-tab');
    if (tabName === hash) {
      link.classList.add('active');
    } else {
      link.classList.remove('active');
    }
  });

  // Render current view
  renderCurrentView();
}

/**
 * Renders the currently selected view inside #view-container.
 */
function renderCurrentView() {
  const container = document.getElementById('view-container');
  if (!container) return;

  const renderer = viewRenderers[appState.currentTab];
  if (typeof renderer === 'function') {
    renderer(container, appState.selectedClusterId, appState);
  }
}

/**
 * Auto-refresh polling ticker.
 */
function setupAutoRefresh() {
  if (appState.refreshTimerId) {
    clearInterval(appState.refreshTimerId);
    appState.refreshTimerId = null;
  }

  if (appState.refreshIntervalMs > 0) {
    appState.refreshTimerId = setInterval(() => {
      if (appState.isTabVisible) {
        renderCurrentView();
      }
    }, appState.refreshIntervalMs);
  }
}

/**
 * Wire DOM event listeners.
 */
function setupEventListeners() {
  // Hash route changes
  window.addEventListener('hashchange', handleRoute);
  window.addEventListener('popstate', handleRoute);

  // Cluster dropdown change
  const clusterSelect = document.getElementById('cluster-select');
  if (clusterSelect) {
    clusterSelect.addEventListener('change', (e) => {
      const newId = e.target.value;
      appState.selectedClusterId = newId;
      localStorage.setItem('seaweedfs_selected_cluster', newId);

      const activeCluster = appState.clusters.find(c => c.id === newId);
      updateClusterBadges(activeCluster);

      renderCurrentView();
    });
  }

  // Refresh interval select
  const refreshSelect = document.getElementById('refresh-interval-select');
  if (refreshSelect) {
    refreshSelect.addEventListener('change', (e) => {
      appState.refreshIntervalMs = parseInt(e.target.value, 10) || 0;
      setupAutoRefresh();
      showToast(`Auto-refresh interval set to ${e.target.options[e.target.selectedIndex].text}`, 'info', 2000);
    });
  }

  // Manual refresh button
  const manualBtn = document.getElementById('manual-refresh-btn');
  if (manualBtn) {
    manualBtn.addEventListener('click', () => {
      renderCurrentView();
      showToast('Refreshed current view', 'info', 1500);
    });
  }

  // Theme toggle button
  const themeBtn = document.getElementById('theme-toggle-btn');
  if (themeBtn) {
    themeBtn.addEventListener('click', toggleTheme);
  }

  // Document visibility detection
  document.addEventListener('visibilitychange', () => {
    appState.isTabVisible = !document.hidden;
    if (appState.isTabVisible && appState.refreshIntervalMs > 0) {
      renderCurrentView();
    }
  });
}

/**
 * Dark/light theme initialization and toggle.
 */
function initTheme() {
  const savedTheme = localStorage.getItem('seaweedfs_theme') || 'dark';
  document.documentElement.setAttribute('data-theme', savedTheme);
}

function toggleTheme() {
  const current = document.documentElement.getAttribute('data-theme') || 'dark';
  const next = current === 'dark' ? 'light' : 'dark';
  document.documentElement.setAttribute('data-theme', next);
  localStorage.setItem('seaweedfs_theme', next);
}

// Bootstrap
if (document.readyState === 'loading') {
  document.addEventListener('DOMContentLoaded', initApp);
} else {
  initApp();
}
