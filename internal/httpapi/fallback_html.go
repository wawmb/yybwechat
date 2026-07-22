// Package httpapi 的 fallback HTML 和 OpenAPI spec 定义。
// 当本地模板文件缺失时，服务将使用内嵌的 HTML 回退页面。
package httpapi

// fallbackIndexHTML 是首页的内嵌后备 HTML。
const fallbackIndexHTML = `<!doctype html><html lang="zh-CN">
<meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<link rel="icon" type="image/png" href="/static/favicon.png">
<title>yybwechat 控制台</title>
<style>
:root{--bg:oklch(1 0 0);--bg2:oklch(0.974 0.004 250);--sf:oklch(0.986 0.004 250);--ink:oklch(0.19 0.025 252);--mu:oklch(0.43 0.025 252);--line:oklch(0.885 0.012 250);--pri:oklch(0.54 0.205 3);--r:8px}
[data-theme="dark"]{--bg:oklch(0.18 0.018 252);--bg2:oklch(0.15 0.014 252);--sf:oklch(0.21 0.019 252);--ink:oklch(0.94 0.008 250);--mu:oklch(0.68 0.018 252);--line:oklch(0.30 0.018 252)}
body{margin:0;background:var(--bg2);color:var(--ink);font-family:system-ui,-apple-system,'Segoe UI','Microsoft YaHei',sans-serif;font-size:14px}
.head{background:var(--bg);border-bottom:1px solid var(--line);padding:16px 28px;display:flex;align-items:center;gap:12px;position:sticky;top:0}
.logo{width:36px;height:36px;border-radius:var(--r);display:grid;place-items:center;background:var(--ink);color:var(--bg);font-weight:800}
.title{font-size:18px;font-weight:760}.sub{color:var(--mu);font-size:12px}
main{max-width:960px;margin:24px auto;padding:0 24px;display:grid;gap:16px}
.card{background:var(--bg);border:1px solid var(--line);border-radius:var(--r);padding:20px}
.card h2{margin:0 0 8px;font-size:18px}.card p{margin:0 0 16px;color:var(--mu)}
.row{display:flex;gap:8px;flex-wrap:wrap}
.btn{padding:10px 14px;border-radius:var(--r);text-decoration:none;font-weight:680;transition:filter .16s}
.btn:hover{filter:brightness(1.12)}
.btn.p{background:var(--pri);color:#fff}.btn.s{border:1px solid var(--line);color:var(--ink);background:var(--sf)}
.stat{display:inline-flex;align-items:center;gap:7px;padding:6px 12px;border-radius:999px;background:var(--sf);border:1px solid var(--line);color:var(--mu);font-size:13px}
.stat strong{color:var(--ink)}.dot{width:8px;height:8px;border-radius:50%;background:var(--mu)}
.dot.ok{background:oklch(0.55 0.135 145)}.dot.bad{background:oklch(0.55 0.18 25)}
</style>
<body>
<div class="head"><div class="logo">Y</div><div><div class="title">yybwechat 控制台</div><div class="sub">模板文件缺失 · 服务仍可正常使用</div></div></div>
<main>
<div class="card">
<h2>系统入口</h2>
<p>模板文件未找到，请通过以下入口使用服务。正常部署后模板会自动加载。</p>
<div class="row">
<a class="btn p" href="/scan">➕ 扫码添加账号</a>
<a class="btn s" href="/docs/index.html">📖 Swagger 文档</a>
<a class="btn s" href="/openapi.json">📋 OpenAPI JSON</a>
<a class="btn s" href="/accounts">👤 账号列表 JSON</a>
</div>
</div>
<div class="card">
<h2>服务状态</h2>
<div class="row" id="stats">
<span class="stat"><span class="dot" id="hd"></span><span id="ht">检测中</span></span>
</div>
</div>
</main>
<script>
fetch("/health").then(r=>r.json()).then(d=>{
 var ok=d.code===0;document.getElementById("hd").className="dot "+(ok?"ok":"bad");
 document.getElementById("ht").textContent=ok?"服务正常":"服务异常";
}).catch(()=>{document.getElementById("hd").className="dot bad";document.getElementById("ht").textContent="无法连接"});
fetch("/accounts").then(r=>r.json()).then(d=>{
 var n=(d.data||[]).length;
 document.getElementById("stats").innerHTML+='<span class="stat">账号 <strong>'+n+'</strong></span>';
}).catch(()=>{});
</script>
</body></html>`

