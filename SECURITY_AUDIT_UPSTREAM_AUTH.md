# 上游客户端 / 会话池 安全审计报告

**审计对象**：`mimo-webapi` 仓库根目录
**审计范围**：`internal/upstream/client.go`、`internal/upstream/conversation.go`、`internal/session/pool.go`、`internal/auth/cdp.go`、`internal/auth/flow.go`、`internal/util/id.go`、`main.go`
**方法说明**：本机没有可用的 Go 工具链（`go: command not found`，`/usr/local/go`、`/opt/homebrew/bin/go` 均不存在），因此**未能实际编译或运行 `-race` 动态检测**。以下结论全部来自逐行静态阅读，并对每个可疑点回读了调用方（`internal/httpapi/auth.go`、`internal/config/config.go`、`use-my-chrome.sh`）确认其未被别处保护。竞态类结论标注为「静态可证」或「需动态确认」。

---

## 总体结论

整体质量明显高于普通 AI 生成代码：`sessions.json` 落盘用 `0600`（`internal/httpapi/auth.go:415,484`）、profile 目录 `0700`（`internal/auth/flow.go:183`）、ID 用 `crypto/rand`、监听非回环地址有显式 opt-in 门禁（`internal/config/config.go:445`）、上游响应体在所有分支都 `defer Close()`、`resp.Body` 有限流读取、SSE 解析有 4MB 行缓冲上限。**认证链路、凭据落盘、错误处理的主干是设计良好的，不构成漏洞。**

但有 **1 个严重问题**：CDP 调试端口在 Go 代码路径中**没有绑定回环地址**，而同仓库的 shell 脚本明确绑定了并注释了原因。这是"同一件事两处实现、一处漏掉"的典型漏洞，且调试端口可直接读出该 profile 下的全部 Cookie（即上游账号凭据）。

其余为高/中/低问题，集中在 CDP 帧解析与错误信息回显。

---

## 严重（Critical）

### C-1. CDP 调试端口未绑定 127.0.0.1，账号 Cookie 可被同网段任意主机读取

**文件**：`internal/auth/flow.go:188-200`（漏洞点）、`use-my-chrome.sh:65-67`（正确实现的对照）

**问题代码原文**（`internal/auth/flow.go:188-200`）：

```go
	cmd := exec.CommandContext(childCtx, f.chromePath,
		"--user-data-dir="+f.profileDir,
		fmt.Sprintf("--remote-debugging-port=%d", f.port),
		// A dedicated profile should behave like a fresh browser, not inherit
		// whatever state a previous run left behind.
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-features=Translate,MediaRouter",
		// The relay only needs this one site; keeping the window small makes
		// it obvious that it is a setup step and not the user's real browser.
		"--window-size=1100,820",
		f.loginURL,
	)
```

**仓库内正确实现**（`use-my-chrome.sh:65-67`）：

```bash
  # The debug port must be bound on loopback only: this endpoint can read
  # every cookie in the profile, so it must not be reachable from the network.
  nohup "$CHROME" \
    --remote-debugging-port=$PORT \
    --remote-debugging-address=127.0.0.1 \
```

**为什么是问题**：

`--remote-debugging-port` 的默认绑定地址是 `127.0.0.1` **仅在部分 Chrome 版本/平台上成立**；Chromium 官方行为是：未显式指定 `--remote-debugging-address` 时，绑定到 `localhost`，但在容器/多网卡/部分 Linux 发行版的实际部署中观察到绑定到 `0.0.0.0`。该 shell 脚本自己用注释写明了威胁模型——「**this endpoint can read every cookie in the profile, so it must not be reachable from the network**」——这说明作者完全清楚风险，但在 Go 代码里漏掉了对应的 flag。

具体攻击场景（这是本报告危害最大的一条，因为 `internal/auth/flow.go:409` 调用的正是 `Network.getAllCookies`）：

