# -*- coding: utf-8 -*-
"""
阶段 7：双端落盘与 CPA 自动导入
  1) codex-<email>-free.json -> PostgreSQL accounts 表 (payload.cliproxyapi_type='codex')
  2) 同步到 /www/wwwroot/cpa-manager-plus/cliproxyapi/auths/
用法:
  python3 deliver_codex.py            # 全量同步
  python3 deliver_codex.py <email>    # 单账号
"""
import base64, json, os, sys, time
from datetime import datetime, timezone
from pathlib import Path

import psycopg

CPA_LOCAL = Path('/app/data/cpa_auth_files')
PG_DSN = os.environ.get('PG_DSN', 'postgresql://grok2api:grok2api@gptcli-2api-postgres:5432/grok2api')
OIDC_CLIENT_ID = 'app_EMoamEEZ73f0CkXaXp7hrann'
# 宿主机 CPA 目录（容器内看不到，写清单给宿主机脚本消费）
MANIFEST = Path('/app/data/cpa_sync_manifest.txt')


def _b64url_decode(s: str) -> bytes:
    s += '=' * (-len(s) % 4)
    return base64.urlsafe_b64decode(s)


def parse_jwt(token: str) -> dict:
    try:
        parts = token.split('.')
        if len(parts) < 2:
            return {}
        return json.loads(_b64url_decode(parts[1]))
    except Exception:
        return {}


def sync_to_chatgpt2api(cred: dict) -> bool:
    import urllib.request
    auth_key = "X1HmVbEhEl6--KCmBlw0Le_l_93xwhoxtb2ZdBmM1M6MtbJml8jd9MDi-vA4E4RE"
    body = json.dumps({"accounts": [cred], "tokens": []}).encode("utf-8")
    for endpoint in ["http://172.17.0.1:3000/api/accounts", "http://134.185.81.98:3000/api/accounts"]:
        try:
            req = urllib.request.Request(
                endpoint,
                data=body,
                headers={
                    "Authorization": "Bearer " + auth_key,
                    "Content-Type": "application/json"
                }
            )
            with urllib.request.urlopen(req, timeout=10) as res:
                if res.status == 200:
                    return True
        except Exception:
            continue
    return False

def build_payload(cred: dict) -> tuple[dict, str, str]:
    """返回 (payload, id, user_id)"""
    id_token = cred.get('id_token', '')
    claims = parse_jwt(id_token)
    auth = claims.get('https://api.openai.com/auth', {}) or {}
    principal = (
        auth.get('chatgpt_user_id')
        or auth.get('user_id')
        or claims.get('https://api.openai.com/principal_id')
        or claims.get('sub')
        or ''
    )
    user_id = principal if str(principal).startswith('user-') else (f'user-{principal}' if principal else '')
    email = cred.get('email', '')

    exp_raw = cred.get('expired', '')
    exp_unix = 0.0
    if exp_raw:
        try:
            dt = datetime.fromisoformat(str(exp_raw).replace('Z', '+00:00'))
            exp_unix = dt.timestamp()
        except Exception:
            pass
    if not exp_unix:
        exp_unix = float(claims.get('exp') or (time.time() + 864000))

    payload = {
        'key': cred.get('access_token', ''),
        'sso': cred.get('id_token', ''),
        'email': email,
        'source': 'cliproxyapi',
        'user_id': user_id,
        'id_token': id_token,
        'auth_mode': 'cliproxyapi_import',
        'sso_token': cred.get('access_token', ''),
        'expires_at': exp_unix,
        'sso_cookie': cred.get('access_token', ''),
        'create_time': datetime.now(timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ'),
        'principal_id': user_id,
        'refresh_token': cred.get('refresh_token', ''),
        'oidc_client_id': OIDC_CLIENT_ID,
        'session_cookies': {'sso': cred.get('access_token', '')},
        'cliproxyapi_type': 'codex',
    }
    row_id = f'https://auth.openai.com::{user_id}'
    return payload, row_id, user_id


def upsert_pg(conn, email: str, payload: dict, row_id: str, user_id: str, expires_at) -> str:
    cur = conn.cursor()
    cur.execute('SELECT id, row_version FROM accounts WHERE id = %s', (row_id,))
    exist = cur.fetchone()
    if exist:
        cur.execute(
            """UPDATE accounts SET email=%s, user_id=%s, payload=%s, expires_at=%s,
                   updated_at=now(), row_version=row_version+1 WHERE id=%s""",
            (email, user_id, json.dumps(payload, ensure_ascii=False), expires_at, row_id),
        )
        conn.commit()
        return 'updated'
    cur.execute(
        """INSERT INTO accounts (id, email, user_id, team_id, payload, expires_at, updated_at, row_version)
               VALUES (%s,%s,%s,NULL,%s,%s,now(),1)""",
        (row_id, email, user_id, json.dumps(payload, ensure_ascii=False), expires_at),
    )
    conn.commit()
    return 'inserted'


def main():
    target = sys.argv[1].strip().lower() if len(sys.argv) > 1 else None
    files = sorted(CPA_LOCAL.glob('codex-*.json'))
    if target:
        files = [f for f in files if target in f.name.lower()]
    if not files:
        print('没有找到待落盘的 codex 凭证')
        return

    stats = {'inserted': 0, 'updated': 0, 'skipped': 0, 'synced': []}
    with psycopg.connect(PG_DSN, connect_timeout=10) as conn:
        for f in files:
            try:
                cred = json.loads(f.read_text(encoding='utf-8'))
            except Exception as e:
                print(f'✕ 读取失败 {f.name}: {e}')
                stats['skipped'] += 1
                continue
            email = cred.get('email') or f.name[len('codex-'):].replace('-free.json', '')
            if not cred.get('id_token') or not cred.get('refresh_token'):
                print(f'✕ 凭证不完整 {email}')
                stats['skipped'] += 1
                continue
            payload, row_id, user_id = build_payload(cred)
            if not user_id:
                print(f'✕ id_token 解析不出 user_id: {email}')
                stats['skipped'] += 1
                continue
            expires_at = datetime.fromtimestamp(payload['expires_at'], tz=timezone.utc)
            try:
                act = upsert_pg(conn, email, payload, row_id, user_id, expires_at)
                stats[act] += 1
                stats['synced'].append(f.name)
                print(f'✅ [{act}] {email}  id={row_id}  exp={expires_at.isoformat()}')
                try:
                    sync_to_chatgpt2api(cred)
                    print(f'  > 同步 chatgpt2api 成功: {email}')
                except Exception as _c2e:
                    print(f'  > 同步 chatgpt2api 异常: {_c2e}')
            except Exception as e:
                print(f'✕ PG 写入失败 {email}: {type(e).__name__} {e}')
                stats['skipped'] += 1

    MANIFEST.write_text('\n'.join(stats['synced']) + '\n', encoding='utf-8')
    print()
    print(f"汇总: 新增 {stats['inserted']} | 更新 {stats['updated']} | 跳过 {stats['skipped']}")
    print(f'待同步 CPA 目录清单: {MANIFEST} ({len(stats["synced"])} 个)')


if __name__ == '__main__':
    main()
