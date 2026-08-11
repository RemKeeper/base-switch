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
      <td><span class="badge">${escapeHtml(provider.api_type || 'openai')}</span></td>
      <td>${escapeHtml(provider.base_url)}</td>
      <td>${provider.proxy_url ? `<span class="badge on">已配置</span><br><small>${escapeHtml(provider.proxy_url)}</small>` : '<span class="badge off">直连</span>'}</td>
      <td>${provider.models?.length || 0}</td>
      <td><span class="badge ${provider.enabled ? 'on' : 'off'}">${provider.enabled ? '启用' : '停用'}</span></td>
      <td>
        <button class="ghost" data-refresh-models="${escapeAttr(provider.name)}" ${provider.enabled ? '' : 'disabled'}>刷新模型</button>
        <button class="ghost" data-edit="${escapeAttr(provider.name)}">编辑</button>
      </td>
    </tr>
  `).join('');
  $('providersTable').innerHTML = rows || '<tr><td colspan="7">暂无 Provider</td></tr>';
  document.querySelectorAll('[data-edit]').forEach((button) => {
    button.addEventListener('click', () => openProviderDialog(state.providers.find((item) => item.name === button.dataset.edit)));
  });
  document.querySelectorAll('[data-refresh-models]').forEach((button) => {
    button.addEventListener('click', () => refreshProviderModels(button.dataset.refreshModels, button));
  });
}

async function refreshProviderModels(name, button) {
  const originalText = button.textContent;
  button.disabled = true;
  button.textContent = '刷新中...';
  try {
    const data = await api(`/admin/providers/${encodeURIComponent(name)}/refresh-models`, { method: 'POST' });
    await loadProviders();
    toast(`${name} 已刷新，共 ${data.count || 0} 个模型`);
  } catch (error) {
    toast(error.message);
  } finally {
    if (button.isConnected) {
      button.disabled = false;
      button.textContent = originalText;
    }
  }
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
  $('providerApiType').value = provider?.api_type || 'openai';
  $('providerBaseUrl').value = provider?.base_url || '';
  $('providerApiKey').value = '';
  $('providerApiKey').required = !provider;
  $('providerApiKey').placeholder = provider ? '留空表示保留原 API Key' : 'sk-...';
  $('providerProxyUrl').value = provider?.proxy_url || '';
  $('providerModels').value = (provider?.models || []).join('\n');
  $('providerEnabled').checked = provider?.enabled ?? true;
  $('deleteProvider').hidden = !provider;
  $('providerDialog').showModal();
}

async function saveProvider(event) {
  event.preventDefault();
  const editingName = $('editingName').value;
  const apiKey = $('providerApiKey').value.trim();
  const payload = {
    name: $('providerName').value.trim(),
    api_type: $('providerApiType').value,
    base_url: $('providerBaseUrl').value.trim(),
    proxy_url: $('providerProxyUrl').value.trim(),
    models: lines($('providerModels').value),
    enabled: $('providerEnabled').checked
  };
  if (apiKey) payload.api_key = apiKey;
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
  state.checkResults = [];
  try {
    const response = await fetch(`${state.apiBase}/admin/models/check`, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        'Authorization': `Bearer ${state.adminKey}`
      },
      body: JSON.stringify({
        provider: $('checkProvider').value,
        models: lines($('checkModelsInput').value),
        timeout_seconds: Number($('timeoutSeconds').value || 20)
      })
    });
    if (!response.ok) {
      const text = await response.text();
      let data = null;
      try { data = text ? JSON.parse(text) : null; } catch { data = { raw: text }; }
      throw new Error(data?.error?.message || data?.message || text || `HTTP ${response.status}`);
    }
    await readModelCheckStream(response);
    updateMetrics();
    toast('模型检测完成');
  } catch (error) {
    if (!state.checkResults.length) $('checkResults').innerHTML = '';
    toast(error.message);
  } finally {
    $('checkModels').disabled = false;
  }
}

async function readModelCheckStream(response) {
  if (!response.body) throw new Error('当前浏览器不支持流式读取响应');
  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = '';
  while (true) {
    const { value, done } = await reader.read();
    buffer += decoder.decode(value || new Uint8Array(), { stream: !done });
    const lines = buffer.split('\n');
    buffer = lines.pop() || '';
    for (const line of lines) appendModelCheckLine(line);
    if (done) break;
  }
  appendModelCheckLine(buffer);
}

function appendModelCheckLine(line) {
  line = line.trim();
  if (!line) return;
  state.checkResults.push(JSON.parse(line));
  renderCheckResults();
  updateMetrics();
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
