#!/usr/bin/env bash
# login.sh — QoderWork OAuth 设备授权登录 → 落盘 auth 文件
#
# 用法:
#   ./login.sh
#
# 流程:
#   1. 打印 OAuth 授权 URL
#   2. 你在浏览器打开 URL 完成登录授权
#   3. 回到这里按 y → poll 一次拿 dt-/drt- → 查 nickname → 落盘 auths/qoderwork-<uid>.json
#   4. 重启 qoderwork2api 容器加载新账号
set -euo pipefail

cd "$(dirname "$0")"
AUTH_DIR="./auths"
CONTAINER="qoderwork2api"

mkdir -p "$AUTH_DIR"

# login 工具：不存在才编译（源码改动后手动 go build -o login ./cmd/login）
LOGIN_BIN="./login"
if [[ ! -x "$LOGIN_BIN" ]]; then
    go build -o "$LOGIN_BIN" ./cmd/login
fi

echo "============================================================"
echo "  QoderWork OAuth 登录"
echo "============================================================"
echo ""

AUTH_URL=$("$LOGIN_BIN" url)

echo "请在浏览器中打开以下链接完成登录授权："
echo ""
echo "  $AUTH_URL"
echo ""

# 尝试复制到剪贴板
if command -v xclip &>/dev/null; then
    echo -n "$AUTH_URL" | xclip -selection clipboard 2>/dev/null && echo "(已复制到剪贴板)"
elif command -v xsel &>/dev/null; then
    echo -n "$AUTH_URL" | xsel --clipboard 2>/dev/null && echo "(已复制到剪贴板)"
fi

echo ""
read -rp "完成授权后按 y 继续: " ans
if [[ "$ans" != "y" && "$ans" != "Y" ]]; then
    echo "已取消"
    exit 1
fi

echo ""
echo "正在获取 token..."

RESULT=$("$LOGIN_BIN" poll) || {
    echo ""
    echo "获取 token 失败。可能原因："
    echo "  - 授权还没完成就按了 y（重新运行 ./login.sh 再试）"
    echo "  - 授权页面报'参数无效'（把报错截图发出来排查）"
    exit 1
}

TOKEN=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin)['token'])")
REFRESH=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin)['refresh_token'])")
USER_ID=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin)['user_id'])")
EXPIRES_IN=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin)['expires_in'])")

if [[ -z "$USER_ID" ]]; then
    echo "无法获取 user_id，请检查 token 是否有效"
    exit 1
fi

EXPIRES_AT=$(( $(date +%s) + EXPIRES_IN / 1000 ))

