"""批次代理流量计量。

为什么需要
----------
住宅代理按流量计费，跑批量注册时最关心的就是「这批用了多少」。但代理
凭据会被脱敏，直接看日志看不出用量。

做法
----
不额外起计量代理，而是在已有的本地代理桥（proxy_bridge）中继路径上
累加字节数。桥本来就要转发每一个字节，所以零额外开销、零额外端口。

只记录字节数与连接数，**不记录**目标域名、URL 或代理地址。

用法：
    from traffic_meter import begin_batch, record, snapshot, finish_batch

    begin_batch()                     # 批次开始（可重复调用，幂等）
    record("up", 1234)                # 中继路径上调用
    print(snapshot())                 # {'bytes_up':…, 'bytes_down':…, …}
    finish_batch()                    # 批次结束，落盘并归档历史

落盘位置由 GROK_BATCH_TRAFFIC_FILE 指定；未设置时仅内存计数，
不影响任何既有行为。
"""
from __future__ import annotations

import atexit
import datetime
import json
import os
import threading
import time
from pathlib import Path

TRAFFIC_FILE_ENV = "GROK_BATCH_TRAFFIC_FILE"
HISTORY_FILE_ENV = "GROK_BATCH_TRAFFIC_HISTORY_FILE"
CALIBRATION_ENV = "GROK_BATCH_TRAFFIC_FACTOR"
HISTORY_LIMIT = 200

# 本地计量只覆盖「浏览器 ↔ 本地代理桥」这一段。曾怀疑它系统性低于 NovProxy
# 面板扣量，两次实测确实如此：
#   5 账号批次: 本地 93.23 MB / 面板扣 143 MB (0.57→0.43GB) = 1.53x
#   单账号验证: 本地 22.81 MB / 面板扣 ~39 MB (0.43→0.39GB) = 1.71x
# 但厂商计费可能存在多倍率（按 IP 段/地区/时段），固定系数不可靠，因此默认
# 不做校准，面板只显示本地实际统计值。需要时用配置项填实测倍率。
DEFAULT_CALIBRATION = 1.0

_LOCK = threading.RLock()
_STATE = {
    "running": False,
    "started_at": None,
    "finished_at": None,
    "bytes_up": 0,
    "bytes_down": 0,
    "connections": 0,
    "accounts": 0,
    "archived": False,
}


_TZ_OFFSET = None


def _fixed_offset():
    """进程启动时的本地时区偏移，之后固定不变。

    注册流程会通过 us_consistency 设置 TZ 环境变量并 time.tzset()，把
    整个进程切成美国时区。若 _now() 直接调 datetime.now()，同一批次会
    「开始时用服务器时区、结束时用美国时区」打时间戳，实测出现过结束
    时间比开始时间早 7 小时的记录，窗口统计随之全错。

    这里在首次调用时锁定偏移量，之后即使 TZ 变了也不受影响。
    """
    global _TZ_OFFSET
    if _TZ_OFFSET is None:
        _TZ_OFFSET = datetime.datetime.now().astimezone().utcoffset() or datetime.timedelta(0)
    return _TZ_OFFSET


def _now_dt():
    """按固定偏移量取的本地时间（naive）。"""
    return (datetime.datetime.now(datetime.timezone.utc) + _fixed_offset()).replace(tzinfo=None)


def _now():
    return _now_dt().strftime("%Y-%m-%d %H:%M:%S")


def _traffic_path():
    value = str(os.environ.get(TRAFFIC_FILE_ENV) or "").strip()
    return Path(value) if value else None


def _history_path():
    value = str(os.environ.get(HISTORY_FILE_ENV) or "").strip()
    return Path(value) if value else None


def calibration_factor():
    """本地计量 → 面板实际扣量的经验倍率。

    环境变量没设或值非法时回退到 DEFAULT_CALIBRATION，绝不返回 0 或负数
    （否则预估用量会恒为 0，面板会误导用户以为不花钱）。
    """
    raw = str(os.environ.get(CALIBRATION_ENV) or "").strip()
    if not raw:
        return DEFAULT_CALIBRATION
    try:
        value = float(raw)
    except Exception:
        return DEFAULT_CALIBRATION
    return value if value > 0 else DEFAULT_CALIBRATION


def estimate_bytes(value):
    """把本地字节数换算成预估的实际扣量。"""
    try:
        return int(int(value or 0) * calibration_factor())
    except Exception:
        return 0


