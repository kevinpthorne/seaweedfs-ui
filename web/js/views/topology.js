// views/topology.js - DC -> Rack -> DataNode hierarchy, volume tables, filters, & maintenance actions

import { apiFetch, showToast, showConfirmModal, formatBytes, escapeHtml } from '../api.js';

export async function renderTopology(container, clusterId, appState) {
  if (!clusterId) {
    container.innerHTML = `
      <div class="empty-state">
        <div class="empty-state-icon">🌐</div>
        <h3>No Cluster Selected</h3>
        <p>Please select a SeaweedFS cluster from the dropdown above.</p>
      </div>
    `;
    return;
  }

  container.innerHTML = `
    <div class="view-loading">
      <div class="spinner"></div>
      <p>Loading topology and volume data...</p>
    </div>
  `;

  try {
    const [topoData, volumesData] = await Promise.all([
      apiFetch(`/clusters/${encodeURIComponent(clusterId)}/topology`),
      apiFetch(`/clusters/${encodeURIComponent(clusterId)}/volumes`)
    ]);

    const canAdmin = !!(appState.config && appState.config.allowAdminActions);
    const disabledAttr = canAdmin ? '' : 'disabled title="Admin actions are disabled in server configuration"';

    const dcs = topoData.dataCenters || [];
    const allVolumes = Array.isArray(volumesData) ? volumesData : [];

    container.innerHTML = `
      <!-- Action Toolbar -->
      <div class="toolbar">
        <div class="toolbar-group">
          <button id="act-vacuum-btn" class="btn btn-primary" ${disabledAttr}>🧹 Vacuum Volumes</button>
          <button id="act-balance-btn" class="btn" ${disabledAttr}>⚖️ Balance Volumes</button>
          <button id="act-fixrep-btn" class="btn" ${disabledAttr}>🔧 Fix Replication</button>
          <button id="act-grow-btn" class="btn" ${disabledAttr}>🌱 Grow Volumes</button>
        </div>
        <div class="toolbar-group">
          <span class="badge ${canAdmin ? 'badge-admin' : 'badge-readonly'}">
            ${canAdmin ? 'Admin Write Enabled' : 'Read-Only Mode'}
          </span>
        </div>
      </div>

      <!-- Collapsible Topology Tree Card -->
      <div class="card" style="margin-bottom: 1.5rem;">
        <div class="card-header">
          <h2 class="card-title">Cluster Physical Topology (${dcs.length} Data Centers)</h2>
          <span style="font-size: 0.8rem; color: var(--text-secondary);">Max Slots: ${topoData.maxVolumeSlots || 0} | Free: ${topoData.freeVolumeSlots || 0}</span>
        </div>
        <div id="topo-tree" style="display: flex; flex-direction: column; gap: 0.75rem;">
          ${dcs.length === 0 ? '<p style="color: var(--text-secondary);">No topology data centers found.</p>' : ''}
          ${dcs.map((dc, dcIdx) => `
            <div class="dc-block" style="border: 1px solid var(--border-color); border-radius: var(--radius-md); padding: 0.75rem; background: var(--bg-surface-elevated);">
              <div class="dc-header" style="cursor: pointer; display: flex; justify-content: space-between; align-items: center; font-weight: 600;" data-toggle="dc-${dcIdx}">
                <span>🏢 DataCenter: ${escapeHtml(dc.Id || dc.Name || 'Default')}</span>
                <span style="font-size: 0.8rem; color: var(--text-secondary);">▼ ${(dc.Racks || []).length} Racks</span>
              </div>
              <div id="dc-${dcIdx}" class="dc-children" style="margin-top: 0.5rem; display: flex; flex-direction: column; gap: 0.5rem;">
                ${(dc.Racks || []).map((rack, rIdx) => `
                  <div class="rack-block" style="border-left: 2px solid var(--color-primary); padding-left: 0.75rem; margin-left: 0.5rem;">
                    <div style="font-size: 0.85rem; font-weight: 500; color: var(--text-secondary); margin-bottom: 0.25rem;">
                      🗄️ Rack: ${escapeHtml(rack.Id || rack.Name || 'Default')} (${(rack.DataNodes || []).length} nodes)
                    </div>
                    <div style="display: flex; flex-wrap: wrap; gap: 0.5rem;">
                      ${(rack.DataNodes || []).map(dn => `
                        <div style="background: var(--bg-surface); border: 1px solid var(--border-color); border-radius: var(--radius-sm); padding: 0.35rem 0.65rem; font-size: 0.8rem; font-family: var(--font-mono);">
                          🖥️ ${escapeHtml(dn.Url || dn.PublicUrl)}
                          <span style="color: var(--text-muted); margin-left: 0.25rem;">[${dn.Volumes || 0}/${dn.Max || 0}]</span>
                        </div>
                      `).join('')}
                    </div>
                  </div>
                `).join('')}
              </div>
            </div>
          `).join('')}
        </div>
      </div>

      <!-- Volumes Table & Filter Controls -->
      <div class="card">
        <div class="card-header">
          <h2 class="card-title">Volumes List (<span id="vol-count">${allVolumes.length}</span>)</h2>
        </div>

        <div class="toolbar" style="margin-top: 0.5rem;">
          <div class="toolbar-group">
            <input type="text" id="filter-collection" placeholder="Filter by collection..." aria-label="Filter collection" style="width: 180px;">
            <select id="filter-readonly" aria-label="Filter ReadOnly">
              <option value="">All Access Modes</option>
              <option value="false">Writable Only</option>
              <option value="true">Read-Only Only</option>
            </select>
            <label style="display: flex; align-items: center; gap: 0.35rem; font-size: 0.85rem; color: var(--text-secondary); cursor: pointer;">
              <input type="checkbox" id="filter-garbage"> High Garbage (&gt;30%)
            </label>
          </div>
        </div>

        <div class="table-container">
          <table class="data-table">
            <thead>
              <tr>
                <th>Volume ID</th>
                <th>Collection</th>
                <th>Size</th>
                <th>Files</th>
                <th>Deleted</th>
                <th>Garbage %</th>
                <th>Replication</th>
                <th>Status</th>
              </tr>
            </thead>
            <tbody id="volumes-tbody">
              <!-- Dynamically populated -->
            </tbody>
          </table>
        </div>
      </div>
    `;

    // Render volume rows with filters
    const tbody = container.querySelector('#volumes-tbody');
    const volCountSpan = container.querySelector('#vol-count');
    const filterCol = container.querySelector('#filter-collection');
    const filterRO = container.querySelector('#filter-readonly');
    const filterGarbage = container.querySelector('#filter-garbage');

    function updateVolumeRows() {
      const colVal = filterCol.value.trim().toLowerCase();
      const roVal = filterRO.value;
      const garbageOnly = filterGarbage.checked;

      const filtered = allVolumes.filter(v => {
        if (colVal && !(v.collection || '').toLowerCase().includes(colVal)) return false;
        if (roVal !== '') {
          const isRO = roVal === 'true';
          if (v.readOnly !== isRO) return false;
        }
        if (garbageOnly && (v.garbageRatio || 0) < 0.30) return false;
        return true;
      });

      volCountSpan.textContent = filtered.length;

      if (filtered.length === 0) {
        tbody.innerHTML = `<tr><td colspan="8" style="text-align: center; color: var(--text-secondary); padding: 2rem;">No volumes match the selected filters.</td></tr>`;
        return;
      }

      tbody.innerHTML = filtered.map(v => {
        const garbage = v.garbageRatio || 0;
        const isHighGarbage = garbage >= 0.30;
        const garbagePct = (garbage * 100).toFixed(1) + '%';
        const rowClass = isHighGarbage ? 'row-high-garbage' : '';

        return `
          <tr class="${rowClass}">
            <td style="font-family: var(--font-mono); font-weight: 600;">#${v.id}</td>
            <td>${escapeHtml(v.collection || 'default')}</td>
            <td>${formatBytes(v.size || 0)}</td>
            <td>${v.fileCount || 0}</td>
            <td>${v.deleteCount || 0}</td>
            <td>
              <span class="badge ${isHighGarbage ? 'badge-warning' : 'badge-source'}">
                ${garbagePct} ${isHighGarbage ? '⚠️' : ''}
              </span>
            </td>
            <td style="font-family: var(--font-mono);">${escapeHtml(v.replicaPlacement || '000')}</td>
            <td>
              ${v.readOnly ? '<span class="badge badge-warning">Read-Only</span>' : '<span class="badge badge-success">Writable</span>'}
            </td>
          </tr>
        `;
      }).join('');
    }

    filterCol.addEventListener('input', updateVolumeRows);
    filterRO.addEventListener('change', updateVolumeRows);
    filterGarbage.addEventListener('change', updateVolumeRows);
    updateVolumeRows();

    // Collapsible hierarchy toggle
    container.querySelectorAll('.dc-header').forEach(header => {
      header.addEventListener('click', () => {
        const targetId = header.getAttribute('data-toggle');
        const el = document.getElementById(targetId);
        if (el) {
          el.style.display = el.style.display === 'none' ? 'flex' : 'none';
        }
      });
    });

    // Wire Maintenance Actions (Gated by canAdmin)
    if (canAdmin) {
      container.querySelector('#act-vacuum-btn').addEventListener('click', () => {
        showConfirmModal({
          title: 'Vacuum Volumes',
          message: 'Execute volume compaction (vacuum) across all eligible volumes with >30% garbage? This frees deleted block space.',
          confirmText: 'Run Vacuum',
          confirmClass: 'btn-primary',
          onConfirm: async () => {
            showToast('Running volume vacuum...', 'info');
            try {
              const res = await apiFetch(`/clusters/${encodeURIComponent(clusterId)}/actions/vacuum`, {
                method: 'POST',
                body: {}
              });
              showToast(`Vacuum completed (${res.durationMs || 0}ms)`, 'success');
              renderTopology(container, clusterId, appState);
            } catch (e) {
              // error toast already shown by apiFetch
            }
          }
        });
      });

      container.querySelector('#act-balance-btn').addEventListener('click', () => {
        showConfirmModal({
          title: 'Balance Volumes',
          message: 'Rebalance volume placement across storage nodes in the cluster? This may transfer volume files between data nodes.',
          confirmText: 'Run Balance',
          confirmClass: 'btn-primary',
          onConfirm: async () => {
            showToast('Running volume balance...', 'info');
            try {
              const res = await apiFetch(`/clusters/${encodeURIComponent(clusterId)}/actions/balance`, {
                method: 'POST',
                body: {}
              });
              showToast(`Balance completed (${res.durationMs || 0}ms)`, 'success');
              renderTopology(container, clusterId, appState);
            } catch (e) {}
          }
        });
      });

      container.querySelector('#act-fixrep-btn').addEventListener('click', () => {
        showConfirmModal({
          title: 'Fix Volume Replication',
          message: 'Inspect and replicate under-replicated volume replicas to meet configured replication policies?',
          confirmText: 'Run Fix Replication',
          confirmClass: 'btn-primary',
          onConfirm: async () => {
            showToast('Fixing replication...', 'info');
            try {
              const res = await apiFetch(`/clusters/${encodeURIComponent(clusterId)}/actions/fix.replication`, {
                method: 'POST',
                body: {}
              });
              showToast(`Fix replication completed (${res.durationMs || 0}ms)`, 'success');
              renderTopology(container, clusterId, appState);
            } catch (e) {}
          }
        });
      });

      container.querySelector('#act-grow-btn').addEventListener('click', () => {
        showConfirmModal({
          title: 'Grow Volumes',
          message: 'Allocate 1 new writable volume slot on the cluster?',
          confirmText: 'Grow Volume',
          confirmClass: 'btn-primary',
          onConfirm: async () => {
            showToast('Growing volume...', 'info');
            try {
              const res = await apiFetch(`/clusters/${encodeURIComponent(clusterId)}/actions/volume.grow`, {
                method: 'POST',
                body: { count: 1 }
              });
              showToast(`Volume grown successfully (${res.durationMs || 0}ms)`, 'success');
              renderTopology(container, clusterId, appState);
            } catch (e) {}
          }
        });
      });
    }

  } catch (err) {
    container.innerHTML = `
      <div class="empty-state">
        <div class="empty-state-icon">⚠️</div>
        <h3>Failed to Load Topology</h3>
        <p>${escapeHtml(err.message)}</p>
      </div>
    `;
  }
}
