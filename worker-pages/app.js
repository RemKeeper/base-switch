const state = {
  apiBase: localStorage.getItem('baseSwitch.apiBase') || '',
  adminKey: localStorage.getItem('baseSwitch.adminKey') || '',
  providers: [],
  checkResults: []
};

const $ = (id) => document.getElementById(id);

function init() {
  $('apiBase').value = state.apiBase;
  $('adminKey').value = state.adminKey;
  $('settingsForm').addEventListener('submit', saveSettings);
  $('refreshAll').addEventListener('click', refreshAll);
  $('newProvider').addEventListener('click', () => openProviderDialog());
  $('providerForm').addEventListener('submit', saveProvider);
  $('deleteProvider').addEventListener('click', deleteProvider);
  $('checkModels').addEventListener('click', checkModels);
  $('loadUsage').addEventListener('click', loadUsage);
  if (state.apiBase && state.adminKey) refreshAll();
}

function saveSettings(event) {
  event.preventDefault();
  state.apiBase = $('apiBase').value.trim().replace(/\/+$/, '');
  state.adminKey = $('adminKey').value.trim();
  localStorage.setItem('baseSwitch.apiBase', state.apiBase);
  localStorage.setItem('baseSwitch.adminKey', state.adminKey);
  refreshAll();
}

async function api(path, options = {}) {
  if (!state.apiBase || !state.adminKey) throw new Error('请先填写 API 地址和管理 API Key');
  const response = await fetch(`${state.apiBase}${path}`, {
    ...options,
    headers: {
      'Content-Type': 'application/json',
      'Authorization': `Bearer ${state.adminKey}`,
      ...(options.headers || {})
    }
  });
  const text = await response.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch { data = { raw: text }; }
  if (!response.ok) {
    throw new Error(data?.error?.message || data?.message || text || `HTTP ${response.status}`);
  }
  return data;
}

async function refreshAll() {
  setStatus('连接中...', '');
  try {
    await Promise.all([loadProviders(), loadUsage(true)]);
    setStatus('已连接', 'ok');
    toast('刷新完成');
  } catch (error) {
    setStatus('连接失败', 'bad');
    toast(error.message);
  }
}

async function loadProviders() {
  const data = await api('/admin/providers');
  state.providers = data.data || [];
  renderProviders();
  renderProviderOptions();
  updateMetrics();
}

function renderProviders() {
  const rows = state.providers.map((provider) => `
    <tr>
      <td><strong>${escapeHtml(provider.name)}</strong><br><small>${escapeHtml(provider.api_key || '')}</small></td>
      <td>${escapeHtml(provider.base_url)}</td>
      <td>${provider.models?.length || 0}</td>
      <td><span class="badge ${provider.enabled ? 'on' : 'off'}">${provider.enabled ? '启用' : '停用'}</span></td>
      <td><button class="ghost" data-edit="${escapeAttr(provider.name)}">编辑</button></td>
    </tr>
  `).join('');
  $('providersTable').innerHTML = rows || '<tr><td colspan="5">暂无 Provider</td></tr>';
  document.querySelectorAll('[data-edit]').forEach((button) => {
    button.addEventListener('click', () => openProviderDialog(state.providers.find((item) => item.name === button.dataset.edit)));
  });
}

function renderProviderOptions() {
  $('checkProvider').innerHTML = '<option value="">全部启用 Provider</option>' + state.providers
    .map((provider) => `<option value="${escapeAttr(provider.name)}">${escapeHtml(provider.name)}</option>`)
    .join('');
}

function updateMetrics() {
  const models = state.providers.reduce((total, provider) => total + (provider.models?.length || 0), 0);
  $('providerCount').textContent = state.providers.length;
  $('modelCount').textContent = models;
  $('aliveCount').textContent = state.checkResults.filter((item) => item.alive).length || '-';
}

