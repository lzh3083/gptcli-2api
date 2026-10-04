import importlib
import sys
from pathlib import Path

_pkg_dir = str(Path(__file__).resolve().parent)
if _pkg_dir not in sys.path:
    sys.path.insert(0, _pkg_dir)

# Putting this directory on sys.path lets the submodules import each other with
# bare absolute names ("import mail_service"), but those same files are also
# importable as "grok2api.upstream.browser_register.mail_service". Python then
# loads one physical file twice, producing two module objects whose globals
# (config, _BOUND_CONFIG, browser/page handles, …) are independent. The symptom
# is silent: bind_runtime() configures one instance while the code that reads
# the setting uses the other, so the mail provider and captcha switches appear
# to be ignored.
#
# Pre-import the submodules in dependency order and register top-level aliases
# pointing at the canonical package-qualified module, so every later
# "import mail_service" resolves to the single instance that bind_runtime()
# configured.
_ALIAS_ORDER = (
    "app_config",
    "cancel_utils",
    "proxy_pool",
    "proxy_bridge",
    "browser_runtime",
    "sso_risk",
    "registration_flow",
    "mail_service",
    "account_outputs",
    "captcha_solver",
    "us_consistency",
    "proxy_pool_v3",
    "proxy_protocols",
    "proxy_protocol_runtime",
    "novproxy",
    "traffic_meter",
    "outlook_mail",
    "outlook_mailbox_pool",
    "registration_browser",
)

for _name in _ALIAS_ORDER:
    try:
        _module = importlib.import_module("%s.%s" % (__name__, _name))
    except Exception:
        # A submodule with an optional dependency missing is skipped here; it
        # will still be importable later through the legacy top-level path.
        continue
    sys.modules.setdefault(_name, _module)
