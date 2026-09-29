/* ZCode Proxy 管理界面 */
'use strict';

// ---- 基础设施 ----

async function api(path, opts = {}) {
  const init = {
    method: opts.method || 'GET',
    headers: { 'Content-Type': 'application/json' },
    credentials: 'same-origin',
  };
  if (opts.body !== undefined) init.body = JSON.stringify(opts.body);
  const resp = await fetch(path, init);
  let data = null;
  try { data = await resp.json(); } catch (e) { /* empty */ }
  if (resp.status === 401 && !path.startsWith('/api/login')) {
    showLogin();
    throw new Error((data && data.error) || '未登录或会话过期');
  }
  if (!resp.ok) {
    const msg = (data && (data.error?.message || data.error || data.message)) || ('HTTP ' + resp.status);
    throw new Error(typeof msg === 'string' ? msg : JSON.stringify(msg));
  }
  return data;
}

let toastTimer = null;
function toast(msg, type = 'success') {
  const el = document.getElementById('toast');
  el.textContent = msg;
  el.className = 'toast toast-' + type + ' show';
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => el.classList.remove('show'), 3200);
}

function openModal(html) {
  document.getElementById('modalBox').innerHTML = html;
  document.getElementById('modalOverlay').classList.add('show');
}
function closeModal() {
  document.getElementById('modalOverlay').classList.remove('show');
  // 关闭弹窗时清理 OAuth 轮询，避免遮罩关闭后定时器泄漏
  if (typeof cancelOAuth === 'function' && window._oauthState) cancelOAuth();
}

function esc(s) {
  if (s === null || s === undefined) return '';
  return String(s).replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}

function fmtNum(n) {
  n = Number(n || 0);
  if (n >= 1e8) return (Math.round(n / 1e7) / 10) + '亿';
  if (n >= 1e4) return (Math.round(n / 1e3) / 10) + '万';
  return String(Math.round(n));
}

function fmtEpoch(sec) {
  if (!sec) return '-';
  const d = new Date(sec * 1000);
  return d.toLocaleString('zh-CN', { hour12: false });
}

function fmtAgo(sec) {
  if (!sec) return '从未';
  const diff = Math.floor(Date.now() / 1000) - sec;
  if (diff < 60) return diff + '秒前';
  if (diff < 3600) return Math.floor(diff / 60) + '分钟前';
  if (diff < 86400) return Math.floor(diff / 3600) + '小时前';
  return Math.floor(diff / 86400) + '天前';
}

const STATUS_META = {
  active:    ['badge-success', '正常'],
  cooling:   ['badge-warning', '冷却中'],
  exhausted: ['badge-danger', '额度耗尽'],
  invalid:   ['badge-danger', '凭证失效'],
  inactive:  ['badge-info', '未激活'],
  disabled:  ['badge-secondary', '已禁用'],
};
function statusBadge(st) {
  const m = STATUS_META[st] || ['badge-secondary', esc(st || '-')];
  return `<span class="badge ${m[0]}">${m[1]}</span>`;
}

// ---- 登录 ----

function showLogin() {
  document.getElementById('loginPage').style.display = 'flex';
  document.getElementById('mainApp').style.display = 'none';
}
function showApp(username) {
  document.getElementById('loginPage').style.display = 'none';
  document.getElementById('mainApp').style.display = 'block';
  document.getElementById('welcomeUser').textContent = username || 'admin';
  document.getElementById('userAvatar').textContent = (username || 'A')[0].toUpperCase();
  loadAll();
}

async function doLogin() {
  const username = document.getElementById('loginUser').value.trim() || 'admin';
  const password = document.getElementById('loginPass').value;
  const errEl = document.getElementById('loginError');
  errEl.textContent = '';
  errEl.style.display = 'none';
  const btn = document.querySelector('#loginPage .btn-primary');
  if (btn) btn.disabled = true;
  try {
    const data = await api('/api/login', { method: 'POST', body: { username, password } });
    showApp(data.username);
  } catch (e) {
    const msg = e.message || '登录失败';
    errEl.textContent = msg;
    errEl.style.display = 'block';   // 关键：显示错误框（CSS 默认 display:none）
    toast(msg, 'error');             // 同时弹 toast，确保可见
  } finally {
    if (btn) btn.disabled = false;
  }
}

async function doLogout() {
  try { await api('/api/logout', { method: 'POST' }); } catch (e) { /* ignore */ }
  showLogin();
}

async function checkAuth() {
  try {
    await api('/api/auth/check');
    showApp('admin');
  } catch (e) {
    showLogin();
  }
}

document.getElementById('loginPass').addEventListener('keydown', e => { if (e.key === 'Enter') doLogin(); });

// ---- 导航 ----

document.querySelectorAll('.pill-nav .nav-link[data-section]').forEach(link => {
  link.addEventListener('click', () => switchSection(link.dataset.section));
});

function switchSection(name) {
  document.querySelectorAll('.pill-nav .nav-link').forEach(l => l.classList.toggle('active', l.dataset.section === name));
  document.querySelectorAll('.section').forEach(s => s.classList.toggle('active', s.id === 'section-' + name));
  if (name === 'dashboard') loadDashboard();
  if (name === 'accounts') { loadGroups(); loadAccounts(); }
  if (name === 'activity') { loadPlans(); loadClaimRecords(); loadPlanRuns(); }
  if (name === 'usage') { loadUsageStats(); loadUsageRecords(); }
  if (name === 'llmtest') { loadLlmKey(); }
  if (name === 'settings') loadSettings();
}

function navMoreDo(btn, fn) {
  btn.closest('.nav-more').classList.remove('open');
  fn();
}

function switchSettingsTab(tab) {
  document.querySelectorAll('.sub-tab').forEach(t => t.classList.toggle('active', t.dataset.stab === tab));
  ['security', 'strategy', 'proxy', 'tls', 'captcha', 'models'].forEach(t => {
    const el = document.getElementById('stab-' + t);
    if (el) el.style.display = t === tab ? '' : 'none';
  });
}

// ---- 仪表盘 ----

async function loadDashboard() {
  try {
    const d = await api('/api/dashboard');
    document.getElementById('statTotal').textContent = d.account_total ?? '-';
    document.getElementById('statSelectable').textContent = d.account_selectable ?? '-';
    document.getElementById('statRemaining').textContent = fmtNum(d.total_remaining);
    const u = d.usage_7d || {};
    document.getElementById('statRequests').textContent = fmtNum(u.requests);
    document.getElementById('statTokens').textContent = fmtNum(u.total_tokens);
    document.getElementById('statTtft').textContent = (u.avg_ttft_ms || 0) + 'ms';

    const cap = d.captcha || {};
    document.getElementById('dashStatus').innerHTML = `
      <div class="kv-grid">
        <div class="kv-item"><div class="k">客户端版本伪装</div><div class="v">ZCode/${esc(d.app_version)}</div></div>
        <div class="kv-item"><div class="k">验证码参数</div><div class="v">${cap.has_param ? (cap.fresh ? '✅ 新鲜' : '⏳ 宽限期(' + cap.param_age_s + 's)') : '— 未求解'}</div></div>
        <div class="kv-item"><div class="k">验证码模式</div><div class="v">${cap.manual_mode ? '有头手动' : '无头自动'}</div></div>
        <div class="kv-item"><div class="k">验证码配置</div><div class="v">${cap.config ? (cap.config.enabled ? '上游已启用 scene=' + esc(cap.config.scene_id) : '上游未启用') : '未获取'}</div></div>
      </div>`;

    const sc = d.status_count || {};
    const gc = d.group_count || {};
    document.getElementById('dashAccounts').innerHTML = `
      <div style="display:flex;gap:8px;flex-wrap:wrap;margin-bottom:14px">
        ${Object.entries(sc).map(([k, v]) => statusBadge(k) + ' <b style="margin-right:10px">' + v + '</b>').join('')}
      </div>
      <div style="display:flex;gap:8px;flex-wrap:wrap">
        ${Object.entries(gc).map(([k, v]) => `<span class="pill-group">${esc(k)} · ${v}</span>`).join('')}
      </div>`;
  } catch (e) { /* 未登录时静默 */ }
}

// ---- 分组 ----

let groupCache = [];
async function loadGroups() {
  try {
    const d = await api('/api/groups');
    groupCache = d.groups || [];
    const sel = document.getElementById('groupFilter');
    const cur = sel.value;
    sel.innerHTML = '<option value="">全部分组</option>' + groupCache.map(g => `<option value="${esc(g)}">${esc(g)}</option>`).join('');
    sel.value = cur;
  } catch (e) { /* ignore */ }
}

function groupOptions(selected) {
  return '<option value="">未分组</option>' + groupCache.map(g =>
    `<option value="${esc(g)}" ${g === selected ? 'selected' : ''}>${esc(g)}</option>`).join('') +
    '<option value="__new__">＋ 新分组…</option>';
}

// ---- 账号 ----

let accountsCache = [];

async function loadAccounts() {
  const group = document.getElementById('groupFilter').value;
  try {
    const d = await api('/api/accounts' + (group ? '?group=' + encodeURIComponent(group) : ''));
    accountsCache = d.accounts || [];
    renderAccounts();
  } catch (e) { toast(e.message, 'error'); }
}

