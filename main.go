package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

// ===== 消息类型常量 =====
const (
	// NotifyPC / NotifyMobile / NotifyAll / Clipboard 见下
	TypeNotifyPC     = "notify_pc"     // 只发给 PC 端（例：短信验证码同步到电脑）
	TypeNotifyMobile = "notify_mobile" // 只发给移动端
	TypeNotifyAll    = "notify_all"    // 广播给所有端（除自己）

	// 剪贴板类：广播给所有端；接收方按开关决定是否自动写入本机剪贴板
	TypeClipboard = "clipboard"
)

// version 在编译时通过 -ldflags 注入：
//
//	go build -ldflags "-X main.version=1.0.0" .
var version = "dev"

// ===== Category：消息业务大类（三端统一） =====
// 用于日志和 UI 分组，跟传输通道（TypeNotifyPC 等）解耦。
const (
	CategorySms          = "sms"          // 短信
	CategoryClipboard    = "clipboard"    // 剪切板
	CategoryNotification = "notification" // 通知（其它）
)

// ===== Content：内容格式（三端统一） =====
const (
	ContentText  = "text"  // 文字
	ContentImage = "image" // 图片
)

// ===== 客户端角色 =====
const (
	RolePC     = "pc"
	RoleMobile = "mobile"
)

// 消息结构，跟三端协议一致
type Message struct {
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	From    string          `json:"from"`
	To      string          `json:"to"`
	TS      int64           `json:"ts"`
	Payload json.RawMessage `json:"payload"`
}

// 客户端连接
type Client struct {
	conn     *websocket.Conn
	token    string
	deviceID string
	role     string // pc | mobile
	ip       string // 客户端 IP（优先取 X-Forwarded-For / X-Real-IP，否则 RemoteAddr）
	send     chan []byte

	// userID 鉴权后的账号 ID。同一账号的所有设备共享一个分组，
	// 取代改造前"按 token 字符串分组"的做法。
	userID int64
}

// globalConfig 保存运行时配置，main 启动时从文件 / 环境变量加载。
var globalConfig *Config

// 集线器：按 token 分组管理连接
type Hub struct {
	mu      sync.RWMutex
	clients map[int64]map[*Client]bool // user_id -> clients
}

var hub = &Hub{
	clients: make(map[int64]map[*Client]bool),
}

// msgLog 专用记录"消息推送流水"（收/发/丢弃），独立成文件避免淹没通用日志。
// 具体见 logger.go 的目录约定：logs/message.log（当日）+ logs/message/message-YYYY-MM-DD.log（归档）。
var msgLog *log.Logger

// upgrader 在 main 里被赋值，CheckOrigin 根据配置决定
var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

func (h *Hub) register(c *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.clients[c.userID]; !ok {
		h.clients[c.userID] = make(map[*Client]bool)
	}
	h.clients[c.userID][c] = true
	logInfo("🟢 上线: %s (%s) user=%d token=%s ip=%s — 该组在线 %d 台",
		shortID(c.deviceID), c.role, c.userID, shortToken(c.token), c.ip, len(h.clients[c.userID]))
}

func (h *Hub) unregister(c *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if set, ok := h.clients[c.userID]; ok {
		delete(set, c)
		if len(set) == 0 {
			delete(h.clients, c.userID)
		}
	}
	close(c.send)
	logInfo("⚪ 下线: %s (%s) user=%d token=%s ip=%s",
		shortID(c.deviceID), c.role, c.userID, shortToken(c.token), c.ip)
}

// shortID 只显示 device_id 前 8 位，日志更清爽
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// shortToken 只显示 token 前 6 位，避免整段泄漏到日志
func shortToken(t string) string {
	if len(t) > 6 {
		return t[:6] + "…"
	}
	return t
}

// extractIP 从 HTTP 请求中提取客户端真实 IP：
// 优先级：X-Forwarded-For（第一个）> X-Real-IP > RemoteAddr。
// 当 TrustProxy=false 时只取 RemoteAddr，避免被伪造头欺骗。
// 用于反向代理（Nginx / Caddy）转发场景。
func extractIP(r *http.Request) string {
	if globalConfig != nil && globalConfig.Server.TrustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			// XFF 可能是"client, proxy1, proxy2"，取最左边的原始客户端 IP
			if idx := strings.IndexByte(xff, ','); idx >= 0 {
				return strings.TrimSpace(xff[:idx])
			}
			return strings.TrimSpace(xff)
		}
		if xri := r.Header.Get("X-Real-IP"); xri != "" {
			return strings.TrimSpace(xri)
		}
	}
	// RemoteAddr 形如 "1.2.3.4:5678"，去掉端口
	addr := r.RemoteAddr
	if i := strings.LastIndexByte(addr, ':'); i > 0 {
		return addr[:i]
	}
	return addr
}

