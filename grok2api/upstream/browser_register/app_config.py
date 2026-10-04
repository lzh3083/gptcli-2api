"""负责应用配置的默认值、加载保存、规范化和运行前校验。"""
import json
import os
import tempfile
import urllib.parse
from pathlib import Path

def _resolve_config_file() -> str:
    env_cfg = os.environ.get("GROK_REGISTER_CONFIG") or os.environ.get("CONFIG_FILE")
    if env_cfg and os.path.exists(env_cfg):
        return env_cfg
    candidates = [
        "/app/data/config.json",
        "/app/config.json",
        os.path.join(os.getcwd(), "config.json"),
        os.path.join(os.path.dirname(os.path.abspath(__file__)), "config.json"),
    ]
    for c in candidates:
        if os.path.exists(c):
            return c
    return candidates[0] if os.path.isdir("/app/data") else candidates[-1]

CONFIG_FILE = _resolve_config_file()

DEFAULT_CONFIG = {
    "duckmail_api_key": "",
    "cloudflare_api_base": "",
    "cloudflare_api_key": "",
    "cloudflare_auth_mode": "none",
    "cloudflare_path_domains": "/api/domains",
    "cloudflare_path_accounts": "/api/new_address",
    "cloudflare_path_token": "/api/token",
    "cloudflare_path_messages": "/api/mails",
    # 固定邮箱模式：直接复用已存在的地址与其地址级 JWT，不再调用建址接口。
    # 适用于实例已关闭建址（或只有单个地址凭证）的场景。
    # 留空则维持原行为（每次自动创建新地址）。
    "cloudflare_fixed_address": "",
    "cloudflare_fixed_jwt": "",
    "cloudmail_api_base": "",
    "cloudmail_public_token": "",
    "cloudmail_domains": "",
    "cloudmail_path_messages": "/api/public/emailList",
    "outlook_accounts_file": "./output/mailboxes/outlook-accounts.txt",
    "proxy_mode": "auto",
    "proxy": "",
    "proxy_fallback": "none",
    "proxy_pool_file": "",
    "proxy_pool_subscription_url": "",
    "proxy_pool_subscription_proxy": "",
    "proxy_pool_endpoint_mode": "auto",
    "proxy_pool_refresh_interval_sec": 900,
    "proxy_pool_probe_interval_sec": 900,
    "proxy_pool_probe_timeout_sec": 15,
    "proxy_pool_probe_provider": "cloudflare",
    "proxy_pool_probe_dual_stack": True,
    "proxy_pool_max_concurrent_per_node": 1,
    "proxy_pool_acquire_timeout_sec": 30,
    "proxy_protocol_backend": "auto",
    "proxy_singbox_path": "",
    "proxy_protocol_start_timeout_sec": 10,
    "proxy_runtime_idle_ttl_sec": 120,
    "proxy_runtime_cache_max": 32,
    "proxy_pool_persist_health": False,
    "proxy_pool_state_file": "./proxy_pool_state.json",
    "proxy_pool_subscription_public_only": False,
    "proxy_pool_preflight_enabled": True,
    "enable_nsfw": True,
    "sso_risk_gate_enabled": True,
    "sso_risk_rejected_file": "./sso_risk_rejected.txt",
    "register_count": 1,
    "multi_thread_enabled": False,
    "multi_thread_workers": 4,
    "user_agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36",
    "grok2api_auto_add_local": False,
    "grok2api_local_token_file": "",
    "grok2api_pool_name": "ssoBasic",
    "grok2api_auto_add_remote": False,
    "grok2api_remote_base": "",
    "grok2api_remote_app_key": "",
    "grok2api_remote_admin_username": "",
    "grok2api_remote_admin_password": "",
    "api_reverse_tools": "",
    "cpa_export_enabled": True,
    "cpa_auth_dir": "./cpa_auths",
    "cpa_copy_to_hotload": False,
    "cpa_hotload_dir": "",
    # ---- 远程 CPA 同步（本仓库新增）----
    # cpa_copy_to_hotload 只能复制到本机目录；CPA 部署在另一台服务器时
    # （Docker 卷映射）用它同步。实测 CPA 会自动扫描 auth 目录，新增
    # 凭据文件即生效，无需重启。
    "cpa_sync_enabled": False,
    "cpa_sync_target": "",
    "cpa_sync_auth_dir": "",
    "cpa_sync_use_sudo": True,
    # ---- 批次代理流量计量（本仓库新增）----
    # 住宅代理按流量计费；计量在本地代理桥的中继路径上累加字节数，
    # 面板直接展示本批用量与历史均值。留空则仅内存计数、不落盘。
    "traffic_file": "./logs/traffic.json",
    "traffic_history_file": "./logs/traffic_history.json",
    # 流量校准系数：本地计量 → 面板实际扣量 的倍率。默认 1.0 = 不做校准，
    # 面板只显示本地桥的实际统计值。
    #
    # 曾经默认 1.6：本地桥只覆盖「浏览器 ↔ 本地代理桥」，两次实测发现
    # NovProxy 面板扣量约为本地读数的 1.53x / 1.71x。但厂商计费可能存在
    # 多倍率（按 IP 段/地区/时段），固定系数并不靠谱，故回到原始读数。
    # 需要时填实测倍率即可启用校准。
    "traffic_calibration_factor": 1.0,
    "cpa_base_url": "https://cli-chat-proxy.grok.com/v1",
    "cpa_proxy": "",
    "cpa_headless": False,
    "cpa_force_standalone": True,
    "cpa_mint_timeout_sec": 300,
    "cpa_mint_cookie_inject": True,
    "cpa_oidc_request_timeout_sec": 15,
    "cpa_oidc_poll_timeout_sec": 15,
    # ---- YesCaptcha 打码（本仓库新增）----
    # 住宅 IP 下登录/注册页可能弹 Cloudflare Turnstile。开启后遇到
    # Turnstile 会交给 YesCaptcha 云端解题并注入 token。
    # 注意：只对「页面能加载出来、能看到 sitekey」的情况有效；若整站被
    # 托管挑战挡住（Just a moment...），页面拿不到 sitekey，打码也无能为力。
    "captcha_solver_enabled": False,
    "captcha_solver_provider": "yescaptcha",
    "captcha_solver_api_key": "",
    "captcha_solver_api_base": "https://api.yescaptcha.com",
    "captcha_solver_timeout_sec": 120,
    # 先给 Cloudflare 多少秒自动完成的机会，之后才交给打码。自动完成通常
    # 只要 5 秒，过早打码纯属浪费（本地 solver 也要占浏览器资源）。
    "captcha_solver_auto_wait_sec": 10,
    # 留空则从页面自动提取 sitekey（含 iframe src）。某些隐式渲染的页面
    # 提取不到，可在这里写死，例如 x.ai: 0x4AAAAAAAhr9JGVDZbrZOo0
    "captcha_solver_sitekey": "",
    # solver 的 proxies.txt 路径；留空则用 turnstile-solver/proxies.txt
    "captcha_solver_proxies_file": "",
    "grok2api_allow_legacy_full_save": False,
    "email_provider": "duckmail",
    "yyds_api_key": "",
    "yyds_jwt": "",
    "defaultDomains": "",
    # ---- 美国住宅 IP 环境一致性（本仓库新增）----
    # 把浏览器时区/语言/platform 归一化到"美国 Windows 桌面用户"，
    # 避免 IP=US 而时区=UTC、platform=Linux 这类自相矛盾特征。
    "us_consistency_enabled": True,
    "us_consistency_timezone": "America/New_York",
    "us_consistency_locale": "en-US",
    # 代理出口的期望国家。启动浏览器前会探测真实出口，只有国家匹配时
    # 才按落地州对齐时区；不匹配则保持原时区不动，避免"将错就错"。
    "us_consistency_expect_country": "US",
    # ---- NovProxy 美国动态住宅代理 ----
    # 提取接口。节点按会话粘性保持，minutes 必须大于单账号完整流程耗时
    # （注册 + CPA 导出），否则 IP 会在流程中途变化。
    "novproxy_api": "https://white.novproxy.com/white/api",
    "novproxy_region": "US",
    "novproxy_minutes": 120,
    "novproxy_num": 5,
    # ---- 降智测试（Quality Probe）----
    # 账号注册后是否自动做一次降智检测并写入记录。默认开启：xAI 会对部分账号
    # 静默降级（接口照常 200 但不再逐步推理），只有真发一次需要推理的请求、
    # 看 usage.completion_tokens_details.reasoning_tokens 才能发现。检测同步
    # 跑在注册流程内，每个账号约 6~15 秒。判为降智的账号保留记录但暂停轮询。
    "quality_auto_probe": True,
    # 新注册账号冷却静置观察期（秒），默认 0（立即入池），可设如 21600 (6小时)
    "account_cooldown_sec": 0,
    # 注册完成后是否执行 1 轮破冰日常会话以生成合法会话上下文
    "warmup_conversation_enabled": True,
    # 降智测试可疑阈值（低于此推理 token 数量视为可疑）。
    "quality_soft_threshold": 50,
    # 注册会话内是否实测 Grok Imagine 生图能力。默认关闭：这个探测要真的
    # 打开 grok.com/imagine 并提交一次生成，很费住宅代理流量，而结论基本
    # 是固定的（免费账号网页端界面可用、API 端一律 403 需要订阅）。
    "check_imagine_capability": False,
    # 降智扫描是否改走 CPA API 抽样。默认开启：本地和 Oracle CPA 各自持有
    # 一份凭据副本、都在刷新 refresh_token，而 xAI 的 RT 是一次性轮换的，
    # 先刷的拿到新 token、后刷的直接 revoked（实测 42 个账号废了 17 个）。
    # 走 CPA 之后刷新者只剩 CPA 一个，从根上消除冲突；代价是只能拿到整体
    # 质量分布，没有逐账号明细。
    "quality_probe_via_cpa": True,
    # SSH 隧道目标：CPA 只监听 127.0.0.1:8317，本地必须经隧道访问。
    "quality_cpa_tunnel": "oracle-singapore",
    "quality_cpa_local_port": 18317,
    "quality_cpa_remote_port": 8317,
    # CPA 的 API key（config.json 权限 0600）。
    "quality_cpa_api_key": "",
    # 抽样次数；0 表示按本地凭据数量自动取值。
    "quality_cpa_samples": 0,
    "quality_cpa_model": "grok-4.7",
    # Chromium 可执行文件路径。留空则按环境变量与常见安装位置自动探测；
    # 容器/服务器上浏览器常装在非标准目录，此时需显式指定。
    "browser_path": "",
}


