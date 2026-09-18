# QoderWork2API

> QoderWork CN 的 OpenAI 兼容反向代理，支持 OAuth 设备授权、多账号轮转、工具调用与流式响应。

## 功能特性

- 🔐 **OAuth 设备授权** — 无需 PAT，通过 PKCE Device Flow 获取 `dt-/drt-` 凭证（30 天 / 1 年轮换）
- 🔄 **多账号轮转** — 按积分降序选号，自动冷却 / 禁用 / 恢复，防雪崩设计
- 🛠 **工具调用** — 完整支持 OpenAI tools/tool_choice，流式 `tool_calls` 按 index 合并
- 📡 **流式 + 非流式** — 嵌套 SSE 解析，标准 OpenAI chunk 透传
- 🧩 **COSY 签名** — RSA + AES-CBC identity 加密，QoderEncoding body 编码
- ⏰ **定时签到** — 每日 09:00 / 21:00 自动签到 + 积分查询
- 📊 **积分监控** — `credit.sh` 一键查询全部账号剩余/总量/百分比
- 🔑 **OAuth 登录工具** — `login.sh` 交互式登录，落盘即生效
- 🏗 **Docker 部署** — 一键 `docker compose up`，healthcheck 常驻

## 快速开始

### 1. 克隆 & 配置

```bash
git clone https://github.com/Sliverkiss/qoderwork2api.git
cd qoderwork2api
cp config.example.json config.json
# 编辑 config.json，设置 api_key
```

### 2. 添加账号

```bash
./login.sh
# 打开浏览器授权 → 按 y → 自动落盘 auths/ → 重启容器
```

### 3. 启动服务

```bash
docker compose up -d --build
```

### 4. 验证

```bash
curl -s http://localhost:7864/v1/models -H "Authorization: Bearer your-api-key"
curl -s http://localhost:7864/v1/chat/completions \
  -H "Authorization: Bearer your-api-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"qwen3.8-max-preview","messages":[{"role":"user","content":"hi"}]}'
```

## 支持的模型

| 模型名 | 上游 key |
|---|---|
| `qwen3.8-max-preview` | `qmodel_preview` |
| `qwen3.7-max` | `qmodel_latest` |
| `deepseek-v4-pro` | `dmodel` |
| `deepseek-v4-flash` | `dfmodel` |
| `glm-5.2` | `gm51model` |
| `kimi-k2.7-code` | `kmodel` |
| `minimax-m2.7` | `mmodel` |
| `auto` | `auto` |

动态模型列表每小时从上游拉取，失败回退静态表。

## 配置说明

```json
{
  "listen": ":7864",
  "api_key": "your-api-key",
  "auth_dir": "./auths",
  "state_file": "./data/state.json",
  "region": "cn",
  "cooldown": {
    "hard_credit": "12h",    // 余额不足冷却
    "soft_rate": "60s",     // 429 / 404 短冷却
    "err_threshold": 5,      // 连续错误阈值
    "err_cooldown": "10m"    // 达阈值后冷却
  },
  "schedule": {
    "checkin_hours": [9, 21],
    "keepalive_hours": [0, 12]
  },
  "upstream": {
    "timeout_seconds": 180   // 上游请求超时
  }
}
```

## 工具脚本

| 脚本 | 用途 |
|---|---|
| `./login.sh` | OAuth 登录，落盘 auth 文件 |
| `./credit.sh` | 积分日报（美化输出） |
| `./credit.sh -json` | 积分原始 JSON |

## 架构文档

详细架构规格见 [SPEC.md](SPEC.md)，稳定性优化记录见 [LOOP.md](LOOP.md)。

## 免责声明

本项目仅供学习和研究使用。使用者需遵守 QoderWork 的服务条款，自行承担使用风险。作者不对任何因使用本项目产生的直接或间接损失负责。

## License

MIT