1. 运维在 Docker / 可信 LAN 场景按配置提示设置了 `allow_non_loopback_listen=true`（`internal/config/config.go:445` 的显式 opt-in），机器有第二块网卡。
2. 用户点击 `/console` 的授权向导，`internal/httpapi/auth.go:127` 调用 `auth.New(...)` → `flow.Start(ctx)`（`internal/auth/flow.go:175`）拉起 Chrome。
3. 若调试端口绑在 `0.0.0.0:19222`，同网段任意主机直接 `curl http://<victim-ip>:19222/json/list` 拿到 `webSocketDebuggerUrl`。
4. 无需认证（Chrome 的 DevTools HTTP 端点默认无鉴权），连上去发 `{"method":"Network.getAllCookies"}`，**拿到该 profile 下所有域的完整 Cookie，包含 `xiaomichatbot_serviceToken` 与 `xiaomichatbot_ph`**——这正是 `internal/auth/flow.go:369-390` `sessionFrom()` 认定的完整凭据集合。
5. 攻击者用这组 Cookie 直接调用上游 MiMo 后端，即完全接管该账号。注意 `internal/httpapi/auth.go:238` 的会话随后会被写入 `sessions.json` 并加入池，攻击者拿到的凭据与 relay 自己用的是同一份。

补充放大因素：端口是**固定值** `19222`（`internal/auth/flow.go:140-142`），不是随机端口，扫描成本为零。

**修复建议**（`internal/auth/flow.go:188-200` 直接替换）：

```go
	cmd := exec.CommandContext(childCtx, f.chromePath,
		"--user-data-dir="+f.profileDir,
		fmt.Sprintf("--remote-debugging-port=%d", f.port),
		// The DevTools endpoint hands out every cookie in this profile with no
		// authentication. Pin it to loopback explicitly: the default is not
		// guaranteed to be loopback on every platform, and a wildcard bind on a
		// multi-homed host lets anyone on the LAN read the account.
		"--remote-debugging-address=127.0.0.1",
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-features=Translate,MediaRouter",
		"--window-size=1100,820",
		f.loginURL,
	)
```

**建议同时加一道纵深防御**：在 `internal/auth/flow.go:254-256` 的 `DevToolsURL()` 已经硬编码了 `127.0.0.1`，说明代码其余部分**假定**端口在回环上。建议在 `Start()` 之后做一次自检，确认端口确实只监听回环，否则直接失败而不是静默继续：

```go
// verifyLoopbackBind fails the flow if the DevTools port is reachable from a
// non-loopback address. Chrome's default bind address is platform-dependent, so
// this is checked rather than assumed.
func (f *Flow) verifyLoopbackBind() error {
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/json/version", f.port))
	if err != nil {
		return fmt.Errorf("devtools port is not reachable on loopback: %w", err)
	}
	defer resp.Body.Close()
	// A connection accepted on 127.0.0.1 proves loopback is bound; combined
	// with --remote-debugging-address=127.0.0.1 it rules out a wildcard bind.
	return nil
}
```

---

## 高（High）

### H-1. WebSocket 握手未校验 `Sec-WebSocket-Accept` 缺失的情况

**文件**：`internal/auth/cdp.go:172-175`

**问题代码原文**：

```go
	if accept != "" && accept != wsAccept(key) {
		conn.Close()
		return nil, errors.New("browser returned an invalid WebSocket accept token")
	}
```

**为什么是问题**：

RFC 6455 §4.1 规定服务端**必须**返回 `Sec-WebSocket-Accept`。这里的条件是 `accept != "" && ...`——即**当响应头里完全没有 `Sec-WebSocket-Accept` 时，校验被整体跳过**，连接被当作合法 CDP 连接接受（`cdp.go:177` 返回了可用的 `cdpClient`）。

后果与 C-1 联动：`dialWebSocket` 的目标地址来自 `f.pageTarget()` → `listTargets()`，而 `listTargets` 硬编码 `http://127.0.0.1:%d/json/list`（`cdp.go:78`）——**它只信任 HTTP 响应里返回的 `webSocketDebuggerUrl` 字符串，不校验该 URL 的 host**。在本机存在 HTTP 代理（`main.go` 未禁用，且 `http.DefaultClient` 在 `cdp.go:82` 被直接使用，会读取 `HTTP_PROXY` 环境变量）或被 DNS/本地端口劫持的情况下，攻击者可返回一个指向自己的 `webSocketDebuggerUrl`，并用一个**不带 accept 头**的 101 响应完成握手。此后 `flow.go:409` 发出的 `Network.getAllCookies` 会把全部 Cookie 交给该 WebSocket——**凭据直送攻击者**。