# 已废弃的配置项：旧 config.json 里可能还留着，校验时静默忽略并剔除。
# 直接报「未知配置项」会让老配置文件升级后无法启动，所以这里显式列出。
DEPRECATED_CONFIG_KEYS = frozenset({
    # MooProxy：节点存活时间只有几分钟，已由 NovProxy 取代。
    "mooproxy_api", "mooproxy_country", "mooproxy_state",
    "mooproxy_via", "mooproxy_bridge_port",
    # 辣椒HTTP：接口已不再使用，统一走 NovProxy。
    "lajiao_api_base", "lajiao_regions", "lajiao_num",
    "lajiao_sticky_minutes", "lajiao_extract_via",
    "lajiao_require_residential", "lajiao_require_country",
})


config = DEFAULT_CONFIG.copy()


class ConfigError(RuntimeError):
    pass


def _require_bool(cfg, key):
    value = cfg.get(key)
    if type(value) is not bool:
        raise ConfigError(f"配置项 {key} 必须是布尔值 true/false")
    return value


def _require_int(cfg, key, minimum, maximum):
    value = cfg.get(key)
    if type(value) is not int:
        raise ConfigError(f"配置项 {key} 必须是整数")
    if not minimum <= value <= maximum:
        raise ConfigError(f"配置项 {key} 必须在 {minimum} 到 {maximum} 之间")
    return value