function quotaCell(a) {
  if (!a.plan_tier && !a.total_units) return '<span style="color:var(--c-text-lighter)">未刷新</span>';
  const pct = a.total_units > 0 ? Math.min(100, a.used_units / a.total_units * 100) : 0;
  return `<div class="quota-bar"><div class="progress-track" style="flex:1"><div class="progress-fill" style="width:${pct}%"></div></div>
    <span class="quota-num">${fmtNum(a.remaining)} / ${fmtNum(a.total_units)}</span></div>
    <div style="font-size:11px;color:var(--c-text-lighter);margin-top:3px">${esc(a.plan_tier || '')}${a.plan_expire ? ' · ' + esc(a.plan_expire) + ' 到期' : ''}</div>`;
}

// showQuotaModal 点开额度 → 弹窗展示具体套餐与额度构成
function showQuotaModal(id) {
  const a = (accountsCache || []).find(x => x.id === id);
  if (!a) return;
  const q = a.quota;
  const fmtT = (s) => s ? new Date(s * 1000).toLocaleString('zh-CN', { hour12: false }) : '-';
  const bar = (pct) => `<div class="progress-track" style="height:6px"><div class="progress-fill" style="width:${Math.min(100, pct || 0)}%"></div></div>`;
  let body;
  if (!q || (!q.plans || !q.plans.length) && (!q.items || !q.items.length)) {
    body = `<div class="empty"><p>暂无额度数据（${esc(a.status)}）</p></div>
      <div class="hint" style="margin-top:8px">${q && q.auth_failed ? '凭证鉴权失败（401/403），请重新登录该账号。' : q && q.not_entitled ? '该账号无 Coding Plan / 未激活。' : '点击账号行「刷新」拉取额度后再查看。'}</div>`;
  } else {
    const slots = (q.plans && q.plans.length) ? q.plans : [{ plan_id: '-', name: q.plan_tier || '套餐', tier: q.plan_tier || '', status: a.status, expire: q.plan_expire, total: q.total, used: q.used, remaining: q.remaining, percent_used: q.percent_used, items: q.items || [] }];
    body = `
      <div class="kv-grid" style="margin-bottom:14px">
        <div class="kv-item"><div class="k">套餐档位</div><div class="v">${esc(q.plan_tier || '-')}</div></div>
        <div class="kv-item"><div class="k">到期时间</div><div class="v">${esc(q.plan_expire || '-')}</div></div>
        <div class="kv-item"><div class="k">总 / 已用 / 剩余</div><div class="v">${fmtNum(q.total)} / ${fmtNum(q.used)} / ${fmtNum(q.remaining)}</div></div>
        <div class="kv-item"><div class="k">使用占比</div><div class="v">${(q.percent_used || 0).toFixed(1)}%</div></div>
        <div class="kv-item"><div class="k">数据源</div><div class="v">${esc(q.source || '-')}</div></div>
        <div class="kv-item"><div class="k">刷新时间</div><div class="v">${fmtT(q.refreshed_at)}</div></div>
      </div>
      ${slots.map(s => `
        <div style="border:1px solid var(--c-border);border-radius:10px;padding:12px 14px;margin-bottom:12px">
          <div style="display:flex;justify-content:space-between;align-items:center;flex-wrap:wrap;gap:6px;margin-bottom:8px">
            <div style="font-weight:700">${esc(s.name || s.plan_id)} <span class="badge badge-purple">${esc(s.tier || '-')}</span> <span class="badge ${String(s.status).toLowerCase() === 'active' ? 'badge-success' : 'badge-secondary'}">${esc(s.status || '-')}</span></div>
            <div style="font-size:12px;color:var(--c-text-light)">${s.expire ? '到期 ' + esc(s.expire) : ''}</div>
          </div>
          ${bar(s.percent_used)}
          <div style="font-size:12px;color:var(--c-text-light);margin:6px 0 8px">总 ${fmtNum(s.total)} · 已用 ${fmtNum(s.used)} · 剩余 ${fmtNum(s.remaining)}（${(s.percent_used || 0).toFixed(1)}%）</div>
          ${(s.items && s.items.length) ? `<div class="table-wrap"><table>
            <thead><tr><th>模型 / 权益</th><th>总额</th><th>已用</th><th>剩余</th><th>占比</th><th>周期 / 到期</th></tr></thead>
            <tbody>${s.items.map(it => `<tr>
              <td>${esc(it.name)}</td>
              <td>${fmtNum(it.total)}</td>
              <td>${fmtNum(it.used)}</td>
              <td><b>${fmtNum(it.remaining)}</b></td>
              <td>${(it.percent_used || 0).toFixed(1)}%</td>
              <td style="font-size:11.5px;color:var(--c-text-light)">${esc(it.period_end || '-')}</td>
            </tr>`).join('')}</tbody></table></div>` : '<div class="hint">该套餐无明细额度项</div>'}
        </div>`).join('')}
    `;
  }
  openModal(`<h3>套餐与额度构成 · ${esc(a.display_name || a.email || ('#' + id))}</h3>
    ${body}
    <div class="actions">
      <button class="btn btn-secondary" onclick="refreshQuota(${id});closeModal()">刷新额度</button>
      <button class="btn btn-secondary" onclick="closeModal()">关闭</button>
    </div>`);
}

function renderAccounts() {
  const el = document.getElementById('accountsTable');
  if (!accountsCache.length) {
    el.innerHTML = '<div class="empty"><p>暂无账号，点击「导入本地账号」或「OAuth 登录」开始</p></div>';
    return;
  }
  el.innerHTML = `<div class="table-wrap"><table>
    <thead><tr>
      <th>账号</th><th>分组</th><th>认证</th><th>状态</th><th>套餐 / 剩余额度</th>
      <th>设备指纹</th><th>使用/失败</th><th>最近活动</th><th style="width:200px">操作</th>
    </tr></thead>
    <tbody>${accountsCache.map(a => `
      <tr>
        <td><div style="font-weight:700">${esc(a.display_name || a.email || a.user_id)}</div>
            <div style="font-size:11px;color:var(--c-text-lighter)">${esc(a.email || '')}</div>
            ${a.remark ? `<div style="font-size:11px;color:var(--c-text-lighter)">备注: ${esc(a.remark)}</div>` : ''}</td>
        <td>${a.group ? `<span class="pill-group">${esc(a.group)}</span>` : '<span style="color:var(--c-text-lighter)">-</span>'}</td>
        <td><span class="badge ${a.auth_type === 'jwt' ? 'badge-info' : 'badge-purple'}">${a.auth_type === 'jwt' ? 'JWT' : 'API Key'}</span>
            ${a.has_api_key && a.auth_type === 'jwt' ? '<div style="font-size:10.5px;color:var(--c-text-lighter);margin-top:3px">+APIKey回退</div>' : ''}</td>
        <td>${statusBadge(a.status)}${!a.enabled ? ' <span class="badge badge-secondary">停用</span>' : ''}
            ${a.last_error ? `<div style="font-size:10.5px;color:var(--c-danger);margin-top:3px;max-width:160px;overflow:hidden;text-overflow:ellipsis" title="${esc(a.last_error)}">${esc(a.last_error)}</div>` : ''}</td>
        <td style="min-width:190px;cursor:pointer" title="点击查看套餐与额度构成" onclick="showQuotaModal(${a.id})">${quotaCell(a)}</td>
        <td><span class="mono" title="${esc(a.device_mid)}">${a.device_mid ? esc(a.device_mid.slice(0, 8)) + '…' : '-'}</span></td>
        <td>${a.use_count} / ${a.fail_count}<div style="font-size:10.5px;color:var(--c-text-lighter)">${fmtAgo(a.last_used_at)}</div></td>
        <td style="max-width:170px">${a.last_claim_at ? `<div style="font-size:11px">${esc(a.last_claim_plan || '')}</div><div style="font-size:10.5px;color:var(--c-text-lighter)">${esc(a.last_claim_msg || '').slice(0, 40)}</div>` : '<span style="color:var(--c-text-lighter)">-</span>'}</td>
        <td class="actions-cell">
          <button class="btn btn-sm btn-secondary" onclick="refreshQuota(${a.id})">刷新</button>
          <button class="btn btn-sm btn-primary" onclick="claimNow(${a.id})">领活动</button>
          <button class="btn btn-sm btn-secondary" onclick="showAccountActions(${a.id})">更多 ▾</button>
        </td>
      </tr>`).join('')}
    </tbody></table></div>`;
}

