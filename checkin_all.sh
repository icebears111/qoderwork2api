#!/bin/bash
# checkin_all.sh 批量签到测试所有 qoderwork auth 文件
set -e

AUTHS_DIR="/root/cpa-manager-plus/cliproxyapi/auths"
cd /root/qoderwork2api

# 构建测试二进制
cat > /tmp/checkin_test.go <<'EOF'
package main

import (
	"fmt"
	"os"
	"qoderwork2api/internal/cred"
	"qoderwork2api/internal/upstream"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: checkin_test <auth.json>")
		os.Exit(1)
	}
	c, err := cred.LoadFile(os.Args[1])
	if err != nil {
		fmt.Printf("LOAD_ERR %v\n", err)
		os.Exit(1)
	}
	up := upstream.New()
	if err := c.EnsureJT(up.Base); err != nil {
		fmt.Printf("JT_ERR %v\n", err)
		os.Exit(1)
	}
	ok, err := up.DailyCheckin(c.JT)
	if err != nil {
		fmt.Printf("CHECKIN_FAIL %v\n", err)
		os.Exit(1)
	}
	remain, exceeded, err := up.QuotaUsage(c.JT)
	if err != nil {
		fmt.Printf("QUOTA_ERR %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("OK success=%v remain=%d exceeded=%v\n", ok, remain, exceeded)
}
EOF

mkdir -p /tmp/checkin_bin
cp /tmp/checkin_test.go /tmp/checkin_bin/main.go
cd /tmp/checkin_bin && go mod init checkin_test 2>/dev/null || true
go mod edit -replace qoderwork2api=/root/qoderwork2api
go mod tidy 2>/dev/null
go build -o /tmp/checkin_bin/checkin_test . 2>&1

# 批量执行
echo "file|uid|status|detail"
echo "---|---|---|---"
for f in "$AUTHS_DIR"/qoderwork-*.json; do
	uid=$(python3 -c "import json; d=json.load(open('$f')); print(d.get('account',{}).get('uid','?')[:13])" 2>/dev/null || echo "?")
	out=$(/tmp/checkin_bin/checkin_test "$f" 2>&1 || true)
	status=$(echo "$out" | awk '{print $1}')
	detail=$(echo "$out" | cut -d' ' -f2-)
	echo "$(basename $f)|$uid|$status|$detail"
done
