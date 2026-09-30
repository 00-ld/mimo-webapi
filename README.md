# MiMo WebAPI

把小米 MiMo（aistudio.xiaomimimo.com）封装成 **OpenAI / Anthropic 双协议** 的本地 API 服务。
给团队或朋友用的时候，每个人发一把密钥，用量和限额各算各的。

**一条命令装好，一个网页点完接入。** 不需要手动复制 cookie，不需要手写配置。

```bash
./install.sh          # 编译 + 生成配置，一条命令
./mimowebapi -config config.json
```

然后打开 <http://127.0.0.1:8793/console>，**向导会引着你登录小米账号**，
登录完就能用了。

### 能接什么

控制台会**自动检测**本机装了哪些工具，每个给一张卡片，点「一键接入」就写好了：

| 工具 | 协议 | 接入方式 |
|---|---|---|
| **Codex CLI** | Responses API | 一键写入 `~/.codex/config.toml` |
| **Claude Code** | Anthropic Messages | 一键写入 `~/.claude/settings.json` |
| **DeepSeek Harness** | 三种都支持 | 一键写入 `cordis.patch.yml` |
| **ZCode** | OpenAI 兼容 | 按提示手填 |
| **WorkBuddy** | OpenAI 兼容 | 按提示手填 |
| 任何 OpenAI / Anthropic SDK | 双协议 | 填 base_url 即可 |

**工具调用（function calling）三条协议都实现了** —— 模型能真的调 shell、读写文件、
跑 `apply_patch`，不是只能聊天。

改写配置前**自动备份**原文件为 `<文件名>.bak-<时间戳>`，重复接入不会写坏。

---

## 开箱流程

第一次用只有三步：

| 步骤 | 你做什么 | 系统做什么 |
|---|---|---|
| 1 | 跑 `./install.sh` | 编译、生成带随机密码的配置 |
| 2 | 启动，打开控制台 | 检测到没有账号，自动跳到「账号」页 |
| 3 | 点「打开登录窗口」，正常登录 | 弹出独立 Chrome，登录后自动抓会话 |

第 3 步的浏览器是**独立的配置目录**，跟你日常用的 Chrome 互不干扰。
向导看不到你的密码，也不读你的浏览历史 —— 它只在你登录完成后
从浏览器自己的 cookie 里取会话凭证。

> **不想再登一次？**
> 你日常的 Chrome 里已经登着小Mi了，可以直接借用：
> ```bash
> ./use-my-chrome.sh              # 接上已开的 Chrome，直接抓会话
> ./use-my-chrome.sh --restart    # Chrome 没开调试端口？这个先帮你重开
> ```
> 走 DevTools 端口跟 Chrome 说话，不需要在菜单里开任何权限。

> **服务器上没有图形界面？**
> 「账号」页有「手动粘贴」入口：在任意已登录的浏览器里按 F12 →
> Network → 随便点一个请求 → 复制 `Cookie:` 整行粘进去就行。
> 也可以用命令行：
> ```bash
> curl -X POST http://127.0.0.1:8793/admin/api/auth/manual \
>   -H "Authorization: Bearer $ADMIN_PASSWORD" \
>   -H 'Content-Type: application/json' \
>   -d '{"cookie":"xiaomichatbot_serviceToken=...; userId=...; xiaomichatbot_ph=..."}'
> ```

登录状态会存到 `sessions.json`（权限 600），**重启服务不用重新登录**。


## 两种用法

**单人用** —— 只有你自己调。设个 `client_tokens`，完事。

**分给别人用** —— 打开 `admin.password`，你就有：

- 一个 Web 控制台（`/console`），图形化签发、停用、限额、看用量
- 每人一串独立的 `sk-mimo-...`，只显示一次，服务端只存哈希
- 按密钥计的请求数配额、Token 配额、每分钟限速
- 内置测试台，直接在你浏览器里试跑