// showAccountActions 用模态菜单承载账号操作（表格内下拉会被 overflow 容器裁剪）
function showAccountActions(id) {
  const a = (accountsCache || []).find(x => x.id === id);
  if (!a) return;
  openModal(`<h3>账号操作 · ${esc(a.display_name || a.email || ('#' + id))}</h3>
    <div style="display:grid;gap:8px">
      <button class="btn btn-secondary" style="justify-content:flex-start" onclick="detectNow(${id})">🔍 检测活动</button>
      <button class="btn btn-secondary" style="justify-content:flex-start" onclick="activateNow(${id})">⚡ 激活套餐</button>
      <button class="btn btn-secondary" style="justify-content:flex-start" onclick="resetQuota(${id})">♻️ 配额重置（Coding Plan）</button>
      <button class="btn btn-secondary" style="justify-content:flex-start" onclick="editAccount(${id})">✏️ 编辑分组 / 备注</button>
      ${a.has_creds_snapshot || a.has_jwt ? `<button class="btn btn-secondary" style="justify-content:flex-start" onclick="switchBack(${id})">💾 切回本地客户端</button>` : ''}
      ${a.has_creds_snapshot ? `<button class="btn btn-secondary" style="justify-content:flex-start" onclick="restoreLocal(${id})">↩️ 从快照还原本地</button>` : ''}
      <button class="btn btn-danger" style="justify-content:flex-start" onclick="deleteAccount(${id})">🗑 删除账号</button>
    </div>
    <div class="actions"><button class="btn btn-secondary" onclick="closeModal()">关闭</button></div>`);
}

async function refreshQuota(id) {
  try {
    await api(`/api/accounts/${id}/refresh`, { method: 'POST' });
    toast('额度已刷新');
    loadAccounts();
  } catch (e) { toast('刷新失败: ' + e.message, 'error'); }
}

async function refreshAllQuota() {
  toast('正在刷新所有账号额度…', 'info');
  for (const a of accountsCache.length ? accountsCache : (await api('/api/accounts')).accounts || []) {
    try { await api(`/api/accounts/${a.id}/refresh`, { method: 'POST' }); } catch (e) { /* 单个失败继续 */ }
  }
  toast('全部刷新完成');
  loadAccounts(); loadDashboard();
}

async function claimNow(id) {
  toast('正在检测并领取活动（含验证码求解，约 10-30 秒）…', 'info');
  try {
    const r = await api(`/api/accounts/${id}/claim`, { method: 'POST' });
    if (r.ok) toast('领取成功: ' + (r.plan_name || ''));
    else toast('领取未成功: ' + r.message, 'error');
    loadAccounts();
  } catch (e) { toast('领取失败: ' + e.message, 'error'); }
}

async function detectNow(id) {
  closeModal();
  try {
    const r = await api(`/api/accounts/${id}/detect`);
    const plans = r.plans || [];
    openModal(`<h3>活动检测</h3>
      ${plans.length ? plans.map(p => `
        <div style="border:1px solid var(--c-border);border-radius:10px;padding:12px 14px;margin-bottom:10px">
          <div style="font-weight:700;margin-bottom:4px">${esc(p.name)} <span class="badge badge-purple">优先级 ${p.priority}</span></div>
          <div style="font-size:12px;color:var(--c-text-light);margin-bottom:6px">${esc(p.description || '')}</div>
          ${(p.grants || []).map(g => `<div style="font-size:12.5px">🎁 ${esc(g)}</div>`).join('')}
          <div class="mono" style="color:var(--c-text-lighter);margin-top:6px">${esc(p.plan_id)}</div>
        </div>`).join('') : '<div class="empty"><p>当前无可领取活动</p></div>'}
      <div class="actions"><button class="btn btn-secondary" onclick="closeModal()">关闭</button>
      ${plans.length ? `<button class="btn btn-primary" onclick="claimNow(${id})">立即领取最高优先级</button>` : ''}</div>`);
  } catch (e) { toast('检测失败: ' + e.message, 'error'); }
}

async function detectAllAccounts() {
  const list = accountsCache.length ? accountsCache : (await api('/api/accounts')).accounts || [];
  toast(`正在检测 ${list.length} 个账号的活动…`, 'info');
  let found = 0;
  for (const a of list) {
    try {
      const r = await api(`/api/accounts/${a.id}/detect`);
      if ((r.plans || []).length) found++;
    } catch (e) { /* continue */ }
  }
  toast(`检测完成：${found}/${list.length} 个账号发现活动`);
  loadClaimRecords();
}

async function resetQuota(id) {
  closeModal();
  toast('正在查询重置机会…', 'info');
  try {
    const st = await api(`/api/accounts/${id}/reset-status`);
    if (!st.ok) {
      openModal(`<h3>配额重置</h3><div class="empty"><p>${esc(st.message || '不可用')}</p></div>
        <div class="hint" style="margin-top:10px">该接口仅付费 Coding Plan 账号可用（Start Plan 返回 3101 coding plan is required）。</div>
        <div class="actions"><button class="btn btn-secondary" onclick="closeModal()">关闭</button></div>`);
      return;
    }
    const s = st.status;
    const five = (s.available_five_hour_resets || []).length;
    const week = (s.available_week_resets || []).length;
    openModal(`<h3>配额重置机会</h3>
      <div class="kv-grid" style="margin-bottom:14px">
        <div class="kv-item"><div class="k">5 小时窗口重置</div><div class="v">${five} 次可用</div></div>
        <div class="kv-item"><div class="k">周重置</div><div class="v">${week} 次可用</div></div>
        <div class="kv-item"><div class="k">最近 5h 重置</div><div class="v">${s.latest_five_hour_reset_history ? fmtEpoch(s.latest_five_hour_reset_history.used_at) : '无'}</div></div>
        <div class="kv-item"><div class="k">最近周重置</div><div class="v">${s.latest_week_reset_history ? fmtEpoch(s.latest_week_reset_history.used_at) : '无'}</div></div>
      </div>
      <div class="hint">消耗一次机会立即恢复对应窗口配额（优先 five_hour）。</div>
      <div class="actions"><button class="btn btn-secondary" onclick="closeModal()">取消</button>
      <button class="btn btn-warning" onclick="doReset(${id})" ${five + week === 0 ? 'disabled' : ''}>执行重置</button></div>`);
  } catch (e) { toast(e.message, 'error'); }
}

async function doReset(id) {
  closeModal();
  toast('正在执行配额重置…', 'info');
  try {
    const r = await api(`/api/accounts/${id}/reset`, { method: 'POST' });
    if (r.ok) toast(r.message || '重置成功');
    else toast('重置失败: ' + r.message, 'error');
    loadAccounts();
  } catch (e) { toast(e.message, 'error'); }
}

async function activateNow(id) {
  closeModal();
  toast('正在上报激活事件…', 'info');
  try {
    const r = await api(`/api/accounts/${id}/activate`, { method: 'POST' });
    if (r.ok) toast('激活成功: ' + (r.message || ''));
    else toast('激活未完成: ' + r.message, 'error');
    loadAccounts();
  } catch (e) { toast('激活失败: ' + e.message, 'error'); }
}

function editAccount(id) {
  closeModal();
  const a = accountsCache.find(x => x.id === id);
  if (!a) return;
  openModal(`<h3>编辑账号</h3>
    <div class="form-group"><label>分组</label><select id="editGroup">${groupOptions(a.group)}</select></div>
    <div class="form-group"><label>备注</label><input type="text" id="editRemark" value="${esc(a.remark)}"></div>
    <div class="form-group"><label><input type="checkbox" id="editEnabled" ${a.enabled ? 'checked' : ''} style="width:auto;margin-right:6px">启用（参与轮询）</label></div>
    <div class="actions"><button class="btn btn-secondary" onclick="closeModal()">取消</button>
    <button class="btn btn-primary" onclick="submitEditAccount(${id})">保存</button></div>`);
}

async function submitEditAccount(id) {
  let group = document.getElementById('editGroup').value;
  if (group === '__new__') {
    group = prompt('新分组名称:');
    if (!group) return;
  }
  try {
    await api(`/api/accounts/${id}`, { method: 'PUT', body: {
      group, remark: document.getElementById('editRemark').value,
      enabled: document.getElementById('editEnabled').checked,
    }});
    closeModal(); toast('已保存'); loadGroups(); loadAccounts();
  } catch (e) { toast(e.message, 'error'); }
}

async function deleteAccount(id) {
  closeModal();
  const a = accountsCache.find(x => x.id === id);
  if (!confirm(`确认删除账号 ${a ? (a.email || a.display_name) : id}？此操作不可恢复。`)) return;
  try {
    await api(`/api/accounts/${id}`, { method: 'DELETE' });
    toast('已删除'); loadAccounts(); loadDashboard();
  } catch (e) { toast(e.message, 'error'); }
}

async function switchBack(id) {
  closeModal();
  const a = accountsCache.find(x => x.id === id);
  openModal(`<h3>一键切回本地客户端</h3>
    <p style="font-size:13px;color:var(--c-text-light);margin-bottom:14px">
    将把账号 <b>${esc(a ? a.email || a.display_name : id)}</b> 的凭证重新加密写回本机
    <span class="mono">~/.zcode/v2/credentials.json</span> 与 <span class="mono">config.json</span>（原文件自动备份到 data/backups/）。</p>
    <div class="form-group"><label><input type="checkbox" id="killClient" style="width:auto;margin-right:6px">写回后结束 ZCode.exe 进程（下次启动生效）</label></div>
    <div class="actions"><button class="btn btn-secondary" onclick="closeModal()">取消</button>
    <button class="btn btn-warning" onclick="doSwitchBack(${id})">确认切回</button></div>`);
}

