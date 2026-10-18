// views/overview.js - Raft leader, quorum status, capacity & volume slots

import { apiFetch, formatBytes, escapeHtml } from '../api.js';

export async function renderOverview(container, clusterId, appState) {
  if (!clusterId) {
    container.innerHTML = `
      <div class="empty-state">
        <div class="empty-state-icon">☁️</div>
        <h3>No Cluster Selected</h3>
        <p>Please select a SeaweedFS cluster from the dropdown above.</p>
      </div>
    `;
    return;
  }

  container.innerHTML = `
    <div class="view-loading">
      <div class="spinner"></div>
      <p>Loading cluster overview...</p>
    </div>
  `;

  try {
    const data = await apiFetch(`/clusters/${encodeURIComponent(clusterId)}/overview`);

    const leader = data.leader || 'None';
    const isLeader = data.isLeader ? '<span class="badge badge-success">Leader</span>' : '<span class="badge badge-info">Follower</span>';
    const peers = data.raftPeers || [];
    const quorumOk = peers.length > 0;

    const usedBytes = data.capacity?.usedBytes || 0;
    const volTotal = data.volumes?.total || 0;
    const volMax = data.volumes?.max || 0;
    const volFree = data.volumes?.free || 0;

    let slotPercent = 0;
    if (volMax > 0) {
      slotPercent = Math.min(100, Math.round((volTotal / volMax) * 100));
    }

    let slotMeterClass = 'normal';
    if (slotPercent > 85) slotMeterClass = 'critical';
    else if (slotPercent > 70) slotMeterClass = 'warning';

    const filerBadge = data.filerAvailable
      ? '<span class="badge badge-success">Available</span>'
      : '<span class="badge badge-danger">Not Configured</span>';

    const s3Badge = data.s3Available
      ? '<span class="badge badge-success">Available</span>'
      : '<span class="badge badge-source">Not Configured</span>';

    container.innerHTML = `
      <div class="grid-2">
        <!-- Master & Raft Quorum -->
        <div class="card">
          <div class="card-header">
            <h2 class="card-title">Master &amp; Raft Quorum</h2>
            ${quorumOk ? '<span class="badge badge-success">Quorum OK</span>' : '<span class="badge badge-warning">Degraded</span>'}
          </div>
          <div class="stat-item">
            <div class="stat-label">Active Master Leader</div>
            <div class="stat-value" style="font-size: 1.05rem;">
              ${escapeHtml(leader)} ${isLeader}
            </div>
          </div>
          <div class="stat-item" style="margin-top: 1rem;">
            <div class="stat-label">Raft Peers (${peers.length})</div>
            <div style="margin-top: 0.5rem; display: flex; flex-direction: column; gap: 0.35rem;">
              ${peers.map(p => `
                <div style="font-family: var(--font-mono); font-size: 0.85rem; padding: 0.35rem 0.6rem; background: var(--bg-surface-elevated); border-radius: var(--radius-sm); border: 1px solid var(--border-subtle); display: flex; justify-content: space-between; align-items: center;">
                  <span>${escapeHtml(p)}</span>
                  ${p === leader ? '<span class="badge badge-success" style="font-size: 0.65rem;">Active Leader</span>' : '<span class="badge badge-source" style="font-size: 0.65rem;">Peer</span>'}
                </div>
              `).join('')}
            </div>
          </div>
        </div>

        <!-- Component Endpoints & Services -->
        <div class="card">
          <div class="card-header">
            <h2 class="card-title">Component Endpoints</h2>
          </div>
          <div class="stat-item">
            <div class="stat-label">Filer Gateway</div>
            <div style="display: flex; align-items: center; justify-content: space-between; margin-top: 0.25rem;">
              <span style="font-family: var(--font-mono); font-size: 0.9rem;">
                ${data.filerAvailable ? 'SeaweedFS Filer API' : 'No filer configured'}
              </span>
              ${filerBadge}
            </div>
          </div>
          <div class="stat-item" style="margin-top: 1rem;">
            <div class="stat-label">S3 Gateway</div>
            <div style="display: flex; align-items: center; justify-content: space-between; margin-top: 0.25rem;">
              <span style="font-family: var(--font-mono); font-size: 0.9rem;">
                ${data.s3Available ? 'SeaweedFS S3 Gateway' : 'No S3 configured'}
              </span>
              ${s3Badge}
            </div>
          </div>
          <div class="stat-item" style="margin-top: 1rem;">
            <div class="stat-label">Cluster Identifier</div>
            <div style="font-family: var(--font-mono); font-size: 0.9rem; margin-top: 0.25rem;">
              ${escapeHtml(data.clusterId || clusterId)}
            </div>
          </div>
        </div>
      </div>

      <div class="grid-2">
        <!-- Storage Capacity Meter -->
        <div class="card">
          <div class="card-header">
            <h2 class="card-title">Storage Data</h2>
          </div>
          <div class="stat-item">
            <div class="stat-label">Total Volume Data Stored</div>
            <div class="stat-value">${formatBytes(usedBytes)}</div>
          </div>
          <div class="stat-item" style="margin-top: 1rem;">
            <div class="stat-label">Active Storage Volumes</div>
            <div class="stat-value">${volTotal}</div>
          </div>
        </div>

        <!-- Volume Slots Gauge -->
        <div class="card">
          <div class="card-header">
            <h2 class="card-title">Volume Slots Allocation</h2>
            <span class="badge badge-source">${slotPercent}% Allocated</span>
          </div>
          <div class="stat-item">
            <div class="stat-label">Volume Slots: Total / Max (Free)</div>
            <div class="stat-value">${volTotal} / ${volMax || '∞'} <span style="font-size: 0.9rem; font-weight: normal; color: var(--text-secondary);">(${volFree} free)</span></div>
          </div>
          <div class="meter">
            <div class="meter-fill ${slotMeterClass}" style="width: ${slotPercent}%;"></div>
          </div>
          <div style="display: flex; justify-content: space-between; font-size: 0.75rem; color: var(--text-secondary); margin-top: 0.5rem;">
            <span>0 slots</span>
            <span>${volMax} max slots</span>
          </div>
        </div>
      </div>
    `;
  } catch (err) {
    container.innerHTML = `
      <div class="empty-state">
        <div class="empty-state-icon">⚠️</div>
        <h3>Failed to Load Overview</h3>
        <p>${escapeHtml(err.message)}</p>
      </div>
    `;
  }
}