```
浏览器  ──►  /console          控制台（登录后可用）
                 │
                 ├─ /admin/api/keys    签发 / 停用 / 限额 / 轮换 / 删除
                 └─ /admin/api/status  全局用量
                          
别人  ──►  /v1/chat/completions  用你发的 sk-mimo-... 调用
```

## 手动安装（不用 install.sh）

> **注意路径**：项目在 `~/Desktop/AI_Lrean/mimo-webapi`，**不在**家目录。
> 而且这台机器的 Go 工具链不在 `PATH` 里（在 `~/go-toolchain/`）。
> 下面的一键脚本把这两件事都处理了。

### 0. 一键脚本

```bash
bash ~/Desktop/AI_Lrean/mimo-webapi/setup.sh
```

它会自动找 Go 工具链、编译、检查配置；没配 cookie 时打印具体怎么配，
配好了就直接起服务并打印客户端 token。可以重复跑。

（想永久把 Go 加进 `PATH`：`echo 'export PATH="$HOME/go-toolchain/go127/bin:$PATH"' >> ~/.zshrc`）

### 1. 拿到 cookie

先在浏览器里登录 <https://aistudio.xiaomimimo.com>，随便发一句话确认能用。
然后二选一：

**A. 自动读取浏览器 cookie（macOS，Chrome/Edge/Brave/Chromium）**

```bash
cd ~/Desktop/AI_Lrean/mimo-webapi
python3 tools/export_cookies.py --auto --browser chrome
```

> 如果你的浏览器 profile 是空的（比如从没用 Chrome 登录过），这一步会
> 明确报 `no Cookies database` —— 那就走 B。Safari 的 cookie 库格式不同，
> 不支持自动读取。

**B. 手动复制（最可靠）**

1. 浏览器打开 <https://aistudio.xiaomimimo.com> 并登录
2. `F12` → **Network** 标签
3. 随便发一句话，找到名为 `chat` 的请求
4. 在 **Request Headers** 里找到 `Cookie:` 那一行，把冒号后面**整行**复制出来
5. 跑：

```bash
python3 tools/export_cookies.py \
  --cookie-string "serviceToken=xxx; userId=123; cUserId=456"
```

两种方式都会生成 `config.json`（权限 600），并在 stdout 打印一个
**客户端 token** —— 那是你之后调用本机 API 用的钥匙，不是 MiMo 的。

### 2. 编译并启动

```bash
cd ~/Desktop/AI_Lrean/mimo-webapi
export PATH="$HOME/go-toolchain/go127/bin:$PATH"   # 如果 go 找不到

go build -o mimowebapi .

./mimowebapi -check -config config.json     # 校验配置
./mimowebapi -config config.json            # 启动
```

### 3. 用起来

```bash
TOKEN=$(python3 -c "import json;print(json.load(open('config.json'))['client_tokens'][0])")

curl -s http://127.0.0.1:8793/v1/chat/completions \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"model":"mimo-v2.6-flash","messages":[{"role":"user","content":"你好"}]}'
```

---

## 客户端接入

### OpenAI 兼容（Cherry Studio / Cline / Continue / 任意 SDK）

```
Base URL: http://127.0.0.1:8793/v1
API Key:  <config.json 里的 client_tokens[0]>
Model:    mimo-v2.6-flash  (或 mimo-v2.6-pro)
```

Python：

```python
from openai import OpenAI
client = OpenAI(base_url="http://127.0.0.1:8793/v1", api_key=TOKEN)
r = client.chat.completions.create(
    model="mimo-v2.6-flash",
    messages=[{"role": "user", "content": "你好"}],
    stream=True,
)
for chunk in r:
    print(chunk.choices[0].delta.content or "", end="", flush=True)
```