async function doSwitchBack(id) {
  const kill = document.getElementById('killClient').checked;
  try {
    const r = await api(`/api/accounts/${id}/switch-back`, { method: 'POST', body: { kill_client: kill } });
    closeModal(); toast(r.message || '已切回本地客户端');
  } catch (e) { toast('切回失败: ' + e.message, 'error'); }
}

async function restoreLocal(id) {
  closeModal();
  if (!confirm('用该账号导入时的快照覆盖本地客户端当前登录态？')) return;
  try {
    const r = await api(`/api/accounts/${id}/restore-local`, { method: 'POST' });
    toast(r.message || '已还原');
  } catch (e) { toast('还原失败: ' + e.message, 'error'); }
}

// ---- 账号导入 ----

async function importLocalAccount() {
  toast('正在从本地 ZCode 客户端导入…', 'info');
  try {
    const group = document.getElementById('groupFilter')?.value || '';
    const r = await api('/api/accounts/import/local', { method: 'POST', body: { group } });
    toast('导入成功: ' + (r.account.email || r.account.display_name));
    switchSection('accounts');
  } catch (e) { toast('导入失败: ' + e.message, 'error'); }
}

function showPasteModal() {
  openModal(`<h3>粘贴导入</h3>
    <div class="form-group"><label>通道</label>
      <select id="pasteProvider"><option value="zai">Z.AI (zcode.z.ai)</option><option value="bigmodel">BigModel (open.bigmodel.cn)</option></select></div>
    <div class="form-group"><label>名称（可选）</label><input type="text" id="pasteName" placeholder="账号备注名"></div>
    <div class="form-group"><label>凭证（JWT 或 API Key）</label>
      <textarea id="pasteSecret" placeholder="eyJhbGci… 三段 JWT，或 xxx.yyy 格式 API Key"></textarea></div>
    <div class="form-group"><label>分组</label><select id="pasteGroup">${groupOptions('')}</select></div>
    <div class="actions"><button class="btn btn-secondary" onclick="closeModal()">取消</button>
    <button class="btn btn-primary" onclick="submitPaste()">导入</button></div>`);
}

async function submitPaste() {
  let group = document.getElementById('pasteGroup').value;
  if (group === '__new__') { group = prompt('新分组名称:'); if (!group) return; }
  try {
    const r = await api('/api/accounts/import/paste', { method: 'POST', body: {
      provider: document.getElementById('pasteProvider').value,
      name: document.getElementById('pasteName').value,
      secret: document.getElementById('pasteSecret').value.trim(),
      group,
    }});
    closeModal(); toast('导入成功'); switchSection('accounts');
  } catch (e) { toast('导入失败: ' + e.message, 'error'); }
}

// ---- OAuth 登录 ----

let oauthPollTimer = null;

function showOAuthModal() {
  openModal(`<h3>OAuth 登录新账号</h3>
    <div class="form-group"><label>分组</label><select id="oauthGroup">${groupOptions('')}</select></div>
    <div class="form-group"><label>方式</label>
      <select id="oauthMode">
        <option value="poll">免回调登录（推荐：与桌面端相同的 CLI 轮询流程，授权后自动完成）</option>
        <option value="manual">手动粘贴（备用）</option>
        <option value="auto">环回自动（实验：当前会报 Redirect URI not registered）</option>
      </select></div>
    <div id="oauthStep2"></div>
    <div class="actions"><button class="btn btn-secondary" onclick="cancelOAuth()">取消</button>
    <button class="btn btn-primary" id="oauthStartBtn" onclick="startOAuth()">开始登录</button></div>`);
}

async function startOAuth() {
  let group = document.getElementById('oauthGroup').value;
  if (group === '__new__') { group = prompt('新分组名称:'); if (!group) return; }
  const mode = document.getElementById('oauthMode').value;
  const manual = mode === 'manual';
  const poll = mode === 'poll';
  const btn = document.getElementById('oauthStartBtn');
  btn.disabled = true;
  try {
    const r = await api('/api/accounts/oauth/start', { method: 'POST', body: { manual, poll, group } });
    window._oauthState = r.state;
    if (!manual) {
      window.open(r.authorize_url, '_blank');
      const hint = poll
        ? '已在新标签页打开授权页。完成授权后服务端自动收到结果（无需回跳本机），下方状态会自动更新。'
        : '已在新标签页打开 Z.AI 授权页，登录并授权后自动跳回本网关完成入库。';
      document.getElementById('oauthStep2').innerHTML =
        `<div class="hint" style="margin-top:10px">${hint}</div>
         <div class="hint" style="margin-top:6px;color:var(--c-warning-dark)">若长时间未完成，可切换到「手动粘贴」模式：复制授权后地址栏的完整 URL 提交。</div>
         <div id="oauthStatus" style="margin-top:8px;font-size:13px"></div>`;
      pollOAuth();
    } else {
      window.open(r.authorize_url, '_blank');
      document.getElementById('oauthStep2').innerHTML =
        `<div class="hint" style="margin-top:10px">已在新标签页打开 Z.AI 授权页。步骤：① 登录并同意授权 → ② 浏览器会跳到 <b>zcode.z.ai/login?code=…</b> → ③ 复制该地址栏<b>完整 URL</b>粘贴到下面 → ④ 提交兑换。</div>
         <div class="form-group" style="margin-top:10px"><label>粘贴回跳 URL（或仅 code）</label>
         <textarea id="oauthManualInput" class="form-textarea" style="min-height:70px" placeholder="https://zcode.z.ai/login?code=...&state=..."></textarea></div>
         <button class="btn btn-success" onclick="submitOAuthManual()">提交兑换</button>
         <div id="oauthStatus" style="margin-top:8px;font-size:13px"></div>`;
    }
  } catch (e) {
    btn.disabled = false;
    toast('发起登录失败: ' + e.message, 'error');
  }
}

async function submitOAuthManual() {
  const input = document.getElementById('oauthManualInput').value.trim();
  if (!input) return toast('请粘贴回跳 URL 或 code', 'error');
  const st = document.getElementById('oauthStatus');
  st.textContent = '兑换中…';
  try {
    await api('/api/accounts/oauth/manual', { method: 'POST', body: { state: window._oauthState, input } });
    st.textContent = '';
    toast('登录成功，账号已入库');
    cancelOAuth();
    switchSection('accounts');
  } catch (e) { st.textContent = ''; toast(e.message, 'error'); }
}

function pollOAuth() {
  clearInterval(oauthPollTimer);
  oauthPollTimer = setInterval(async () => {
    if (!window._oauthState) return;
    try {
      const f = await api('/api/accounts/oauth/status?state=' + encodeURIComponent(window._oauthState));
      const el = document.getElementById('oauthStatus');
      if (f.status === 'ready') {
        clearInterval(oauthPollTimer);
        toast('登录成功: ' + (f.email || ''));
        setTimeout(() => { closeModal(); switchSection('accounts'); }, 800);
      } else if (f.status === 'failed') {
        clearInterval(oauthPollTimer);
        if (el) el.innerHTML = `<span style="color:var(--c-danger)">${esc(f.message)}</span>`;
        document.getElementById('oauthStartBtn').disabled = false;
      } else if (el && f.status === 'exchanging') {
        el.textContent = '正在兑换 token 并提取 API Key…';
      }
    } catch (e) { /* 流程过期 */ }
  }, 1500);
}

function cancelOAuth() {
  clearInterval(oauthPollTimer);
  window._oauthState = null;
  closeModal();
}

// ---- 活动计划 ----

let plansCache = [];

async function loadPlans() {
  try {
    const d = await api('/api/plans');
    plansCache = d.plans || [];
    renderPlans();
  } catch (e) { toast(e.message, 'error'); }
}

const TASK_LABEL = { detect: '检测活动', claim: '一键领取', activate: '激活套餐', reset: '配额重置' };

function renderPlans() {
  const el = document.getElementById('plansTable');
  if (!plansCache.length) {
    el.innerHTML = '<div class="empty"><p>暂无计划，点击「+ 新建计划」创建</p></div>';
    return;
  }
  el.innerHTML = `<div class="table-wrap"><table>
    <thead><tr><th>计划名</th><th>任务</th><th>cron</th><th>目标</th><th>间隔</th><th>下次运行</th><th>最近运行</th><th>状态</th><th style="width:190px">操作</th></tr></thead>
    <tbody>${plansCache.map(p => `
      <tr>
        <td style="font-weight:700">${esc(p.plan_name)}</td>
        <td><span class="badge badge-info">${TASK_LABEL[p.task_type] || p.task_type}</span></td>
        <td class="mono">${esc(p.cron_expr)}</td>
        <td>${p.target_type === 'single_account' ? '账号#' + p.account_id : p.target_type === 'group' ? '分组: ' + esc(p.account_group) : '全部账号'}</td>
        <td>${p.delay_seconds}s</td>
        <td style="font-size:12px">${esc(p.next_run_at || '-')}</td>
        <td style="font-size:12px">${esc(p.last_run_at || '-')}<div style="color:var(--c-text-lighter);font-size:11px;max-width:200px;overflow:hidden;text-overflow:ellipsis" title="${esc(p.last_run_msg || '')}">${esc(p.last_run_msg || '')}</div></td>
        <td>${p.is_active ? '<span class="badge badge-success">启用</span>' : '<span class="badge badge-secondary">停用</span>'}
            ${p.last_run_status ? `<div style="margin-top:3px">${p.last_run_status === 'success' ? '✅' : '❌'}</div>` : ''}</td>
        <td class="actions-cell">
          <button class="btn btn-sm btn-primary" onclick="runPlan(${p.id})">立即运行</button>
          <button class="btn btn-sm btn-secondary" onclick="showPlanModal(${p.id})">编辑</button>
          <button class="btn btn-sm btn-danger" onclick="deletePlan(${p.id})">删除</button>
        </td>
      </tr>`).join('')}</tbody></table></div>`;
}

