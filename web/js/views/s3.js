// views/s3.js - S3 buckets, creation/deletion, & IAM users/policies

import { apiFetch, showToast, showConfirmModal, escapeHtml } from '../api.js';

export async function renderS3(container, clusterId, appState) {
  if (!clusterId) {
    container.innerHTML = `
      <div class="empty-state">
        <div class="empty-state-icon">🪣</div>
        <h3>No Cluster Selected</h3>
        <p>Please select a SeaweedFS cluster from the dropdown above.</p>
      </div>
    `;
    return;
  }

  container.innerHTML = `
    <div class="view-loading">
      <div class="spinner"></div>
      <p>Loading S3 buckets and IAM configuration...</p>
    </div>
  `;

  try {
    const [bucketData, usersData] = await Promise.all([
      apiFetch(`/clusters/${encodeURIComponent(clusterId)}/s3/buckets`),
      apiFetch(`/clusters/${encodeURIComponent(clusterId)}/s3/users`)
    ]);

    const buckets = bucketData.buckets || [];
    const users = usersData.users || [];
    const canAdmin = !!(appState.config && appState.config.allowAdminActions);
    const disabledAttr = canAdmin ? '' : 'disabled title="Admin actions are disabled in server configuration"';

    container.innerHTML = `
      <!-- S3 Buckets Card -->
      <div class="card" style="margin-bottom: 1.5rem;">
        <div class="card-header">
          <h2 class="card-title">S3 Buckets (${buckets.length})</h2>
          <button id="create-bucket-btn" class="btn btn-primary" ${disabledAttr}>+ Create Bucket</button>
        </div>

        <div class="table-container" style="margin-bottom: 0;">
          <table class="data-table">
            <thead>
              <tr>
                <th>Bucket Name</th>
                <th style="width: 120px; text-align: right;">Actions</th>
              </tr>
            </thead>
            <tbody>
              ${buckets.length === 0 ? `
                <tr>
                  <td colspan="2" style="text-align: center; color: var(--text-secondary); padding: 2rem;">
                    No S3 buckets found on this cluster.
                  </td>
                </tr>
              ` : buckets.map(b => {
                const name = typeof b === 'string' ? b : (b.name || '');
                return `
                  <tr>
                    <td style="font-family: var(--font-mono); font-weight: 600;">
                      🪣 ${escapeHtml(name)}
                    </td>
                    <td style="text-align: right;">
                      <button class="btn btn-danger btn-sm delete-bucket-btn" data-bucket="${escapeHtml(name)}" ${disabledAttr}>
                        Delete
                      </button>
                    </td>
                  </tr>
                `;
              }).join('')}
            </tbody>
          </table>
        </div>
      </div>

      <!-- S3 IAM Users & Policies Card -->
      <div class="card">
        <div class="card-header">
          <h2 class="card-title">S3 IAM Users &amp; Policies (${users.length})</h2>
        </div>

        <div class="table-container" style="margin-bottom: 0;">
          <table class="data-table">
            <thead>
              <tr>
                <th>Username</th>
                <th>Allowed Actions</th>
                <th>Credentials Count</th>
              </tr>
            </thead>
            <tbody>
              ${users.length === 0 ? `
                <tr>
                  <td colspan="3" style="text-align: center; color: var(--text-secondary); padding: 2rem;">
                    No S3 IAM users configured (all S3 buckets operate with default access).
                  </td>
                </tr>
              ` : users.map(u => `
                <tr>
                  <td style="font-family: var(--font-mono); font-weight: 600;">
                    👤 ${escapeHtml(u.username || u.name)}
                  </td>
                  <td>
                    <div style="display: flex; gap: 0.35rem; flex-wrap: wrap;">
                      ${(u.actions || []).map(a => `
                        <span class="badge badge-source" style="font-size: 0.7rem;">${escapeHtml(a)}</span>
                      `).join('')}
                    </div>
                  </td>
                  <td style="font-family: var(--font-mono); color: var(--text-secondary);">
                    ${(u.credentials || []).length} key(s)
                  </td>
                </tr>
              `).join('')}
            </tbody>
          </table>
        </div>
      </div>
    `;

    // Wire Create Bucket
    if (canAdmin) {
      container.querySelector('#create-bucket-btn').addEventListener('click', () => {
        const modalContainer = document.getElementById('modal-container');
        const backdrop = document.createElement('div');
        backdrop.className = 'modal-backdrop';
        backdrop.innerHTML = `
          <div class="modal-dialog" role="dialog" aria-modal="true">
            <div class="modal-header">
              <h3 class="modal-title">Create S3 Bucket</h3>
              <button class="btn btn-icon btn-sm modal-close">✕</button>
            </div>
            <div class="modal-body">
              <div class="form-group">
                <label for="new-bucket-name" class="form-label">Bucket Name</label>
                <input type="text" id="new-bucket-name" class="form-control" placeholder="my-data-bucket" pattern="^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$" autofocus>
                <small style="color: var(--text-muted); font-size: 0.75rem;">Only lowercase letters, numbers, hyphens and dots (3-63 chars).</small>
              </div>
            </div>
            <div class="modal-footer">
              <button class="btn modal-cancel">Cancel</button>
              <button id="modal-submit-create" class="btn btn-primary">Create Bucket</button>
            </div>
          </div>
        `;

        const closeModal = () => backdrop.remove();
        backdrop.querySelector('.modal-close').addEventListener('click', closeModal);
        backdrop.querySelector('.modal-cancel').addEventListener('click', closeModal);

        backdrop.querySelector('#modal-submit-create').addEventListener('click', async () => {
          const input = backdrop.querySelector('#new-bucket-name');
          const name = input.value.trim().toLowerCase();
          if (!name) {
            showToast('Bucket name is required', 'error');
            return;
          }
          closeModal();
          showToast(`Creating bucket ${name}...`, 'info');
          try {
            await apiFetch(`/clusters/${encodeURIComponent(clusterId)}/s3/buckets`, {
              method: 'POST',
              body: { name }
            });
            showToast(`Bucket ${name} created successfully!`, 'success');
            renderS3(container, clusterId, appState);
          } catch (e) {}
        });

        modalContainer.appendChild(backdrop);
      });

      // Wire Delete Bucket
      container.querySelectorAll('.delete-bucket-btn').forEach(btn => {
        btn.addEventListener('click', () => {
          const bucket = btn.getAttribute('data-bucket');
          showConfirmModal({
            title: `Delete Bucket "${bucket}"`,
            message: `Are you sure you want to delete S3 bucket "${bucket}"? All files in this bucket may become permanently inaccessible.`,
            confirmText: 'Delete Bucket',
            confirmClass: 'btn-danger',
            onConfirm: async () => {
              showToast(`Deleting bucket ${bucket}...`, 'info');
              try {
                await apiFetch(`/clusters/${encodeURIComponent(clusterId)}/s3/buckets/${encodeURIComponent(bucket)}`, {
                  method: 'DELETE'
                });
                showToast(`Bucket ${bucket} deleted`, 'success');
                renderS3(container, clusterId, appState);
              } catch (e) {}
            }
          });
        });
      });
    }

  } catch (err) {
    container.innerHTML = `
      <div class="empty-state">
        <div class="empty-state-icon">⚠️</div>
        <h3>Failed to Load S3 Details</h3>
        <p>${escapeHtml(err.message)}</p>
      </div>
    `;
  }
}