### Claude Code / Anthropic 兼容

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:8793
export ANTHROPIC_API_KEY=$TOKEN
```

`/v1/messages` 已实现，且流式事件顺序（`message_start` →
`content_block_start` → `content_block_delta` → `content_block_stop` →
`message_delta` → `message_stop`）与 Anthropic 契约一致。

---

## 接入 Agent / CLI 工具

**不用手填配置。** 打开控制台 → 「接入」标签页 → 点「一键接入」。

登录后控制台会自动检测本机装了哪些工具,每个给一张卡片:

| 工具 | 状态 | 操作 |
|---|---|---|
| Codex CLI | `已检测到` | 点「一键接入」 |
| Claude Code | `已检测到` | 点「一键接入」 |
| DeepSeek Harness | `已检测到` | 点「一键接入」 |
| ZCode | `已检测到` | 点「查看手动步骤」 |
| WorkBuddy | `已检测到` | 点「查看手动步骤」 |

点完徽章变 `已接入 ✓`。**改写前会自动备份**原文件为
`<文件名>.bak-<年月日-时分秒>`,出错可以手动还原。

已经接入过的,按钮变成「重新接入」,重复点不会写坏文件(幂等)。

### 手动接入(如果不想让 relay 改你的配置)

各工具自己在配置里填:

| 工具 | 它会请求 | 该填的 base_url | 配置文件 |
|---|---|---|---|
| sub2api、LobeChat、NextChat | `/v1/chat/completions` | `http://HOST:8793/v1` | 各自的设置界面 |
| Codex CLI | `/v1/responses` | `http://HOST:8793/v1` | `~/.codex/config.toml` |
| Claude Code | `/v1/messages` | `http://HOST:8793` | `~/.claude/settings.json` |
| DeepSeek Harness | 三选一 | `http://HOST:8793/v1` | `$DSH_HOME/profiles/web/cordis.patch.yml` |

填错端点是**最常见的失败**,症状很有迷惑性:工具报一句 `404`,看起来像上游挂了,
实际上只是请求发到了没人监听的路径。

**工具调用（function calling）三条协议都可用。**

### 关于工具调用

上游（网页版对话接口）**只接受一段纯文本 query,不知道工具是什么**。所以工具调用
不是透传的,而是由 relay 重建:

```
出站：tools 定义 → 渲染成提示词（告诉模型有哪些函数、怎么回复）
入站：模型输出   → 解析回标准 tool_calls / function_call item
```

这意味着 **tool_call 契约由 relay 保证,而不是上游**。对客户端透明:
Codex 发标准 tools,收到标准 tool_calls,中间的重建它看不见。

三条协议各自的形状:

| 协议 | 请求里的工具 | 响应里的调用 |
|---|---|---|
| Chat Completions | `tools[]` | `choices[].message.tool_calls` |
| Responses | `tools[]` | `output[]` 里的 `function_call` item |
| Anthropic Messages | `tools[{name,input_schema}]` | `content[]` 里的 `tool_use` 块 + `stop_reason:"tool_use"` |

模型输出两种格式都会认:

| 格式 | 例子 |
|---|---|
| JSON（提示词要求的） | `{"tool":"shell","arguments":{"cmd":["ls"]}}` |
| 模型原生语法 | `<tool_call><function=shell><parameter=cmd>["ls"]</parameter></function></tool_call>` |

**原生语法必须支持** —— 模型在训练里就学会了它,提示词改不掉,它会优先用这个。

工具结果通过 `role:"tool"`（Chat）/ `function_call_output`（Responses）/
`tool_result`（Anthropic）回传,relay 把它折进提示词,模型就能接着作答。

**第一次没按格式输出,relay 会带着纠错提示重发一次。** 只在「试图调工具但格式坏了」
时触发 —— 纯文本回答不会触发,已成功的调用不会重发。

### 关于思考内容

上游会把模型的思考过程用 `<think>...</think>` 包着,混在正文里返回。

- **`/v1/responses`** 没有承载思考的字段,所以思考内容被剥离,`output_text` 只含答案
- **`/v1/chat/completions`** 有 `reasoning_content`,所以思考进那里,`content` 只含答案
- 标签可能被切在两个数据块之间,剥离是按字符状态机做的,不会漏出半个标签

如果发现 `content` 里出现 `<think>`,那是 bug,报告它。

---

## 端点

