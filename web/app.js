/* UniGate WebUI */
"use strict";

// ---- 基础 ----
const $ = (sel) => document.querySelector(sel);
const $$ = (sel) => Array.from(document.querySelectorAll(sel));

let TOKEN = localStorage.getItem("unigate_token") || "";
let STATE = null;          // /admin/api/state 缓存
let editChannel = null;    // 正在编辑的渠道对象（深拷贝）
let logsTimer = null;

function toast(msg, isError) {
  let el = $(".toast");
  if (el) el.remove();
  el = document.createElement("div");
  el.className = "toast" + (isError ? " error" : "");
  el.textContent = msg;
  document.body.appendChild(el);
  setTimeout(() => el.remove(), isError ? 5000 : 2500);
}

async function api(method, path, body) {
  const opts = { method, headers: {} };
  if (TOKEN) opts.headers["Authorization"] = "Bearer " + TOKEN;
  if (body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  const resp = await fetch(path, opts);
  const text = await resp.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch { data = { raw: text }; }
  if (resp.status === 401 && path !== "/login") { logout(); throw new Error("登录已过期"); }
  if (!resp.ok) {
    const msg = (data && (data.error && (data.error.message || data.error))) || ("HTTP " + resp.status);
    throw new Error(typeof msg === "string" ? msg : JSON.stringify(msg));
  }
  return data;
}

const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));

// ---- 登录 ----
function showLogin() {
  $("#loginView").classList.remove("hidden");
  $("#appView").classList.add("hidden");
}
function showApp() {
  $("#loginView").classList.add("hidden");
  $("#appView").classList.remove("hidden");
}
function logout() {
  localStorage.removeItem("unigate_token");
  TOKEN = "";
  if (logsTimer) clearInterval(logsTimer);
  showLogin();
}

$("#loginForm").addEventListener("submit", async (e) => {
  e.preventDefault();
  $("#loginErr").classList.add("hidden");
  try {
    const resp = await fetch("/login", {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ username: $("#loginUser").value, password: $("#loginPass").value }),
    });
    const data = await resp.json();
    if (!resp.ok) throw new Error(data && data.error ? (data.error.message || data.error) : "登录失败");
    TOKEN = data.token;
    localStorage.setItem("unigate_token", TOKEN);
    $("#whoami").textContent = data.user || "";
    showApp();
    await loadState();
  } catch (err) {
    $("#loginErr").textContent = err.message;
    $("#loginErr").classList.remove("hidden");
  }
});
$("#logoutBtn").addEventListener("click", logout);

// ---- Tab 切换 ----
$$(".tab").forEach((btn) => btn.addEventListener("click", () => {
  $$(".tab").forEach((b) => b.classList.remove("active"));
  btn.classList.add("active");
  $$(".tabpane").forEach((p) => p.classList.add("hidden"));
  $("#tab-" + btn.dataset.tab).classList.remove("hidden");
  if (btn.dataset.tab === "logs") refreshLogs();
  if (btn.dataset.tab === "usage") refreshUsage();
  if (btn.dataset.tab === "leases") refreshLeases();
}));

// ---- 状态加载 ----
async function loadState() {
  STATE = await api("GET", "/admin/api/state");
  renderChannels();
  renderGWKeys();
  if (!$("#tab-leases").classList.contains("hidden")) refreshLeases();
}

// ---- 渠道列表 ----
// 渠道过滤：按关键字（名称/分组/BaseURL/模型）与分组下拉筛选
function filterChannels(chans) {
  const q = ($("#channelSearch").value || "").trim().toLowerCase();
  const g = $("#channelGroupFilter").value;
  return chans.filter((ch) => {
    if (g && (ch.group || "") !== g) return false;
    if (!q) return true;
    const hay = [ch.name, ch.group, ch.base_url, (ch.models || []).join(" ")].join(" ").toLowerCase();
    return hay.includes(q);
  });
}

// 分组下拉选项 + 编辑器 datalist 候选
function refreshGroupOptions() {
  const chans = (STATE && STATE.channels) || [];
  const groups = [...new Set(chans.map((c) => (c.group || "").trim()).filter(Boolean))].sort();
  const sel = $("#channelGroupFilter");
  const cur = sel.value;
  sel.innerHTML = '<option value="">全部分组</option>' +
    groups.map((g) => `<option value="${esc(g)}">${esc(g)}</option>`).join("");
  if (groups.includes(cur)) sel.value = cur;
  $("#groupSuggestions").innerHTML = groups.map((g) => `<option value="${esc(g)}">`).join("");
}

