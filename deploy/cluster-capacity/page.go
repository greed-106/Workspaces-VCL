package main

// 卷申请页(与实时容量条同源、同风格):只在这里显示 HDD 容量。
// 容量限制(单卷 100-1000G,且是每人总量上限)只在本页做,API 不限制。
const volumesPageHTML = `<!doctype html>
<html lang="zh-CN"><head>
<meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>HDD 持久卷</title>
<style>
  :root{color-scheme:light}
  body{margin:0;padding:24px;background:#fff;color:#18181b;
       font:14px/1.6 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,"Helvetica Neue",Arial,"PingFang SC","Microsoft YaHei",sans-serif}
  h1{font-size:18px;margin:0 0 16px}
  .bar{display:flex;flex-wrap:wrap;gap:16px;align-items:center;
       border:1px solid #e4e4e7;background:#fafafa;border-radius:8px;padding:10px 14px;font-size:13px;color:#52525b}
  .bar b{color:#18181b;font-weight:600}
  .warn{margin-top:8px;color:#b45309;font-size:12px}
  section{margin-top:24px;border:1px solid #e4e4e7;border-radius:8px;padding:16px}
  label{display:block;font-size:12px;color:#52525b;margin-bottom:4px}
  input{width:100%;box-sizing:border-box;padding:7px 9px;border:1px solid #d4d4d8;border-radius:6px;font-size:14px}
  .row{display:flex;gap:12px;flex-wrap:wrap;margin-bottom:12px}
  .row>div{flex:1;min-width:150px}
  button{padding:7px 14px;border-radius:6px;border:1px solid #d4d4d8;background:#18181b;color:#fff;font-size:13px;cursor:pointer}
  button.ghost{background:#fff;color:#18181b}
  button:disabled{opacity:.5;cursor:not-allowed}
  table{width:100%;border-collapse:collapse;margin-top:12px;font-size:13px}
  th,td{text-align:left;padding:8px 6px;border-bottom:1px solid #f4f4f5;vertical-align:top}
  th{color:#71717a;font-weight:500}
  .msg{margin-top:10px;font-size:13px}
  .msg.err{color:#b91c1c}.msg.ok{color:#15803d}
  code{background:#f4f4f5;padding:1px 5px;border-radius:4px;font-size:12px}
</style></head><body>
<h1>HDD 持久卷</h1>
<p style="color:#71717a;font-size:13px;margin:-8px 0 16px">申请一块 HDD 空间放冷数据;建好后到工作区里挂载它(路径 <code>/mnt/data</code>)。</p>
<div class="bar" id="bar">正在读取容量…</div>
<div class="warn" id="quotaWarn" style="display:none"></div>

<section>
  <div class="row">
    <div><label>所属用户</label><input id="owner" placeholder="例如 mjyang"></div>
    <div><label>卷名</label><input id="name" placeholder="例如 dataset(卷名)"></div>
    <div><label>容量(GB)</label><input id="size" type="number" value="100" min="100" max="1000" step="100"></div>
  </div>
  <button id="create">申请卷</button>
  <div class="msg" id="createMsg"></div>
</section>

<section>
  <b style="font-size:13px">已有卷</b>
  <table><thead><tr><th>卷名</th><th>用户</th><th>容量</th><th>状态</th><th>使用中</th><th></th></tr></thead>
  <tbody id="rows"><tr><td colspan="6" style="color:#a1a1aa">加载中…</td></tr></tbody></table>
</section>

<script>
const fmt = (gb) => gb >= 1024 ? (gb/1024).toFixed(2)+" TB" : Math.round(gb)+" GB";
const $ = (id) => document.getElementById(id);
// 只显示"我的"卷:Coder 顶部导航会带 ?owner=<用户名> 进来,也允许手工指定。
const params = new URLSearchParams(location.search);
const onlyOwner = params.get("owner") || "";
let all = [];
let volumes = [];

async function load() {
  const [hdd, vols] = await Promise.all([
    fetch("/hdd").then(r => r.json()),
    fetch("/volumes").then(r => r.json()),
  ]);
  all = vols.volumes || [];
  volumes = onlyOwner ? all.filter(v => v.owner === onlyOwner) : all;
  if (onlyOwner && !$("owner").value) $("owner").value = onlyOwner;
  $("bar").innerHTML =
    '<span><b>HDD 容量</b></span>' +
    '<span>总计 <b>' + fmt(hdd.total_gb) + '</b></span>' +
    '<span>已用 <b>' + fmt(hdd.used_gb) + '</b></span>' +
    '<span>空闲 <b>' + fmt(hdd.free_gb) + '</b></span>' +
    '<span>卷 <b>' + volumes.length + '</b> 个' + (onlyOwner ? '(仅显示 ' + onlyOwner + ' 的卷)' : '') + '</span>';
  if (!hdd.prjquota_active) {
    $("quotaWarn").style.display = "block";
    $("quotaWarn").textContent = "提示:硬配额尚未生效(需重启宿主后由定时任务启用),当前卷的容量限制为记账性质。";
  }
  render();
}

function render() {
  const rows = $("rows");
  if (!volumes.length) { rows.innerHTML = '<tr><td colspan="6" style="color:#a1a1aa">还没有卷</td></tr>'; return; }
  rows.innerHTML = volumes.map(v => {
    const inuse = (v.in_use_by || []).length ? v.in_use_by.join(", ") : "—";
    return '<tr><td><code>' + v.pvc + '</code></td><td>' + v.owner + '</td><td>' + fmt(v.size_gb) +
      '</td><td>' + v.phase + '</td><td>' + inuse +
      '</td><td><button class="ghost" data-del="' + v.pvc + '" data-name="' + v.name + '">删除</button></td></tr>';
  }).join("");
  rows.querySelectorAll("button[data-del]").forEach(b => b.onclick = () => del(b.dataset.del, b.dataset.name));
}

function msg(el, text, ok) { el.className = "msg " + (ok ? "ok" : "err"); el.textContent = text; }

$("create").onclick = async () => {
  const owner = $("owner").value.trim(), name = $("name").value.trim(), size = Number($("size").value);
  const out = $("createMsg");
  if (!owner || !name) { msg(out, "请填写所属用户和卷名", false); return; }
  if (!(size >= 100 && size <= 1000)) { msg(out, "容量需要在 100 - 1000 GB 之间", false); return; }
  if (onlyOwner && owner !== onlyOwner) { msg(out, "只能查看和管理自己的数据卷", false); return; }
  const used = volumes.filter(v => v.owner === owner).reduce((a, v) => a + v.size_gb, 0);
  if (used + size > 1000) {
    msg(out, "该用户已有 " + Math.round(used) + " GB,再加 " + size + " GB 会超过每人 1000 GB 的上限", false);
    return;
  }
  $("create").disabled = true;
  const r = await fetch("/volumes", {method:"POST", headers:{"Content-Type":"application/json"},
    body: JSON.stringify({owner, name, size_gb: size})});
  $("create").disabled = false;
  if (r.ok) { msg(out, "已创建", true); $("name").value = ""; load(); }
  else { const e = await r.json().catch(() => ({})); msg(out, e.error || "创建失败", false); }
};

async function del(pvc, name) {
  const typed = prompt("删除后数据不可恢复。请输入卷名以确认删除:" + name);
  if (typed !== name) return;
  const r = await fetch("/volumes/" + pvc, {method:"DELETE", headers:{"Content-Type":"application/json"},
    body: JSON.stringify({confirm: pvc})});
  if (!r.ok) { const e = await r.json().catch(() => ({})); alert(e.error || "删除失败"); return; }
  load();
}

load();
setInterval(load, 15000);
</script></body></html>
`