// pushTargetLabel 把内部路由角色转成日志里更易读的推送范围标签
func pushTargetLabel(msgType string) string {
	switch targetRoleForType(msgType) {
	case RolePC:
		return "→PC"
	case RoleMobile:
		return "→Mobile"
	case "*":
		return "→All"
	default:
		return "→?"
	}
}

// categorize 把「消息业务类型」判定为 Category 常量（sms / clipboard / notification）。
// 依据是 payload.kind（sms_code / text / image / share ...）+ 传输 msgType（notify_pc 等）。
//   - kind 是 sms 家族 → sms
//   - kind 是 text/image/share 或 msgType 是 clipboard → clipboard
//   - 其余 → notification
func categorize(msgType, kind string) string {
	switch {
	case strings.HasPrefix(kind, "sms"):
		return CategorySms
	case msgType == TypeClipboard,
		kind == "text", kind == "image", kind == "share":
		return CategoryClipboard
	default:
		return CategoryNotification
	}
}

// contentTypeOf 把「内容格式」判定为 Content 常量（text / image）。
//   - kind == image 或 mime 以 image/ 开头 → image
//   - 其余 → text
func contentTypeOf(kind, mime string) string {
	if kind == ContentImage || strings.HasPrefix(mime, "image/") {
		return ContentImage
	}
	return ContentText
}

// zhCategory / zhContent / zhPush 只用于日志展示：把英文常量翻成中文释义，
// 代码/协议层依然全用英文，避免 label 到处传。
func zhCategory(c string) string {
	switch c {
	case CategorySms:
		return "短信"
	case CategoryClipboard:
		return "剪切板"
	case CategoryNotification:
		return "通知"
	default:
		return c
	}
}

func zhContent(c string) string {
	switch c {
	case ContentText:
		return "文字"
	case ContentImage:
		return "图片"
	default:
		return c
	}
}

func zhPush(t string) string {
	switch t {
	case TypeNotifyPC:
		return "推送至PC"
	case TypeNotifyMobile:
		return "推送至移动"
	case TypeNotifyAll:
		return "广播"
	case TypeClipboard:
		return "剪贴板广播"
	default:
		return t
	}
}

// targetRoleForType 根据消息类型决定应该投递到哪种 role
// 返回值："pc" / "mobile" / "*"(全部) / ""(未知类型也当作全部)
func targetRoleForType(msgType string) string {
	switch msgType {
	case TypeNotifyPC:
		return RolePC
	case TypeNotifyMobile:
		return RoleMobile
	case TypeNotifyAll, TypeClipboard:
		return "*"
	default:
		// 未知类型：为了向后兼容，默认广播
		return "*"
	}
}

// route 根据消息类型决定转发范围。
//
//   - notify_pc：只发给同 token 下的 PC 端
//   - notify_mobile：只发给同 token 下的移动端
//   - notify_all：发给同 token 下所有端
//   - clipboard：发给同 token 下所有端（接收方按开关决定是否自动写入本机剪贴板）
//   - 未知类型：默认按 notify_all 处理（向后兼容）
//
// 该函数只负责路由和转发结果日志（"已转发/无接收方/丢弃"），
// 主消息行由调用方 readPump 在收到时先打印。
func (h *Hub) route(sender *Client, msgType string, raw []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	targets := h.clients[sender.userID]

	wantRole := targetRoleForType(msgType)

	dispatched := 0
	for c := range targets {
		// 铁律 1：永远不发回给自己
		if c == sender {
			continue
		}
		// 铁律 2：role 不匹配的直接跳过
		if wantRole != "*" && c.role != wantRole {
			continue
		}
		select {
		case c.send <- raw:
			dispatched++
		default:
			msgLog.Printf("  ⚠ 丢弃：%s 队列已满", shortID(c.deviceID))
		}
	}
	if dispatched > 0 {
		msgLog.Printf("  → 已转发到 %d 台设备", dispatched)
	} else {
		msgLog.Printf("  ⏸ 无接收方（同组在线 %d 台）", len(targets))
	}
}