function renderChannels() {
  refreshGroupOptions();
  const wrap = $("#channelList");
  const chans = filterChannels((STATE && STATE.channels) || []);
  if (!(STATE && STATE.channels || []).length) {
    wrap.innerHTML = `<p class="muted">还没有渠道。点击右上角「新建渠道」添加第一个 OpenAI 兼容上游。</p>`;
    return;
  }
  if (!chans.length) {
    wrap.innerHTML = `<p class="muted">没有匹配筛选条件的渠道。</p>`;
    return;
  }
  wrap.innerHTML = chans.map((ch) => {
    const keys = ch.keys || [];
    const keyLines = keys.map((k) => {
      const p = k.proxy && k.proxy.kind ? proxyDesc(k.proxy) : "直连";
      return `<div class="key-line">
        <span class="badge ${k.enabled ? "on" : "off"}">${k.enabled ? "启用" : "停用"}</span>
        <span>${esc(k.name || "(未命名)")}</span>
        <span class="pname">代理: ${esc(p)}</span>
      </div>`;
    }).join("");
    const modelLine = (ch.models || []).length
      ? `<div class="key-line"><span class="pname">模型 ${ch.models.length} 个：${esc(ch.models.slice(0, 4).join("、"))}${ch.models.length > 4 ? " …" : ""}</span></div>`
      : "";
    return `<div class="channel-card" data-id="${esc(ch.id)}">
      <div class="head">
        <span class="badge ${ch.enabled ? "on" : "off"}">${ch.enabled ? "启用" : "停用"}</span>
        <span class="name">${esc(ch.name)}</span>
        ${ch.group ? `<span class="badge group">${esc(ch.group)}</span>` : ""}
        <span class="muted">${esc(ch.base_url)}</span>
        ${ch.rewrite_reasoning ? '<span class="badge info">reasoning改写</span>' : ""}
        ${ch.cooldown_scope === "key_model" ? '<span class="badge info">按(Key,模型)冷却</span>' : ""}
        <span class="spacer"></span>
        <button class="btn small" data-act="edit">编辑</button>
        <button class="btn small danger" data-act="del">删除</button>
      </div>
      ${modelLine}
      ${keyLines || '<div class="key-line muted">无 key</div>'}
    </div>`;
  }).join("");

  wrap.querySelectorAll('[data-act="edit"]').forEach((b) => b.addEventListener("click", () => {
    const id = b.closest(".channel-card").dataset.id;
    openChannelEditor(JSON.parse(JSON.stringify(chans.find((c) => c.id === id))));
  }));
  wrap.querySelectorAll('[data-act="del"]').forEach((b) => b.addEventListener("click", async () => {
    const id = b.closest(".channel-card").dataset.id;
    const ch = chans.find((c) => c.id === id);
    if (!confirm(`删除渠道「${ch.name}」？其绑定的代理池租约将被释放。`)) return;
    try { await api("DELETE", "/admin/api/channels/" + id); toast("已删除"); await loadState(); }
    catch (e) { toast(e.message, true); }
  }));
}
$("#channelSearch").addEventListener("input", renderChannels);
$("#channelGroupFilter").addEventListener("change", renderChannels);

function proxyDesc(p) {
  if (p.kind === "static") return p.url || "static";
  if (p.kind === "ipv6pool") {
    const parts = [`池 ${p.pool_url}`, `租约 ${p.lease_id || "(自动)"}`];
    if (p.share) parts.push("跨渠道复用(同渠道各用IP/跨渠道可共用)");
    if (p.rotate_on_net_err) parts.push("网络失败换IP");
    if (p.rotate_statuses && p.rotate_statuses.length) parts.push(`状态码${p.rotate_statuses.join("/")}换IP`);
    if (p.rotate_interval_sec) parts.push(`${p.rotate_interval_sec}s换IP`);
    if (p.rotate_requests) parts.push(`${p.rotate_requests}次换IP`);
    return parts.join(", ");
  }
  return "直连";
}