def _write_json(path, payload):
    """原子写，避免面板读到半截 JSON。"""
    try:
        path.parent.mkdir(parents=True, exist_ok=True)
        tmp = path.with_suffix(path.suffix + ".tmp")
        tmp.write_text(json.dumps(payload, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
        os.replace(str(tmp), str(path))
        try:
            os.chmod(str(path), 0o600)
        except Exception:
            pass
    except Exception:
        pass


def snapshot():
    """当前计量快照。任何时候都可安全调用。"""
    with _LOCK:
        data = dict(_STATE)
    data["bytes_total"] = int(data.get("bytes_up", 0)) + int(data.get("bytes_down", 0))
    return data


def begin_batch():
    """开始新批次。已有批次仍在跑时不重置，避免丢失计数。"""
    with _LOCK:
        if _STATE["running"]:
            return snapshot()
        _STATE.update({
            "running": True,
            "started_at": _now(),
            "finished_at": None,
            "bytes_up": 0,
            "bytes_down": 0,
            "connections": 0,
            "accounts": 0,
            "archived": False,
        })
        data = snapshot()
    _flush(data)
    return data


def record(direction, nbytes):
    """累加一个中继块的字节数。热路径，必须极快且不抛异常。"""
    try:
        count = int(nbytes)
    except Exception:
        return
    if count <= 0:
        return
    with _LOCK:
        if direction == "up":
            _STATE["bytes_up"] += count
        else:
            _STATE["bytes_down"] += count


def count_connection():
    with _LOCK:
        _STATE["connections"] += 1


def count_account(count=1):
    with _LOCK:
        _STATE["accounts"] += max(0, int(count))


def _flush(data=None):
    path = _traffic_path()
    if path is None:
        return
    _write_json(path, data if data is not None else snapshot())


def flush():
    """把当前计数落盘（面板轮询用）。"""
    _flush()


def finish_batch():
    """结束批次：落盘并追加一条历史记录。

    归档后把内存态标记为 archived：内存里的数字保留着（面板「本批」还要
    显示刚跑完的这批），但 totals()/window() 不能再把它加一遍 —— 它已经
    进历史了，否则刚跑完的批次会被重复计成双倍。
    """
    with _LOCK:
        if not _STATE["running"] and _STATE["started_at"] is None:
            return snapshot()
        _STATE["running"] = False
        _STATE["finished_at"] = _now()
        _STATE["archived"] = True
        data = snapshot()
    _flush(data)
    _append_history(data)
    return data


def _append_history(data):
    path = _history_path()
    if path is None:
        return
    entry = {
        "started_at": data.get("started_at"),
        "finished_at": data.get("finished_at"),
        "bytes_up": data.get("bytes_up", 0),
        "bytes_down": data.get("bytes_down", 0),
        "bytes_total": data.get("bytes_total", 0),
        "connections": data.get("connections", 0),
        "accounts": data.get("accounts", 0),
    }
    try:
        history = []
        if path.is_file():
            try:
                loaded = json.loads(path.read_text(encoding="utf-8"))
                if isinstance(loaded, list):
                    history = loaded
                elif isinstance(loaded, dict) and isinstance(loaded.get("batches"), list):
                    history = loaded["batches"]
            except Exception:
                history = []
        history.append(entry)
        _write_json(path, {"batches": history[-HISTORY_LIMIT:]})
    except Exception:
        pass


def read_history():
    """读取历史批次，返回列表（新到旧）。"""
    path = _history_path()
    if path is None or not path.is_file():
        return []
    try:
        loaded = json.loads(path.read_text(encoding="utf-8"))
    except Exception:
        return []
    if isinstance(loaded, dict):
        loaded = loaded.get("batches") or []
    if not isinstance(loaded, list):
        return []
    return list(reversed(loaded))


def read_metrics():
    """面板用：读取计量数据。

    批次进行中时以内存为准 —— 内存是实时值，落盘只在批次开始/结束和
    显式 flush 时发生。若优先读文件，面板会显示陈旧数据（例如刚跑完
    一批却仍显示 0）。
    """
    with _LOCK:
        live = _STATE["running"] or _STATE["bytes_up"] or _STATE["bytes_down"]
    if live:
        return snapshot()
    path = _traffic_path()
    if path is not None and path.is_file():
        try:
            loaded = json.loads(path.read_text(encoding="utf-8"))
            if isinstance(loaded, dict):
                loaded.setdefault("bytes_total",
                                  int(loaded.get("bytes_up", 0)) + int(loaded.get("bytes_down", 0)))
                return loaded
        except Exception:
            pass
    return snapshot()


def _parse_stamp(value):
    """解析 "YYYY-MM-DD HH:MM:SS"，失败返回 None。"""
    text = str(value or "").strip()
    if not text:
        return None
    for fmt in ("%Y-%m-%d %H:%M:%S", "%Y-%m-%dT%H:%M:%S"):
        try:
            return datetime.datetime.strptime(text[:19], fmt)
        except Exception:
            continue
    return None


def _history_records():
    """历史批次，按时间从新到旧，附解析后的时间戳。"""
    records = []
    for item in read_history():
        if not isinstance(item, dict):
            continue
        record = dict(item)
        record["_started"] = _parse_stamp(item.get("started_at"))
        record["_finished"] = _parse_stamp(item.get("finished_at"))
        records.append(record)
    return records


def summarize(records):
    """把若干批次记录汇总成一个计量块。"""
    up = sum(int(r.get("bytes_up") or 0) for r in records)
    down = sum(int(r.get("bytes_down") or 0) for r in records)
    return {
        "batches": len(records),
        "bytes_up": up,
        "bytes_down": down,
        "bytes_total": up + down,
        "connections": sum(int(r.get("connections") or 0) for r in records),
        "accounts": sum(int(r.get("accounts") or 0) for r in records),
    }


def _live_unarchived():
    """取内存里「尚未归档」的批次；已归档的返回 None。

    归档后内存态与历史记录指的是同一批，再加一次就是双倍。
    """
    with _LOCK:
        live = dict(_STATE)
    if live.get("archived"):
        return None
    if not (live.get("running") or live.get("bytes_up") or live.get("bytes_down")):
        return None
    return live


def totals():
    """累计总量：历史批次 + 未归档的当前批次。"""
    records = _history_records()
    result = summarize(records)
    live = _live_unarchived()
    if live is not None:
        result["batches"] += 1
        result["bytes_up"] += int(live.get("bytes_up") or 0)
        result["bytes_down"] += int(live.get("bytes_down") or 0)
        result["bytes_total"] += int(live.get("bytes_up") or 0) + int(live.get("bytes_down") or 0)
        result["connections"] += int(live.get("connections") or 0)
        result["accounts"] += int(live.get("accounts") or 0)
    return result


def window(hours=24, now=None):
    """最近 N 小时内的流量。

    批次没有结束时间时（仍在跑）按开始时间归属；跨越窗口边界的批次
    整批计入，不做按比例摊分 —— 代理流量没有细粒度时间戳，摊分只会
    制造虚假精度。
    """
    try:
        span = float(hours)
    except Exception:
        span = 24.0
    if span <= 0:
        span = 24.0
    current = now or _now_dt()
    cutoff = current - datetime.timedelta(hours=span)
    picked = []
    for record in _history_records():
        stamp = record.get("_finished") or record.get("_started")
        if stamp is not None and stamp >= cutoff:
            picked.append(record)
    result = summarize(picked)
    live = _live_unarchived()
    live_stamp = _parse_stamp(live.get("started_at")) if live is not None else None
    if live is not None and live_stamp is not None and live_stamp >= cutoff:
        result["batches"] += 1
        result["bytes_up"] += int(live.get("bytes_up") or 0)
        result["bytes_down"] += int(live.get("bytes_down") or 0)
        result["bytes_total"] += int(live.get("bytes_up") or 0) + int(live.get("bytes_down") or 0)
        result["connections"] += int(live.get("connections") or 0)
        result["accounts"] += int(live.get("accounts") or 0)
    result["hours"] = span
    return result


def format_bytes(value):
    """人类可读的字节数。"""
    try:
        size = float(value)
    except Exception:
        return "—"
    for unit in ("B", "KB", "MB", "GB", "TB"):
        if abs(size) < 1024.0:
            return ("%.0f %s" if unit == "B" else "%.2f %s") % (size, unit)
        size /= 1024.0
    return "%.2f PB" % size


@atexit.register
def _close_on_exit():
    try:
        with _LOCK:
            if _STATE["running"]:
                _STATE["running"] = False
                _STATE["finished_at"] = _now()
                data = snapshot()
            else:
                data = None
        if data is not None:
            _flush(data)
            _append_history(data)
    except Exception:
        pass