| 端点 | 鉴权 | 说明 |
|---|---|---|
| `GET /healthz` | 否 | 存活探测 |
| `GET /status` | 是 | 会话健康（冷却 / 失败次数），**不含 cookie 值** |
| `GET /v1/models` | 是 | 本地配置的模型清单，不打上游 |
| `POST /v1/chat/completions` | 是 | OpenAI 协议，支持 `stream` |
| `POST /v1/responses` | 是 | OpenAI Responses 协议，支持 `stream`（`/responses` 同样可用） |
| `POST /v1/messages` | 是 | Anthropic 协议，支持 `stream` |
| `GET /console` | 密码 | Web 控制台（需设 `admin.password`） |
| `GET /admin/api/auth/status` | 是 | 授权向导状态 |
| `POST /admin/api/auth/start` | 是 | 打开登录窗口并开始等待 |
| `POST /admin/api/auth/cancel` | 是 | 取消本次登录 |
| `POST /admin/api/auth/manual` | 是 | 直接粘贴 Cookie 头 |
| `GET /admin/api/auth/sessions` | 是 | 已授权账号及健康状态 |
| `DELETE /admin/api/auth/sessions/{label}` | 是 | 移除一个账号（拒绝清空最后一个） |
| `POST /admin/api/login` | 否 | 用管理密码换会话 cookie |
| `GET /admin/api/keys` | 是 | 列出密钥（永不返回明文） |
| `POST /admin/api/keys` | 是 | 签发密钥，**唯一一次**返回明文 |
| `PATCH /admin/api/keys/{id}` | 是 | 改名 / 停用 / 改额度 |
| `POST /admin/api/keys/{id}/rotate` | 是 | 换密钥 |
| `POST /admin/api/keys/{id}/reset-usage` | 是 | 清零用量 |
| `DELETE /admin/api/keys/{id}` | 是 | 删除 |
| `GET /admin/api/status` | 是 | 全局用量与上游会话健康 |

管理 API 的「是」表示需要管理凭证：浏览器用登录后的签名 cookie，
脚本可以直接用 `Authorization: Bearer <admin.password>`。

非标准扩展字段（其他 OpenAI 服务会忽略，不影响可移植性）：

- `"thinking": true|false` —— 是否开启深度思考
- `"web_search": true|false` —— 是否开启联网搜索
- `"reasoning_effort": "none"` —— 等价于 `thinking:false`

---

## 它是怎么做到的

网页版后端不是给外部用的，协议是逆向出来的。要点：

**1. 端点**

```
POST /open-apis/bot/chat          ← 对话，返回 SSE
GET  /open-apis/bot/config        ← 模型清单（无需登录）
GET  /open-apis/user/mi/get       ← 会话校验，未登录返回 401 + loginUrl
```

**2. 请求体** —— 网页版不是 `messages` 数组，是"一问一答"：

```json
{
  "msgId": "<随机 hex>",
  "conversationId": "<稳定 id，多轮靠它保持上下文>",
  "query": "用户这一句",
  "isEditedQuery": false,
  "sceneType": null,
  "params": {},
  "modelConfig": {
    "enableThinking": true,
    "webSearchStatus": "auto",
    "model": "mimo-v2.6-flash"
  },
  "multiMedias": []
}
```

因为上游只有**一个** `query` 字段，OpenAI 的多轮消息会被折叠成带标注的
上下文再拼上最后一轮（见 `internal/upstream/conversation.go`）：

```text
[Instructions]
<system 内容>

[Conversation so far]
User: ...
Assistant: ...

[Current message]
<最后一轮>
```

**3. 会话 id 的稳定性**

上游按 `conversationId` 在服务端保存上下文。客户端不发这个 id，所以本项目
用**历史消息前缀的 SHA-256** 反推：同一段对话继续追加 → 前缀稳定 → 复用
上游会话；开新对话 → 前缀变了 → 新会话。缓存上界 1024 条。

**4. 鉴权** —— 纯 cookie，没有自定义 header。网页版只额外发：

```
Cookie: serviceToken=...; userId=...
Accept-Language: zh-CN
x-timeZone: Asia/Shanghai
```