// ---- 渠道编辑器 ----
$("#addChannelBtn").addEventListener("click", () => openChannelEditor({
  id: "", name: "", group: "", base_url: "", models_url: "", models: [], headers: {},
  rewrite_reasoning: false, cooldown_scope: "key", enabled: true, keys: [],
}));

function openChannelEditor(ch) {
  editChannel = ch;
  $("#channelModalTitle").textContent = ch.id ? "编辑渠道：" + ch.name : "新建渠道";
  $("#chName").value = ch.name || "";
  $("#chGroup").value = ch.group || "";
  $("#chBaseURL").value = ch.base_url || "";
  $("#chModelsURL").value = ch.models_url || "";
  $("#chModels").value = (ch.models || []).join("\n");
  $("#chEnabled").checked = !!ch.enabled;
  $("#chRewrite").checked = !!ch.rewrite_reasoning;
  $("#chCooldownScope").value = ch.cooldown_scope === "key_model" ? "key_model" : "key";
  renderHeaderRows(ch.headers || {});
  renderKeyBlocks(ch.keys || []);
  renderModelChips();
  $("#channelErr").textContent = "";
  $("#channelModal").classList.remove("hidden");
}

// 可用模型 chips 展示（跟随左侧文本框实时变化）；freeSet 非空时为对应模型标注「免费」
let freeModelSet = new Set();
function renderModelChips() {
  const el = $("#chModelList");
  const models = $("#chModels").value.split("\n").map((s) => s.trim()).filter(Boolean);
  el.innerHTML = models.map((m) =>
    `<span class="chip">${esc(m)}${freeModelSet.has(m) ? '<i class="badge free">免费</i>' : ""}</span>`
  ).join("");
}
$("#chModels").addEventListener("input", () => { renderModelChips(); });

$$('[data-close="channelModal"]').forEach((b) => b.addEventListener("click", () => $("#channelModal").classList.add("hidden")));

// 自定义请求头
function renderHeaderRows(headers) {
  const wrap = $("#chHeaders");
  wrap.innerHTML = "";
  const entries = Object.entries(headers || {});
  if (!entries.length) entries.push(["", ""]);
  entries.forEach(([k, v]) => wrap.appendChild(headerRow(k, v)));
}
function headerRow(name, value) {
  const div = document.createElement("div");
  div.className = "row";
  div.innerHTML = `<input placeholder="头名（如 http-referer）" value="${esc(name)}" style="max-width:260px">
    <input placeholder="值（空=不发送）" value="${esc(value)}">
    <button class="btn small danger">删除</button>`;
  div.querySelector(".danger").addEventListener("click", () => div.remove());
  return div;
}
$("#addHeaderBtn").addEventListener("click", () => $("#chHeaders").appendChild(headerRow("", "")));

