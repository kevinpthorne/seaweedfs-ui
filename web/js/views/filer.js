// views/filer.js - Directory breadcrumbs, file explorer, & metadata inspection drawer

import { apiFetch, showToast, showDrawer, formatBytes, formatDate, escapeHtml } from '../api.js';

let currentBrowsePath = '/';

export async function renderFiler(container, clusterId, appState, targetPath) {
  if (!clusterId) {
    container.innerHTML = `
      <div class="empty-state">
        <div class="empty-state-icon">📁</div>
        <h3>No Cluster Selected</h3>
        <p>Please select a SeaweedFS cluster from the dropdown above.</p>
      </div>
    `;
    return;
  }

  if (targetPath) {
    currentBrowsePath = targetPath;
  }

  container.innerHTML = `
    <div class="view-loading">
      <div class="spinner"></div>
      <p>Loading filer directory ${escapeHtml(currentBrowsePath)}...</p>
    </div>
  `;

  try {
    const data = await apiFetch(`/clusters/${encodeURIComponent(clusterId)}/filer?path=${encodeURIComponent(currentBrowsePath)}&limit=200`);
    const pathVal = data.path || currentBrowsePath;
    const entries = data.entries || [];

    // Parse breadcrumb segments
    const parts = pathVal.split('/').filter(Boolean);
    const breadcrumbs = [
      { name: 'Root (/)', fullPath: '/' }
    ];
    let accum = '';
    for (const part of parts) {
      accum += '/' + part;
      breadcrumbs.push({ name: part, fullPath: accum });
    }

    container.innerHTML = `
      <div class="card">
        <!-- Breadcrumb Navigation -->
        <div class="breadcrumb">
          ${breadcrumbs.map((b, idx) => {
            const isLast = idx === breadcrumbs.length - 1;
            if (isLast) {
              return `<span class="breadcrumb-current">📁 ${escapeHtml(b.name)}</span>`;
            }
            return `
              <a href="#" class="breadcrumb-item filer-nav" data-path="${escapeHtml(b.fullPath)}">${escapeHtml(b.name)}</a>
              <span class="breadcrumb-separator">/</span>
            `;
          }).join('')}
        </div>

        <!-- Directory Entries Table -->
        <div class="table-container" style="margin-bottom: 0;">
          <table class="data-table">
            <thead>
              <tr>
                <th style="width: 40px;"></th>
                <th>Name</th>
                <th>Size</th>
                <th>Mode</th>
                <th>Modified</th>
                <th>MIME Type</th>
                <th>Chunks</th>
                <th style="width: 140px; text-align: right;">Metadata</th>
              </tr>
            </thead>
            <tbody>
              ${pathVal !== '/' ? `
                <tr>
                  <td>📁</td>
                  <td colspan="7">
                    <a href="#" class="filer-nav-up" style="color: var(--color-primary); text-decoration: none; font-weight: 500;">
                      .. (Parent Directory)
                    </a>
                  </td>
                </tr>
              ` : ''}

              ${entries.length === 0 ? `
                <tr>
                  <td colspan="8" style="text-align: center; color: var(--text-secondary); padding: 2rem;">
                    Directory is empty.
                  </td>
                </tr>
              ` : entries.map(e => {
                const isDir = !!e.isDir;
                const icon = isDir ? '📁' : '📄';
                const fullEntryPath = pathVal === '/' ? `/${e.name}` : `${pathVal}/${e.name}`;

                return `
                  <tr>
                    <td>${icon}</td>
                    <td>
                      ${isDir ? `
                        <a href="#" class="filer-nav" data-path="${escapeHtml(fullEntryPath)}" style="color: var(--color-primary); font-weight: 600; text-decoration: none;">
                          ${escapeHtml(e.name)}
                        </a>
                      ` : `
                        <span style="font-family: var(--font-mono); font-size: 0.85rem;">
                          ${escapeHtml(e.name)}
                        </span>
                      `}
                    </td>
                    <td>${isDir ? '-' : formatBytes(e.size || 0)}</td>
                    <td style="font-family: var(--font-mono); font-size: 0.8rem; color: var(--text-secondary);">
                      ${e.mode ? '0' + (e.mode & 0o777).toString(8) : '-'}
                    </td>
                    <td style="font-size: 0.8rem; color: var(--text-secondary);">${formatDate(e.mtime)}</td>
                    <td style="font-size: 0.8rem; color: var(--text-muted);">${escapeHtml(e.mime || (isDir ? 'directory' : 'binary'))}</td>
                    <td style="font-family: var(--font-mono);">${e.chunks !== undefined ? e.chunks : '-'}</td>
                    <td style="text-align: right;">
                      <button class="btn btn-sm inspect-meta-btn" data-path="${escapeHtml(fullEntryPath)}">
                        Inspect
                      </button>
                    </td>
                  </tr>
                `;
              }).join('')}
            </tbody>
          </table>
        </div>
      </div>
    `;

    // Wire breadcrumbs and directory click navigation
    container.querySelectorAll('.filer-nav').forEach(link => {
      link.addEventListener('click', (e) => {
        e.preventDefault();
        const p = link.getAttribute('data-path');
        renderFiler(container, clusterId, appState, p);
      });
    });

    const upLink = container.querySelector('.filer-nav-up');
    if (upLink) {
      upLink.addEventListener('click', (e) => {
        e.preventDefault();
        const parentParts = pathVal.split('/').filter(Boolean);
        parentParts.pop();
        const parentPath = '/' + parentParts.join('/');
        renderFiler(container, clusterId, appState, parentPath);
      });
    }

    // Wire Metadata Inspection Drawer
    container.querySelectorAll('.inspect-meta-btn').forEach(btn => {
      btn.addEventListener('click', async () => {
        const filePath = btn.getAttribute('data-path');
        showToast(`Fetching metadata for ${filePath}...`, 'info');
        try {
          const res = await apiFetch(`/clusters/${encodeURIComponent(clusterId)}/filer/meta?path=${encodeURIComponent(filePath)}`);
          showDrawer({
            title: `Metadata: ${filePath}`,
            contentHtml: `
              <div style="margin-bottom: 1rem;">
                <div class="stat-label">Full Path</div>
                <div style="font-family: var(--font-mono); font-size: 0.85rem; word-break: break-all;">
                  ${escapeHtml(res.path || filePath)}
                </div>
              </div>
              <div class="stat-label" style="margin-bottom: 0.35rem;">Raw Metadata Output (fs.meta.cat)</div>
              <pre style="background: #0b0e14; color: #e6edf3; padding: 1rem; border-radius: var(--radius-md); font-family: var(--font-mono); font-size: 0.8rem; overflow-x: auto; white-space: pre-wrap; word-break: break-all;">${escapeHtml(res.metadata || 'No metadata returned')}</pre>
            `
          });
        } catch (e) {}
      });
    });

  } catch (err) {
    container.innerHTML = `
      <div class="empty-state">
        <div class="empty-state-icon">⚠️</div>
        <h3>Failed to Browse Filer</h3>
        <p>${escapeHtml(err.message)}</p>
      </div>
    `;
  }
}