func (c *Client) readPump() {
	defer func() {
		hub.unregister(c)
		c.conn.Close()
	}()
	readLimit := int64(10 * 1024 * 1024)
	readDeadline := 60 * time.Second
	if globalConfig != nil {
		if globalConfig.WebSocket.ReadLimit > 0 {
			readLimit = globalConfig.WebSocket.ReadLimit
		}
		if globalConfig.WebSocket.ReadDeadlineSec > 0 {
			readDeadline = time.Duration(globalConfig.WebSocket.ReadDeadlineSec) * time.Second
		}
	}
	c.conn.SetReadLimit(readLimit) // 剪贴板图片可能大
	c.conn.SetReadDeadline(time.Now().Add(readDeadline))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(readDeadline))
		return nil
	})
	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			// 网络断开是正常事件，不作为错误级别打印
			return
		}
		var msg Message
		if err := json.Unmarshal(data, &msg); err != nil {
			logError("✗ 消息解析失败: %v", err)
			continue
		}
		if msg.TS == 0 {
			msg.TS = time.Now().Unix()
		}
		// 强制覆盖 from，防止客户端伪造
		msg.From = c.deviceID

		// 加密策略闸门：识别密文 / 按配置拒绝明文
		encrypted, encErr := checkEncryption(msg.Payload)
		if encErr != nil {
			logWarn("⚠ 拒绝转发 %s(%s) 的消息: %v", shortID(c.deviceID), c.role, encErr)
			msgLog.Printf("  ⛔ 已拒绝：%v (from=%s user=%d)", encErr, shortID(c.deviceID), c.userID)
			continue
		}

		// 明文消息才做短信清洗；密文服务端看不到内容，清洗由发送端在加密前完成
		if !encrypted {
			msg.Payload = sanitizeSmsPayload(msg.Type, msg.Payload)
		}
		out, _ := json.Marshal(msg)

		// 拆出元数据用于日志
		kind, mime, preview := extractPayloadMeta(msg.Payload)
		if encrypted {
			env, _ := parseEnvelope(msg.Payload)
			preview = encPreview(env)
		}
		category := categorize(msg.Type, kind)
		content := contentTypeOf(kind, mime)
		// 消息为主，元数据（分类·格式·推送方式）以中文释义附在括号中
		msgLog.Printf("↑ 收到「%s」 [%s·%s·%s] from=%s(%s) user=%d token=%s ip=%s",
			preview,
			zhCategory(category), zhContent(content), zhPush(msg.Type),
			shortID(c.deviceID), c.role, c.userID, shortToken(c.token), c.ip)
		hub.route(c, msg.Type, out)
	}
}

// ===== 短信 payload 清洗 =====
//
// 目的：让下游 Mac 端不用再处理杂七杂八的手机端注入的前缀。
// 处理规则：
//   - text/preview 开头的 `【+86xxx】` / `【+8613xxx】` 手机号前缀 → 剥离
//   - text/preview 里 `[N条]` / `[xN]` 合并提示 → 剥离
//   - 从原始文本中抽出 11 位手机号 → 塞入 payload.sender（Mac 端拿这个显示到标题）
//   - text/preview 首尾空白 trim
//
// 只处理 payload.kind = sms_code 或 msg.type = notify_pc 的短信类消息，
// 不改动剪贴板/图片等其它 payload。

var (
	rePhonePrefix   = regexp.MustCompile(`^\s*【\s*\+?86\s*\d{6,15}\s*】`)
	rePhoneInside   = regexp.MustCompile(`【\s*\+?(?:86)?\s*(1\d{10})\s*】`)
	reMergeCount1   = regexp.MustCompile(`\[\s*\d+\s*条\s*\]`)
	reMergeCount2   = regexp.MustCompile(`(?i)\[x\s*\d+\s*\]`)
	reLeadingEllips = regexp.MustCompile(`^\s*(?:\.{3,}|…+)\s*`)
)