def _require_string(cfg, key, path=False):
    value = cfg.get(key)
    if not isinstance(value, str):
        raise ConfigError(f"配置项 {key} 必须是字符串")
    value = value.strip() if key not in ("user_agent",) else value
    if "\x00" in value:
        raise ConfigError(f"配置项 {key} 包含非法空字符")
    if path and value:
        os.path.expanduser(value)
    return value


def validate_config_structure(raw):
    if not isinstance(raw, dict):
        raise ConfigError("config root must be a JSON object")
    unknown = sorted(set(raw) - set(DEFAULT_CONFIG) - DEPRECATED_CONFIG_KEYS)
    if unknown:
        raise ConfigError("未知配置项: " + ", ".join(unknown))
    cfg = {**DEFAULT_CONFIG, **{k: v for k, v in raw.items() if k not in DEPRECATED_CONFIG_KEYS}}
    bool_keys = (
        "enable_nsfw", "sso_risk_gate_enabled", "grok2api_auto_add_local", "grok2api_auto_add_remote",
        "grok2api_allow_legacy_full_save", "cpa_export_enabled",
        "cpa_copy_to_hotload", "cpa_headless", "cpa_force_standalone",
        "cpa_mint_cookie_inject", "multi_thread_enabled",
        "cpa_sync_enabled", "cpa_sync_use_sudo",
        "proxy_pool_probe_dual_stack", "proxy_pool_persist_health",
        "proxy_pool_subscription_public_only", "proxy_pool_preflight_enabled",
        "us_consistency_enabled",
        "captcha_solver_enabled",
        "quality_auto_probe",
    )
    for key in bool_keys:
        cfg[key] = _require_bool(cfg, key)
    cfg["register_count"] = _require_int(cfg, "register_count", 1, 2500)
    cfg["multi_thread_workers"] = _require_int(cfg, "multi_thread_workers", 1, 8)
    cfg["proxy_pool_refresh_interval_sec"] = _require_int(cfg, "proxy_pool_refresh_interval_sec", 0, 86400)
    cfg["proxy_pool_probe_interval_sec"] = _require_int(cfg, "proxy_pool_probe_interval_sec", 0, 86400)
    cfg["proxy_pool_probe_timeout_sec"] = _require_int(cfg, "proxy_pool_probe_timeout_sec", 3, 120)
    cfg["proxy_pool_max_concurrent_per_node"] = _require_int(cfg, "proxy_pool_max_concurrent_per_node", 1, 64)
    cfg["proxy_pool_acquire_timeout_sec"] = _require_int(cfg, "proxy_pool_acquire_timeout_sec", 1, 600)
    cfg["proxy_protocol_start_timeout_sec"] = _require_int(cfg, "proxy_protocol_start_timeout_sec", 3, 60)
    cfg["proxy_runtime_idle_ttl_sec"] = _require_int(cfg, "proxy_runtime_idle_ttl_sec", 0, 3600)
    cfg["proxy_runtime_cache_max"] = _require_int(cfg, "proxy_runtime_cache_max", 1, 256)
    cfg["cpa_mint_timeout_sec"] = _require_int(cfg, "cpa_mint_timeout_sec", 30, 1800)
    cfg["cpa_oidc_request_timeout_sec"] = _require_int(cfg, "cpa_oidc_request_timeout_sec", 3, 120)
    cfg["cpa_oidc_poll_timeout_sec"] = _require_int(cfg, "cpa_oidc_poll_timeout_sec", 3, 120)
    cfg["captcha_solver_timeout_sec"] = _require_int(cfg, "captcha_solver_timeout_sec", 10, 600)
    cfg["captcha_solver_auto_wait_sec"] = _require_int(cfg, "captcha_solver_auto_wait_sec", 0, 120)
    cfg["novproxy_minutes"] = _require_int(cfg, "novproxy_minutes", 1, 1440)
    cfg["novproxy_num"] = _require_int(cfg, "novproxy_num", 1, 500)
    cfg["quality_soft_threshold"] = _require_int(cfg, "quality_soft_threshold", 1, 5000)
    string_keys = tuple(key for key, value in DEFAULT_CONFIG.items() if isinstance(value, str))
    path_keys = {
        "grok2api_local_token_file", "api_reverse_tools", "cpa_auth_dir", "cpa_hotload_dir",
        "proxy_pool_file", "proxy_singbox_path", "proxy_pool_state_file",
        "sso_risk_rejected_file", "outlook_accounts_file", "browser_path",
        "traffic_file", "traffic_history_file",
    }
    for key in string_keys:
        cfg[key] = _require_string(cfg, key, path=key in path_keys)
    enums = {
        "email_provider": {"duckmail", "yyds", "cloudflare", "cloudmail", "outlook"},
        "cloudflare_auth_mode": {"query-key", "bearer", "x-api-key", "x-admin-auth", "x-user-token", "none"},
        "grok2api_pool_name": {"ssoBasic", "ssoSuper"},
        "proxy_mode": {"auto", "direct", "single", "pool", "novproxy"},
        "proxy_fallback": {"none", "direct", "single"},
        "proxy_pool_endpoint_mode": {"auto", "fixed", "rotating"},
        "proxy_pool_probe_provider": {"cloudflare", "ipinfo"},
        "proxy_protocol_backend": {"auto", "sing-box", "native-only"},
    }
    for key, allowed in enums.items():
        value = cfg.get(key, DEFAULT_CONFIG.get(key, ""))
        if value not in allowed:
            raise ConfigError(f"配置项 {key} 的值无效: {value!r}; 允许值: {sorted(allowed)}")
        cfg[key] = value

    api_path_keys = {
        "cloudflare_path_domains", "cloudflare_path_accounts",
        "cloudflare_path_token", "cloudflare_path_messages",
        "cloudmail_path_messages",
    }
    for key in api_path_keys:
        value = cfg[key]
        if value and not value.startswith("/"):
            value = "/" + value
        cfg[key] = value

    url_keys = {
        "cloudflare_api_base", "cloudmail_api_base",
        "grok2api_remote_base", "cpa_base_url",
        "proxy_pool_subscription_url",
    }
    for key in url_keys:
        value = cfg[key]
        if not value:
            continue
        parsed = urllib.parse.urlsplit(value)
        if parsed.scheme not in ("http", "https") or not parsed.netloc:
            raise ConfigError(f"配置项 {key} 必须是有效的 http/https URL")

    for key in path_keys:
        value = cfg[key]
        if value.startswith("~"):
            cfg[key] = os.path.expanduser(value)
    return cfg


