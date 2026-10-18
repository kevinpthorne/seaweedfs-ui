// views/filersync.js - Directional FilerSync cards, lag badges, & performance metrics

import { apiFetch, formatLag, formatBytes, formatDate, escapeHtml } from '../api.js';

export async function renderFilerSync(container, clusterId, appState) {
  if (!clusterId) {
    container.innerHTML = `
      <div class="empty-state">
        <div class="empty-state-icon">🔄</div>
        <h3>No Cluster Selected</h3>
        <p>Please select a SeaweedFS cluster from the dropdown above.</p>
      </div>
    `;
    return;
  }

  container.innerHTML = `
    <div class="view-loading">
      <div class="spinner"></div>
      <p>Loading filer sync jobs...</p>
    </div>
  `;

  try {
    const data = await apiFetch(`/clusters/${encodeURIComponent(clusterId)}/filersync`);
    const jobs = Array.isArray(data) ? data : [];

    if (jobs.length === 0) {
      container.innerHTML = `
        <div class="card">
          <div class="card-header">
            <h2 class="card-title">Filer Sync Replication Monitor</h2>
          </div>
          <div class="empty-state">
            <div class="empty-state-icon">🔄</div>
            <h3>No Active Filer Sync Jobs</h3>
            <p>No <code>weed filer.sync</code> pods or replication targets were discovered for this cluster.</p>
          </div>
        </div>
      `;
      return;
    }

    // Compute summary metrics
    let totalSyncedFiles = 0;
    let totalErrors = 0;
    let sumLag = 0;
    let lagCount = 0;

    for (const job of jobs) {
      const metrics = job.metrics || {};
      const checkpoint = job.checkpoint || {};
      totalSyncedFiles += metrics.syncedFilesTotal || 0;
      totalErrors += metrics.syncErrorsTotal || 0;
      if (typeof checkpoint.replicationLagSeconds === 'number') {
        sumLag += checkpoint.replicationLagSeconds;
        lagCount++;
      }
    }

    const avgLag = lagCount > 0 ? (sumLag / lagCount) : 0;

    container.innerHTML = `
      <!-- Summary Banner Grid -->
      <div class="grid-4">
        <div class="card">
          <div class="stat-label">Active Sync Pairs</div>
          <div class="stat-value">${jobs.length}</div>
        </div>
        <div class="card">
          <div class="stat-label">Synced Files Total</div>
          <div class="stat-value">${totalSyncedFiles.toLocaleString()}</div>
        </div>
        <div class="card">
          <div class="stat-label">Average Replication Lag</div>
          <div class="stat-value" style="font-size: 1.15rem;">${formatLag(avgLag)}</div>
        </div>
        <div class="card">
          <div class="stat-label">Total Sync Errors</div>
          <div class="stat-value" style="color: ${totalErrors > 0 ? 'var(--color-danger)' : 'var(--color-success)'}">
            ${totalErrors}
          </div>
        </div>
      </div>

      <!-- Sync Cards List -->
      <div class="sync-cards">
        ${jobs.map(job => {
          const mode = job.mode || 'Active-Active';
          const isAA = mode === 'Active-Active';
          const checkpoint = job.checkpoint || {};
          const metrics = job.metrics || {};
          const lag = typeof checkpoint.replicationLagSeconds === 'number' ? checkpoint.replicationLagSeconds : null;

          let lagBadgeClass = 'badge-source';
          if (lag !== null) {
            if (lag < 5) lagBadgeClass = 'badge-success';
            else if (lag <= 60) lagBadgeClass = 'badge-warning';
            else lagBadgeClass = 'badge-danger';
          }

          const state = job.state || (job.ready ? 'HEALTHY' : 'PENDING');
          const isHealthy = state === 'HEALTHY' || job.ready;

          return `
            <div class="sync-card">
              <div class="sync-card-header">
                <div>
                  <h3 style="font-size: 1.05rem; font-weight: 600;">
                    ${escapeHtml(job.name || job.id)}
                  </h3>
                  <div style="font-size: 0.8rem; color: var(--text-secondary); margin-top: 0.2rem;">
                    Pod: <span style="font-family: var(--font-mono);">${escapeHtml(job.podName || 'N/A')}</span>
                    ${job.namespace ? ` | Namespace: <span style="font-family: var(--font-mono);">${escapeHtml(job.namespace)}</span>` : ''}
                  </div>
                </div>
                <div style="display: flex; gap: 0.5rem; align-items: center; flex-wrap: wrap;">
                  <span class="badge ${isAA ? 'badge-info' : 'badge-source'}">${escapeHtml(mode)}</span>
                  <span class="badge ${lagBadgeClass}">Lag: ${formatLag(lag)}</span>
                  <span class="badge ${isHealthy ? 'badge-success' : 'badge-danger'}">${escapeHtml(state)}</span>
                </div>
              </div>

              <!-- Directional Flow Graphic -->
              <div class="sync-flow">
                <div class="sync-node">
                  <div class="sync-node-label">Source Filer</div>
                  <div class="sync-node-val">${escapeHtml(job.source?.filer || 'N/A')}</div>
                  <div style="font-size: 0.8rem; color: var(--text-secondary); font-family: var(--font-mono);">
                    Path: ${escapeHtml(job.source?.path || '/')}
                  </div>
                </div>

                <div class="sync-arrow">──►</div>

                <div class="sync-node">
                  <div class="sync-node-label">Target Filer</div>
                  <div class="sync-node-val">${escapeHtml(job.target?.filer || 'N/A')}</div>
                  <div style="font-size: 0.8rem; color: var(--text-secondary); font-family: var(--font-mono);">
                    Path: ${escapeHtml(job.target?.path || '/')}
                  </div>
                </div>
              </div>

              <!-- Performance & Metrics Sub-Grid -->
              <div class="grid-4" style="margin-bottom: 0;">
                <div class="stat-item">
                  <div class="stat-label">Files Synced</div>
                  <div style="font-family: var(--font-mono); font-weight: 600;">
                    ${(metrics.syncedFilesTotal || 0).toLocaleString()}
                  </div>
                </div>
                <div class="stat-item">
                  <div class="stat-label">Throughput</div>
                  <div style="font-family: var(--font-mono); font-weight: 600;">
                    ${(metrics.eventsPerSecond || 0).toFixed(1)} events/s
                  </div>
                </div>
                <div class="stat-item">
                  <div class="stat-label">Sync Errors</div>
                  <div style="font-family: var(--font-mono); font-weight: 600; color: ${(metrics.syncErrorsTotal || 0) > 0 ? 'var(--color-danger)' : 'inherit'};">
                    ${metrics.syncErrorsTotal || 0}
                  </div>
                </div>
                <div class="stat-item">
                  <div class="stat-label">Last Checkpoint</div>
                  <div style="font-size: 0.8rem; color: var(--text-secondary);">
                    ${formatDate(checkpoint.lastSyncedAt)}
                  </div>
                </div>
              </div>
            </div>
          `;
        }).join('')}
      </div>
    `;
  } catch (err) {
    container.innerHTML = `
      <div class="empty-state">
        <div class="empty-state-icon">⚠️</div>
        <h3>Failed to Load Filer Sync Monitor</h3>
        <p>${escapeHtml(err.message)}</p>
      </div>
    `;
  }
}