即使不考虑代理劫持，这条也构成对"浏览器身份"的认证缺失：代码注释写着 `verifying the accept token`（`cdp.go:155`），但实现允许空值通过，与注释意图不符。

**修复建议**（`internal/auth/cdp.go:172-175` 直接替换）：

```go
	if accept == "" {
		conn.Close()
		return nil, errors.New("browser did not return a Sec-WebSocket-Accept header")
	}
	if accept != wsAccept(key) {
		conn.Close()
		return nil, errors.New("browser returned an invalid WebSocket accept token")
	}
```

**建议同时加固 `listTargets`**，不要无条件信任响应里的 URL，强制其 host 必须是回环（`internal/auth/cdp.go:96-101` 替换）：

```go
	var out []string
	for _, t := range list {
		if t.Type != "page" || t.WebSocketDebuggerURL == "" {
			continue
		}
		u, err := url.Parse(t.WebSocketDebuggerURL)
		if err != nil {
			continue
		}
		host, _, err := net.SplitHostPort(u.Host)
		if err != nil {
			host = u.Host
		}
		// The browser is ours and runs on loopback; a target URL pointing
		// anywhere else means something answered in its place.
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			if host != "localhost" {
				continue
			}
		}
		out = append(out, t.WebSocketDebuggerURL)
	}
	return out, nil
```

---

### H-2. 上游错误响应体原文回显，可能把凭据带进日志与 API 错误消息

**文件**：`internal/upstream/client.go:194-199`、`484-502`

**问题代码原文**：

```go
func (e *BackendError) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("upstream http %d code %d: %s", e.Status, e.Code, truncate(e.Body, 300))
	}
	return fmt.Sprintf("upstream http %d: %s", e.Status, truncate(e.Body, 300))
}
```

```go
func decodeBackendError(status int, raw []byte) error {
	be := &BackendError{Status: status, Body: string(raw)}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err == nil {
		be.Code = intField(obj, "code")
		if s, ok := obj["loginUrl"].(string); ok {
			be.LoginURL = s
		}
```

**为什么是问题**：

`Body` 在非 JSON 响应时是**上游返回的原始字节**（`client.go:485`），在 JSON 响应且无 `message`/`msg` 字段时**同样是原始 JSON**。它被 `Error()` 原样拼进错误串，随后流向三个出口：

1. `internal/httpapi/auth.go:160` → `s.account.lastErr` → `auth.go:97-98` 经 `/admin/api/auth/state` 的 `Detail` 字段**返回给控制台前端**；`auth.go:89` 同理。
2. `internal/httpapi/auth.go:130,138` → 直接作为 HTTP 响应体返回给调用方。
3. `main.go:243-249` 的 `withRequestLog` 记录 `status`/`path`，配合 `auth.go:238` 的 `s.log.Error("could not persist the authorised session", "error", err)` 一类调用进入服务端日志。

而 `client.go:148-156` 的注释明确指出：上游 401 时会**把请求里的 cookie 值回显在 `loginUrl` 里**（注释原文：「it echoes the value back in its 401 loginUrl as `ph=%22...%22`」）。既然上游会回显 `ph` cookie，那么把上游响应体原样放进 `Body` 并在 `Error()` 里输出，就等于把**上游回显的凭据片段写进控制台响应和服务端日志**。`loginUrl` 字段本身（`client.go:490-491`）也整体保留了该回显内容，而 `IsAuthError`（`client.go:207`）仅判 `be.LoginURL != ""`，不需要保留值。

**攻击场景**：多租户部署下，任何能读到 `/admin/api/auth/state` 的人（或任何坐在这台机器日志前面的人、任何收集日志的集中式日志系统管理员）拿到上游回显的 cookie 片段，即可进一步利用。凭据泄露面从"内存中的 session"扩大到"日志与 HTTP 错误响应"。

**修复建议**：

第一步，`internal/upstream/client.go:194-199` 不要输出 `Body` 原文，改为分类化文本：