def validate_run_requirements(cfg):
    cfg = validate_config_structure(cfg)
    provider = cfg["email_provider"]
    if provider == "cloudflare" and not cfg["cloudflare_api_base"]:
        raise ConfigError("Cloudflare 模式需要配置 cloudflare_api_base")
    if provider == "cloudmail":
        missing = [
            key for key in ("cloudmail_api_base", "cloudmail_public_token", "cloudmail_domains")
            if not cfg[key]
        ]
        if missing:
            raise ConfigError("Cloud Mail 模式缺少必需配置: " + ", ".join(missing))
    if provider == "yyds" and not (cfg["yyds_api_key"] or cfg["yyds_jwt"]):
        raise ConfigError("YYDS 模式需要至少配置 yyds_api_key 或 yyds_jwt")
    if provider == "outlook":
        path = os.path.realpath(os.path.abspath(os.path.expanduser(cfg["outlook_accounts_file"])))
        if not os.path.isfile(path):
            raise ConfigError(f"Outlook 模式需要有效的账号池文件: {path}")
        try:
            from outlook_mailbox_pool import get_outlook_mailbox_pool_capacity
            count = get_outlook_mailbox_pool_capacity(path)
        except Exception as exc:
            raise ConfigError(f"Outlook 账号池校验失败: {exc}") from exc
        if int(count) <= 0:
            raise ConfigError("Outlook 账号池没有有效账号")

    if cfg["proxy_mode"] == "single" and not cfg["proxy"]:
        raise ConfigError("single 代理模式必须配置 proxy")
    if cfg["proxy_mode"] == "pool" and not (cfg["proxy_pool_file"] or cfg["proxy_pool_subscription_url"]):
        raise ConfigError("pool 代理模式至少需要 proxy_pool_file 或 proxy_pool_subscription_url")
    if cfg["proxy_fallback"] == "single" and not cfg["proxy"]:
        raise ConfigError("proxy_fallback=single 时必须配置 proxy")
    if cfg["proxy_pool_persist_health"] and not cfg["proxy_pool_state_file"]:
        raise ConfigError("启用代理健康状态持久化时必须配置 proxy_pool_state_file")

    if cfg["grok2api_auto_add_remote"]:
        if not cfg["grok2api_remote_base"]:
            raise ConfigError("远端 token 入池缺少必需配置: grok2api_remote_base")
        has_legacy = bool(cfg["grok2api_remote_app_key"])
        has_go = bool(cfg["grok2api_remote_admin_username"] and cfg["grok2api_remote_admin_password"])
        has_partial_go = bool(cfg["grok2api_remote_admin_username"] or cfg["grok2api_remote_admin_password"])
        if has_legacy and has_partial_go:
            raise ConfigError("旧版 app_key 与新版管理员账号密码不能同时配置")
        if has_partial_go and not has_go:
            raise ConfigError("新版 grok2api 必须同时配置管理员账号和密码")
        if not has_legacy and not has_go:
            raise ConfigError("远端 token 入池需要旧版 app_key 或新版管理员账号密码")
    if cfg["cpa_export_enabled"] and cfg["cpa_copy_to_hotload"] and not cfg["cpa_hotload_dir"]:
        raise ConfigError("启用 CPA 热加载复制时必须配置 cpa_hotload_dir")
    _apply_traffic_env(cfg)
    return cfg


