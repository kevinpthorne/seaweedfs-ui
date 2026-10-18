// api.js - REST client, toasts, modals, and formatting utilities

/**
 * Escapes HTML characters to prevent XSS injection.
 */
export function escapeHtml(str) {
  if (str === null || str === undefined) return '';
  return String(str)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#039;');
}

/**
 * Formats raw bytes into human-readable B, KB, MB, GB, TB.
 */
export function formatBytes(bytes, decimals = 2) {
  if (bytes === 0 || bytes === '0') return '0 B';
  const k = 1024;
  const dm = decimals < 0 ? 0 : decimals;
  const sizes = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'];
  const num = Number(bytes);
  if (isNaN(num) || num < 0) return '0 B';
  const i = Math.floor(Math.log(num) / Math.log(k));
  if (i >= sizes.length) return (num / Math.pow(k, sizes.length - 1)).toFixed(dm) + ' ' + sizes[sizes.length - 1];
  return parseFloat((num / Math.pow(k, i)).toFixed(dm)) + ' ' + sizes[i];
}

/**
 * Formats replication lag seconds into human-readable representation.
 */
export function formatLag(seconds) {
  if (seconds === null || seconds === undefined || isNaN(seconds)) return 'N/A';
  const sec = Number(seconds);
  if (sec < 0) return '0s';
  if (sec < 60) return `${sec.toFixed(1)}s`;
  if (sec < 3600) return `${Math.floor(sec / 60)}m ${Math.floor(sec % 60)}s`;
  return `${(sec / 3600).toFixed(1)}h`;
}

/**
 * Formats a timestamp (ISO string or unix seconds) into a readable date string.
 */
export function formatDate(val) {
  if (!val) return 'N/A';
  let date;
  if (typeof val === 'number') {
    date = new Date(val * 1000);
  } else {
    date = new Date(val);
  }
  if (isNaN(date.getTime())) return String(val);
  return date.toLocaleString();
}

/**
 * Shows a toast alert notification.
 */
export function showToast(message, type = 'info', duration = 4000) {
  const container = document.getElementById('toast-container');
  if (!container) return;

  const toast = document.createElement('div');
  toast.className = `toast toast-${type}`;
  toast.setAttribute('role', 'alert');

  let icon = 'ℹ️';
  if (type === 'success') icon = '✅';
  if (type === 'warning') icon = '⚠️';
  if (type === 'error') icon = '❌';

  toast.innerHTML = `
    <span class="toast-icon">${icon}</span>
    <span class="toast-message">${escapeHtml(message)}</span>
    <button class="btn btn-icon btn-sm toast-close" aria-label="Close" style="margin-left:auto; border:none; background:transparent;">×</button>
  `;

  const closeBtn = toast.querySelector('.toast-close');
  const removeToast = () => {
    toast.style.opacity = '0';
    toast.style.transform = 'translateX(100%)';
    toast.style.transition = 'all 0.2s ease';
    setTimeout(() => toast.remove(), 200);
  };

  closeBtn.addEventListener('click', removeToast);
  container.appendChild(toast);

  if (duration > 0) {
    setTimeout(removeToast, duration);
  }
}

/**
 * Shows a confirmation modal dialog.
 */