function showPlanModal(id) {
  const p = plansCache.find(x => x.id === id) || {};
  const accountOpts = (accountsCache.length ? accountsCache : []).map(a =>
    `<option value="${a.id}" ${p.account_id === a.id ? 'selected' : ''}>${esc(a.email || a.display_name || ('#' + a.id))}</option>`).join('');
  openModal(`<h3>${id ? '编辑计划' : '新建计划'}</h3>
    <div class="form-group"><label>计划名称</label><input type="text" id="planName" value="${esc(p.plan_name || '')}" placeholder="例如：每日领取活动"></div>
    <div class="form-group"><label>任务类型</label>
      <select id="planTask">
        <option value="claim" ${p.task_type === 'claim' ? 'selected' : ''}>一键领取（检测+验证码+领取）</option>
        <option value="detect" ${p.task_type === 'detect' ? 'selected' : ''}>仅检测活动</option>
        <option value="activate" ${p.task_type === 'activate' ? 'selected' : ''}>激活套餐（上报激活事件）</option>
        <option value="reset" ${p.task_type === 'reset' ? 'selected' : ''}>配额重置（耗尽时恢复窗口配额）</option>
      </select></div>
    <div class="form-group"><label>cron 表达式（分 时 日 月 周）</label>
      <input type="text" id="planCron" class="mono" value="${esc(p.cron_expr || '0 9 * * *')}" placeholder="0 9 * * *">
      <div class="hint">示例：0 9 * * * = 每天 09:00；*/30 * * * * = 每 30 分钟</div></div>
    <div class="form-group"><label>目标</label>
      <select id="planTarget" onchange="planTargetChange()">
        <option value="all_accounts" ${p.target_type === 'all_accounts' || !p.target_type ? 'selected' : ''}>全部可用账号</option>
        <option value="group" ${p.target_type === 'group' ? 'selected' : ''}>指定分组</option>
        <option value="single_account" ${p.target_type === 'single_account' ? 'selected' : ''}>单个账号</option>
      </select></div>
    <div class="form-group" id="planGroupWrap" style="display:none"><label>分组</label><select id="planGroup">${groupOptions(p.account_group || '')}</select></div>
    <div class="form-group" id="planAccountWrap" style="display:none"><label>账号</label><select id="planAccount"><option value="0">选择账号</option>${accountOpts}</select></div>
    <div class="form-group"><label>账号间隔（秒，防风控，含 0~50% 随机抖动）</label>
      <input type="number" id="planDelay" value="${p.delay_seconds ?? 30}" min="0" max="3600"></div>
    <div class="form-group"><label><input type="checkbox" id="planActive" ${p.is_active !== false ? 'checked' : ''} style="width:auto;margin-right:6px">启用</label></div>
    <div class="actions"><button class="btn btn-secondary" onclick="closeModal()">取消</button>
    <button class="btn btn-primary" onclick="savePlan(${id || 0})">保存</button></div>`);
  planTargetChange();
}

function planTargetChange() {
  const t = document.getElementById('planTarget').value;
  document.getElementById('planGroupWrap').style.display = t === 'group' ? '' : 'none';
  document.getElementById('planAccountWrap').style.display = t === 'single_account' ? '' : 'none';
}

async function savePlan(id) {
  let group = '';
  const gw = document.getElementById('planGroupWrap');
  if (gw.style.display !== 'none') {
    group = document.getElementById('planGroup').value;
    if (group === '__new__') { group = prompt('新分组名称:'); if (!group) return; }
  }
  const body = {
    plan_name: document.getElementById('planName').value.trim() || '未命名计划',
    task_type: document.getElementById('planTask').value,
    cron_expr: document.getElementById('planCron').value.trim(),
    target_type: document.getElementById('planTarget').value,
    account_group: group,
    account_id: Number(document.getElementById('planAccount')?.value || 0),
    delay_seconds: Number(document.getElementById('planDelay').value || 0),
    is_active: document.getElementById('planActive').checked,
    auto_pick: true,
  };
  try {
    if (id) await api('/api/plans/' + id, { method: 'PUT', body });
    else await api('/api/plans', { method: 'POST', body });
    closeModal(); toast('计划已保存'); loadPlans();
  } catch (e) { toast(e.message, 'error'); }
}

async function deletePlan(id) {
  if (!confirm('确认删除该计划？')) return;
  try { await api('/api/plans/' + id, { method: 'DELETE' }); toast('已删除'); loadPlans(); }
  catch (e) { toast(e.message, 'error'); }
}

async function runPlan(id) {
  try { await api(`/api/plans/${id}/run`, { method: 'POST' }); toast('已开始执行，见顶部进度', 'info'); }
  catch (e) { toast(e.message, 'error'); }
}

async function pollRunning() {
  try {
    const d = await api('/api/plans/running');
    const bar = document.getElementById('planRunningBar');
    if (!bar) return;
    const running = d.running || [];
    if (!running.length) { bar.innerHTML = ''; return; }
    bar.innerHTML = running.map(s => `
      <div class="running-bar">
        <span class="rb-title">▶ ${esc(s.plan_name)}（${TASK_LABEL[s.task_type] || s.task_type}）</span>
        <div class="progress-track" style="flex:1;min-width:120px"><div class="progress-fill" style="width:${s.total ? s.done / s.total * 100 : 0}%"></div></div>
        <span class="rb-meta">${s.done}/${s.total} · ✅${s.success} ❌${s.fail}${s.current_account ? ' · ' + esc(s.current_account) : ''}</span>
      </div>`).join('');
  } catch (e) { /* ignore */ }
}

async function loadClaimRecords() {
  try {
    const d = await api('/api/claim-records?limit=60');
    const el = document.getElementById('claimRecordsTable');
    const recs = d.records || [];
    if (!recs.length) { el.innerHTML = '<div class="empty"><p>暂无领取记录</p></div>'; return; }
    el.innerHTML = `<div class="table-wrap"><table>
      <thead><tr><th>时间</th><th>账号</th><th>类型</th><th>活动</th><th>结果</th><th>信息</th></tr></thead>
      <tbody>${recs.map(r => `<tr>
        <td style="font-size:12px">${esc(r.created_at)}</td>
        <td>${esc(r.email || ('#' + r.account_id))}</td>
        <td><span class="badge badge-info">${TASK_LABEL[r.task_type] || r.task_type}</span></td>
        <td>${esc(r.plan_name || r.plan_id || '-')}</td>
        <td>${r.success ? '<span class="badge badge-success">成功</span>' : '<span class="badge badge-danger">失败</span>'}${r.code ? ` <span class="mono" style="font-size:11px">code=${r.code}</span>` : ''}</td>
        <td style="max-width:280px" title="${esc(r.message)}">${esc(r.message || '')}</td>
      </tr>`).join('')}</tbody></table></div>`;
  } catch (e) { /* ignore */ }
}

async function loadPlanRuns() {
  try {
    const d = await api('/api/plan-runs?limit=40');
    const el = document.getElementById('planRunsTable');
    const recs = d.records || [];
    if (!recs.length) { el.innerHTML = '<div class="empty"><p>暂无运行记录</p></div>'; return; }
    el.innerHTML = `<div class="table-wrap"><table>
      <thead><tr><th>运行时间</th><th>计划</th><th>任务</th><th>状态</th><th>成功/失败</th><th>耗时</th><th>摘要</th></tr></thead>
      <tbody>${recs.map(r => `<tr>
        <td style="font-size:12px">${esc(r.run_at)}</td>
        <td>${esc(r.plan_name)}</td>
        <td><span class="badge badge-info">${TASK_LABEL[r.task_type] || r.task_type}</span></td>
        <td>${r.status === 'success' ? '<span class="badge badge-success">成功</span>' : '<span class="badge badge-danger">失败</span>'}</td>
        <td>${r.success_count} / ${r.fail_count}</td>
        <td>${(r.duration_ms / 1000).toFixed(1)}s</td>
        <td style="max-width:320px" title="${esc(r.message)}">${esc(r.message || '')}</td>
      </tr>`).join('')}</tbody></table></div>`;
  } catch (e) { /* ignore */ }
}

// ---- 使用记录 ----

async function loadUsageStats() {
  try {
    const u = await api('/api/stats?days=7');
    document.getElementById('uStatReq').textContent = fmtNum(u.requests);
    document.getElementById('uStatIn').textContent = fmtNum(u.prompt_tokens);
    document.getElementById('uStatOut').textContent = fmtNum(u.completion_tokens);
    document.getElementById('uStatDur').textContent = (u.avg_duration_ms || 0) + 'ms';
    document.getElementById('uStatTtft').textContent = (u.avg_ttft_ms || 0) + 'ms';
  } catch (e) { /* ignore */ }
}