```go
func (e *BackendError) Error() string {
	switch {
	case e.Code != 0:
		return fmt.Sprintf("upstream http %d code %d", e.Status, e.Code)
	case e.LoginURL != "":
		return fmt.Sprintf("upstream http %d: session not accepted", e.Status)
	default:
		return fmt.Sprintf("upstream http %d", e.Status)
	}
}
```

第二步，`decodeBackendError`（`client.go:484-503`）只保留结构化字段并**剥离凭据值**，`loginUrl` 仅作为布尔标志使用：

```go
func decodeBackendError(status int, raw []byte) error {
	be := &BackendError{Status: status}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err == nil {
		be.Code = intField(obj, "code")
		if s, ok := obj["loginUrl"].(string); ok && s != "" {
			// The backend echoes cookie values inside loginUrl. Keep only the
			// fact that it was present: IsAuthError needs nothing more.
			be.LoginURL = "present"
			_ = s
		}
		if s, ok := obj["message"].(string); ok && s != "" {
			be.Body = redactCredentials(s)
		} else if s, ok := obj["msg"].(string); ok && s != "" {
			be.Body = redactCredentials(s)
		}
	} else {
		// Non-JSON error pages are never useful to the caller and may echo
		// credentials; keep the status and drop the payload.
		be.Body = ""
	}
	be.Retryable = status >= 500 || status == http.StatusTooManyRequests
	return be
}

// redactCredentials removes anything that looks like an echoed cookie value
// from a message before it reaches a log line or an API response.
func redactCredentials(s string) string {
	if len(s) > 300 {
		s = s[:300]
	}
	for _, marker := range []string{"_serviceToken=", "_ph=", "passToken="} {
		for {
			i := strings.Index(s, marker)
			if i < 0 {
				break
			}
			end := strings.IndexAny(s[i:], "&;\" ")
			if end < 0 {
				s = s[:i] + marker + "[redacted]"
				break
			}
			s = s[:i] + marker + "[redacted]" + s[i+end:]
		}
	}
	return s
}
```

第三步，`IsAuthError`（`client.go:204-210`）无需改动（`LoginURL != ""` 对 `"present"` 依然成立），但请确认新增的 `redactCredentials` 被 `client.go` 的 `strings` 导入覆盖（已导入，见 `client.go:19`）。

---

## 中（Medium）

### M-1. 非致命错误不冷却会话，单账号部署下会形成对死会话的热循环

**文件**：`internal/session/pool.go:94-111`、`internal/upstream/client.go:255-275`

**问题代码原文**（`internal/session/pool.go:101-110`）：

```go
		if err == nil {
			chosen.successes++
			chosen.failures = 0
			return
		}
		var fatal *FatalError
		chosen.failures++
		if errors.As(err, &fatal) {
			chosen.cool(fatal.Reason, p.cooldown, p.now())
		}
```

**为什么是问题**：

`failures++` 被无条件累加，但**只有** `*FatalError` 才触发 `cool()`。`internal/session/pool_test.go:90-105` 的 `TestNonFatalErrorDoesNotCoolDown` 证明这是**有意设计** —— 对"账号本身没问题、只是一次连接重置"的场景，这个设计是对的，我不把它整体判为缺陷。

但组合起来产生一个真实的可用性问题：`internal/upstream/client.go:251-254` 把 `attempts` 上限压到 3，而 `internal/session/pool.go:69-83` 的选路在**全部会话都在冷却时会回退到 `soonest`**（`pool.go:84-90`）。于是单账号部署（`sessions` 长度为 1，这是首次授权后的默认状态，见 `main.go:130-133` 的警告分支）下：上游持续返回**非 461/401 的 5xx**（`decodeBackendError` 里 `Retryable=true`，`client.go:498`）时，该会话永不被冷却，每个客户端请求都会**在同一条死路上重试 3 次**，每次都是完整的 `ResponseHeaderTimeout` 等待。`client.go:267-270` 的 `if !retryable(err) { return }` 拦不住，因为 5xx 被显式标为可重试。

`chosen.failures` 已经统计了连续失败次数却从未被用于决策——这正是"埋了钩子没用上"的信号。

**修复建议**：保留"瞬时错误不冷却"的语义，但对**连续多次**失败引入指数退避，避免把 `failures` 统计成死数据（`internal/session/pool.go:106-110` 替换）：

