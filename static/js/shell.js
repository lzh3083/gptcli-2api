/* shell: sidebar / topbar / mobile nav */
window.G2A = window.G2A || {};
(function (G2A) {
  "use strict";
  const PAGE_META = {
    overview: { title: "概览", sub: "服务状态、账号池与 Token 健康度一览", path: "/admin" },
    keys: { title: "API Keys", sub: "创建、复制、停用客户端访问密钥", path: "/admin/keys" },
    accounts: { title: "账号 / 轮询", sub: "Grok 账号、设备码登录、额度与导入导出", path: "/admin/accounts" },
    usage: { title: "用量", sub: "Token 消耗与请求使用情况（今日 / 近 N 天 / 累计）", path: "/admin/usage" },
    logs: { title: "任务日志", sub: "查询后台任务结果（协议注册、SSO 导入、测活、Token 续期等）", path: "/admin/logs" },
    models: { title: "模型", sub: "上游模型缓存与探测结果", path: "/admin/models" },
    settings: { title: "系统设置", sub: "修改管理员密码、轮询策略与 sub2api / 维护参数", path: "/admin/settings" },
    guide: { title: "接入指南", sub: "OpenAI / Anthropic 客户端配置示例", path: "/admin/guide" },
  };

  function buildMobileNav(active) {
    const host = G2A.$("mobile-nav");
    if (!host) return;
    host.innerHTML = Object.keys(PAGE_META).map((k) => {
      const m = PAGE_META[k];
      const cls = k === active ? "nav-btn active" : "nav-btn";
      return `<a class="${cls}" href="${m.path}" data-page="${k}">${m.title}</a>`;
    }).join("");
  }

  async function init({ page }) {
    const meta = PAGE_META[page] || PAGE_META.overview;
    document.title = meta.title + " · grokcli-2api";
    if (G2A.$("page-title")) G2A.$("page-title").textContent = meta.title;
    if (G2A.$("page-sub")) G2A.$("page-sub").textContent = meta.sub;
    buildMobileNav(page);

    // session gate
    let st = null;
    try {
      st = await G2A.auth.requireSession();
    } catch (e) {
      if (String(e.message) === "file://") {
        G2A.toast("请通过服务打开管理台", false);
        return;
      }
      G2A.toast(e.message || "加载失败", false);
      return;
    }
    if (!st) return;

    // version + pill
    const ver = st.version || (G2A.state.dashboard && G2A.state.dashboard.version) || "";
    G2A.$$("#app-version, .ver-chip#app-version").forEach((el) => {
      if (el) el.textContent = ver ? ("v" + ver) : "";
    });
    // brand version maybe single id
    const av = G2A.$("app-version");
    if (av) av.textContent = ver ? ("v" + ver) : "";
    const pill = G2A.$("status-pill");
    if (pill) {
      const mode = st.account_mode || (G2A.state.dashboard && G2A.state.dashboard.account_mode) || "";
      const live = st.accounts_live ?? G2A.state.dashboard?.accounts_live;
      pill.textContent = [mode, live != null ? (`账号 ${live}`) : ""].filter(Boolean).join(" · ");
    }

    G2A.$("btn-logout")?.addEventListener("click", () => G2A.auth.logout());
    G2A.$("btn-refresh")?.addEventListener("click", async () => {
      const btn = G2A.$("btn-refresh");
      G2A.setBusy(btn, true, "刷新中…");
      try {
        await G2A.refreshStatus();
        await G2A.refreshDashboard();
        if (G2A.pages[page]?.refresh) await G2A.pages[page].refresh();
        else if (G2A.pages[page]?.init) await G2A.pages[page].init();
        G2A.toast("已刷新");
      } catch (e) {
        G2A.toast(e.message || "刷新失败", false);
      } finally {
        G2A.setBusy(btn, false);
      }
    });

    G2A.pages = G2A.pages || {};
    if (G2A.pages[page]?.init) {
      await G2A.pages[page].init();
    }
  }

  G2A.PAGE_META = PAGE_META;
  G2A.shell = { init };
})(window.G2A);