async function loadUsageRecords() {
  try {
    const d = await api('/api/usage-records?limit=100');
    const el = document.getElementById('usageTable');
    const recs = d.records || [];
    if (!recs.length) { el.innerHTML = '<div class="empty"><p>暂无使用记录</p></div>'; return; }
    el.innerHTML = `<div class="table-wrap"><table>
      <thead><tr><th>时间</th><th>账号</th><th>模型</th><th>输入</th><th>输出</th><th>合计</th><th>流式</th><th>状态</th><th>耗时</th><th>TTFT</th></tr></thead>
      <tbody>${recs.map(r => `<tr>
        <td style="font-size:12px">${esc(r.created_at)}</td>
        <td>${esc(r.email || ('#' + r.account_id))}</td>
        <td>${esc(r.model)}</td>
        <td>${fmtNum(r.prompt_tokens)}</td>
        <td>${fmtNum(r.completion_tokens)}</td>
        <td><b>${fmtNum(r.total_tokens)}</b></td>
        <td>${r.stream ? '✓' : '-'}</td>
        <td>${r.status_code === 200 ? '<span class="badge badge-success">200</span>' : `<span class="badge badge-danger">${r.status_code}</span>`}</td>
        <td>${(r.duration_ms / 1000).toFixed(1)}s</td>
        <td>${r.ttft_ms ? r.ttft_ms + 'ms' : '-'}</td>
      </tr>`).join('')}</tbody></table></div>`;
  } catch (e) { /* ignore */ }
}

// ---- 设置 ----

async function loadSettings() {
  try {
    const s = await api('/api/settings');
    document.getElementById('setStrategy').value = s.selection_strategy || 'round_robin';
    document.getElementById('setQuotaInterval').value = s.quota_refresh_interval || '60';
    document.getElementById('setGlobalProxy').value = s.upstream_proxy || '';
    document.getElementById('setCaptchaMode').value = s.captcha_mode || 'auto';
    document.getElementById('setGatewayModels').value = s.gateway_models || '';
    window._fpCurrent = s.fingerprint || 'chrome';
    window._ja3Current = s.custom_ja3 || '';
    loadGatewayKey();
    loadModels();
    loadCaptchaStatus();
    loadProxies();
    loadFingerprints();
  } catch (e) { /* ignore */ }
}

// ---- TLS 指纹 ----

async function loadFingerprints() {
  try {
    const d = await api('/api/fingerprints');
    const sel = document.getElementById('setFingerprint');
    const groups = {};
    (d.fingerprints || []).forEach(f => { (groups[f.group] = groups[f.group] || []).push(f); });
    sel.innerHTML = Object.entries(groups).map(([g, list]) =>
      `<optgroup label="${esc(g)}">` + list.map(f =>
        `<option value="${esc(f.id)}">${esc(f.label)}</option>`).join('') + '</optgroup>').join('');
    // 选中当前生效值；若不在预置表中（旧库残留）回落到 chrome，避免静默显示第一项
    const cur = window._fpCurrent || 'chrome';
    sel.value = [...sel.options].some(o => o.value === cur) ? cur : 'chrome';
    document.getElementById('setCustomJA3').value = window._ja3Current || '';
    const hint = document.getElementById('fpCurrentHint');
    if (hint) hint.textContent = '当前生效: ' + sel.value + '（保存后新连接生效）';
    fpModeChange();
  } catch (e) { /* ignore */ }
}

function fpModeChange() {
  const v = document.getElementById('setFingerprint').value;
  document.getElementById('ja3Wrap').style.display = v === 'custom' ? '' : 'none';
}

async function saveFingerprint() {
  try {
    await api('/api/settings', { method: 'PUT', body: {
      fingerprint: document.getElementById('setFingerprint').value,
      custom_ja3: document.getElementById('setCustomJA3').value.trim(),
    }});
    toast('指纹设置已保存（新连接生效）');
  } catch (e) { toast(e.message, 'error'); }
}

async function saveStrategySettings() {
  try {
    await api('/api/settings', { method: 'PUT', body: {
      selection_strategy: document.getElementById('setStrategy').value,
      quota_refresh_interval: document.getElementById('setQuotaInterval').value,
    }});
    toast('策略已保存');
  } catch (e) { toast(e.message, 'error'); }
}

async function saveCaptchaSettings() {
  try {
    await api('/api/settings', { method: 'PUT', body: { captcha_mode: document.getElementById('setCaptchaMode').value } });
    toast('验证码设置已保存');
  } catch (e) { toast(e.message, 'error'); }
}

async function saveModelsSettings() {
  try {
    await api('/api/settings', { method: 'PUT', body: { gateway_models: document.getElementById('setGatewayModels').value.trim() } });
    toast('模型清单已保存'); loadModels();
  } catch (e) { toast(e.message, 'error'); }
}

async function loadModels() {
  try {
    const d = await api('/api/models');
    document.getElementById('currentModels').innerHTML =
      '当前生效: ' + (d.models || []).map(m => `<span class="pill-group" style="margin:2px">${esc(m)}</span>`).join('');
    loadCatalog();
  } catch (e) { /* ignore */ }
}

async function loadCatalog() {
  try {
    const d = await api('/api/models/catalog');
    const el = document.getElementById('catalogModels');
    if (!el) return;
    const ms = d.models || [];
    el.innerHTML = ms.length
      ? ms.map(m => `<span class="pill-group" style="margin:2px" title="${esc(`ctx=${m.contextWindow} prio=${m.priority}${m.vision ? ' vision' : ''}`)}">${esc(m.modelId)}</span>`).join('')
      : '<span style="color:var(--c-text-lighter)">尚未同步，点击「同步官方目录」</span>';
  } catch (e) { /* ignore */ }
}

async function syncModelCatalog() {
  toast('正在同步官方模型目录…', 'info');
  try {
    const r = await api('/api/models/sync', { method: 'POST' });
    toast(`已同步 ${r.total} 个模型`);
    loadCatalog();
  } catch (e) { toast(e.message, 'error'); }
}

async function applyCatalogToGateway() {
  try {
    const d = await api('/api/models/catalog');
    const ids = (d.models || []).map(m => m.modelId);
    if (!ids.length) return toast('目录为空，请先同步', 'error');
    await api('/api/settings', { method: 'PUT', body: { gateway_models: ids.join(',') } });
    toast('已将目录应用为网关模型清单');
    loadModels();
  } catch (e) { toast(e.message, 'error'); }
}

async function loadGatewayKey() {
  try {
    const d = await api('/api/settings/api-key');
    document.getElementById('gatewayKeyDisplay').value = d.api_key || '';
  } catch (e) { /* ignore */ }
}

async function generateAPIKey() {
  if (!confirm('重新生成后旧 Key 立即失效，确认？')) return;
  try {
    const d = await api('/api/settings/api-key/generate', { method: 'POST' });
    document.getElementById('gatewayKeyDisplay').value = d.api_key;
    toast('已生成新 API Key');
  } catch (e) { toast(e.message, 'error'); }
}

function copyGatewayKey() {
  const v = document.getElementById('gatewayKeyDisplay').value;
  if (!v) return toast('尚未生成', 'error');
  navigator.clipboard.writeText(v).then(() => toast('已复制'));
}

async function changePassword() {
  try {
    await api('/api/auth/password', { method: 'POST', body: {
      old_password: document.getElementById('oldPassword').value,
      new_password: document.getElementById('newPassword').value,
    }});
    toast('密码已修改，请重新登录'); setTimeout(doLogout, 1200);
  } catch (e) { toast(e.message, 'error'); }
}

// ---- 验证码状态 ----

async function loadCaptchaStatus() {
  try {
    const c = await api('/api/captcha/status');
    const el = document.getElementById('captchaStatus');
    if (!el) return;
    el.innerHTML = c.has_param
      ? `当前参数: ${c.fresh ? '✅ 新鲜' : '⏳ 已过期 ' + c.param_age_s + 's'}${c.config ? ' · scene=' + esc(c.config.scene_id) : ''}`
      : '当前无缓存参数（下次请求时自动求解）';
  } catch (e) { /* ignore */ }
}

async function solveCaptchaNow() {
  toast('正在求解验证码（无头浏览器约 5-15 秒）…', 'info');
  try {
    const r = await api('/api/captcha/solve', { method: 'POST', body: {} });
    if (r.success) toast(`求解成功（参数 ${r.param_len} 字节）`);
    else toast('求解未返回参数', 'error');
    loadCaptchaStatus();
  } catch (e) { toast('求解失败: ' + e.message, 'error'); }
}

async function invalidateCaptcha() {
  try { await api('/api/captcha/invalidate', { method: 'POST' }); toast('缓存已失效'); loadCaptchaStatus(); }
  catch (e) { toast(e.message, 'error'); }
}

// ---- 出口代理 ----

let proxiesCache = [];

async function loadProxies() {
  try {
    const d = await api('/api/proxies');
    proxiesCache = d.proxies || [];
    renderProxies();
  } catch (e) { /* ignore */ }
}