// key 块
function renderKeyBlocks(keys) {
  const wrap = $("#chKeys");
  wrap.innerHTML = "";
  keys.forEach((k) => wrap.appendChild(keyBlock(k)));
  if (!keys.length) wrap.appendChild(keyBlock({ name: "", api_key: "", enabled: true }));
}
function keyBlock(k) {
  const div = document.createElement("div");
  div.className = "keyblock";
  const kind = (k.proxy && k.proxy.kind) || "";
  div.innerHTML = `
    <div class="head">
      <input class="kb-name" placeholder="名称" value="${esc(k.name || "")}">
      <input class="kb-key" placeholder="上游 API Key（无需鉴权的渠道可留空）" value="${esc(k.api_key || "")}" style="flex:1">
      <label class="inline"><input type="checkbox" class="kb-enabled" ${k.enabled ? "checked" : ""}> 启用</label>
      <button class="btn small danger kb-del">删除</button>
    </div>
    <div class="proxybox">
      <div class="row" style="margin:4px 0">
        <label class="inline"><input type="radio" name="pk" value="" ${!kind ? "checked" : ""}> 直连</label>
        <label class="inline"><input type="radio" name="pk" value="static" ${kind === "static" ? "checked" : ""}> 固定代理</label>
        <label class="inline"><input type="radio" name="pk" value="ipv6pool" ${kind === "ipv6pool" ? "checked" : ""}> IPv6 代理池</label>
        <span class="spacer"></span>
        <button class="btn small kb-test">测试</button>
        <button class="btn small kb-rotate">换IP</button>
        <button class="btn small kb-release">释放租约</button>
      </div>
      <div class="kb-static ${kind === "static" ? "" : "hidden"}">
        <label>代理 URL <input class="kb-url" placeholder="http://user:pass@host:port 或 socks5://user:pass@host:port" value="${esc(k.proxy && k.proxy.url || "")}"></label>
      </div>
      <div class="kb-pool ${kind === "ipv6pool" ? "" : "hidden"}">
        <div class="grid3">
          <label>池管理端 URL <input class="kb-poolurl" placeholder="http://1.2.3.4:8080" value="${esc(k.proxy && k.proxy.pool_url || "")}"></label>
          <label>池 Token（可空） <input class="kb-pooltoken" value="${esc(k.proxy && k.proxy.pool_token || "")}"></label>
          <label>租约 ID（空=自动 gw-keyID） <input class="kb-leaseid" value="${esc(k.proxy && k.proxy.lease_id || "")}"></label>
        </div>
        <div class="grid3">
          <label>SOCKS5 地址（空=池管理端同机） <input class="kb-sockshost" placeholder="1.2.3.4" value="${esc(k.proxy && k.proxy.socks_host || "")}"></label>
          <label>换IP状态码（逗号分隔，如 403,429） <input class="kb-rotstatus" value="${esc((k.proxy && k.proxy.rotate_statuses || []).join(","))}"></label>
          <label>每 N 次请求换IP（0=关闭） <input class="kb-rotreq" type="number" min="0" value="${(k.proxy && k.proxy.rotate_requests) || 0}"></label>
        </div>
        <div class="row" style="margin:4px 0">
          <label class="inline"><input type="checkbox" class="kb-persist" ${k.proxy && k.proxy.persistent ? "checked" : ""}> 常驻租约（免空闲回收）</label>
          <label class="inline"><input type="checkbox" class="kb-rotnet" ${k.proxy && k.proxy.rotate_on_net_err ? "checked" : ""}> 网络失败自动换IP</label>
          <label class="inline">每 N 秒换IP（0=关闭）<input class="kb-rotsec" type="number" min="0" value="${(k.proxy && k.proxy.rotate_interval_sec) || 0}" style="width:90px"></label>
        </div>
      </div>
    </div>`;
  div.querySelector(".kb-del").addEventListener("click", () => div.remove());
  const syncProxyKind = () => {
    const val = div.querySelector('input[name=pk]:checked').value;
    div.querySelector(".kb-static").classList.toggle("hidden", val !== "static");
    div.querySelector(".kb-pool").classList.toggle("hidden", val !== "ipv6pool");
  };
  div.querySelectorAll('input[name=pk]').forEach((r) => r.addEventListener("change", syncProxyKind));

  // 池操作
  const needSaved = () => {
    if (!editChannel.id) { toast("请先保存渠道，再执行池操作", true); return false; }
    const keyID = div.dataset.keyId || "";
    if (!keyID) { toast("请先保存渠道以生成 key ID", true); return false; }
    return true;
  };
  div.querySelector(".kb-test").addEventListener("click", async () => {
    try {
      const r = await api("POST", "/admin/api/testkey", {
        channel_id: editChannel.id, key_id: div.dataset.keyId || "",
        model: $("#chModels").value.split("\n")[0].trim() || "",
      });
      if (r.ok) toast(`测试成功 ${r.status}（${r.latency_ms}ms，经 ${r.proxy}）`);
      else toast("测试失败: " + (r.error || r.snippet || r.status), true);
    } catch (e) { toast(e.message, true); }
  });
  div.querySelector(".kb-rotate").addEventListener("click", async () => {
    if (!needSaved()) return;
    try {
      const lease = await api("POST", "/admin/api/pool/rotate", { channel_id: editChannel.id, key_id: div.dataset.keyId });
      toast("已换 IP: " + (lease.ipv6 || "(未知)"));
    } catch (e) { toast(e.message, true); }
  });
  div.querySelector(".kb-release").addEventListener("click", async () => {
    if (!needSaved()) return;
    try {
      await api("POST", "/admin/api/pool/release", { channel_id: editChannel.id, key_id: div.dataset.keyId });
      toast("租约已释放");
    } catch (e) { toast(e.message, true); }
  });
  return div;
}
$("#addKeyBtn").addEventListener("click", () => $("#chKeys").appendChild(keyBlock({ name: "", api_key: "", enabled: true })));

