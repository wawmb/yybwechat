package httpapi

// fallbackAccountLinkHTML 是一次性授权链接页面的内嵌后备版本。
//
// 为什么需要它：正式页面在 resource/templates/account-link.html，由运营者
// 随版本更新。如果部署时挂载的是旧的 resource 目录，这个文件就不存在——
// 此时打开链接的往往是「拿着手机、没有管理员口令」的人，让他看到一个 500
// 既无法自助解决、也没法反馈清楚。所以这里保留一个功能完整的精简版：
// 生成二维码 → 轮询 → 确认，三步都在。
//
// 精简版刻意不做视觉还原（不追求与正式页面一致），只保证这条路径能用。
const fallbackAccountLinkHTML = `<!doctype html><html lang="zh-CN">
<meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>账号授权</title>
<style>
body{margin:0;min-height:100vh;display:grid;place-items:center;padding:20px;
font-family:system-ui,-apple-system,"Segoe UI",sans-serif;background:#f3f7f8;color:#172529}
.panel{width:min(420px,100%);background:#fff;border:1px solid #d9e3e5;border-radius:16px;
padding:22px;text-align:center}
h1{font-size:19px;margin:0 0 8px}p{font-size:13px;color:#68797d;line-height:1.6;margin:0 0 14px}
.qr{width:240px;height:240px;margin:0 auto 14px;border:1px solid #e1e9ea;border-radius:12px;
display:grid;place-items:center;background:#fafcfc;color:#77868a;font-size:13px;overflow:hidden}
.qr img{width:100%;height:100%;object-fit:contain}
.state{min-height:22px;font-size:13px;color:#4e666a}.state.error{color:#a53b30}.state.ok{color:#2f7d46}
.hint{margin-top:14px;padding:10px;border-radius:9px;background:#f0f7f7;color:#547174;
font-size:12px;text-align:left}
</style>
<div class="panel">
  <h1 id="title">正在准备扫码</h1>
  <p id="desc"></p>
  <div class="qr" id="qr">加载中…</div>
  <div class="state" id="state"></div>
  <div class="hint" id="hint"></div>
</div>
<script>
(function(){
  var token = location.pathname.split('/').filter(Boolean).pop();
  var base = '/account-link/' + encodeURIComponent(token);
  var current = '', done = false, attempt = 0, timer = null;
  function $(id){ return document.getElementById(id); }
  function say(text, cls){ var el = $('state'); el.textContent = text || ''; el.className = 'state' + (cls ? ' ' + cls : ''); }
  async function api(path, options){
    options = options || {};
    options.cache = 'no-store';
    options.headers = Object.assign({'Content-Type':'application/json'}, options.headers || {});
    var res = await fetch(base + path, options);
    var text = await res.text(), data = null;
    try { data = text ? JSON.parse(text) : null; } catch(e) { data = null; }
    if (!res.ok) { var err = new Error((data && data.msg) || ('请求失败 HTTP ' + res.status)); err.status = res.status; throw err; }
    return (data && data.data !== undefined) ? data.data : data;
  }
  async function create(){
    $('qr').textContent = '加载中…';
    try {
      var data = await api('/qr', {method:'POST', body:'{}'});
      current = data.session_id;
      var img = new Image();
      img.onload = function(){ $('qr').innerHTML = ''; $('qr').appendChild(img); };
      img.onerror = function(){ $('qr').textContent = '二维码加载失败'; };
      img.src = base + '/qr/' + encodeURIComponent(current) + '/image?t=' + Date.now();
      say('请使用手机微信扫码');
      if (!timer) { timer = setInterval(poll, 1500); }
    } catch(e) { done = true; say(e.message, 'error'); }
  }
  async function poll(){
    if (!current || done) return;
    var sid = current;
    try {
      var result = await api('/qr/' + encodeURIComponent(sid) + '/poll');
      if (sid !== current) return;
      if (result.status === 'confirmed' || result.status === 'scanned') {
        say('已扫码，正在完成…');
        try {
          var saved = await api('/qr/' + encodeURIComponent(sid) + '/confirm', {method:'POST', body:'{}'});
          done = true; clearInterval(timer); timer = null;
          $('qr').innerHTML = '';
          $('title').textContent = saved.replaced ? '账号授权已更新' : '账号已添加';
          say('可以关闭此页面了', 'ok');
          $('hint').textContent = '本次链接已失效，如需再次操作请重新生成。';
        } catch(e) {
          if (e.status === 409 && /buffer not ready/.test(e.message) && attempt < 12) {
            attempt++; say('授权已确认，正在自动完成保存'); return;
          }
          throw e;
        }
      } else if (result.status === 'expired' || result.status === 'cancelled' || result.status === 'unknown') {
        done = true; clearInterval(timer); timer = null;
        say('二维码已失效，请让发起方重新生成', 'error');
      }
    } catch(e) {
      if (e.status === 410) { done = true; clearInterval(timer); timer = null; say('授权链接已失效', 'error'); }
      else { say(e.message || '网络波动，正在重试', 'error'); }
    }
  }
  api('/meta').then(function(meta){
    if (meta.kind === 'update') {
      $('title').textContent = '更新账号授权';
      $('desc').textContent = '请使用「' + (meta.target || ('账号 ID ' + meta.account_id)) + '」对应的微信扫码。换用其他微信号不会被接受。';
    } else {
      $('title').textContent = '添加微信账号';
      $('desc').textContent = '请使用要添加的微信扫码。';
    }
    $('hint').textContent = '该链接只能使用一次，过期后自动失效。';
    create();
  }).catch(function(e){
    $('qr').textContent = '链接不可用';
    say(e.message, 'error');
  });
})();
</script>
</html>`
