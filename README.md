# 行云API（XingyunAPI）

京东 JoyCode 模型在线服务平台 —— 把 JoyCode（京东 AI 编程助手）的模型能力转成标准 OpenAI / Anthropic 兼容接口，自带 Web 管理面板：在线聊天、账号管理、积分监控、多账号自动轮询。支持飞牛 fnOS 一键安装（FPK）。

## 功能特性

- **在线聊天**：复刻 JoyCode IDE 问答面板 —— 问答 / 编程双模式、深度思考展示、联网搜索自动触发（模型 tool call → 网页搜索 → 回填续跑）
- **账号管理**：添加京东账号（OAuth 授权 / 手动粘贴 pt_key / 本机一键导入），数量不限
- **积分监控**：实时显示每个账号的已用 / 剩余积分、套餐有效期，数据概览页汇总
- **聚合 API Key**：一个 Key 自动在多个账号间按剩余积分轮询，积分不足自动切换，兼容 OpenAI / Anthropic 接口
- **API 兼容**：`/v1/chat/completions`（OpenAI）、`/v1/messages`（Anthropic/Claude Code）、`/v1/models`
- **深浅色主题**：跟随系统 / 浅色 / 深色一键切换
- **飞牛应用中心**：应用详情页「打开」按钮直达面板，安装向导设置密码

## 快速开始

### 方式一：飞牛 fnOS 应用中心安装（推荐）

1. 下载最新的 `xingyun-api_vX.X.X.fpk`（GitHub Releases）
2. 打开飞牛应用中心 → 手动安装 → 上传 FPK
3. 安装向导中设置面板密码（≥6 位，留空自动生成）
4. 安装完成后点击「打开」进入面板（端口 **34891**）

### 方式二：直接运行二进制

```bash
# 准备目录
mkdir -p ~/xingyun && cd ~/xingyun

# Linux x64
wget <release-url>/xingyun-api-linux-amd64
chmod +x xingyun-api-linux-amd64

# 启动（面板端口 34891）
./xingyun-api-linux-amd64 serve --port 34891
```

首次访问 `http://<主机IP>:34891` 按提示设置 root 密码。

## 使用流程

1. **登录面板**：浏览器打开 `http://<NAS_IP>:34891`，输入安装时设置的密码
2. **添加账号**：「账号管理」→「OAuth 授权登录」（NAS 环境在弹窗中直接粘贴 pt_key）或「一键导入本地 JoyCode 已登录账户」（本机已装 JoyCode 且登录时）
3. **聊天**：左侧「聊天」页直接对话，支持联网搜索与深度思考
4. **API 接入**：在「账号管理」复制账号的 API Token，或用「聚合 API Key」（设置页可查看 / 重新生成）

## API 使用

### OpenAI 兼容

```bash
curl http://<NAS_IP>:34891/v1/chat/completions \
  -H "Authorization: Bearer <API_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{"model":"GLM-5.1","messages":[{"role":"user","content":"你好"}]}'
```

### Claude Code

```bash
export ANTHROPIC_BASE_URL="http://<NAS_IP>:34891"
export ANTHROPIC_API_KEY="<API_TOKEN>"
export ANTHROPIC_MODEL="GLM-5.1"
claude
```

### 聚合 API Key（多账号自动轮询）

在「设置」页查看聚合 Key（`sk-joy-...`，可重新生成）。使用聚合 Key 请求时：

- 自动查询所有账号剩余积分（缓存 60 秒）
- 剩余积分 ≥ 阈值（默认 10，可在设置页调整）的账号视为可用
- 可用账号之间轮流切换；全部不足时回退到积分最高的账号

```bash
curl http://<NAS_IP>:34891/v1/chat/completions \
  -H "Authorization: Bearer <聚合KEY>" \
  -H "Content-Type: application/json" \
  -d '{"model":"GLM-5.1","messages":[{"role":"user","content":"你好"}]}'
```

## 常见问题

**Q：无法从本机获取 JoyCode 凭据？**
NAS 上没有安装 JoyCode IDE 属正常提示。请使用「OAuth 授权登录」粘贴 pt_key，或在已登录 JoyCode 的电脑上把 `state.vscdb` 上传到 NAS 后使用「一键导入」。

**Q：模型列表与官方不一致？**
模型列表实时从上游获取（账号有权访问的模型），获取失败时回退内置列表。

**Q：聚合 Key 失效？**
聚合 Key 可在设置页重新生成，旧 Key 立即失效；如客户端 401，请更新为设置页显示的最新 Key。

## 开发构建

```bash
# 前端
cd web && npm install && npm run build

# 二进制（Linux x64）
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "-s -w" -o xingyun-api ./cmd/JoyCode2Api

# 飞牛 FPK 打包
bash xingyun-api/build.sh   # 产物在 xingyun-api/pkg/
```

## 免责声明

本项目仅作为服务集成工具，不对上游软件本身的安全性、稳定性作任何保证。使用过程中产生的所有风险由用户自行承担。

## License

MIT