function openProviderDialog(provider = null) {
  $('dialogTitle').textContent = provider ? `编辑 ${provider.name}` : '新增 Provider';
  $('editingName').value = provider?.name || '';
  $('providerName').value = provider?.name || '';
  $('providerName').disabled = Boolean(provider);
  $('providerBaseUrl').value = provider?.base_url || '';
  $('providerApiKey').value = provider?.api_key || '';
  $('providerModels').value = (provider?.models || []).join('\n');
  $('providerEnabled').checked = provider?.enabled ?? true;
  $('deleteProvider').hidden = !provider;
  $('providerDialog').showModal();
}

async function saveProvider(event) {
  event.preventDefault();
  const editingName = $('editingName').value;
  const payload = {
    name: $('providerName').value.trim(),
    base_url: $('providerBaseUrl').value.trim(),
    api_key: $('providerApiKey').value.trim(),
    models: lines($('providerModels').value),
    enabled: $('providerEnabled').checked
  };
  try {
    if (editingName) {
      await api(`/admin/providers/${encodeURIComponent(editingName)}`, { method: 'PUT', body: JSON.stringify(payload) });
    } else {
      await api('/admin/providers', { method: 'POST', body: JSON.stringify(payload) });
    }
    $('providerDialog').close();
    await loadProviders();
    toast('Provider 已保存');
  } catch (error) {
    toast(error.message);
  }
}

async function deleteProvider() {
  const name = $('editingName').value;
  if (!name || !confirm(`确认删除 Provider：${name}？`)) return;
  try {
    await api(`/admin/providers/${encodeURIComponent(name)}`, { method: 'DELETE' });
    $('providerDialog').close();
    await loadProviders();
    toast('Provider 已删除');
  } catch (error) {
    toast(error.message);
  }
}

async function checkModels() {
  $('checkModels').disabled = true;
  $('checkResults').innerHTML = '<div class="result-item">检测中...</div>';
  try {
    const data = await api('/admin/models/check', {
      method: 'POST',
      body: JSON.stringify({
        provider: $('checkProvider').value,
        models: lines($('checkModelsInput').value),
        timeout_seconds: Number($('timeoutSeconds').value || 20)
      })
    });
    state.checkResults = data.data || [];
    renderCheckResults();
    updateMetrics();
    toast('模型检测完成');
  } catch (error) {
    $('checkResults').innerHTML = '';
    toast(error.message);
  } finally {
    $('checkModels').disabled = false;
  }
}

function renderCheckResults() {
  $('checkResults').innerHTML = state.checkResults.map((item) => `
    <div class="result-item">
      <div class="top">
        <strong>${escapeHtml(item.model_id)}</strong>
        <span class="badge ${item.alive ? 'on' : 'off'}">${item.alive ? '存活' : '异常'}</span>
      </div>
      <small>HTTP ${item.status_code || '-'} · ${item.latency_ms}ms ${item.error ? `· ${escapeHtml(item.error)}` : ''}</small>
    </div>
  `).join('') || '<div class="result-item">暂无检测结果</div>';
}

async function loadUsage(silent = false) {
  try {
    const data = await api('/admin/usage/summary?group_by=model');
    const items = data.data || [];
    $('usageSummary').innerHTML = items.slice(0, 30).map((item) => `
      <div class="result-item">
        <div class="top"><strong>${escapeHtml(item.provider || '-')}/${escapeHtml(item.model || '*')}</strong><span>${item.request_count} 次</span></div>
        <small>prompt ${item.prompt_tokens} · completion ${item.completion_tokens} · total ${item.total_tokens}</small>
      </div>
    `).join('') || '<div class="result-item">暂无用量数据</div>';
    if (!silent) toast('用量统计已加载');
  } catch (error) {
    if (!silent) toast(error.message);
    throw error;
  }
}

function setStatus(text, kind) {
  $('apiStatus').textContent = text;
  $('apiStatus').className = `status ${kind}`.trim();
}

function lines(value) {
  return value.split('\n').map((item) => item.trim()).filter(Boolean);
}

function toast(message) {
  const node = $('toast');
  node.textContent = message;
  node.hidden = false;
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => node.hidden = true, 3600);
}

function escapeHtml(value) {
  return String(value ?? '').replace(/[&<>"]/g, (char) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[char]));
}

function escapeAttr(value) {
  return escapeHtml(value).replace(/'/g, '&#39;');
}

init();