def _apply_traffic_env(cfg):
    """把流量计量的落盘路径同步到环境变量。

    traffic_meter 通过环境变量取路径，这样代理桥（可能在另一个进程里
    以独立命令启动）也能写到同一个文件。路径为相对路径时按项目根解析。
    """
    for env_key, cfg_key in (
        ("GROK_BATCH_TRAFFIC_FILE", "traffic_file"),
        ("GROK_BATCH_TRAFFIC_HISTORY_FILE", "traffic_history_file"),
    ):
        value = str(cfg.get(cfg_key) or "").strip()
        if not value:
            os.environ.pop(env_key, None)
            continue
        path = Path(value).expanduser()
        if not path.is_absolute():
            path = (Path(__file__).resolve().parent / path).resolve()
        os.environ[env_key] = str(path)

    # 校准系数是纯数字，不做路径解析。
    factor = str(cfg.get("traffic_calibration_factor") or "").strip()
    if factor:
        os.environ["GROK_BATCH_TRAFFIC_FACTOR"] = factor
    else:
        os.environ.pop("GROK_BATCH_TRAFFIC_FACTOR", None)


def validate_config(raw):
    """Backward-compatible full validation used before a run or save."""
    return validate_run_requirements(raw)


def _replace_config(value):
    config.clear()
    config.update(value)
    return config