// 从上游拉取模型列表（用渠道 key 鉴权），填充可用模型并临时标注免费模型
$("#fetchModelsBtn").addEventListener("click", async () => {
  if (!editChannel.id) { toast("请先保存渠道（生成 key 后）再拉取模型列表", true); return; }
  const btn = $("#fetchModelsBtn");
  btn.disabled = true;
  btn.textContent = "拉取中…";
  try {
    const r = await api("POST", `/admin/api/channels/${encodeURIComponent(editChannel.id)}/fetch-models`, {});
    $("#chModels").value = (r.models || []).join("\n");
    freeModelSet = new Set(r.free_models || []);
    renderModelChips();
    const nFree = (r.free_models || []).length;
    toast(`已拉取 ${r.total} 个模型（key: ${r.key_used || "?"}）${nFree ? `，免费 ${nFree} 个` : ""}`);
  } catch (e) {
    toast(e.message, true);
  } finally {
    btn.disabled = false;
    btn.textContent = "⤓ 从上游拉取模型列表";
  }
});

// 保存渠道
$("#channelSaveBtn").addEventListener("click", async () => {
  const headers = {};
  $$("#chHeaders .row").forEach((row) => {
    const inputs = row.querySelectorAll("input");
    const name = inputs[0].value.trim(), value = inputs[1].value;
    if (name) headers[name] = value;
  });
  const models = $("#chModels").value.split("\n").map((s) => s.trim()).filter(Boolean);
  const keys = [];
  $$("#chKeys .keyblock").forEach((div) => {
    const kind = div.querySelector('input[name=pk]:checked').value;
    const k = {
      id: div.dataset.keyId || "",
      name: div.querySelector(".kb-name").value.trim(),
      api_key: div.querySelector(".kb-key").value.trim(),
      enabled: div.querySelector(".kb-enabled").checked,
      proxy: null,
    };
    if (kind === "static") {
      k.proxy = { kind: "static", url: div.querySelector(".kb-url").value.trim() };
    } else if (kind === "ipv6pool") {
      const statuses = div.querySelector(".kb-rotstatus").value.split(",").map((s) => parseInt(s.trim(), 10)).filter((n) => !isNaN(n));
      k.proxy = {
        kind: "ipv6pool",
        pool_url: div.querySelector(".kb-poolurl").value.trim(),
        pool_token: div.querySelector(".kb-pooltoken").value.trim(),
        lease_id: div.querySelector(".kb-leaseid").value.trim(),
        socks_host: div.querySelector(".kb-sockshost").value.trim(),
        persistent: div.querySelector(".kb-persist").checked,
        share: div.querySelector(".kb-share").checked,
        rotate_on_net_err: div.querySelector(".kb-rotnet").checked,
        rotate_statuses: statuses,
        rotate_interval_sec: parseInt(div.querySelector(".kb-rotsec").value, 10) || 0,
        rotate_requests: parseInt(div.querySelector(".kb-rotreq").value, 10) || 0,
      };
    }
    keys.push(k);
  });
  const ch = {
    id: editChannel.id || "",
    name: $("#chName").value.trim(),
    group: $("#chGroup").value.trim(),
    base_url: $("#chBaseURL").value.trim(),
    models_url: $("#chModelsURL").value.trim(),
    models,
    headers,
    rewrite_reasoning: $("#chRewrite").checked,
    cooldown_scope: $("#chCooldownScope").value,
    enabled: $("#chEnabled").checked,
    keys,
  };
  try {
    const saved = await api("PUT", "/admin/api/channels", ch);
    editChannel = saved;
    // 回填 key ID，便于后续池操作
    const savedKeys = saved.keys || [];
    $$("#chKeys .keyblock").forEach((div, i) => { div.dataset.keyId = savedKeys[i] ? savedKeys[i].id : ""; });
    toast("已保存");
    await loadState();
  } catch (e) {
    $("#channelErr").textContent = e.message;
  }
});