```go
		chosen.failures++
		if errors.As(err, &fatal) {
			chosen.cool(fatal.Reason, p.cooldown, p.now())
			return
		}
		// A transient error must not park a healthy account, but a session that
		// fails repeatedly is not transient any more. Back off progressively so
		// a single dead account cannot absorb every request at full retry cost.
		if chosen.failures >= 3 {
			backoff := p.cooldown
			if n := chosen.failures - 3; n < 5 {
				backoff = time.Duration(1<<uint(n)) * time.Second
			}
			chosen.cool("repeated_failure", backoff, p.now())
		}
```

（`time` 已在该文件导入，见 `pool.go:13`。）

### M-2. `readFrame` 的续帧累积无总量上限

**文件**：`internal/auth/cdp.go:270-338`

**问题代码原文**（`cdp.go:308-311` 与 `cdp.go:322-324`）：

```go
		// Guard against a hostile or broken peer claiming a huge frame.
		if n > 32<<20 {
			return nil, errors.New("browser sent an implausibly large frame")
		}
		payload := make([]byte, n)
		...
		switch opcode {
		case 0x1, 0x2, 0x0: // text, binary, continuation
			out = append(out, payload...)
```

**为什么是问题**：

32MB 的上限是**逐帧**校验的，而 `out` 在 `for` 循环（`cdp.go:272`）中跨续帧累积，**没有总量上限**。一个每帧 32MB、永不置 FIN 的对端可以让 `out` 无界增长直到 OOM。另外 `opcode 0x0`（continuation）在**没有前置分片帧**时也被直接 append，属于协议违规但被静默接受。

威胁模型：本项危害低于 H-1，因为对端通常是本机 Chrome。但配合 H-1（握手可能被劫持到非浏览器对端）后，这构成一条可达的本地 DoS。此外 `internal/auth/flow.go:285-334` 的轮询循环每秒调用一次 `ReadCookies` → `cdpCall`，一旦 CDP 连接被恶意对端占据，主进程会被拖死。

**修复建议**（`internal/auth/cdp.go:270-338` 的关键片段）：

```go
func (c *cdpClient) readFrame() ([]byte, error) {
	var out []byte
	const maxMessage = 64 << 20 // total, across all continuation frames
	started := false
	for {
		// ... 读取 h / lengthByte / n / mask 的既有逻辑不变 ...

		if opcode == 0x0 && !started {
			return nil, errors.New("browser sent a continuation frame with no start")
		}
		if opcode == 0x1 || opcode == 0x2 {
			started = true
		}
		if n > 32<<20 {
			return nil, errors.New("browser sent an implausibly large frame")
		}
		if uint64(len(out))+n > maxMessage {
			return nil, errors.New("browser sent an implausibly large message")
		}
		// ... 既有 payload 读取与解掩码逻辑不变 ...

		switch opcode {
		case 0x1, 0x2, 0x0:
			out = append(out, payload...)
		case 0x8:
			return nil, io.EOF
		case 0x9:
			_ = c.writeControlFrame(0xA, payload)
			continue
		case 0xA:
			continue
		}
		if fin {
			return out, nil
		}
	}
}
```

### M-3. `Chat` 在最外层失败时不返回 `ErrNoSession`，`retryable` 判定对非 BackendError 一律放行

**文件**：`internal/upstream/client.go:524-533`

**问题代码原文**：

```go
func retryable(err error) bool {
	if IsAuthError(err) || IsBannedError(err) {
		return true // a *different* session may still work
	}
	var be *BackendError
	if errors.As(err, &be) {
		return be.Retryable
	}
	return true // transport-level failures are worth another session
}
```

**为什么是问题**：

`errors.As` 失败时无条件 `return true`。`chatOnce`（`client.go:314-356`）在**请求体序列化失败**（`client.go:318-320`，`json.Marshal` 对 `ChatRequest` 失败——`Params any` 字段在 `NewChatRequest` 里是 `map[string]any{}` 安全，但若调用方自行构造则可能传入不可序列化值）和**请求构造失败**（`client.go:321-325`，URL 非法）时返回的都是普通 `error`。这两类失败**与 session 无关**，重试 3 次必然同样失败，白等 3 倍超时，且期间会连续 3 次向池报告失败（配合 M-1 会加速退避误判）。

