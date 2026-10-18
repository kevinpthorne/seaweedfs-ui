// views/shell.js - Safe presets, terminal viewer with duration, & raw shell executor

import { apiFetch, showToast, escapeHtml } from '../api.js';

export async function renderShell(container, clusterId, appState) {
  if (!clusterId) {
    container.innerHTML = `
      <div class="empty-state">
        <div class="empty-state-icon">💻</div>
        <h3>No Cluster Selected</h3>
        <p>Please select a SeaweedFS cluster from the dropdown above.</p>
      </div>
    `;
    return;
  }

  const canRaw = !!(appState.config && appState.config.enableRawShell);

  const presets = [
    { label: 'volume.list', cmd: 'volume.list' },
    { label: 'cluster.status', cmd: 'cluster.status' },
    { label: 's3.bucket.list', cmd: 's3.bucket.list' },
    { label: 's3.configure', cmd: 's3.configure' },
    { label: 'ec.shard.list', cmd: 'ec.shard.list' },
    { label: 'fs.ls /', cmd: 'fs.ls /' }
  ];

  container.innerHTML = `
    <!-- Read-Only Presets Bar -->
    <div class="card" style="margin-bottom: 1.25rem;">
      <div class="card-header">
        <h2 class="card-title">Curated Shell Presets</h2>
        <span class="badge badge-success">Read-Only Safe</span>
      </div>
      <div style="display: flex; gap: 0.5rem; flex-wrap: wrap;">
        ${presets.map(p => `
          <button class="btn btn-sm preset-btn" data-cmd="${escapeHtml(p.cmd)}">
            ⚡ ${escapeHtml(p.label)}
          </button>
        `).join('')}
      </div>
    </div>

    <!-- Raw Command Input (Gated by enableRawShell) -->
    <div class="card" style="margin-bottom: 1.25rem;">
      <div class="card-header">
        <h2 class="card-title">Command Execution Console</h2>
        <span class="badge ${canRaw ? 'badge-admin' : 'badge-source'}">
          ${canRaw ? 'Raw Shell Enabled' : 'Raw Shell Disabled'}
        </span>
      </div>
      <div class="terminal-prompt-bar">
        <input type="text" id="shell-cmd-input" class="form-control"
          placeholder="${canRaw ? 'Enter weed shell command (e.g. volume.list, cluster.status)...' : 'Raw shell disabled by configuration. Use presets above.'}"
          ${canRaw ? '' : 'disabled'}
          style="font-family: var(--font-mono); font-size: 0.9rem;">
        <button id="shell-run-btn" class="btn btn-primary" ${canRaw ? '' : 'disabled'}>
          Run
        </button>
      </div>
    </div>

    <!-- Terminal Output Panel -->
    <div class="terminal-window">
      <div class="terminal-header">
        <div id="term-cmd-display">Ready for command input</div>
        <div style="display: flex; gap: 0.75rem; align-items: center;">
          <span id="term-timing-pill" style="display: none; font-size: 0.75rem; padding: 0.1rem 0.4rem; background: #21262d; border-radius: 4px;">⏱️ 0ms</span>
          <span id="term-exit-badge" class="badge" style="display: none; font-size: 0.65rem;">Exit 0</span>
          <button id="term-copy-btn" class="btn btn-icon btn-sm" title="Copy Output" style="border: none; background: transparent; font-size: 0.8rem; color: #8b949e;">📋 Copy</button>
        </div>
      </div>
      <div id="term-body" class="terminal-body" role="log" aria-live="polite">Click any preset above or type a command to view output here...</div>
    </div>
  `;

  const inputEl = container.querySelector('#shell-cmd-input');
  const runBtn = container.querySelector('#shell-run-btn');
  const bodyEl = container.querySelector('#term-body');
  const cmdDisplay = container.querySelector('#term-cmd-display');
  const timingPill = container.querySelector('#term-timing-pill');
  const exitBadge = container.querySelector('#term-exit-badge');
  const copyBtn = container.querySelector('#term-copy-btn');

  async function executeCommand(cmdStr) {
    if (!cmdStr || !cmdStr.trim()) return;
    cmdStr = cmdStr.trim();

    cmdDisplay.textContent = `$ ${cmdStr}`;
    bodyEl.textContent = 'Executing command via weed shell...';
    timingPill.style.display = 'none';
    exitBadge.style.display = 'none';

    try {
      const res = await apiFetch(`/clusters/${encodeURIComponent(clusterId)}/shell`, {
        method: 'POST',
        body: { command: cmdStr }
      });

      const stdout = res.stdout || '';
      const stderr = res.stderr || '';
      const output = stdout + (stderr ? (stdout ? '\n[STDERR]\n' : '') + stderr : '');
      bodyEl.textContent = output || '(Command completed with no output)';

      timingPill.textContent = `⏱️ ${res.durationMs || 0}ms`;
      timingPill.style.display = 'inline-block';

      exitBadge.textContent = `Exit ${res.exitCode !== undefined ? res.exitCode : 0}`;
      exitBadge.className = `badge ${res.exitCode === 0 || res.exitCode === undefined ? 'badge-success' : 'badge-danger'}`;
      exitBadge.style.display = 'inline-block';
    } catch (err) {
      bodyEl.textContent = `Error executing command: ${err.message}`;
      exitBadge.textContent = 'Failed';
      exitBadge.className = 'badge badge-danger';
      exitBadge.style.display = 'inline-block';
    }
  }

  // Presets
  container.querySelectorAll('.preset-btn').forEach(btn => {
    btn.addEventListener('click', () => {
      const cmd = btn.getAttribute('data-cmd');
      if (inputEl) inputEl.value = cmd;
      executeCommand(cmd);
    });
  });

  // Raw run
  if (canRaw) {
    runBtn.addEventListener('click', () => {
      executeCommand(inputEl.value);
    });
    inputEl.addEventListener('keydown', (e) => {
      if (e.key === 'Enter') {
        executeCommand(inputEl.value);
      }
    });
  }

  // Copy output
  copyBtn.addEventListener('click', () => {
    const text = bodyEl.textContent;
    if (!text) return;
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text);
      showToast('Terminal output copied to clipboard', 'info', 2000);
    }
  });
}