export function showConfirmModal({ title, message, confirmText = 'Confirm', confirmClass = 'btn-primary', onConfirm }) {
  const container = document.getElementById('modal-container');
  if (!container) return;

  const backdrop = document.createElement('div');
  backdrop.className = 'modal-backdrop';

  backdrop.innerHTML = `
    <div class="modal-dialog" role="dialog" aria-modal="true" aria-labelledby="modal-title">
      <div class="modal-header">
        <h3 id="modal-title" class="modal-title">${escapeHtml(title)}</h3>
        <button class="btn btn-icon btn-sm modal-close" aria-label="Close">✕</button>
      </div>
      <div class="modal-body">
        <p>${escapeHtml(message)}</p>
      </div>
      <div class="modal-footer">
        <button class="btn modal-cancel">Cancel</button>
        <button class="btn ${confirmClass} modal-confirm">${escapeHtml(confirmText)}</button>
      </div>
    </div>
  `;

  const closeModal = () => backdrop.remove();

  backdrop.querySelector('.modal-close').addEventListener('click', closeModal);
  backdrop.querySelector('.modal-cancel').addEventListener('click', closeModal);
  backdrop.addEventListener('click', (e) => {
    if (e.target === backdrop) closeModal();
  });

  backdrop.querySelector('.modal-confirm').addEventListener('click', async () => {
    closeModal();
    if (typeof onConfirm === 'function') {
      await onConfirm();
    }
  });

  container.appendChild(backdrop);
}

/**
 * Shows a slide-out drawer on the right side.
 */
export function showDrawer({ title, contentHtml }) {
  const container = document.getElementById('drawer-container');
  if (!container) return;

  const backdrop = document.createElement('div');
  backdrop.className = 'drawer-backdrop';

  backdrop.innerHTML = `
    <div class="drawer" role="dialog" aria-modal="true">
      <div class="drawer-header">
        <h3 class="modal-title">${escapeHtml(title)}</h3>
        <button class="btn btn-icon btn-sm drawer-close" aria-label="Close">✕</button>
      </div>
      <div class="drawer-body">
        ${contentHtml}
      </div>
    </div>
  `;

  const closeDrawer = () => backdrop.remove();

  backdrop.querySelector('.drawer-close').addEventListener('click', closeDrawer);
  backdrop.addEventListener('click', (e) => {
    if (e.target === backdrop) closeDrawer();
  });

  container.appendChild(backdrop);
}

/**
 * Universal API fetch wrapper with JSON parsing and guardrail alerting.
 */
export async function apiFetch(endpoint, options = {}) {
  let url = endpoint;
  if (!url.startsWith('/api/v1')) {
    if (!url.startsWith('/')) url = '/' + url;
    url = '/api/v1' + url;
  }

  const headers = new Headers(options.headers || {});
  headers.set('Accept', 'application/json');

  if (options.body && typeof options.body === 'object' && !(options.body instanceof FormData)) {
    headers.set('Content-Type', 'application/json');
    options.body = JSON.stringify(options.body);
  }

  // Generate unique X-Request-ID
  let reqId;
  if (typeof crypto !== 'undefined' && crypto.randomUUID) {
    reqId = crypto.randomUUID();
  } else {
    reqId = 'req-' + Math.random().toString(36).substring(2, 10);
  }
  headers.set('X-Request-ID', reqId);

  options.headers = headers;

  try {
    const res = await fetch(url, options);

    if (res.status === 204) {
      return null;
    }

    let data;
    const text = await res.text();
    try {
      data = text ? JSON.parse(text) : {};
    } catch (e) {
      data = { error: text || 'Invalid JSON response from server' };
    }

    if (!res.ok) {
      if (res.status === 403) {
        if (data.code === 'ADMIN_ACTIONS_DISABLED') {
          showToast('Action Forbidden: Admin actions are disabled in server configuration (allowAdminActions=false)', 'warning', 6000);
        } else if (data.code === 'RAW_SHELL_DISABLED') {
          showToast('Raw Shell Forbidden: Raw shell execution is disabled in server configuration (enableRawShell=false)', 'warning', 6000);
        } else {
          showToast(data.error || 'Access Forbidden (403)', 'error');
        }
      } else {
        showToast(data.error || `Request failed with status ${res.status}`, 'error');
      }

      const err = new Error(data.error || `HTTP ${res.status}`);
      err.status = res.status;
      err.code = data.code;
      err.details = data.details;
      throw err;
    }

    return data;
  } catch (err) {
    if (err.name === 'AbortError') {
      throw err;
    }
    if (!err.status) {
      showToast(err.message || 'Network connection failed', 'error');
    }
    throw err;
  }
}