// fallbackScanHTML 是扫码页的内嵌后备 HTML。
const fallbackScanHTML = `<!doctype html><html lang="zh-CN">
<meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<link rel="icon" type="image/png" href="/static/favicon.png">
<title>扫码添加账号</title>
<style>
:root{--bg:oklch(1 0 0);--bg2:oklch(0.974 0.004 250);--ink:oklch(0.19 0.025 252);--mu:oklch(0.43 0.025 252);--faint:oklch(0.58 0.018 252);--line:oklch(0.885 0.012 250);--pri:oklch(0.54 0.205 3);--suc:oklch(0.55 0.135 145);--err:oklch(0.55 0.18 25);--r:10px}
[data-theme="dark"]{--bg:oklch(0.18 0.018 252);--bg2:oklch(0.15 0.014 252);--ink:oklch(0.94 0.008 250);--mu:oklch(0.68 0.018 252);--faint:oklch(0.52 0.016 252);--line:oklch(0.30 0.018 252)}
body{margin:0;min-height:100vh;display:grid;place-items:center;background:var(--bg2);color:var(--ink);font-family:system-ui,-apple-system,'Segoe UI','Microsoft YaHei',sans-serif;font-size:14px;padding:16px}
.card{width:min(400px,100%);background:var(--bg);border:1px solid var(--line);border-radius:var(--r);padding:28px;text-align:center}
h1{margin:0 0 8px;font-size:20px;font-weight:760}.sub{color:var(--mu);margin:0 0 20px;font-size:13px}
.qr{width:220px;height:220px;margin:0 auto 16px;border:1px solid var(--line);border-radius:var(--r);display:grid;place-items:center;overflow:hidden}
.qr img{width:100%;height:100%;object-fit:contain}
#status{margin:0 0 6px;font-size:14px;font-weight:680;min-height:22px}
#cd{margin:0 0 20px;font-size:12px;color:var(--faint);min-height:16px}.expiring{color:var(--err)!important}
#result{display:none;background:oklch(0.94 0.035 145);border:1px solid var(--suc);border-radius:var(--r);padding:12px;margin-bottom:16px;text-align:left;font-size:13px}
.btn{border:none;border-radius:var(--r);padding:10px 16px;font:inherit;font-weight:680;cursor:pointer;text-decoration:none;display:inline-flex;align-items:center;gap:6px;transition:filter .16s}
.btn:hover{filter:brightness(1.12)}.btn.p{background:var(--pri);color:#fff}.btn.s{border:1px solid var(--line);background:var(--bg);color:var(--ink)}
.row{display:flex;gap:8px;justify-content:center;flex-wrap:wrap}
</style>
<body>
<div class="card">
<h1>扫码添加账号</h1>
<p class="sub">使用<strong>手机微信扫一扫</strong>扫描二维码</p>
<div class="qr" id="qr"><span style="color:var(--faint)">生成中…</span></div>
<p id="status">正在获取二维码…</p>
<p id="cd"></p>
<div id="result"></div>
<div class="row">
<button class="btn p" onclick="newQR()">🔄 重新生成</button>
<a class="btn s" href="/">← 返回首页</a>
</div>
</div>
<script>
(function(){
 var s=localStorage.getItem("yybwechat-theme");
 if(s==="dark"||(!s&&matchMedia("(prefers-color-scheme:dark)").matches))document.documentElement.setAttribute("data-theme","dark");
})();
var sid,timer,cd,secs=0,TTL=180;
function api(url,opts){return fetch(url,opts).then(function(r){return r.text().then(function(t){var d;try{d=t?JSON.parse(t):null}catch(e){d=t};var e=d&&typeof d=="object"&&d.code!==undefined&&d.msg!==undefined&&d.data!==undefined;if(!r.ok||(e&&d.code!==0))throw new Error(e?d.msg:"HTTP "+r.status);return e?d.data:d})})}
function state(t,c){var s=document.getElementById("status");s.textContent=t;s.style.color=c||"inherit"}
function stop(){clearInterval(timer);clearInterval(cd);timer=cd=null}
function tick(){secs--;var e=document.getElementById("cd");e.textContent=secs>0?"有效期 "+secs+"s":"已过期";if(secs<=15)e.classList.add("expiring");if(secs<=0){stop();newQR()}}
function startCD(){secs=TTL;var e=document.getElementById("cd");e.textContent="有效期 "+secs+"s";e.classList.remove("expiring");cd=setInterval(tick,1e3)}
function newQR(){stop();document.getElementById("qr").innerHTML='<span style="color:var(--faint)">生成中…</span>';document.getElementById("result").style.display="none";state("正在获取…","");document.getElementById("cd").textContent="";
 api("/qr",{method:"POST"}).then(function(r){sid=r.session_id;document.getElementById("qr").innerHTML='<img src="'+r.image_url+'?t='+Date.now()+'" alt="二维码">';state("等待扫码","");startCD();timer=setInterval(poll,1500)}).catch(function(e){state("获取失败: "+e.message,oklch(0.55 0.18 25))})}
function poll(){if(!sid)return;api("/qr/"+sid+"/poll").then(function(r){
 switch(r.status){
  case"pending":state("等待扫码","");break;
  case"scanned":state("已扫码，请在手机上确认",oklch(0.44 0.112 190));break;
  case"authorized":case"confirmed":stop();state("授权成功，入库中…",oklch(0.55 0.135 145));
   api("/qr/"+sid+"/confirm",{method:"POST"}).then(function(a){
    var n=a.nickname||a.alias||a.openid;state("✓ 添加成功",oklch(0.55 0.135 145));
    document.getElementById("result").style.display="block";
    document.getElementById("result").innerHTML='<div>昵称：'+n+'</div><div>openid：'+a.openid+'</div>';
    document.getElementById("qr").innerHTML='<span style="font-size:40px">✓</span>';
   }).catch(function(e){state("入库失败: "+e.message,oklch(0.55 0.18 25))});break;
  case"expired":case"unknown":stop();state("已过期，重新生成中",oklch(0.55 0.18 25));setTimeout(newQR,800);break;
  case"cancelled":stop();document.getElementById("cd").textContent="已取消";state("已取消",oklch(0.55 0.18 25));break;
 }
}).catch(function(){})}
newQR();
</script>
</body></html>`

// openAPISpec 是 OpenAPI 3.0 规范的运行时实例。
var openAPISpec = newOpenAPISpec()