func sanitizeSmsPayload(msgType string, raw json.RawMessage) json.RawMessage {
	// 解析成通用 map，方便加字段
	var p map[string]any
	if err := json.Unmarshal(raw, &p); err != nil {
		return raw
	}

	kind, _ := p["kind"].(string)
	// 只清洗短信类：payload.kind 以 sms 开头，或 msg.type == notify_pc（默认视为短信）
	isSms := strings.HasPrefix(kind, "sms") || msgType == TypeNotifyPC
	if !isSms {
		return raw
	}

	text, _ := p["text"].(string)
	preview, _ := p["preview"].(string)

	// 抽发件人手机号（优先从 text，再退到 preview）
	sender := ""
	for _, s := range []string{text, preview} {
		if s == "" {
			continue
		}
		if m := rePhoneInside.FindStringSubmatch(s); len(m) > 1 {
			sender = m[1]
			break
		}
	}

	// 清洗文本
	clean := func(s string) string {
		if s == "" {
			return s
		}
		s = rePhonePrefix.ReplaceAllString(s, "")
		s = reMergeCount1.ReplaceAllString(s, "")
		s = reMergeCount2.ReplaceAllString(s, "")
		// 去掉开头残留的 "..." / "…"（部分厂商通知栏折叠预览会在开头加上省略号）
		s = reLeadingEllips.ReplaceAllString(s, "")
		return strings.TrimSpace(s)
	}
	if text != "" {
		p["text"] = clean(text)
	}
	if preview != "" {
		p["preview"] = clean(preview)
	}
	if sender != "" {
		p["sender"] = sender
	}

	out, err := json.Marshal(p)
	if err != nil {
		return raw
	}
	return out
}

// previewLimit 返回预览截断长度，取自 message_protocol.max_payload_preview。
// 未配置或非正数时回落到 40，与改造前的硬编码行为一致。
func previewLimit() int {
	if globalConfig != nil && globalConfig.MessageProtocol.MaxPayloadPreview > 0 {
		return globalConfig.MessageProtocol.MaxPayloadPreview
	}
	return 40
}

// truncatePreview 按 previewLimit 截断，超长时补省略号。
// 按 rune 计数，避免把中文截成半个字。
func truncatePreview(s string) string {
	limit := previewLimit()
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit]) + "…"
}

// ensurePreview 如果 payload.preview 为空，用 text 的前 N 字自动填充
// （N = message_protocol.max_payload_preview）
func ensurePreview(raw json.RawMessage) json.RawMessage {
	var p map[string]any
	if err := json.Unmarshal(raw, &p); err != nil {
		return raw
	}
	preview, _ := p["preview"].(string)
	text, _ := p["text"].(string)
	if preview != "" || text == "" {
		return raw
	}
	p["preview"] = truncatePreview(text)
	out, err := json.Marshal(p)
	if err != nil {
		return raw
	}
	return out
}