**5. SSE 帧** —— 命名事件，不是 OpenAI 的 `data: {choices:[...]}`：

| 上游事件 | 含义 | 映射到 OpenAI |
|---|---|---|
| `message` | 正文增量 | `delta.content` |
| `dialogId` | 会话落库 id | 丢弃 |
| `usage` | token 统计 | `usage` |
| `finish` | 生成结束 | `finish_reason:"stop"` |
| `error` | 上游错误 | 流内报错 |
| `web_search` | 联网结果 | 丢弃 |
| `doc` | 文档解析进度 | 丢弃 |
| `sensitive_query` / `sensitive_title` | 内容审查拦截 | `finish_reason:"content_filter"` |
| `tip_ratio` / `tip_truncate` | 上下文计量 | 丢弃（`tip_truncate` → `max_tokens`） |

丢弃是有意的：硬塞一个自定义字段会让严格的 SDK 解析器直接报错。

---

## 配置项

```jsonc
{
  "listen": "127.0.0.1:8793",     // 非回环默认拒绝启动
  "client_tokens": ["..."],        // 本机调用凭证，必填

  "upstream": {
    "base_url": "https://aistudio.xiaomimimo.com",
    "sessions": [                  // 可配多个账号，轮询 + 故障转移
      { "label": "mimo-1", "cookies": [
          { "name": "serviceToken", "value": "..." } ] }
    ],
    "cooldown_seconds": 300,        // cookie 失效后被停用的时长
    "max_body_bytes": 16777216,
    "max_query_chars": 95000,       // 单次请求文本上限（字符，非字节）；0 = 交给上游判定
    "user_agent": "...",            // 伪装成浏览器
    "locale": "zh-CN",
    "time_zone": "Asia/Shanghai"
  },

  "models": {
    "default": "mimo-v2.6-flash",
    "list": ["mimo-v2.6-pro", "mimo-v2.6-flash"],
    "alias": { "gpt-4o": "mimo-v2.6-pro" }
  },

  "behavior": {
    "enable_thinking_default": true,
    "web_search_default": "auto",   // auto | enabled | disabled
    "system_prompt_mode": "prepend" // prepend | drop
  }
}
```

环境变量覆盖：

```bash
export MIMO_RELAY_CLIENT_TOKENS="token1,token2"
export MIMO_RELAY_COOKIES="mimo-1=serviceToken=xxx; userId=123"
export MIMO_RELAY_LISTEN="127.0.0.1:9000"
export MIMO_RELAY_BASE_URL="https://aistudio.xiaomimimo.com"
```

### 为什么默认只绑回环

这个进程手里握着你的**完整账号会话**。绑到公网等于把账号交出去。
所以非回环监听**默认拒绝启动**，必须显式设 `allow_non_loopback_listen: true`。

需要远程访问就开 SSH 隧道：

```bash
ssh -N -L 8793:127.0.0.1:8793 user@server
```

### 为什么校验会拒绝占位符

`client_tokens` 里留着 `REPLACE_ME`，等于留了一把能用的钥匙。同理，
cookie 写成 `PASTE_HERE` 会被拒绝 —— 那不是配置，那是还没配。

---

---

## 账号管理

「账号」标签页管的是**上游账号**（也就是你的小米账号），跟「密钥」是两回事：

```
小米账号  ──授权──►  本服务  ──签发──►  sk-mimo-...  ──发给──►  用的人
（账号页）                        （密钥页）
```

所以你可以：

- 授权**多个**小米账号，请求会轮询着用，单个被限流时自动切下一个
- 账号被限流或掉登录时，该账号进入冷却并显示原因，其余账号继续服务
- 同一个账号重新授权会**替换**而不是叠加

账号状态一览：

| 字段 | 含义 |
|---|---|
| 可用 | 正常参与轮询 |
| 冷却 Ns | 连续失败后被暂时摘出，倒计时结束自动恢复 |
| 成功 | 累计成功请求数 |
| 连续失败 | 连续错误次数，超过阈值进冷却 |

### 授权是怎么做的