function renderProxies() {
  const el = document.getElementById('proxyTable');
  if (!el) return;
  if (!proxiesCache.length) {
    el.innerHTML = '<div class="empty"><p>暂无代理节点（未配置时全部直连）</p></div>';
    return;
  }
  el.innerHTML = `<div class="table-wrap"><table>
    <thead><tr><th>名称</th><th>类型</th><th>地址</th><th>绑定分组</th><th>默认</th><th>启用</th><th>检测</th><th style="width:180px">操作</th></tr></thead>
    <tbody>${proxiesCache.map(n => `<tr>
      <td style="font-weight:700">${esc(n.name || '-')}</td>
      <td>${esc(n.type)}</td>
      <td class="mono">${esc(n.host)}:${n.port}${n.username ? ' · ' + esc(n.username) : ''}</td>
      <td>${n.group_name ? n.group_name.split(',').map(g => `<span class="pill-group" style="margin:1px">${esc(g.trim())}</span>`).join('') : '-'}</td>
      <td>${n.is_default ? '⭐' : '-'}</td>
      <td>${n.enabled ? '<span class="badge badge-success">是</span>' : '<span class="badge badge-secondary">否</span>'}</td>
      <td style="font-size:12px">${n.check_status === 'ok' ? `✅ ${esc(n.check_ip)} ${n.check_latency}ms` : n.check_status === 'fail' ? `❌ ${esc(n.check_msg || '')}` : '-'}
        ${n.check_at ? `<div style="font-size:10.5px;color:var(--c-text-lighter)">${esc(n.check_at)}</div>` : ''}</td>
      <td class="actions-cell">
        <button class="btn btn-sm btn-secondary" onclick="testProxy(${n.id})">测试</button>
        <button class="btn btn-sm btn-secondary" onclick="showProxyModal(${n.id})">编辑</button>
        <button class="btn btn-sm btn-danger" onclick="deleteProxy(${n.id})">删除</button>
      </td>
    </tr>`).join('')}</tbody></table></div>`;
}

function showProxyModal(id) {
  const n = proxiesCache.find(x => x.id === id) || { type: 'socks5', enabled: true };
  openModal(`<h3>${id ? '编辑代理节点' : '添加代理节点'}</h3>
    <div class="form-group"><label>名称</label><input type="text" id="pxName" value="${esc(n.name || '')}" placeholder="例如：住宅代理-美国"></div>
    <div class="form-group"><label>类型</label><select id="pxType">
      <option value="socks5" ${n.type === 'socks5' ? 'selected' : ''}>SOCKS5</option>
      <option value="http" ${n.type === 'http' ? 'selected' : ''}>HTTP(S)</option></select></div>
    <div style="display:grid;grid-template-columns:2fr 1fr;gap:10px">
      <div class="form-group"><label>主机</label><input type="text" id="pxHost" value="${esc(n.host || '')}" placeholder="127.0.0.1"></div>
      <div class="form-group"><label>端口</label><input type="number" id="pxPort" value="${n.port || ''}" placeholder="7897"></div>
    </div>
    <div style="display:grid;grid-template-columns:1fr 1fr;gap:10px">
      <div class="form-group"><label>用户名（可选）</label><input type="text" id="pxUser" value="${esc(n.username || '')}"></div>
      <div class="form-group"><label>密码（可选）</label><input type="password" id="pxPass" placeholder="${id ? '留空保持不变' : ''}"></div>
    </div>
    <div class="form-group"><label>绑定分组（逗号分隔，留空=不绑定）</label><input type="text" id="pxGroup" value="${esc(n.group_name || '')}" placeholder="default,work"></div>
    <div class="form-group"><label>
      <input type="checkbox" id="pxDefault" ${n.is_default ? 'checked' : ''} style="width:auto;margin-right:6px">设为默认节点（未绑定组的账号走它）
      <input type="checkbox" id="pxEnabled" ${n.enabled !== false ? 'checked' : ''} style="width:auto;margin:0 6px 0 16px">启用</label></div>
    <div class="actions"><button class="btn btn-secondary" onclick="closeModal()">取消</button>
    <button class="btn btn-primary" onclick="saveProxy(${id || 0})">保存</button></div>`);
}

async function saveProxy(id) {
  const body = {
    name: document.getElementById('pxName').value.trim(),
    type: document.getElementById('pxType').value,
    host: document.getElementById('pxHost').value.trim(),
    port: Number(document.getElementById('pxPort').value || 0),
    username: document.getElementById('pxUser').value.trim(),
    password: document.getElementById('pxPass').value,
    group_name: document.getElementById('pxGroup').value.trim(),
    is_default: document.getElementById('pxDefault').checked,
    enabled: document.getElementById('pxEnabled').checked,
  };
  try {
    if (id) await api('/api/proxies/' + id, { method: 'PUT', body });
    else await api('/api/proxies', { method: 'POST', body });
    closeModal(); toast('已保存'); loadProxies(); loadGroups();
  } catch (e) { toast(e.message, 'error'); }
}

async function deleteProxy(id) {
  if (!confirm('确认删除该代理节点？')) return;
  try { await api('/api/proxies/' + id, { method: 'DELETE' }); toast('已删除'); loadProxies(); }
  catch (e) { toast(e.message, 'error'); }
}

async function testProxy(id) {
  toast('测试中…', 'info');
  try {
    const r = await api(`/api/proxies/${id}/test`, { method: 'POST' });
    if (r.ok) toast(`出口 IP: ${r.exit_ip}（${r.elapsed_ms}ms）`);
    else toast('测试失败: ' + (r.message || ''), 'error');
    loadProxies();
  } catch (e) { toast('测试失败: ' + e.message, 'error'); }
}

async function probeProxyPorts() {
  try {
    const d = await api('/api/proxies/probe-ports');
    const ports = d.ports || [];
    if (!ports.length) return toast('未发现开放的本机代理端口', 'error');
    openModal(`<h3>本机代理端口探测</h3>
      ${ports.map(p => `<div style="display:flex;justify-content:space-between;align-items:center;padding:9px 4px;border-bottom:1px solid var(--c-border-light)">
        <div><b class="mono">${esc(p.url)}</b><div style="font-size:11.5px;color:var(--c-text-light)">${esc(p.label)}</div></div>
        <button class="btn btn-sm btn-primary" data-url="${esc(p.url)}" onclick="useProbedPort(this.dataset.url)">使用</button></div>`).join('')}
      <div class="actions"><button class="btn btn-secondary" onclick="closeModal()">关闭</button></div>`);
  } catch (e) { toast(e.message, 'error'); }
}

function useProbedPort(url) {
  closeModal();
  document.getElementById('setGlobalProxy').value = url;
  saveGlobalProxy();
}

async function detectSystemProxy() {
  try {
    const d = await api('/api/proxies/system');
    if (d.enabled && d.url) {
      document.getElementById('setGlobalProxy').value = d.url;
      toast('已填入系统代理: ' + d.url, 'info');
    } else toast('系统未启用代理', 'error');
  } catch (e) { toast(e.message, 'error'); }
}

async function saveGlobalProxy() {
  try {
    await api('/api/settings', { method: 'PUT', body: { upstream_proxy: document.getElementById('setGlobalProxy').value.trim() } });
    toast('全局代理已保存');
  } catch (e) { toast(e.message, 'error'); }
}

async function testGlobalProxy() {
  const url = document.getElementById('setGlobalProxy').value.trim();
  const el = document.getElementById('globalProxyTest');
  el.textContent = '测试中…';
  try {
    const r = await api('/api/proxies/test-url', { method: 'POST', body: { url } });
    el.innerHTML = r.ok
      ? `✅ 出口 IP: <b>${esc(r.exit_ip)}</b>（${r.elapsed_ms}ms）`
      : `❌ ${esc(r.message || '不可用')}`;
  } catch (e) { el.textContent = '❌ ' + e.message; }
}

// ---- 加密账号包迁移 ----

function showExportBundleModal() {
  openModal(`<h3>导出加密账号包</h3>
    <p style="font-size:13px;color:var(--c-text-light);margin-bottom:12px">导出全部账号的凭证（JWT / API Key / 设备指纹 / 本地快照），PBKDF2+AES-256-GCM 加密，可跨机器导入。</p>
    <div class="form-group"><label>加密密码</label><input type="password" id="expPass" placeholder="导入时需要相同密码"></div>
    <div class="form-group"><label>管理员密码（确认身份）</label><input type="password" id="expAdminPass" autocomplete="current-password"></div>
    <div class="actions"><button class="btn btn-secondary" onclick="closeModal()">取消</button>
    <button class="btn btn-primary" onclick="doExportBundle()">生成账号包</button></div>`);
}

async function doExportBundle() {
  const pass = document.getElementById('expPass').value;
  const adminPass = document.getElementById('expAdminPass').value;
  if (!pass) return toast('请设置密码', 'error');
  if (!adminPass) return toast('请输入管理员密码', 'error');
  try {
    const r = await api('/api/accounts/export', { method: 'POST', body: { password: pass, admin_password: adminPass } });
    openModal(`<h3>账号包已生成</h3>
      <div class="form-group"><textarea id="bundleText" style="min-height:160px">${esc(r.bundle)}</textarea></div>
      <div class="actions"><button class="btn btn-secondary" onclick="closeModal()">关闭</button>
      <button class="btn btn-primary" onclick="navigator.clipboard.writeText(document.getElementById('bundleText').value).then(()=>toast('已复制'))">复制</button></div>`);
  } catch (e) { toast(e.message, 'error'); }
}