// ---- 通用密钥 ----
function renderGWKeys() {
  const tbody = $("#gwKeyTable tbody");
  const keys = (STATE && STATE.gateway_keys) || [];
  tbody.innerHTML = keys.map((k) => `<tr data-id="${esc(k.id)}">
    <td>${esc(k.name)}</td>
    <td><code class="gwkey">${esc(k.key)}</code> <button class="btn small" data-act="copy">复制</button></td>
    <td><label class="inline"><input type="checkbox" data-act="toggle" ${k.enabled ? "checked" : ""}> ${k.enabled ? "启用" : "停用"}</label></td>
    <td class="muted">${esc((k.created_at || "").replace("T", " ").slice(0, 19))}</td>
    <td><button class="btn small danger" data-act="del">删除</button></td>
  </tr>`).join("");

  tbody.querySelectorAll('[data-act="copy"]').forEach((b) => b.addEventListener("click", () => {
    navigator.clipboard.writeText(b.parentElement.querySelector("code").textContent).then(() => toast("已复制"));
  }));
  tbody.querySelectorAll('[data-act="toggle"]').forEach((c) => c.addEventListener("change", async () => {
    const tr = c.closest("tr");
    const k = keys.find((x) => x.id === tr.dataset.id);
    try { await api("PUT", "/admin/api/gwkeys", { id: k.id, name: k.name, key: k.key, enabled: c.checked }); await loadState(); }
    catch (e) { toast(e.message, true); }
  }));
  tbody.querySelectorAll('[data-act="del"]').forEach((b) => b.addEventListener("click", async () => {
    const id = b.closest("tr").dataset.id;
    if (!confirm("删除该通用密钥？使用它的下游将立即 401。")) return;
    try { await api("DELETE", "/admin/api/gwkeys/" + id); toast("已删除"); await loadState(); }
    catch (e) { toast(e.message, true); }
  }));
}

$("#addGWKeyBtn").addEventListener("click", async () => {
  const name = prompt("密钥名称（如 sub2api、cline-desktop）:");
  if (!name) return;
  try {
    const r = await api("PUT", "/admin/api/gwkeys", { name, key: "", enabled: true });
    await loadState();
    toast("已生成密钥: " + (r.key ? r.key.key : ""));
  } catch (e) { toast(e.message, true); }
});

// ---- 请求日志 ----
async function refreshLogs() {
  try {
    const data = await api("GET", "/admin/api/requests?limit=200");
    const tbody = $("#logTable tbody");
    const recs = data.records || [];
    if (!recs.length) {
      tbody.innerHTML = `<tr><td colspan="11" class="muted">暂无大模型请求记录（仅记录 /v1/* 网关接口请求，如 chat/completions、models、responses）。</td></tr>`;
      return;
    }
    tbody.innerHTML = recs.map((r) => `<tr>
      <td class="muted">${esc((r.time || "").replace("T", " ").slice(2, 19))}</td>
      <td class="muted">${esc(r.path || "")}</td>
      <td><span class="badge ${r.status < 400 ? "on" : "off"}">${r.status}</span></td>
      <td>${r.duration_ms}ms</td>
      <td>${esc(r.channel || "")}</td>
      <td>${esc(r.key || "")}</td>
      <td>${esc(r.model || "")}</td>
      <td class="muted">${r.prompt_tokens || 0} / ${r.completion_tokens || 0}</td>
      <td>${esc(r.user || "")}</td>
      <td>${esc(r.client_ip || "")}</td>
      <td class="err">${esc(r.error || "")}</td>
    </tr>`).join("");
  } catch (e) { toast(e.message, true); }
}
$("#logsRefresh").addEventListener("click", refreshLogs);
$("#logsAuto").addEventListener("change", (e) => {
  if (logsTimer) clearInterval(logsTimer);
  if (e.target.checked) logsTimer = setInterval(refreshLogs, 5000);
});