服务端用 Chrome DevTools 协议驱动一个独立浏览器配置：

```
启动 Chrome（--user-data-dir=chrome-profile）
      ↓
你在这个窗口里正常登录（密码 / 扫码 / 短信都行）
      ↓
服务端每秒通过 CDP 读一次 cookie
      ↓
发现 serviceToken + ph 齐了 → 存下来 → 账号可用
```

**为什么读 cookie 而不是用磁盘解密**：macOS 上 Chrome 的 cookie 库用
钥匙串密钥加密，读取会弹系统授权框。走 CDP 完全不用碰这些。

**为什么需要 `ph`**：小米的对话接口除了登录态还校验一个反爬 cookie。
只拿到 `serviceToken` 不够 —— 所以向导会提示你「打开一次对话页发条消息」，
那一步就是为了让浏览器把 `ph` 写进去。

## 多用户：把 API 分给别人

### 两种分法

**A. 各人自己跑一份**（推荐，最省事）

把仓库地址发过去，让他们自己 `./install.sh`。每人用自己的小米账号，
额度互不影响，你的账号零风险。**这是本项目设计的默认用法。**

**B. 你跑一份，别人连你**

你在服务器上跑，给别人发密钥。适合局域网或内网团队。要点：

| 事项 | 怎么做 |
|---|---|
| 监听地址 | 改成 `0.0.0.0:8793` 或具体网卡 IP（默认只绑回环，别人连不上） |
| 给每个人的密钥 | 控制台「生成密钥」，可设请求数/Token 上限和限速 |
| 告诉对方填什么 | 控制台的「接入」标签页会按你当前地址生成配置，直接复制发过去 |
| 防火墙 | 只放行需要的那台机器，别整个网段开 |
| 上限 | **所有人共用你的账号额度**，控制台能看每人用量 |

> **别把 8793 直接暴露到公网。** 要么放内网，要么前面加一层反代 + TLS。
> 这个服务本身不做限流之外的防护。

### 开启

在 `config.json` 里加：

```jsonc
{
  "admin": {
    "password": "换成一个强密码",     // 空 = 关闭控制台，只认 client_tokens
    "key_store_path": "keys.json",   // 密钥持久化位置，权限自动 600
    "console": true,                 // 是否挂载 /console
    "browser_profile_dir": "",       // 向导用的浏览器配置目录，空=自动
    "session_label": "mimo",         // 向导授权后给账号起的名字
    "default_rate_limit_rpm": 60,    // 新密钥默认每分钟请求数
    "default_quota_requests": 0,     // 0 = 不限
    "default_quota_tokens": 0        // 0 = 不限
  }
}
```

重启后打开 <http://127.0.0.1:8793/console>，用这个密码登录。

### 控制台能做什么

| 操作 | 说明 |
|---|---|
| 签发密钥 | 填名称、请求数上限、Token 上限、每分钟限速、有效期、备注 |
| 停用 / 启用 | 立刻生效，被停用的密钥返回 `403 key_disabled` |
| 换密钥 | 生成新串，旧的立刻失效——密钥泄漏时用这个 |
| 清零用量 | 保留配额，只把计数器归零 |
| 删除 | 彻底移除 |
| 测试台 | 选一个密钥，直接在页面里流式对话，用的是真实额度 |

密钥**只在生成时显示一次**，之后服务端只有 SHA-256 哈希。丢了就「换密钥」。

### 鉴权行为

`/v1/*` 接受两种凭证：

| 凭证 | 来源 | 配额 |
|---|---|---|
| `client_tokens` 里的值 | 配置文件，你自己用 | 不限额 |
| `sk-mimo-...` | 控制台签发，给别人用 | 按密钥计 |

失败时的响应：

| 状态 | code | 含义 |
|---|---|---|
| 401 | `invalid_api_key` | 密钥不存在或写错 |
| 403 | `key_disabled` | 被停用或已过期 |
| 429 | `insufficient_quota` | 请求数或 Token 额度用尽 |
| 429 | `rate_limit_exceeded` | 触发每分钟限速 |