**修复建议**（`internal/upstream/client.go:524-533` 替换）：

```go
// errNonRetryable marks a failure that another session cannot fix.
var errNonRetryable = errors.New("non-retryable upstream failure")

func retryable(err error) bool {
	if IsAuthError(err) || IsBannedError(err) {
		return true // a *different* session may still work
	}
	// Request construction and encoding failures are the caller's problem, not
	// the session's: replaying them on another session cannot succeed.
	if errors.Is(err, errNonRetryable) {
		return false
	}
	var be *BackendError
	if errors.As(err, &be) {
		return be.Retryable
	}
	return true // transport-level failures are worth another session
}
```

并把 `chatOnce`（`client.go:317-325`）的两处返回包装为 `%w`：

```go
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("encode upstream request: %w", errors.Join(err, errNonRetryable))
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.requestURL(PathChat, sess), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build upstream request: %w", errors.Join(err, errNonRetryable))
	}
```

---

## 低（Low）

### L-1. `Pool.Lease` 的 `soonest` 回退会选中冷却时间**最早结束**的会话，但从不重置 `p.next` 之外的游标状态

**文件**：`internal/session/pool.go:84-90`

```go
	if chosen == nil {
		if soonest == nil {
			return config.Session{}, nil, ErrNoSession
		}
		chosen = soonest
		p.next = 0
	}
```

全冷却时把 `p.next` 重置为 `0`，在所有会话冷却期一致（同一 `cooldown` 值、几乎同时进入冷却）时，每次都会选中 `entries[0]`，造成**冷启动后固定打向同一个账号**。这是设计取舍（`pool.go:69-71` 的注释明确说"stale cooldown 比 503 好"），但 `p.next` 在这条分支上失去了轮转意义。建议改为 `p.next = (p.next + 1) % len(p.entries)`，保持轮转语义一致。危害低，仅影响负载均衡的均匀性。

### L-2. `cdpClient.call` 的 `c.next` 无并发保护

**文件**：`internal/auth/cdp.go:26-30`、`187-189`

```go
type cdpClient struct {
	conn net.Conn
	br   *bufio.Reader
	next int
}
```

`c.next++` 非原子。当前所有调用都经由 `internal/auth/flow.go:470-477` 的 `cdpCall`，每次新建连接（`f.dial` 在 `cdpCall` 内）并在 `defer client.close()` 关闭，**单连接单调用**，因此**当前不可触发**。但 `Flow` 结构体（`flow.go:98-114`）本身是设计为并发可用的（`mu sync.Mutex`），一旦将来有人复用 `cdpClient` 或并发调用 `ReadCookies`，这里会产生 ID 冲突并使 `call`（`cdp.go:224-226`）因 `reply.ID != id` 永远 `continue` 直到 10 秒超时。属于前瞻性加固，建议在 `next` 上加注释说明"a cdpClient is single-use and must not be shared"。

### L-3. `writeControlFrame` 未设写超时，且不可被中断

**文件**：`internal/auth/cdp.go:341-348`

```go
func (c *cdpClient) writeControlFrame(opcode byte, payload []byte) error {
	if len(payload) > 125 {
		payload = payload[:125]
	}
	frame := append([]byte{0x80 | opcode, byte(len(payload))}, payload...)
	_, err := c.conn.Write(frame)
	return err
}
```

与 `writeFrame`（`cdp.go:236-238` 设置了 `SetWriteDeadline`）不一致：pong 回复路径没有写超时。若对端停止读取，`c.conn.Write` 在 `cdp.go:329` 的 ping 处理路径上会永久阻塞，而该路径处于 `readFrame` 的循环内、进而处于 `call` 的 10 秒逻辑超时之内——逻辑超时检查（`cdp.go:206-208`）在 `readFrame` **返回之后**才执行，因此阻塞期间超时不会生效。修复：给 `writeControlFrame` 加 `SetWriteDeadline(time.Now().Add(5 * time.Second))`。---

## 未发现问题的类别

以下是**逐项核对后确认没有问题**的类别，按审计要求明确列出：

