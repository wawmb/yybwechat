/* =============================================================================
   外壳脚本：主题、左侧栏、顶栏
   =============================================================================
   每个页面加载它即可获得统一外壳。页面自己的内容放在 <main> 里，
   顶栏按钮放在 #pageActions 里，由本脚本整体搬进外壳顶栏（id 与事件绑定不变，
   页面脚本不用关心自己显示在哪一层）。

   图标全部内联 SVG：emoji 在不同系统上字形、基线、颜色都不可控。
   ============================================================================= */
(() => {
  // 主题必须在建壳之前落定：外壳与内容共用同一套 token，
  // 晚一步设置 data-theme 会先闪一帧浅色再跳深色。
  // 没有显式选择过时跟随系统，与「切换一次就记住」的预期一致。
  const THEME_KEY = "yybwechat-theme";
  const applyTheme = dark => {
    if (dark) document.documentElement.setAttribute("data-theme", "dark");
    else document.documentElement.removeAttribute("data-theme");
  };
  const saved = localStorage.getItem(THEME_KEY);
  applyTheme(saved ? saved === "dark" : matchMedia("(prefers-color-scheme: dark)").matches);

  const ICON = {
    grid: '<rect x="3.5" y="3.5" width="7" height="7" rx="1.6"/><rect x="13.5" y="3.5" width="7" height="7" rx="1.6"/><rect x="3.5" y="13.5" width="7" height="7" rx="1.6"/><rect x="13.5" y="13.5" width="7" height="7" rx="1.6"/>',
    users: '<circle cx="9" cy="8" r="3.2"/><path d="M3.5 19.5c.6-3.2 2.7-5 5.5-5s4.9 1.8 5.5 5"/><path d="M16 5.2a3.2 3.2 0 0 1 0 6.1M17.5 15c1.9.5 3 2 3.5 4.5"/>',
    scan: '<rect x="3.5" y="3.5" width="17" height="17" rx="4"/><path d="M12 8.5v7M8.5 12h7"/>',
    bolt: '<path d="M13 2.5 4.5 14H11l-1 7.5L19.5 10H13z"/>',
    docs: '<path d="M5 4h9.5L19 8.5V20H5z"/><path d="M8.5 12h7M8.5 15.5h4.5M8.5 8.5h3"/>',
    globe: '<circle cx="12" cy="12" r="8.5"/><path d="M3.5 12h17M12 3.5c2.4 2.3 3.6 5.2 3.6 8.5S14.4 18.2 12 20.5c-2.4-2.3-3.6-5.2-3.6-8.5S9.6 5.8 12 3.5z"/>',
    link: '<path d="M10.5 13.5a4.5 4.5 0 0 0 6.4.1l1.8-1.8a4.5 4.5 0 0 0-6.4-6.4l-1 1"/><path d="M13.5 10.5a4.5 4.5 0 0 0-6.4-.1l-1.8 1.8a4.5 4.5 0 0 0 6.4 6.4l1-1"/>',
    logs: '<path d="M8 6h12M8 12h12M8 18h12M4 6h.01M4 12h.01M4 18h.01"/>',
    out: '<path d="M10 5H5v14h5M14.5 8.5 18 12l-3.5 3.5M9.5 12H18"/>',
    sun: '<circle cx="12" cy="12" r="4"/><path d="M12 3v2M12 19v2M3 12h2M19 12h2M5.6 5.6 7 7M17 17l1.4 1.4M18.4 5.6 17 7M7 17l-1.4 1.4"/>',
    moon: '<path d="M20 14.5A8.5 8.5 0 0 1 9.5 4a8.5 8.5 0 1 0 10.5 10.5z"/>'
  };
  const svg = name => `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${ICON[name] || ""}</svg>`;

  // 本项目是单管理员模型，没有用户体系：导航项不带上游那套权限标记。
  // 系统维护页已按用户要求移除——镜像由使用者自己构建发布。
  const navGroups = [
    { label: "工作区", items: [
      ["/", "grid", "我的控制台"],
      ["/?focus=accounts", "users", "我的微信账号"],
      ["/scan", "scan", "添加账号"],
      ["/?focus=test", "bolt", "接口测试"],
      ["/docs/index.html", "docs", "接口文档"]
    ]},
    { label: "管理", items: [
      ["/proxies", "globe", "代理设置"],
      ["/account-links", "link", "已激活短链接"],
      ["/logs", "logs", "运行日志"]
    ]}
  ];

  const pages = {
    "/logs": ["运行日志", "服务事件与授权诊断"],
    "/": ["我的控制台", "账号与能力调用"],
    "/scan": ["添加账号", "微信授权"],
    "/proxies": ["代理设置", "账号网络出口"],
    "/account-links": ["已激活短链接", "一次性授权链接"]
  };
  const focus = new URLSearchParams(location.search).get("focus");
  const contextual = location.pathname === "/"
    ? { accounts: ["我的微信账号", "账号与有效期"], test: ["接口测试", "协议能力调用"] }[focus]
    : null;
  const current = contextual || pages[location.pathname] || ["YYB WeChat", "管理控制台"];

  const main = document.querySelector("main");
  if (!main) return;

  const active = href => {
    const [path, query] = href.split("?");
    if (location.pathname !== path) return false;
    const now = new URLSearchParams(location.search);
    if (!query) return !now.has("focus") && !now.has("view");
    const want = new URLSearchParams(query);
    return [...want.keys()].every(key => want.get(key) === now.get(key));
  };

  const shell = document.createElement("div");
  shell.className = "platform-shell";
  shell.innerHTML = `
    <aside class="platform-sidebar" aria-label="主导航">
      <a class="platform-brand" href="/"><span class="platform-brand-mark">Y</span><span class="platform-brand-copy"><strong>YYB WeChat</strong><span>微信协议管理</span></span></a>
      <nav class="platform-nav">${navGroups.map(group => `<div class="platform-nav-section"><div class="platform-nav-group">${group.label}</div>${group.items.map(([href, icon, label]) => `<a href="${href}" ${active(href) ? 'aria-current="page"' : ""}><span class="platform-nav-icon">${svg(icon)}</span><span class="platform-nav-label">${label}</span></a>`).join("")}</div>`).join("")}</nav>
      <div class="platform-sidebar-foot">
        <div class="platform-service">
          <div class="platform-service-row"><span class="pill ok"><i></i>服务正常</span></div>
          <small id="platformRuntime">读取中</small>
        </div>
        <div class="platform-me">
          <span class="platform-avatar" id="platformAvatar">Y</span>
          <span class="platform-me-copy"><strong id="platformUserName">正在读取</strong><span id="platformUserRole">当前用户</span></span>
          <button class="icon-btn" id="platformLogout" type="button" title="退出登录" aria-label="退出登录">${svg("out")}</button>
        </div>
      </div>
    </aside>
    <section class="platform-stage">
      <header class="platform-topbar">
        <div class="platform-topbar-left">
          <button class="platform-menu" id="platformMenu" type="button" aria-label="打开导航">☰</button>
          <div class="platform-page-context">
            <div class="platform-breadcrumb">YYB WeChat / ${current[1]}</div>
            <h1 class="platform-page-title">${current[0]}</h1>
          </div>
        </div>
        <div class="platform-actions" id="platformActions"></div>
        <div class="platform-user">
          <span class="platform-divider"></span>
          <button class="icon-btn" id="platformTheme" type="button" aria-label="切换深色/浅色模式" title="切换主题"></button>
          <span class="platform-version" id="platformVersion" title="当前运行版本"><span>版本</span><b class="num">—</b></span>
        </div>
      </header>
      <div class="platform-main"></div>
    </section>`;
  document.body.insertBefore(shell, document.body.firstChild);
  shell.querySelector(".platform-main").appendChild(main);
  document.body.classList.add("platform-ready");

  // 页面自带的操作按钮搬进顶栏：保留原 id 与事件绑定，页面脚本完全不用改。
  const actionsSource = document.getElementById("pageActions");
  const actionsSlot = document.getElementById("platformActions");
  if (actionsSource && actionsSlot) actionsSlot.append(...actionsSource.children);

  // 主题开关：外壳里有且只有一个，切换后写入 localStorage，其它页面读同一份
  const themeButton = document.getElementById("platformTheme");
  const syncTheme = () => {
    const dark = document.documentElement.hasAttribute("data-theme");
    themeButton.innerHTML = svg(dark ? "sun" : "moon");
    const label = dark ? "切换到浅色模式" : "切换到深色模式";
    themeButton.setAttribute("aria-label", label);
    themeButton.title = label;
  };
  syncTheme();
  themeButton.onclick = () => {
    const dark = !document.documentElement.hasAttribute("data-theme");
    applyTheme(dark);
    localStorage.setItem(THEME_KEY, dark ? "dark" : "light");
    syncTheme();
  };

  // 窄屏抽屉
  const menu = document.getElementById("platformMenu");
  const sidebar = shell.querySelector(".platform-sidebar");
  const mobile = matchMedia("(max-width: 900px)");
  const closeNav = () => {
    const wasOpen = document.body.classList.contains("platform-nav-open");
    document.body.classList.remove("platform-nav-open");
    menu.setAttribute("aria-expanded", "false");
    if (wasOpen && mobile.matches) menu.focus();
  };
  menu.setAttribute("aria-expanded", "false");
  menu.onclick = () => {
    if (document.body.classList.contains("platform-nav-open")) return closeNav();
    document.body.classList.add("platform-nav-open");
    menu.setAttribute("aria-expanded", "true");
    sidebar.querySelector('a[aria-current="page"]')?.focus();
  };
  mobile.addEventListener("change", closeNav);
  shell.querySelectorAll(".platform-nav a").forEach(link => link.addEventListener("click", closeNav));
  document.addEventListener("keydown", event => {
    if (event.key === "Escape" && document.body.classList.contains("platform-nav-open")) closeNav();
  });

  document.getElementById("platformLogout").onclick = async () => {
    await fetch("/logout", { method: "POST" }).catch(() => {});
    location.href = "/login";
  };

  // 版本只做展示：在线更新与系统维护已按用户要求移除，镜像由使用者自己构建。
  fetch("/api/version").then(async response => {
    if (response.status === 401) { location.replace("/login"); return; }
    const body = await response.json();
    if (!response.ok || body.code !== 0) throw new Error(body.msg || "读取版本失败");
    const version = body.data?.version || "dev";
    document.querySelector("#platformVersion b").textContent = `v${version}`;
    const runtime = document.getElementById("platformRuntime");
    if (runtime) runtime.textContent = `v${version}`;
  }).catch(() => {
    document.querySelector("#platformVersion b").textContent = "未知";
  });

  // 用户区读的是本项目的 /api/auth/status：
  // {enabled, initialized, authenticated, username, version, commit, build_date}。
  // 没有上游的 user 对象与 role 字段——用户体系是明确排除的功能。
  // 之前照上游结构取 data.user.display_name，user 恒为 undefined，
  // 一读属性就抛错落到 catch，顶栏于是永远停在「状态未知」。
  fetch("/api/auth/status").then(async response => {
    const body = await response.json();
    if (!response.ok || body.code !== 0) throw new Error(body.msg || "读取登录状态失败");
    const status = body.data || {};
    const authEnabled = status.enabled !== false;
    if (authEnabled && status.authenticated === false) {
      location.replace(status.initialized === false ? "/setup" : "/login");
      return;
    }
    const name = authEnabled ? (status.username || "已登录") : "本机模式";
    document.getElementById("platformUserName").textContent = name;
    document.getElementById("platformUserRole").textContent = authEnabled ? "管理员" : "本机模式";
    document.getElementById("platformAvatar").textContent = Array.from(name)[0]?.toUpperCase() || "Y";
  }).catch(() => {
    // 读不到就如实说读不到，别用「状态未知」这种含糊说法。
    document.getElementById("platformUserName").textContent = "读取失败";
    document.getElementById("platformUserRole").textContent = "刷新页面重试";
  });
})();