### 给别人接入

控制台的「接入」标签页会按你当前的实际地址生成配置，直接复制发过去：

```
Base URL:  http://<你的地址>:8793/v1
API Key:   sk-mimo-...
Model:     mimo-v2.6-flash
```

要让**别的机器**访问，得改两处：

```jsonc
{
  "listen": "0.0.0.0:8793",              // 原本是 127.0.0.1
  "allow_non_loopback_listen": true      // 必须显式确认
}
```

> **想清楚再做。** 这个进程手里握着你的 MiMo 账号会话。绑公网意味着
> 任何能连到这个端口的人都能用你的账号额度（除非他们没密钥——但那把
> 端口暴露在扫描器面前本身就没必要）。**更稳的做法**是保持回环，
> 需要时开隧道：
> ```bash
> ssh -N -L 8793:127.0.0.1:8793 user@你的机器
> ```
> 或者套一层 nginx 做 TLS + 访问控制。

### 额度怎么算

- **请求数**：每次通过鉴权的 `/v1/*` 调用计 1
- **Token**：用上游返回的真实 usage（`total_tokens`）
- 计数器每 5 秒落盘一次，重启不丢
- 一次请求同时受「请求数配额」「Token 配额」「每分钟限速」三重约束，
  任一触发就拒绝


## 已知边界

- **不做工具调用（function calling）**：网页版协议里没有 tools 字段，
  硬造会把参数和正文混在一起。需要 tools 请用官方 API。
- **不做图片/文件上传**：`multiMedias` 需要先走
  `/open-apis/resource/genUploadInfo` 拿 OSS 直传地址，本项目未实现。
- **`max_tokens` 不生效**：上游不接受该参数，只能在上游自己截断时通过
  `tip_truncate` 感知。
- **上下文长度靠折叠而非真多轮**：长对话最终会撞上上游的长度限制。
  上游另有一道**单请求文本上限**（约 10 万字符），与本项目无关；见下节。
- **网页版条款**：这是对你自己的账号做自动化，不是破解。但自动化访问可能
  与你同意的服务条款相抵触 —— 自己判断。
- **cookie 会过期**：失效后 `/status` 会显示 `reason: cookies_expired`，
  重新导出即可。

---

## 单请求文本上限（重要）

网页版有一道**独立于模型上下文窗口**的硬闸：**单次请求的文本约 10 万字符**。

注意这三个数不是一回事：

| 数字 | 含义 |
|---|---|
| 131072 / 262144 | 模型**上下文窗口**（token），页面宣传的那个 |
| ~100000 | 网页版**单请求文本**上限（**字符**），真正卡人的那道闸 |
| 95000 | 本项目 `max_query_chars` 默认值，留了余量 |

实测边界（二分法）：

```
 90000 字符  → ✓ 通过（92,230 token）
110000 字符  → ✗ 被拒
```

**超限时上游不会返回 HTTP 错误**，而是回 `200` + 一条中文提示：

```
抱歉，您发送的文本超长啦！建议您适当简化内容，或者分段发送。
```

这意味着**只看状态码是发现不了的** —— 必须解析流里的 `error` 事件。

### 本项目的处理

1. **本地预检**：请求组装后立刻按字符数（不是字节数）比对
   `max_query_chars`，超了直接返回，不浪费一次上游往返。
2. **流内兜底**：上游在流里回 `error` 帧时，识别文案并归类，
   与预检走同一套错误码。

两种情况都返回同一个结构，**HTTP 400**：

```json
{"error":{"message":"the composed prompt exceeds the MiMo web backend limit of 95000 characters; ...",
          "type":"upstream_error",
          "code":"context_length_exceeded",
          "param":"messages"}}
```

### 为什么是 400 而不是 502

**超长是「请求本身有问题」，不是「上游挂了」。** 客户端对 502 的默认反应
是重试 —— 但重试同样的超长请求，结果永远一样。agent 会卡在无限重试里，
而不是去压缩上下文。

