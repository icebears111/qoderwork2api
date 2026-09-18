#!/bin/bash
# sync-pats.sh — 从 CPA 容器拷出 qoderwork auth，提取 PAT 写到 ./pats（只保留 CN）
set -e
cd /root/qoderwork2api
mkdir -p pats /tmp/qw-sync
rm -rf /tmp/qw-sync
mkdir -p /tmp/qw-sync
docker cp cpa-manager-plus-cli-proxy-api-1:/root/.cli-proxy-api/. /tmp/qw-sync/
kept=0; skipped=0
for f in /tmp/qw-sync/qoderwork*.json; do
  [ -e "$f" ] || continue
  # 只处理 CN：domain 为空或 qoder.com.cn
  dom=$(grep -o '"domain"[[:space:]]*:[[:space:]]*"[^"]*"' "$f" | head -1 | sed 's/.*: *"//;s/"$//')
  case "$dom" in
    ""|*qoder.com.cn*) ;;
    *) skipped=$((skipped+1)); continue;;
  esac
  # 提取 PAT + uid + nickname
  if ! python3 - "$f" > /tmp/qw-pat.json <<'EOF'
import json,sys
d=json.load(open(sys.argv[1]))
auth=d.get("auth",d)
acct=d.get("account",d)
pat=auth.get("personalToken","")
uid=acct.get("uid","")
nick=acct.get("nickname","")
if not pat or not uid: sys.exit(1)
print(json.dumps({"pat":pat,"uid":uid,"nickname":nick},ensure_ascii=False))
EOF
  then
    skipped=$((skipped+1)); continue
  fi
  uid=$(python3 -c "import json;print(json.load(open('/tmp/qw-pat.json'))['uid'])")
  mv /tmp/qw-pat.json "pats/qoderwork-${uid}.json"
  chmod 600 "pats/qoderwork-${uid}.json"
  kept=$((kept+1))
done
echo "kept_cn=$kept skipped=$skipped total_in_pats=$(ls pats/ 2>/dev/null | wc -l)"