function showImportBundleModal() {
  openModal(`<h3>导入加密账号包</h3>
    <div class="form-group"><label>解密密码</label><input type="password" id="impPass"></div>
    <div class="form-group"><label>账号包内容（zcb1: 开头）</label><textarea id="impBundle" placeholder="zcb1:..."></textarea></div>
    <div class="actions"><button class="btn btn-secondary" onclick="closeModal()">取消</button>
    <button class="btn btn-primary" onclick="doImportBundle()">导入</button></div>`);
}

async function doImportBundle() {
  try {
    const r = await api('/api/accounts/import/bundle', { method: 'POST', body: {
      password: document.getElementById('impPass').value,
      bundle: document.getElementById('impBundle').value.trim(),
    }});
    closeModal(); toast(`导入成功：${r.imported} 个账号`); switchSection('accounts');
  } catch (e) { toast(e.message, 'error'); }
}

// ---- LLM 测试 ----

let llmHistory = [];

async function loadLlmKey() {
  try {
    const d = await api('/api/settings/api-key');
    const el = document.getElementById('llmKey');
    if (el) el.value = d.api_key || '';
  } catch (e) { /* ignore */ }
}

// 从 SSE 行中提取增量文本（按协议）
function llmDeltaText(proto, data) {
  try {
    const j = JSON.parse(data);
    if (proto === 'messages') {
      if (j.type === 'content_block_delta' && j.delta && j.delta.type === 'text_delta') return j.delta.text || '';
      return '';
    }
    if (proto === 'chat') {
      const c = (j.choices || [])[0];
      return (c && c.delta && c.delta.content) || '';
    }
    if (proto === 'responses') {
      if (j.type === 'response.output_text.delta') return j.delta || '';
      return '';
    }
  } catch (e) { /* ignore */ }
  return '';
}

// 从 SSE 行中提取增量思考文本（思考型模型可能只输出 thinking）
function llmDeltaThink(proto, data) {
  try {
    const j = JSON.parse(data);
    if (proto === 'messages') {
      if (j.type === 'content_block_delta' && j.delta && j.delta.type === 'thinking_delta') return j.delta.thinking || '';
      return '';
    }
    if (proto === 'chat') {
      const c = (j.choices || [])[0];
      return (c && c.delta && c.delta.reasoning_content) || '';
    }
    if (proto === 'responses') {
      if (j.type === 'response.reasoning_summary_text.delta') return j.delta || '';
      return '';
    }
  } catch (e) { /* ignore */ }
  return '';
}

function llmUsageFrom(json, proto) {
  const u = json && json.usage;
  if (!u) return null;
  if (proto === 'chat') return { in: u.prompt_tokens, out: u.completion_tokens };
  if (proto === 'responses') return { in: u.input_tokens, out: u.output_tokens };
  return { in: u.input_tokens, out: u.output_tokens };
}

async function runLlmTest() {
  const proto = document.getElementById('llmProto').value;
  const model = document.getElementById('llmModel').value.trim() || 'GLM-4.5-Flash';
  const maxTokens = Number(document.getElementById('llmMaxTokens').value || 256);
  const stream = document.getElementById('llmStream').checked;
  const prompt = document.getElementById('llmPrompt').value || '你好';
  const key = document.getElementById('llmKey').value;
  const btn = document.getElementById('llmRunBtn');
  const resultEl = document.getElementById('llmResult');
  const contentEl = document.getElementById('llmContent');
  if (!key) { toast('缺少网关 Key，点击「刷新 Key」', 'error'); return; }

  const path = proto === 'messages' ? '/v1/messages' : proto === 'chat' ? '/v1/chat/completions' : '/v1/responses';
  let body;
  if (proto === 'messages') body = { model, max_tokens: maxTokens, stream, messages: [{ role: 'user', content: prompt }] };
  else if (proto === 'chat') body = { model, max_tokens: maxTokens, stream, messages: [{ role: 'user', content: prompt }] };
  else body = { model, max_output_tokens: maxTokens, stream, input: prompt };
  const headers = { 'Content-Type': 'application/json' };
  if (proto === 'messages') headers['x-api-key'] = key; else headers['Authorization'] = 'Bearer ' + key;

  btn.disabled = true;
  resultEl.innerHTML = '<div class="kv-item"><div class="k">状态</div><div class="v">运行中…</div></div>';
  contentEl.textContent = '';
  const t0 = performance.now();
  let ttft = 0, text = '', think = '', events = 0, usage = null, status = 0;
  try {
    const resp = await fetch(path, { method: 'POST', headers, body: JSON.stringify(body) });
    status = resp.status;
    if (stream && resp.body) {
      const reader = resp.body.getReader();
      const dec = new TextDecoder();
      let buf = '';
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        if (!ttft) ttft = Math.round(performance.now() - t0);
        buf += dec.decode(value, { stream: true });
        let idx;
        while ((idx = buf.indexOf('\n\n')) >= 0) {
          const frame = buf.slice(0, idx); buf = buf.slice(idx + 2);
          const dl = frame.split('\n').find(l => l.startsWith('data:'));
          if (!dl) continue;
          const data = dl.slice(5).trim();
          if (!data || data === '[DONE]') continue;
          events++;
          text += llmDeltaText(proto, data);
          think += llmDeltaThink(proto, data);
          try { const j = JSON.parse(data); const u = llmUsageFrom(j, proto); if (u && (u.in || u.out)) usage = u; } catch (e) { }
        }
      }
    } else {
      const j = await resp.json();
      if (!ttft) ttft = Math.round(performance.now() - t0);
      if (resp.ok) {
        if (proto === 'messages') {
          text = (j.content || []).filter(c => c.type === 'text').map(c => c.text).join('');
          think = (j.content || []).filter(c => c.type === 'thinking').map(c => c.thinking).join('');
        } else if (proto === 'chat') {
          const m = ((j.choices || [])[0] || {}).message || {};
          text = m.content || '';
          think = m.reasoning_content || '';
        } else {
          text = j.output_text || '';
          think = (j.output || []).filter(o => o.type === 'reasoning').map(o => (o.summary || []).map(s => s.text).join('')).join('');
        }
        usage = llmUsageFrom(j, proto);
      } else {
        text = JSON.stringify(j).slice(0, 500);
      }
    }
  } catch (e) {
    status = 0; text = '请求失败: ' + e.message;
  }
  const latency = Math.round(performance.now() - t0);
  btn.disabled = false;
  const ok = status >= 200 && status < 300;
  resultEl.innerHTML = `
    <div class="kv-item"><div class="k">HTTP 状态</div><div class="v" style="color:${ok ? 'var(--c-success-dark)' : 'var(--c-danger)'}">${status || '网络错误'}</div></div>
    <div class="kv-item"><div class="k">总延迟</div><div class="v">${latency}ms</div></div>
    <div class="kv-item"><div class="k">首字 TTFT</div><div class="v">${stream ? ttft + 'ms' : '-'}</div></div>
    <div class="kv-item"><div class="k">Tokens in/out</div><div class="v">${usage ? usage.in + ' / ' + usage.out : '-'}</div></div>
    <div class="kv-item"><div class="k">SSE 事件数</div><div class="v">${stream ? events : '-'}</div></div>
    <div class="kv-item"><div class="k">协议</div><div class="v">${proto}${stream ? ' (stream)' : ''}</div></div>`;
  contentEl.textContent = text
    ? text
    : (think ? '【模型仅输出思考过程（max_tokens 不足或未产出正文）】\n' + think : '（空响应）');
  llmHistory.unshift({ t: new Date().toLocaleTimeString(), proto, model, status, latency, ttft, ok });
  document.getElementById('llmHistory').innerHTML = llmHistory.slice(0, 10).map(h =>
    `<div>${esc(h.t)} · ${esc(h.proto)} · ${esc(h.model)} · <span style="color:${h.ok ? 'var(--c-success-dark)' : 'var(--c-danger)'}">${h.status}</span> · ${h.latency}ms${h.ttft ? ' / ttft ' + h.ttft + 'ms' : ''}</div>`).join('');
  if (ok) toast('测试完成'); else toast('测试返回 ' + status, 'error');
}

function clearLlmHistory() {
  llmHistory = [];
  document.getElementById('llmHistory').innerHTML = '（无）';
  document.getElementById('llmResult').innerHTML = '';
  document.getElementById('llmContent').textContent = '（尚未运行）';
}

// setLlmPrompt 快捷提示词填入
function setLlmPrompt(text) {
  const el = document.getElementById('llmPrompt');
  el.value = text;
  el.focus();
}

// ---- 启动 ----

function loadAll() {
  loadDashboard();
  loadGroups().then(loadAccounts);
}

setInterval(() => {
  if (document.getElementById('mainApp').style.display !== 'none') {
    if (document.getElementById('section-dashboard').classList.contains('active')) loadDashboard();
    if (document.getElementById('section-activity').classList.contains('active')) { pollRunning(); }
  }
}, 5000);

checkAuth();