# ─── 查询 nickname ───────────────────────────────────────
NICKNAME=$(python3 -c "
import json, urllib.request
req = urllib.request.Request(
    'https://openapi.qoder.com.cn/api/v1/userinfo',
    headers={'Authorization': 'Bearer $TOKEN', 'Accept': 'application/json',
             'User-Agent': 'Go-http-client/2.0'})
try:
    with urllib.request.urlopen(req, timeout=10) as r:
        d = json.loads(r.read().decode())
        print(d.get('name') or d.get('username') or '')
except Exception:
    pass
" 2>/dev/null || true)

# ─── 领取 Pro Trial + 签到（KNOWLEDGE：CN 限定，幂等失败不阻塞）───
python3 - <<PYEOF
import json, urllib.request, urllib.error

BASE = "https://openapi.qoder.com.cn"
H = {"Authorization": "Bearer $TOKEN", "Accept": "application/json",
     "Content-Type": "application/json", "User-Agent": "Go-http-client/2.0"}

def call(method, path):
    req = urllib.request.Request(BASE + path, method=method, headers=H,
                                 data=b"{}" if method == "POST" else None)
    try:
        with urllib.request.urlopen(req, timeout=15) as r:
            return r.status, json.loads(r.read().decode() or "{}")
    except urllib.error.HTTPError as e:
        try:
            return e.code, json.loads(e.read().decode() or "{}")
        except Exception:
            return e.code, {}
    except Exception as e:
        return 0, {"error": str(e)}

# Pro Upgrade
st, elig = call("GET", "/sash/api/v1/me/pro-upgrade/eligibility")
if elig.get("eligible") or (elig.get("data") or {}).get("eligible"):
    st2, claim = call("POST", "/sash/api/v1/me/pro-upgrade/claim")
    ok = claim.get("success") or (claim.get("data") or {}).get("success")
    print(f"Pro Trial: {'领取成功' if ok else '领取失败 ' + json.dumps(claim)[:120]}")
else:
    reason = elig.get("reason") or (elig.get("data") or {}).get("reason") or json.dumps(elig)[:120]
    print(f"Pro Trial: 不可领取（{reason}）")

# 签到（status 在顶层，实测：{"status":"CLAIMED_TODAY"|"CLAIMABLE",...}）
st, st_body = call("GET", "/sash/api/v1/me/daily-check-in/status")
status = st_body.get("status") or (st_body.get("data") or {}).get("status", "")
if status == "CLAIMABLE":
    st2, claim = call("POST", "/sash/api/v1/me/daily-check-in/claim")
    data = claim.get("data") or claim
    rc = data.get("rewardCredits", "?")
    res = data.get("result", "")
    print(f"签到: {'+%s credits' % rc if res == 'CLAIMED' else '失败 ' + json.dumps(claim)[:120]}")
elif status:
    streak = st_body.get("currentStreakDays", "?")
    print(f"签到: 今日已签（status={status}, 连续 {streak} 天）")
else:
    print(f"签到: 状态未知 {json.dumps(st_body)[:120]}")
PYEOF

# ─── 落盘 auth 文件 ──────────────────────────────────────
AUTH_FILE="$AUTH_DIR/qoderwork-${USER_ID}.json"
if [[ -f "$AUTH_FILE" ]]; then
    echo "账号已存在（uid=$USER_ID），将覆盖更新凭证"
    ACTION="覆盖"
else
    echo "新账号（uid=$USER_ID），新增 auth 文件"
    ACTION="新增"
fi
python3 - <<PYEOF
import json

auth = {
    "auth": {
        "accessToken": "$TOKEN",
        "refreshToken": "$REFRESH",
        "expiresAt": $EXPIRES_AT,
        "domain": "qoder.com.cn"
    },
    "account": {
        "uid": "$USER_ID",
        "nickname": "$NICKNAME"
    }
}
with open("$AUTH_FILE", "w") as f:
    json.dump(auth, f, indent=1)
print(f"已保存（$ACTION）: $AUTH_FILE")
PYEOF

# ─── 重启服务 ────────────────────────────────────────────
echo ""
if docker ps --format '{{.Names}}' | grep -q "^${CONTAINER}$"; then
    echo "重启 $CONTAINER 加载新账号..."
    docker restart "$CONTAINER" >/dev/null
    sleep 2
    COUNT=$(curl -s http://127.0.0.1:7864/status -H "Authorization: Bearer ${API_KEY:-tistzach}" 2>/dev/null | python3 -c "import json,sys; print(len(json.load(sys.stdin).get('accounts',[])))" 2>/dev/null || echo "?")
    echo "服务已重启，当前账号数: $COUNT"
else
    echo "容器 $CONTAINER 未运行，auth 文件已保存，下次启动自动加载"
fi

echo ""
echo "============================================================"
echo "  登录完成！"
echo "  UID: $USER_ID"
echo "  Nickname: ${NICKNAME:-（未获取到）}"
echo "  Token: ${TOKEN:0:20}..."
echo "  有效期: $(date -d "@$EXPIRES_AT" '+%Y-%m-%d %H:%M' 2>/dev/null || date -r "$EXPIRES_AT" '+%Y-%m-%d %H:%M' 2>/dev/null || echo "$EXPIRES_AT")"
echo "============================================================"