// ---- 用量 ----
async function refreshUsage() {
  const win = $("#usageWindow").value;
  try {
    const u = await api("GET", "/admin/api/usage?window=" + win);
    const fmt = (n) => Number(n).toLocaleString();
    $("#usageCards").innerHTML = `
      <div class="card"><div class="v">${fmt(u.requests)}</div><div class="k">请求</div></div>
      <div class="card"><div class="v">${fmt(u.errors)}</div><div class="k">错误</div></div>
      <div class="card"><div class="v">${fmt(u.prompt_tokens)}</div><div class="k">输入 tokens</div></div>
      <div class="card"><div class="v">${fmt(u.completion_tokens)}</div><div class="k">输出 tokens</div></div>
      <div class="card"><div class="v">${fmt(u.total_tokens)}</div><div class="k">总 tokens</div></div>`;
    const table = (title, rows) => {
      if (!rows || !rows.length) return "";
      return `<h4>${title}</h4><table class="tbl"><thead><tr><th>名称</th><th>请求</th><th>错误</th><th>输入</th><th>输出</th><th>合计</th></tr></thead><tbody>` +
        rows.map((r) => `<tr><td>${esc(r.name)}</td><td>${fmt(r.requests)}</td><td>${fmt(r.errors)}</td><td>${fmt(r.prompt_tokens)}</td><td>${fmt(r.completion_tokens)}</td><td>${fmt(r.total_tokens)}</td></tr>`).join("") +
        `</tbody></table>`;
    };
    $("#usageTables").innerHTML =
      table("按下游密钥", u.by_user) + table("按渠道", u.by_channel) + table("按模型", u.by_model) + table("按上游 key", u.by_key);
  } catch (e) { toast(e.message, true); }
}
$("#usageWindow").addEventListener("change", refreshUsage);

// ---- 本地代理池 ----
async function refreshLeases() {
  try {
    const data = await api("GET", "/admin/api/pool/leases");
    const q = ($("#leaseSearch").value || "").trim().toLowerCase();
    let leases = data.leases || [];
    const total = leases.length;
    if (q) {
      leases = leases.filter((l) =>
        [l.pool_url, l.lease_id, l.ipv6, (l.groups || []).join(" ")]
          .join(" ").toLowerCase().includes(q));
    }
    const tbody = $("#leaseTable tbody");
    if (!leases.length) {
      tbody.innerHTML = `<tr><td colspan="9" class="muted">${q ? `无匹配「${esc(q)}」的租约（共 ${total} 条）。` : "当前没有租约。发起一次请求或点击「测试」后自动申请。"}</td></tr>`;
      return;
    }
    tbody.innerHTML = leases.map((l) => `<tr data-pool="${esc(l.pool_url)}" data-lease="${esc(l.lease_id)}">
      <td class="muted">${esc(l.pool_url)}</td>
      <td><code>${esc(l.lease_id)}</code>${l.shared ? ' <span class="badge info">共享</span>' : ""}</td>
      <td class="muted">${esc((l.groups || []).join("、"))}</td>
      <td>${esc(l.ipv6)}</td>
      <td>${l.multiplex ? "复用基础端口" : esc(l.port)}</td>
      <td>${l.multiplex ? "multiplex" : "per_ipv6"}</td>
      <td>${l.requests}</td>
      <td class="muted">${esc((l.last_rotate || "").replace("T", " ").slice(0, 19))}</td>
      <td>
        <button class="btn small" data-act="lrotate">换IP</button>
        <button class="btn small danger" data-act="lrelease">释放</button>
      </td>
    </tr>`).join("");
    tbody.querySelectorAll('[data-act="lrotate"]').forEach((b) => b.addEventListener("click", async () => {
      const tr = b.closest("tr");
      try {
        const lease = await api("POST", "/admin/api/pool/rotate", { pool_url: tr.dataset.pool, lease_id: tr.dataset.lease });
        toast("已换 IP: " + (lease.ipv6 || "(未知)"));
        refreshLeases();
      } catch (e) { toast(e.message, true); }
    }));
    tbody.querySelectorAll('[data-act="lrelease"]').forEach((b) => b.addEventListener("click", async () => {
      const tr = b.closest("tr");
      if (!confirm(`释放租约 ${tr.dataset.lease}？共享租约的所有使用方将换到新出口（下次请求自动重新申请）。`)) return;
      try {
        await api("POST", "/admin/api/pool/release", { pool_url: tr.dataset.pool, lease_id: tr.dataset.lease });
        toast("已释放");
        refreshLeases();
      } catch (e) { toast(e.message, true); }
    }));
  } catch (e) { toast(e.message, true); }
}
$("#leasesRefresh").addEventListener("click", refreshLeases);
$("#leaseSearch").addEventListener("input", refreshLeases);
let leasesTimer = null;
$("#leasesAuto").addEventListener("change", (e) => {
  if (leasesTimer) clearInterval(leasesTimer);
  if (e.target.checked) leasesTimer = setInterval(refreshLeases, 5000);
});

// ---- 启动 ----
(async function init() {
  if (!TOKEN) { showLogin(); return; }
  try {
    showApp();
    await loadState();
  } catch {
    logout();
  }
})();