// extractPayloadMeta 一次性提取 payload.kind、payload.mime 和一行简短预览。
// kind 缺失时返回 "-"，方便日志对齐。
func extractPayloadMeta(payload json.RawMessage) (kind, mime, preview string) {
	var p struct {
		Preview string `json:"preview"`
		Text    string `json:"text"`
		Kind    string `json:"kind"`
		Mime    string `json:"mime"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return "-", "", "(无法解析)"
	}
	kind = p.Kind
	if kind == "" {
		kind = "-"
	}
	mime = p.Mime
	switch {
	case p.Preview != "":
		preview = p.Preview
	case p.Text != "":
		preview = truncatePreview(p.Text)
	case p.Kind != "":
		preview = "[" + p.Kind + "]"
	default:
		preview = "(空)"
	}
	return kind, mime, preview
}

func (c *Client) writePump() {
	pingInterval := 30 * time.Second
	writeDeadline := 10 * time.Second
	if globalConfig != nil {
		if globalConfig.WebSocket.PingIntervalSec > 0 {
			pingInterval = time.Duration(globalConfig.WebSocket.PingIntervalSec) * time.Second
		}
		if globalConfig.WebSocket.WriteDeadlineSec > 0 {
			writeDeadline = time.Duration(globalConfig.WebSocket.WriteDeadlineSec) * time.Second
		}
	}
	ticker := time.NewTicker(pingInterval)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()
	for {
		select {
		case msg, ok := <-c.send:
			if !ok {
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			c.conn.SetWriteDeadline(time.Now().Add(writeDeadline))
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(writeDeadline))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func wsHandler(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	device := r.URL.Query().Get("device")
	role := r.URL.Query().Get("role")
	// role 决定路由，只允许 pc / mobile；兼容旧值 phone
	switch role {
	case RolePC:
	case RoleMobile:
	case "phone": // 向后兼容旧客户端
		role = RoleMobile
	default:
		role = RolePC // 默认 pc（保守：能收到 notify_pc）
	}

	if token == "" || device == "" {
		http.Error(w, "参数错误: 需要 token 和 device", http.StatusBadRequest)
		return
	}

	// 真鉴权：token 必须是登录接口签发且未过期的（Redis 命中优先，未命中回源 MySQL）
	authCtx, authCancel := context.WithTimeout(r.Context(), 5*time.Second)
	userID, _, err := authGate.AuthenticateToken(authCtx, token)
	authCancel()
	if err != nil {
		logWarn("⚠ 拒绝连接: device=%s ip=%s token 无效或已过期", shortID(device), extractIP(r))
		http.Error(w, "认证失败: token 无效或已过期，请重新登录", http.StatusUnauthorized)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		logError("✗ WebSocket 升级失败: %v", err)
		return
	}
	sendQueue := 32
	if globalConfig != nil && globalConfig.WebSocket.SendQueueSize > 0 {
		sendQueue = globalConfig.WebSocket.SendQueueSize
	}
	client := &Client{
		conn:     conn,
		token:    token,
		deviceID: device,
		role:     role,
		ip:       extractIP(r),
		send:     make(chan []byte, sendQueue),
		userID:   userID,
	}

	// Redis 在线登记：登录接口靠它判断"当前用户是否已有客户端登录"
	onlineTTL := onlineTTLDuration()
	regCtx, regCancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := authGate.MarkOnline(regCtx, userID, device, role, onlineTTL); err != nil {
		logWarn("⚠ 在线登记失败（不影响转发）: %v", err)
	}
	regCancel()
	defer func() {
		offCtx, offCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := authGate.MarkOffline(offCtx, userID, device); err != nil {
			logWarn("⚠ 在线登记清理失败: %v", err)
		}
		offCancel()
	}()

	hub.register(client)
	go client.writePump()
	go client.keepOnline(onlineTTL)
	client.readPump()
}

// onlineTTLDuration 在线登记的 TTL，来自 redis.online_ttl_sec（默认 90s）。
func onlineTTLDuration() time.Duration {
	sec := 90
	if globalConfig != nil && globalConfig.Redis.OnlineTTLSec > 0 {
		sec = globalConfig.Redis.OnlineTTLSec
	}
	return time.Duration(sec) * time.Second
}

// keepOnline 定期给 Redis 在线登记续期，周期取 TTL 的三分之一。
// 连接断开时 c.send 被关闭，这里随之退出。
func (c *Client) keepOnline(ttl time.Duration) {
	interval := ttl / 3
	if interval < time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		hub.mu.RLock()
		_, alive := hub.clients[c.userID][c]
		hub.mu.RUnlock()
		if !alive {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		if err := authGate.TouchOnline(ctx, c.userID, ttl); err != nil {
			logDebug("在线登记续期失败: %v", err)
		}
		cancel()
	}
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte("ok"))
}

// pushHandler 便捷推送接口 —— 用 curl 就能测 Mac 端 Toast，无需 wscat
//
// 用法示例：
//
//	# 短信验证码（会自动提取验证码，Toast 显示"复制 314159"按钮）
//	curl -X POST 'http://localhost:8080/push?token=test123' \
//	  -H 'Content-Type: application/json' \
//	  -d '{"type":"notify_pc","kind":"sms_code","text":"【测试】您的验证码是 314159，5分钟内有效"}'
//
//	# 剪贴板文本（Mac 会自动写入本机剪贴板）
//	curl -X POST 'http://localhost:8080/push?token=test123' \
//	  -H 'Content-Type: application/json' \
//	  -d '{"type":"clipboard","kind":"text","text":"你好世界"}'
//
// 参数：
//   - token: 必须；登录接口签发的 token，决定推送给该账号下的哪些客户端
//     （也可放在 Authorization: Bearer <token> 头里）
//   - body:  JSON，字段 type/kind/text/preview/mime/data 都可选
//     type 缺省 = notify_pc
//     kind 缺省 = 根据 type 自动推导
func pushHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "只支持 POST", http.StatusMethodNotAllowed)
		return
	}
	token := bearerToken(r)
	if token == "" {
		http.Error(w, "缺少 token 参数", http.StatusBadRequest)
		return
	}

	// 鉴权：/push 是明文入口（给 curl 调试用），同样要求有效 token
	authCtx, authCancel := context.WithTimeout(r.Context(), 5*time.Second)
	userID, _, err := authGate.AuthenticateToken(authCtx, token)
	authCancel()
	if err != nil {
		http.Error(w, "认证失败: token 无效或已过期", http.StatusUnauthorized)
		return
	}

	// e2ee.require 开启时，明文推送接口一律关闭——否则就成了绕过加密的后门
	if globalConfig != nil && globalConfig.E2EE.Require {
		http.Error(w, "服务端要求端到端加密，/push 明文接口已禁用", http.StatusForbidden)
		return
	}

	var body struct {
		Type    string `json:"type"`
		Kind    string `json:"kind"`
		Text    string `json:"text"`
		Preview string `json:"preview"`
		Mime    string `json:"mime"`
		Data    string `json:"data"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "JSON 解析失败: "+err.Error(), http.StatusBadRequest)
		return
	}
	if body.Type == "" {
		body.Type = TypeNotifyPC
	}
	if body.Kind == "" && body.Type == TypeNotifyPC {
		body.Kind = "sms_code"
	}
	if body.Mime == "" {
		body.Mime = "text/plain"
	}
	// ⚠️ 不在这里生成 preview，因为 body.Text 可能带 【+86xxx】前缀；
	// 等 sanitize 清洗完 text 之后再让服务端根据清洗后的 text 自动补 preview

	msgID := "push-" + time.Now().Format("20060102150405.000")

	payload, _ := json.Marshal(map[string]any{
		"text":    body.Text,
		"mime":    body.Mime,
		"data":    body.Data,
		"preview": body.Preview,
		"kind":    body.Kind,
	})
	// 短信清洗：剥离 【+86xxx】 / [N条]，抽发件人
	payload = sanitizeSmsPayload(body.Type, payload)

	// 清洗后如果 preview 还是空，用清洗后的 text 生成
	payload = ensurePreview(payload)
	raw, _ := json.Marshal(Message{
		ID:      msgID,
		Type:    body.Type,
		From:    "http-push",
		To:      "*",
		TS:      time.Now().Unix() * 1000,
		Payload: payload,
	})

	hub.mu.RLock()
	targets := hub.clients[userID]
	dispatched := 0
	for c := range targets {
		wantRole := targetRoleForType(body.Type)
		if wantRole != "*" && c.role != wantRole {
			continue
		}
		select {
		case c.send <- raw:
			dispatched++
		default:
		}
	}
	total := len(targets)
	hub.mu.RUnlock()

	logInfo("↑ /push %s (kind=%s) → 转发到 %d/%d 台", body.Type, body.Kind, dispatched, total)
	// 消息推送流水另外写到消息日志，格式跟 WebSocket 通道一致（内容为主，元数据为辅）
	msgLog.Printf("↑ 收到「%s」 [%s·%s·%s] from=http-push user=%d token=%s ip=%s → 转发到 %d/%d 台",
		body.Preview,
		zhCategory(categorize(body.Type, body.Kind)),
		zhContent(contentTypeOf(body.Kind, body.Mime)),
		zhPush(body.Type),
		userID, shortToken(token), extractIP(r), dispatched, total)

	w.Header().Set("Content-Type", "application/json")
	resp, _ := json.Marshal(map[string]any{
		"ok":         true,
		"dispatched": dispatched,
		"total":      total,
		"msg_id":     msgID,
	})
	w.Write(resp)
}