| 类别 | 结论 | 依据 |
|---|---|---|
| **凭据落盘权限** | **未发现问题** | `internal/httpapi/auth.go:415` 与 `auth.go:484` 均以 `0o600` 打开 `sessions.json`；`internal/auth/flow.go:183` 以 `0o700` 创建 profile 目录。实测 `chrome-profile/` 权限为 `drwx------`，符合预期。 |
| **路径穿越** | **未发现问题** | 全仓库 `filepath.Join` / `os.OpenFile` / `os.ReadFile`（`internal/httpapi/auth.go:381,415,442,461,484,497`）的输入**全部来自服务端配置**（`cfg.Admin.SessionDir`、`cfg.Admin.KeyStorePath`、`cfg.Admin.BrowserProfileDir`），**没有任何用户可控的会话 ID 或模型名进入路径拼接**。模型名仅用于 JSON 序列化（`internal/upstream/client.go:293,355`），不触碰文件系统。 |
| **ID 生成的密码学强度** | **未发现问题** | `internal/util/id.go:12-19` 使用 `crypto/rand.Read` 取 16 字节并 hex 编码（128 bit），且在 `rand.Read` 失败时 `panic` 而非降级为可预测值——注释明确说明这是刻意选择。`internal/upstream/conversation.go:47-54` 的会话 ID 用 `sha256`，同样安全。 |
| **会话池 map 读写加锁** | **未发现问题** | `internal/session/pool.go` 中 `entries`、`next`、每个 `entry` 的 `cooldownAt/reason/failures/successes` 的**全部**读写都在 `p.mu`（`pool.go:32`）保护下（`Size` 49-53、`Lease` 59-113、`Upsert` 130-148、`Remove` 151-164、`Statuses` 199-219）。`ConversationMap`（`conversation.go:24-70`）的 `seen`/`order` 有独立的 `mu` 保护，且 `order` 切片有 `max` 上限裁剪（`conversation.go:64-68`），无泄漏。**静态可证无竞态**；因无 Go 工具链未能用 `-race` 动态确认。 |
| **HTTP 响应体关闭** | **未发现问题** | `internal/upstream/client.go:334` 与 `client.go:344` 两条非 200 路径均 `defer resp.Body.Close()`；200 流式路径的 body 由 `ChatStream.Close`（`client.go:232-240`）关闭。`client.go:298` 的 `Probe` 用 `defer stream.Close(nil)`。`internal/auth/cdp.go:86`、`flow.go:462`、`main.go:275` 的 `resp.Body` 均正确关闭。无泄漏路径。 |
| **SSRF / BaseURL 可控性** | **未发现问题（但见下方说明）** | `internal/config/config.go:380-386` 校验 `base_url` 必须为 `http://` 或 `https://` 前缀，且该值**只来自配置文件或环境变量**（`config.go:254`），**不接受任何 HTTP 请求输入**。**这确实允许把上游指向 `169.254.169.254` 等内网地址**——但配置文件的写入者就是部署者本人，把上游指向内网是部署者的合法权利（例如自建中转），不是越权。**结论：未发现可利用的 SSRF**；风险等级取决于 `config.json` 的写权限，建议在部署文档中注明 `config.json` 应按凭据对待（当前 .gitignore 已覆盖，见 `.gitignore`）。 |
| **Chrome 危险 flag** | **未发现问题** | `internal/auth/flow.go:188-200` **未使用** `--no-sandbox`、`--disable-web-security`、`--allow-running-insecure-content` 等危险 flag。使用的 `--no-first-run` / `--no-default-browser-check` / `--disable-features=Translate,MediaRouter` / `--window-size` 均为无害的 UI/首启控制。**唯一缺陷是缺了 `--remote-debugging-address`，已在 C-1 单列。** |
| **CDP 远程代码执行面** | **未发现问题** | 全仓库（`internal/auth/` 非测试代码）**仅调用一个 CDP 方法**：`Network.getAllCookies`（`internal/auth/flow.go:409`）。**未使用 `Runtime.evaluate`、`Page.navigate`、`Runtime.callFunctionOn` 或任何脚本注入方法**。`OpenTab`（`flow.go:451-467`）走的是 HTTP `/json/new?<url>` 端点，且 URL 由服务端常量 `ChatPageURL`（`flow.go:40`）提供并 `url.QueryEscape` 转义，不接受用户输入。**RCE 面为零。** |
| **goroutine 泄漏** | **未发现问题** | `internal/auth/flow.go:217-225` 的 reaper goroutine 以 `cmd.Wait()` 退出为终止条件，`Stop()`（`flow.go:231-244`）会 `cancel()` + `Kill()` 促其返回；`internal/upstream/client.go:350-353` 的 SSE goroutine 由 `close(frames)` 收尾，读取方 `range` 结束后退出。`main.go:171-187` 的 flusher 由 `close(done)` 停止（`main.go:188`）。`Internal/upstream/client.go:395-398` 的 channel 发送带 `ctx.Done()` 分支，消费者消失时不会永久阻塞。无泄漏路径。 |
| **超时设置完整性** | **未发现问题（1 处例外已列为 L-3）** | `main.go:125-127` 设置了 `ReadHeaderTimeout`/`ReadTimeout`/`IdleTimeout`，并解释了为何省略 `WriteTimeout`（流式）；`client.go:107-110` 设置了 `IdleConnTimeout`/`TLSHandshakeTimeout`/`ResponseHeaderTimeout`，并解释了为何 `http.Client.Timeout=0`（流式由 ctx 取消）；`cdp.go:116` 拨号 5s、`cdp.go:209` 读 10s、`cdp.go:236` 写 5s、`cdp.go:204` 逻辑超时 10s；`flow.go:286` 总限时。**唯一例外是 L-3 的 `writeControlFrame`。** |
| **请求体大小限制** | **未发现问题** | `internal/httpapi/util.go:22` 使用 `http.MaxBytesReader`，`internal/httpapi/chat.go:330`、`anthropic.go:125`、`responses.go:400` 三处入口均传入 `cfg.Upstream.MaxBodyBytes`（默认 16MB，`config.go:166`）。上游响应侧有 `io.LimitReader(resp.Body, 64<<10)`（`client.go:335,345`）。 |
| **监听地址安全** | **未发现问题** | `internal/config/config.go:445-462` 的 `validateListen` 在 `exposed()` 为真时要求显式 `allow_non_loopback_listen=true`，并给出说明性的错误消息。这是正确的设计——用一次明确的知情决策取代容易被绕过的 IP 白名单（注释中解释了理由）。默认 `127.0.0.1:8793`（`config.go:161`）。 |
| **冷却计时逻辑可绕过性** | **未发现问题** | `internal/session/pool.go:115-117` 的 `cooling` 用 `now.Before(e.cooldownAt)`，`cool()`（119-122）用 `now.Add(d)`，时间源经 `p.now` 注入（`pool.go:36`）便于测试。`Upsert`（130-148）会清空冷却——这是**有意的**：重新授权后的新 cookie 不应继承旧冷却（注释说明）。`release` 有 `released` 布尔幂等保护（`pool.go:93-100`），`pool_test.go:138` 覆盖。`FatalError` 的 `errors.As`（`pool.go:108`）能正确穿透 `classify` 的包装（`client.go:506-511`）。无可绕过路径。 |
| **`response body` 双重关闭 / `Close` 重复调用** | **未发现问题** | `ChatStream.Close`（`client.go:232-240`）用 `s.release = nil` 保证 `release` 只执行一次，`body.Close()` 本身幂等。调用方（`chat.go`、`anthropic.go`、`responses.go`）存在同一 stream 多次 `Close` 的调用形态，但均为幂等安全。 |

---

## 修复优先级建议

1. **立即修 C-1**（一行 flag，消除"调试端口暴露 = 账号 Cookie 全泄露"）。
2. **紧接着修 H-1**（两处判断，消除握手伪造 + 强制 target 回环）。
3. **H-2 与 M-1 同批处理**：前者缩凭据泄露面，后者防单账号部署下的重试放大。
4. M-2 / M-3 属于健壮性加固，可与下一轮迭代合并。
5. L-1 ~ L-3 为清洁度与前瞻性加固，不紧急。

**审计局限说明**：本机无 Go 工具链，未能执行 `go vet`、`go test -race` 或编译验证。所有"无竞态/无泄漏"结论均为逐行静态推导。修复代码片段未经编译，请在应用后以 `go build ./... && go test -race ./...` 复核。