def load_config():
    if os.path.exists(CONFIG_FILE):
        try:
            with open(CONFIG_FILE, "r", encoding="utf-8") as handle:
                loaded = json.load(handle)
            result = _replace_config(validate_config_structure(loaded))
            return result
        except ConfigError:
            raise
        except Exception as exc:
            raise ConfigError(f"配置文件解析失败: {CONFIG_FILE}: {exc}") from exc
    result = _replace_config(validate_config_structure(DEFAULT_CONFIG.copy()))
    return result


def save_config():
    normalized = validate_config_structure(config)
    _replace_config(normalized)
    config_dir = os.path.dirname(os.path.abspath(CONFIG_FILE))
    os.makedirs(config_dir, exist_ok=True)
    fd = None
    temp_path = None
    try:
        fd, temp_path = tempfile.mkstemp(prefix=".config-", suffix=".json.tmp", dir=config_dir)
        with os.fdopen(fd, "w", encoding="utf-8") as handle:
            fd = None
            json.dump(config, handle, indent=4, ensure_ascii=False)
            handle.write("\n")
            handle.flush()
            os.fsync(handle.fileno())
        try:
            os.chmod(temp_path, 0o600)
        except Exception:
            pass
        os.replace(temp_path, CONFIG_FILE)
        temp_path = None
        try:
            os.chmod(CONFIG_FILE, 0o600)
        except Exception:
            pass
    except Exception as exc:
        raise ConfigError(f"保存配置失败: {exc}") from exc
    finally:
        if fd is not None:
            try:
                os.close(fd)
            except Exception:
                pass
        if temp_path and os.path.exists(temp_path):
            try:
                os.unlink(temp_path)
            except Exception:
                pass
    return config
