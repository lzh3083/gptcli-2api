/* shell: sidebar / topbar / mobile nav */
window.G2A = window.G2A || {};
(function (G2A) {
  "use strict";
  const PAGE_META = {
    accounts: { title: "Codex 注册中心", sub: "ChatGPT / Codex 原生协议流水线 · 一键对接转售与分发 (CPA / Sub2API / OneAPI)", path: "/admin/accounts" },
    settings: { title: "系统与分发设置", sub: "管理员密码、CPA 自动推送与 Sub2API 参数配置", path: "/admin/settings" },
    logs: { title: "注册运行日志", sub: "实时监控端到端注册流水线与任务结果", path: "/admin/logs" },
    guide: { title: "转售与分发指南", sub: "对接 OneAPI / NewAPI / CLIProxyAPI / Sub2API 完整配置指南", path: "/admin/guide" },
  };

  function buildMobileNav(active) {
    const host = G2A.$("mobile-nav");
    if (!host) return;
    const order = ["accounts", "settings", "logs", "guide"];
    host.innerHTML = order.map((k) => {
      const m = PAGE_META[k];
      if (!m) return "";
      const cls = k === active ? "nav-btn active" : "nav-btn";
      return `<a class="${cls}" href="${m.path}" data-page="${k}">${m.title}</a>`;
    }).join("");
  }

  async function init({ page }) {
    const meta = PAGE_META[page] || PAGE_META.overview;
    document.title = meta.title + " · gptcli-2api";
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