func main() {
	// 命令行参数：--config 指向配置文件
	configPath := flag.String("config", "", "配置文件路径（YAML）；也可用环境变量 CLIPSYNC_CONFIG")
	showVersion := flag.Bool("version", false, "打印版本信息并退出")
	flag.Parse()

	if *showVersion {
		fmt.Println("clipsync-server", version)
		return
	}

	// 1. 加载配置
	cfg, err := LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ %v\n", err)
		os.Exit(1)
	}
	globalConfig = cfg

	// 2. 根据配置决定 upgrader 的 CheckOrigin 行为
	upgrader.CheckOrigin = func(r *http.Request) bool {
		if cfg.MessageProtocol.CheckOrigin {
			return true // 允许任意 Origin
		}
		// 简单实现：只放行空 Origin（CLI 客户端）和 localhost
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true
		}
		return strings.HasPrefix(origin, "http://localhost") ||
			strings.HasPrefix(origin, "https://localhost") ||
			strings.HasPrefix(origin, "app://")
	}

	// 3. 初始化通用日志：logs/clipsync.log（当日）+ logs/clipsync/clipsync-YYYY-MM-DD.log（归档）
	logDir := cfg.Logs.Dir
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		log.Fatalf("创建日志目录失败: %v", err)
	}
	logWriter, closer, err := setupLogWriter(logDir, "clipsync", cfg.Logs.Stdout, cfg.Logs.MaxAgeDays)
	if err != nil {
		log.Fatalf("初始化日志失败: %v", err)
	}
	defer closer.Close()
	// setupLogWriter 内部已经按 Logs.Stdout 决定是否混入 stdout：
	//   stdout=true  → 写到 stdout + 文件
	//   stdout=false → 只写文件
	log.SetOutput(logWriter)
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	// 应用日志级别：低于该级别的 logDebug/logInfo/... 调用会被丢弃
	SetLogLevel(parseLevel(cfg.Logs.Level))

	// 4. 初始化消息推送专用日志
	ml, mlCloser, err := newCategoryLogger(logDir, "message", cfg.Logs.MaxAgeDays)
	if err != nil {
		log.Fatalf("初始化消息日志失败: %v", err)
	}
	defer mlCloser.Close()
	msgLog = ml

	// 5. 初始化存储（MySQL 持久化 + Redis 缓存/在线态）与认证服务
	store, err := OpenMySQL(cfg.MySQL)
	if err != nil {
		log.Fatalf("❌ %v", err)
	}
	defer store.Close()
	logError("🗄  已连接 MySQL %s:%d/%s", cfg.MySQL.Host, cfg.MySQL.Port, cfg.MySQL.Database)

	cache, err := OpenRedis(cfg.Redis)
	if err != nil {
		log.Fatalf("❌ %v", err)
	}
	defer cache.Close()
	logError("⚡ 已连接 Redis %s (db=%d)", cfg.Redis.Addr, cfg.Redis.DB)

	authService = NewAuthService(store, cache, cfg.Auth)
	authGate = authService
	limiter = newLoginLimiter(cfg.Auth.LoginRateLimitPerMin)

	// 初始账号：配置了 bootstrap_user 且该用户不存在时自动建，方便首次部署
	bootstrapUser(store, cfg.Auth)

	// 过期会话清理：启动时跑一次，之后每小时一次
	go purgeSessionsLoop(store)

	// 6. 注册路由
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", wsHandler)
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/push", pushHandler)
	mux.HandleFunc("/auth/login", loginHandler)
	mux.HandleFunc("/auth/register", registerHandler)
	mux.HandleFunc("/auth/session", sessionHandler)
	mux.HandleFunc("/auth/logout", logoutHandler)

	srv := &http.Server{
		Addr:         cfg.Server.Addr,
		Handler:      mux,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
	}

	// 7. 启动 + 优雅退出
	// 启动横幅用 Error 级别，保证任何 level 配置下都能看到服务确实起来了
	logError("🚀 ClipSync 服务端启动，监听 %s", cfg.Server.Addr)
	logError("📋 配置摘要: addr=%s logs.dir=%s logs.level=%s logs.max_age_days=%d trust_proxy=%v ws.read_limit=%d preview_limit=%d",
		cfg.Server.Addr, cfg.Logs.Dir, cfg.Logs.Level, cfg.Logs.MaxAgeDays,
		cfg.Server.TrustProxy, cfg.WebSocket.ReadLimit, cfg.MessageProtocol.MaxPayloadPreview)
	logError("🔐 认证配置: token_ttl=%dh allow_register=%v e2ee_require=%v login_rate_limit=%d/min",
		cfg.Auth.TokenTTLHours, cfg.Auth.AllowRegister, cfg.E2EE.Require, cfg.Auth.LoginRateLimitPerMin)

	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	// 监听系统信号实现优雅退出
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		log.Fatalf("❌ HTTP 服务异常退出: %v", err)
	case sig := <-quit:
		logError("👋 收到信号 %v，开始优雅退出（等待 %s）...", sig, cfg.Server.ShutdownTimeout)
		ctx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			logWarn("⚠ 优雅退出失败: %v", err)
		}
		logError("✅ 已退出")
	}
}
