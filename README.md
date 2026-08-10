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

1. 按设备架构下载 FPK（GitHub Releases）：
   - `xingyun-api_vX.X.X.fpk` — x86_64 设备
   - `xingyun-api_vX.X.X_arm64.fpk` — ARM 设备（如飞牛 ARM 机型）
   - `xingyun-api-docker_vX.X.X.fpk` — Docker 版安装包（自动拉取 ghcr.io 镜像，x86_64 / ARM 通用）
2. 打开飞牛应用中心 → 手动安装 → 上传对应 FPK
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

### 方式三：Docker 部署

```bash
# 方式 A：直接拉取官方镜像（ghcr.io，推荐，支持 amd64 / arm64 双架构）
docker pull ghcr.io/veenyi/xingyun-api:latest
docker run -d --name xingyun-api \
  -p 34891:34891 \
  -v ./data:/data \
  --restart unless-stopped \
  ghcr.io/veenyi/xingyun-api:latest

# 方式 B：源码构建
git clone https://github.com/veenyi/XingyunAPI.git && cd XingyunAPI
docker compose -f docker/docker-compose.yml up -d

# 查看状态 / 日志
docker ps
docker logs -f xingyun-api
```

面板：`http://<主机IP>:34891`，数据（账号、设置、聊天历史）持久化在宿主机 `./data` 目录（镜像内 `/data`，即 `~/.joycode-proxy/`）。自定义端口：修改 `PORT` 环境变量与端口映射，或直接 `docker run -e PORT=xxxx -p xxxx:xxxx ghcr.io/veenyi/xingyun-api:latest`。镜像默认以 root 运行以保证绑定卷可写；如需更换数据目录，把 `-v ./data:/data` 改为你的目录即可。

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

## 使用立场与禁止商用

本项目基于 [JoyCode2Api](https://github.com/vibe-coding-labs/JoyCode2Api)（Apache 2.0）衍生开发，并遵守上游开源许可。

**作者立场声明：**

- 本项目定位为**个人学习、内部研究、非商业环境**使用
- 作者**不支持、不授权、不背书**任何将本项目用于商业目的的行为（包括但不限于：转售、打包售卖、对外提供收费服务、企业生产环境商业化部署等）
- 任何商业使用均属于**使用者个人行为**，与作者无关；作者不对商业使用提供任何支持、维护或担保
- 商用衍生、再分发需自行评估并遵守上游 Apache 2.0 许可及相关法律法规，由此产生的一切后果由使用者自行承担

## 免责声明

1. 本项目仅作为服务集成工具，不对上游软件（含第三方 API、模型服务、账号体系等）本身的安全性、稳定性、可用性作任何保证
2. 使用者需自行确保其使用行为符合上游平台的服务条款与当地法律法规（如账号使用、接口调用、数据合规等）
3. 因使用本项目产生的任何直接或间接损失（包括但不限于：数据丢失、账号封禁、服务中断、接口变更、法律纠纷等），作者均不承担责任
4. 本项目按"现状"（AS-IS）提供，无任何明示或默示的担保；使用者自行承担全部使用风险
5. 本项目不包含任何用户的账号、密钥、聊天记录等个人数据（均存储于使用者本地服务器）

## License

Apache 2.0（遵循上游 JoyCode2Api 许可；作者立场声明见上文「使用立场与禁止商用」）
