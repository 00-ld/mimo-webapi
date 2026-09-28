# 服务器部署

本地跑和服务器跑的区别只有一件事:**这个进程手里有一份你真实的 MiMo 登录态。**
谁能连上端口,谁就能用你的账号。下面按"怎么让别人连到它"来组织。

---

## 一、先搞清楚 sub2api 在哪

`base_url` 里的 `127.0.0.1` 指的是**发起请求的那台机器自己**,不是你的电脑。

| sub2api 跑在哪 | base_url 填什么 |
|---|---|
| 和本服务同一台机器 | `http://127.0.0.1:8793/v1` |
| 同一台机器的 Docker 容器里 | `http://mimowebapi:8793/v1`（用 compose 服务名） |
| 另一台机器 | 那台机器能访问到的地址，见下面「二」和「三」 |

**最常见的错误**:sub2api 在服务器上,base_url 填了 `127.0.0.1:8793`,而这个服务跑在你自己的电脑上 —— 服务器去连它自己的 8793 端口,当然连不上,日志里一条请求都不会有。

判断方法:

```bash
# 让 sub2api 点一次「同步模型」，同时在本服务这边看日志
tail -f 服务日志 | grep request
```

- **有请求冒出来** → 网络通,问题在返回格式
- **完全没有动静** → 请求根本没到,先解决可达性

`deploy/diagnose-models.sh` 把这一整套检查做完了:

```bash
./deploy/diagnose-models.sh                    # 自动读 config.json
./deploy/diagnose-models.sh http://HOST:8793 KEY
```

---

## 二、方案 A:Docker Compose(推荐)

服务跑在服务器上,和 sub2api 同机或同网络。

### 1. 准备目录

```bash
mkdir -p /opt/mimowebapi/data && cd /opt/mimowebapi
cp /path/to/config.example.json data/config.json
```

编辑 `data/config.json`,至少改这两处:

```jsonc
{
  "client_tokens": ["sk-mimo-这里换成你自己的随机串"],
  "admin": {
    "password": "换成一个够长的密码",   // 12 位以上，且不能含 admin/password/test 等
    // sessions 和 keys 由环境变量指定到 /data，这里不用填
  }
}
```

`upstream.sessions` 保持为空数组 —— 登录凭据稍后通过向导灌进去,不写在配置里。

### 2. 起服务

```bash
docker compose up -d
docker compose logs -f
```

看到这行就是起来了:

```
listening addr=0.0.0.0:8793 sessions=0 models=3
```

### 3. 灌入 MiMo 登录态

服务器通常没有图形界面,用**手动粘贴**这条路:

1. 在你自己的电脑上,浏览器登录 MiMo 网页版
2. F12 → Network → 随便发一条消息 → 找到 `chat` 请求 → **Copy as cURL**
3. 打开 `http://服务器地址:8793/console`,登录

   > 如果没做端口映射,用 SSH 隧道:
   > ```bash
   > ssh -L 8793:127.0.0.1:8793 user@服务器
   > ```
   > 然后访问 `http://127.0.0.1:8793/console`

4. 账号页 → 「从剪贴板粘贴」→ 把整条 cURL 粘进去 → 提交

控制台会自己从 cURL 里把 cookie 抠出来。

### 4. sub2api 里添加账号

| 字段 | 值 |
|---|---|
| 类型 | OpenAI |
| base_url | `http://mimowebapi:8793/v1`（同 compose 网络）<br>或 `http://127.0.0.1:8793/v1`（同机） |
| api key | `config.json` 里 `client_tokens[0]`，或控制台新建的 `sk-mimo-...` |

**base_url 要带 `/v1`** —— sub2api 会自己往上拼 `/models` 和 `/chat/completions`。

---

## 三、方案 B:直接跑二进制(没有 Docker)

```bash
# 交叉编译（在你自己的电脑上）
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags "-s -w" -o mimowebapi .

scp mimowebapi config.json user@服务器:/opt/mimowebapi/
```

服务器上:

```bash
cd /opt/mimowebapi
./mimowebapi -config config.json        # 前台跑，先确认没问题
```

确认可以之后交给 systemd:

```ini
# /etc/systemd/system/mimowebapi.service
[Unit]
Description=MiMo web relay
After=network-online.target

[Service]
Type=simple
User=mimowebapi
WorkingDirectory=/opt/mimowebapi
ExecStart=/opt/mimowebapi/mimowebapi -config /opt/mimowebapi/config.json
Restart=always
RestartSec=3

# The process needs to write only its own state.
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/opt/mimowebapi

[Install]
WantedBy=multi-user.target
```

```bash
sudo useradd -r -s /usr/sbin/nologin mimowebapi
sudo chown -R mimowebapi:mimowebapi /opt/mimowebapi
sudo systemctl enable --now mimowebapi
```

**用向导登录的话**,服务器上需要 Chromium:

```bash
sudo apt install -y chromium
```

不需要的话跳过 —— 手动粘贴那条路不依赖浏览器。

---

## 四、关于"暴露"这件事

配置里的 `listen` 决定监听范围。服务会在启动时检查:

- **`127.0.0.1:xxxx`** → 只有本机能连。弱密码、空密码都放行。
- **`0.0.0.0:xxxx`** → 网络可达。必须显式设 `allow_non_loopback_listen: true`,
  而且密码要够长、不能是 `admin` / `password` / `test` 这类。

这不是形式主义。这个进程**持有你真实的 MiMo 账号会话**,暴露出去的后果是别人可以直接用你的账号。

**不要用 IP 白名单。** 每次调用方换地址就得改配置,最后所有人都会关掉它。
正确做法是只绑你需要的那个接口:

```yaml
# 只给本机 → 不写 ports，用容器名互访
# 只给本机但要从宿主机访问 → 绑宿主机的回环
ports:
  - "127.0.0.1:8793:8793"

# 真的要给局域网 → 绑具体网卡地址，别写 0.0.0.0
ports:
  - "10.0.0.5:8793:8793"
```

再叠加一层防火墙只放行 sub2api 那台机器的 IP。

---

## 五、更新

```bash
git pull
docker compose build && docker compose up -d
```

`data/` 目录不动,账号和密钥都留着。

---

## 六、排查清单

| 现象 | 多半是 |
|---|---|
| sub2api 同步失败,本服务日志一条请求都没有 | 网络不通 —— base_url 里的 `127.0.0.1` 指向了对方自己 |
| 同步失败,日志里有 401 | key 不对,或用了 sub2api 的 key 而不是本服务的 |
| 同步成功但调用报 503 | 账号池是空的,去 `/console` 灌登录态 |
| 模型列表是空的 | `models.list` 没配 |
| 模型有但选择器里不显示上下文长度 | `models.meta` 里缺 `context_length` |
| 调用返回 401 | cookie 过期了,重新灌一次 |
| 容器起来就退出 | `allow_non_loopback_listen` 没设,或密码太弱被拒 |

启动时的报错都是可操作的,直接照着做就行。

---

## 七、这个服务不做的事

- **不提供多用户计费。** 它是给单人或小团队自用的,要计费和额度分配用 sub2api 那一层。
- **不做协议转换之外的加工。** 请求怎么来就怎么转发,不缓存、不改写内容。
- **不需要公网 IP。** 大多数情况下绑 `127.0.0.1` 就够了。