`400` + `context_length_exceeded` 是 agent 框架（DSH 的溢出恢复、
各类 harness）已经认识的信令，收到就会触发历史压缩。

流式请求同理：状态码在第一个 chunk 时就已经发出去了，所以错误以
**带 `code` 的 `error` 事件**形式在 `[DONE]` 之前送达。

### 调整

```jsonc
"upstream": {
  "max_query_chars": 95000   // 0 = 关掉本地预检，完全交给上游判定
}
```

**不要填得比 10 万更接近边界** —— 边界会随上游版本变动，留余量。

---

## 故障排查

| 现象 | 原因 |
|---|---|
| `503 no_session` | 还没配 cookie，或全部在冷却中 |
| `502 cookies_expired`（但账号能查到） | **`ph` 没进查询串** — 见下节 |
| `502 cookies_expired` | cookie 失效，重新跑 `export_cookies.py` |
| `502 account_banned` | 账号被风控（461/451），换账号 |
| `finish_reason: content_filter` | 触发了网页版内容审查，不是本项目拦的 |
| `400 context_length_exceeded` | 单次请求文本超 10 万字符上限，压缩历史或调 `max_query_chars` |
| `502` 且含 `stream read failed` | 上游连接中途断开，回答被截断**不会**当成正常结束返回 |
| 首字很慢 | 网页版本身有排队，非流式会等完整生成 |
| 模型报不存在 | 上游模型表变了，`/open-apis/bot/config` 可以查当前可用型号 |

### 为什么 cookie 全给了还是 401

**这是本项目最容易踩的坑，几乎是唯一一个。**

`xiaomichatbot_ph` 这个值必须**同时**出现在两个地方：

| 位置 | 形式 |
|---|---|
| Cookie 头 | `xiaomichatbot_ph=EXAMPLEphValue0000==` |
| **URL 查询串** | `.../bot/chat?xiaomichatbot_ph=n3%2F1gwocqsvtO95%2F5n%2Bpgw%3D%3D` |

只给 cookie 不给查询串，后端返回 **401 `loginUrl`** —— 和 cookie 完全缺失时
返回的 401 **字节级一模一样**。所以你会看到这种诡异局面：

```
GET  /open-apis/user/mi/get  → 200  账号正常，能看到昵称
POST /open-apis/bot/chat     → 401  像没登录
```

同一个 cookie，一个端点认、一个不认，报的还是"未登录"。
（本项目已在 `requestURL()` 里自动处理：从会话的 `ph` cookie 取值拼进查询串，
两者不可能不一致。）

如果还是 401，用诊断脚本确认卡在哪一层：

```bash
python3 tools/check_cookies.py --config config.json
```

查当前上游真实模型表（无需登录）：

```bash
curl -s https://aistudio.xiaomimimo.com/open-apis/bot/config \
  | python3 -c "import json,sys; [print(m['model']) for m in json.load(sys.stdin)['data']['modelConfigList']]"
```

---

## 测试

```bash
go test ./...
```

覆盖：

- **协议层**：SSE 帧解析（多行 data、非 JSON payload、无终止空行、取消）、
  `ph` 查询参数、usage 两种格式、多轮消息折叠、会话 id 稳定性与缓存上界
- **密钥层**：签发与哈希校验、明文绝不落盘、停用/过期/删除/轮换、
  请求配额、Token 配额、每分钟限速与窗口翻转、并发计数、重启持久化
- **管理面**：鉴权强制、密码登录与会话 cookie、会话不可伪造、
  密钥生命周期、列表不泄漏明文、控制台页面可服务且不含密码
- **HTTP 面**：OpenAI 与 Anthropic 的流式/非流式、鉴权、内容审查映射、
  无会话时 fail-closed

端到端测试用 mock 上游，不消耗真实额度：

```bash
bash deploy/smoke-test.sh      # 19 项断言
```

授权向导单独测：

```bash
go test ./internal/auth/       # cookie 选择、CDP 帧、握手、超时
go test ./internal/httpapi/ -run Auth   # 向导 API 与持久化
```
