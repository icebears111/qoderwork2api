# QoderWork2API

QoderWork CN（`qoder.com.cn`）的 OpenAI 兼容反向代理。本仓库是 **icebears.cn 生产运行版**，
fork 自 [Sliverkiss/qoderwork2api](https://github.com/Sliverkiss/qoderwork2api)，在上游基础上做了
多用户体系、SSO 接入、全模型思考模式等改造。

```
任意 OpenAI 客户端
   │  POST /v1/chat/completions · GET /v1/models（OpenAI 兼容，SSE）
   ▼
本服务（Go，单二进制，前端 go:embed 内嵌）
   │  OAuth 凭证 + COSY 签名 + QoderEncoding
   ▼
https://gateway.qoder.com.cn  agent_chat_generation（桌面端同一条 Agent 通道）
```

## 与上游的差异

| | 上游原版 | 本仓库 |
|---|---|---|
| 用户体系 | 单 API key | 多用户（users.json，sha256 随机 key + 密码登录），每用户独立账号池 |
| 登录方式 | 仅 `/login` | `/login` + 登录页 + 信任 nginx SSO 身份头（`X-Auth-User`/`X-Auth-Role`）|
| 思考模式 | `is_reasoning` 硬编码 false | **所有模型统一开思考**（与 qwen3.8-flash 同一链路） |
| 模型列表 | 静态表 | 每小时从上游动态拉取（`/v3` 模型接口），失败回退静态表 |
| 部署 | 发布端口 | 不发布端口，挂内网由反向代理接入 |

## 功能特性

- 🔐 **OAuth 设备授权** — PKCE Device Flow 获取 `dt-/drt-` 凭证（access ~30 天，refresh ~1 年轮换），无需官方客户端
- 🔄 **多账号轮转** — 按积分降序选号，402/积分不足冷却 12h、429 短冷却、连续错误阈值冷却，防雪崩
- 🧩 **COSY 签名** — RSA 包裹 AES 会话密钥 + AES-CBC identity + MD5 请求签名；机器指纹随机生成并持久化
- 📡 **流式 + 非流式** — 上游仅支持嵌套 SSE，本服务解析为标准 OpenAI chunk；非流式请求内部聚合后返回
- 🧠 **思考模式全模型开启** — 所有模型都走 qwen3.8-flash 同一条思考链路（`is_reasoning: true` + `source: "system"`）。`source` 是上游触发思考的真正开关，实测缺它 `reasoning_content` 永不下发。客户端可用 OpenAI 风格 `reasoning_effort`（`low`/`medium`/`xhigh`）透传档位，不传走上游默认；非法值自动忽略
- ⏰ **定时签到** — 默认每日 09:00 / 21:00 自动签到 + 积分刷新 + 解冻
- 📊 **管理面板** — `/admin` 单页应用（内嵌于二进制，无外部静态资源）：账号池、积分、签到、用户管理

## 快速开始

```bash
cp config.example.json config.json   # 编辑 api_key 等
docker compose up -d --build         # 默认不发布端口（生产由 nginx 反代接入）
```

需要本机直连调试时，在 compose 里加回 `ports: ["127.0.0.1:8963:8963"]`。

添加 Qoder 账号：

```bash
./login.sh          # 打印授权链接 → 浏览器完成 OAuth → 自动落盘 auths/
./credit.sh         # 全部账号积分日报
./credit.sh -json   # 原始 JSON
```

验证：

```bash
curl -s http://127.0.0.1:8963/healthz
curl -s http://127.0.0.1:8963/v1/models -H "Authorization: Bearer <api_key>"
curl -s http://127.0.0.1:8963/v1/chat/completions \
  -H "Authorization: Bearer <api_key>" -H "Content-Type: application/json" \
  -d '{"model":"qwen3.8-flash","messages":[{"role":"user","content":"9.11和9.9哪个大"}],"stream":true}'
```

## 路由

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/v1/chat/completions` | 对话（OpenAI 兼容；stream 两种都支持） |
| GET | `/v1/models` | 模型目录（含 credits/上下文/能力标记） |
| GET | `/healthz` `/status` `/stats` | 健康与统计 |
| GET | `/` → `/admin` | 管理面板 |
| POST | `/api/login` | 用户密码登录，返回该用户的 api_key |
| GET/POST/DELETE | `/users/` | 用户 CRUD |
| GET/POST/DELETE | `/accounts/` | 当前用户账号池管理（含手动登录 URL/轮询） |
| GET | `/quota` · POST `/quota/refresh` | 账号积分查询/刷新 |

## 配置

`config.json`（挂载进容器，示例见 `config.example.json`）：

| 键 | 说明 |
|---|---|
| `listen` | 监听地址，生产用 `:8963` |
| `api_key` | 管理员 key；生产环境由 SSO 层鉴权时置 `not-required` |
| `auth_dir` / `state_file` | OAuth 凭据目录与账号状态文件 |
| `cooldown` | `hard_credit`(12h) / `soft_rate`(60s) / `err_threshold`(5) / `err_cooldown`(10m) |
| `schedule` | `checkin_hours` 签到时刻，`keepalive_hours` 凭证保活时刻 |
| `upstream.timeout_seconds` | 上游请求超时（生产 300） |

## 上游协议要点（排障用）

- 国内站账号**必须**打 `gateway.qoder.com.cn`（`api3.qoder.sh` 不认国内 uid；打错时挂 66 秒后 HTTP/2 INTERNAL_ERROR，极易误判为网络问题）
- `auth.json` 仅启动时读取 → 凭据落盘后需重启进程生效
- **模型 key 校验极静默**：不认识的 key 不报错，回退默认模型（自称 Qwen）。判别方法：不同 key 返回逐字相同回答 = key 被忽略
- 思考模式下 `reasoning_effort` 参数需原样透传至 `parameters`，缺一则上游不思考
- 上游 Agent 通道对免费/试用身份不计费（响应内 `credits: 0, billable: false` 为上游原样返回，本服务不改写计费字段）

## 免责声明

本项目仅供学习与研究使用，通过复用你自己账号的登录态转发请求。这种方式可能不符合
QoderWork 服务条款，风险（含账号被风控、封禁）由使用者自行承担。与腾讯/阿里官方无关，
上游接口随时可能调整。MIT License。
