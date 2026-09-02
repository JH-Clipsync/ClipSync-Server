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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

	// presence 在线设备列表变更通知：服务端在客户端上下线时主动推给同组所有连接，
	// payload 形如 {"devices":[{"device_id","role","ip","online_at","self"}]}。
	// 客户端据此实时刷新"在线设备"UI。
	TypePresence = "presence"
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

	// onlineAt 连接建立的时间，用于在线设备列表展示
	onlineAt time.Time

	// platform 客户端平台：mac / windows / android / ios / linux / unknown
	platform string
	// name 客户端自定义设备名，由用户在端上设置，存在服务端，原样下发展示。
	name string
	// caps 客户端能力/同步开关状态，由握手 query 上报，原样下发给同组其他端展示。
	// 约定：
	//   clip_up  = 本机剪贴板变化会自动上行
	//   sms_in   = 本机短信会同步上行（通常只有 Android 有）
	//   auto_put = 收到远端剪贴板会自动写入本机剪贴板
	caps map[string]bool

	// replaced 标记该连接已被同 deviceID 的新连接顶替。
	// 旧连接随后被 close 并在 unregister 时摘除；wsHandler 退出时
	// 不再对它执行 MarkOffline，避免误删新连接刚写入的 Redis 在线登记。
	replaced atomic.Bool

	// closeOnce 保证 send channel 全局只关闭一次，避免多条收尾路径
	// （正常断开 / 被顶替 / 管理端踢下线）重复 close 触发 panic。
	closeOnce sync.Once
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
	// 同设备重连兜底：重连风暴时旧连接可能还挂在 hub 里（等读超时才死），
	// 若不收掉它，presence 广播里同一 deviceID 会出现两次，可能导致客户端崩溃。
	// 这里直接摘除并关闭同 deviceID 的旧连接；它们随后走 unregister 收尾。
	var stale []*Client
	for old := range h.clients[c.userID] {
		if old.deviceID == c.deviceID {
			stale = append(stale, old)
		}
	}
	for _, old := range stale {
		delete(h.clients[c.userID], old)
		old.replaced.Store(true)
		logInfo("♻️ 顶替旧连接: %s (%s) user=%d ip=%s — 同设备新连接上线，收掉残留连接",
			shortID(c.deviceID), c.role, c.userID, old.ip)
	}
	if _, ok := h.clients[c.userID]; !ok {
		h.clients[c.userID] = make(map[*Client]bool)
	}
	h.clients[c.userID][c] = true
	logInfo("🟢 上线: %s (%s) user=%d token=%s ip=%s — 该组在线 %d 台",
		shortID(c.deviceID), c.role, c.userID, shortToken(c.token), c.ip, len(h.clients[c.userID]))
	h.mu.Unlock()
	// 关闭放在锁外：close 会让旧连接的 readPump 报错退出，进而回调 unregister
	// （unregister 自带幂等，届时这些连接已不在 set 里，不会重复操作）。
	for _, old := range stale {
		_ = old.conn.Close()
	}
	// 上线后向该组所有连接广播最新在线列表
	h.broadcastPresence(c.userID)
}

func (h *Hub) unregister(c *Client) {
	h.mu.Lock()
	if set, ok := h.clients[c.userID]; ok {
		delete(set, c)
		if len(set) == 0 {
			delete(h.clients, c.userID)
		}
	}
	// 幂等兜底：send channel 全局只关闭一次（sync.Once）。
	// 被顶替的旧连接即使已不在 set 里，也要关掉 send 让 writePump 立即退出，
	// 不必干等到下一次 ping tick；重复收尾时 Do 自动忽略后续调用。
	c.closeOnce.Do(func() { close(c.send) })
	if c.replaced.Load() {
		logInfo("⚪ 旧连接收尾: %s (%s) user=%d（已被同设备新连接顶替）",
			shortID(c.deviceID), c.role, c.userID)
	} else {
		logInfo("⚪ 下线: %s (%s) user=%d token=%s ip=%s",
			shortID(c.deviceID), c.role, c.userID, shortToken(c.token), c.ip)
	}
	h.mu.Unlock()
	// 被顶替的旧连接不触发 presence 广播：它早已被摘出 set，
	// register 时已广播过最终列表，无需再用"旧视角"重复推一遍。
	if !c.replaced.Load() {
		h.broadcastPresence(c.userID)
	}
}

// onlineDevice 在线列表里的单台设备信息，序列化成 presence 消息 payload。
type onlineDevice struct {
	DeviceID string         `json:"device_id"`
	Role     string         `json:"role"`
	Platform string         `json:"platform"`
	Name     string         `json:"name"`
	IP       string         `json:"ip"`
	OnlineAt int64          `json:"online_at"` // 毫秒时间戳
	Self     bool           `json:"self"`      // 是否为接收这条消息的设备本身
	Caps     map[string]bool `json:"caps"`     // 客户端能力/同步开关
}

// onlineDeviceIDs 返回某用户当前在内存 hub 中的在线 deviceID 集合。
// 这是最权威的在线状态——连接在就一定在，连接不在就不在。
func (h *Hub) onlineDeviceIDs(userID int64) map[string]struct{} {
	h.mu.RLock()
	defer h.mu.RUnlock()
	set, ok := h.clients[userID]
	if !ok {
		return nil
	}
	m := make(map[string]struct{}, len(set))
	for c := range set {
		m[c.deviceID] = struct{}{}
	}
	return m
}

// allOnlineDeviceIDs 返回所有在线连接的 "userID:deviceID" 集合，
// 供管理端全量设备列表标记在线状态用。
func (h *Hub) allOnlineDeviceIDs() map[string]struct{} {
	h.mu.RLock()
	defer h.mu.RUnlock()
	m := make(map[string]struct{})
	for userID, set := range h.clients {
		for c := range set {
			m[strconv.FormatInt(userID, 10)+":"+c.deviceID] = struct{}{}
		}
	}
	return m
}

// renameDevice 更新某账号下指定设备所有在线连接的自定义名称，并广播 presence，
// 让该账号所有端实时看到新名称（包括改名设备自身）。
func (h *Hub) renameDevice(userID int64, deviceID, name string) {
	h.mu.Lock()
	for c := range h.clients[userID] {
		if c.deviceID == deviceID {
			c.name = name
		}
	}
	h.mu.Unlock()
	h.broadcastPresence(userID)
}

// broadcastPresence 把某账号当前的在线设备列表推给该组所有连接。
// 在 register/unregister 后调用，让客户端实时刷新"在线设备"UI。
//
// 双重兜底：
//  1. 正常情况下 register 已经把同 deviceID 的残留旧连接收掉，set 里不应再有重复；
//  2. 这里仍按 deviceID 再去一次重，同 ID 只保留最新连接（onlineAt 最晚），
//     保证广播出去的 devices 永远不会出现重复设备——某些客户端遇到重复 ID 会直接崩溃。
//
// 另外，发送动作放在持锁期间进行：旧实现"先解锁再发"，期间连接若被 unregister
// 关掉 send channel，向已关闭 channel 发送会 panic。持锁发送可杜绝该竞态
// （channel 带 32 缓冲且非阻塞发送，锁占用时间极短）。
func (h *Hub) broadcastPresence(userID int64) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	set := h.clients[userID]
	if len(set) == 0 {
		return
	}
	// 按 deviceID 去重，保留 onlineAt 最晚的连接
	byDevice := make(map[string]*Client, len(set))
	dup := 0
	for c := range set {
		if keep, ok := byDevice[c.deviceID]; ok {
			dup++
			if c.onlineAt.After(keep.onlineAt) {
				byDevice[c.deviceID] = c
			}
			continue
		}
		byDevice[c.deviceID] = c
	}
	if dup > 0 {
		logWarn("⚠ presence 去重：user=%d 发现 %d 个重复 deviceID 连接，已只保留最新", userID, dup)
	}
	devices := make([]*Client, 0, len(byDevice))
	for _, c := range byDevice {
		devices = append(devices, c)
	}
	// 序列化放锁外会让 self 标记与发送之间产生竞态；列表不大，直接持锁逐台组装发送
	for _, c := range devices {
		list := make([]onlineDevice, 0, len(devices))
		for _, other := range devices {
			list = append(list, onlineDevice{
				DeviceID: other.deviceID,
				Role:     other.role,
				Platform: other.platform,
				Name:     other.name,
				IP:       other.ip,
				OnlineAt: other.onlineAt.UnixMilli(),
				Self:     other == c,
				Caps:     other.caps,
			})
		}
		payload, _ := json.Marshal(map[string]any{
			"devices": list,
		})
		msg, _ := json.Marshal(Message{
			ID:      "presence",
			Type:    TypePresence,
			From:    "server",
			TS:      time.Now().UnixMilli(),
			Payload: payload,
		})
		select {
		case c.send <- msg:
		default:
			logWarn("⚠ presence 推送丢弃：%s 队列已满", shortID(c.deviceID))
		}
	}
}

// Kick reason：客户端据此显示不同文案，并决定是否需要重连/改密码。
const (
	KickReasonPasswordReset = "password_reset" // 管理端重置了密码
	KickReasonUserDisabled  = "user_disabled"  // 账号被封禁
	KickReasonUserDeleted   = "user_deleted"   // 账号被删除
	KickReasonDeviceKicked  = "device_kicked"  // 管理员主动踢该设备下线
	KickReasonDeviceBanned  = "device_banned"  // 设备被禁用
)

// kickPayload 放进 server_kick 消息的 payload。reason 决定客户端提示语。
type kickPayload struct {
	Reason string `json:"reason"`
}

// sendKick 给单台连接发送 server_kick 消息并立即关闭连接。
func sendKick(c *Client, reason string) {
	body, _ := json.Marshal(kickPayload{Reason: reason})
	kickMsg, _ := json.Marshal(Message{
		ID:      "server_kick",
		Type:    "server_kick",
		From:    "server",
		TS:      time.Now().UnixMilli(),
		Payload: body,
	})
	_ = c.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_ = c.conn.WriteMessage(websocket.TextMessage, kickMsg)
	_ = c.conn.Close()
}

// kickUser 强制断开 userID 下的所有连接。
// 由管理端重置密码 / 封禁用户时触发。
// 调用 Close() 会让 readPump 下一次 ReadMessage 返回错误，
// defer 里自动走 unregister + 清理在线登记 + 关闭 writePump。
func (h *Hub) kickUser(userID int64, reason string) int {
	h.mu.RLock()
	set := h.clients[userID]
	targets := make([]*Client, 0, len(set))
	for c := range set {
		targets = append(targets, c)
	}
	h.mu.RUnlock()

	n := len(targets)
	if n == 0 {
		return 0
	}
	// 先发踢下线通知，客户端收到后主动断开并提示用户
	for _, c := range targets {
		sendKick(c, reason)
	}
	logInfo("👟 强制下线 user=%d reason=%s，断开 %d 台连接", userID, reason, n)
	return n
}

// kickDevice 只断开某用户下指定 deviceID 的连接，不影响该账号其他设备。
// 返回实际断开的连接数（设备可能本就离线，返回 0 也算成功）。
func (h *Hub) kickDevice(userID int64, deviceID, reason string) int {
	h.mu.RLock()
	set := h.clients[userID]
	targets := make([]*Client, 0, 1)
	for c := range set {
		if c.deviceID == deviceID {
			targets = append(targets, c)
		}
	}
	h.mu.RUnlock()

	for _, c := range targets {
		sendKick(c, reason)
	}
	if n := len(targets); n > 0 {
		logInfo("👟 强制下线设备 user=%d device=%s reason=%s", userID, shortID(deviceID), reason)
	}
	return len(targets)
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

// platformFromDeviceID 在客户端未显式上报 platform 时，根据 deviceID 前缀兜底推断平台。
func platformFromDeviceID(deviceID string) string {
	low := strings.ToLower(deviceID)
	switch {
	case strings.HasPrefix(low, "mac-"):
		return "mac"
	case strings.HasPrefix(low, "win-"):
		return "windows"
	case strings.HasPrefix(low, "android-"):
		return "android"
	case strings.HasPrefix(low, "ios-"):
		return "ios"
	case strings.HasPrefix(low, "linux-"):
		return "linux"
	default:
		return "unknown"
	}
}

// parseCaps 把握手 query 里 "clip_up,sms_in,auto_put" 解析成 map，用于 presence 展示。
// 形如 "clip_up=0,sms_in=1" 的形式也支持；默认出现即为开启。
func parseCaps(raw string) map[string]bool {
	m := make(map[string]bool)
	if raw == "" {
		return m
	}
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		// 支持 key=0/1 的写法
		if i := strings.IndexByte(item, '='); i >= 0 {
			key := strings.TrimSpace(item[:i])
			val := strings.TrimSpace(item[i+1:])
			if key != "" {
				m[key] = val != "0" && val != "false"
			}
			continue
		}
		m[item] = true
	}
	return m
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
	writeDeadline := 10 * time.Second
	if globalConfig != nil && globalConfig.WebSocket.WriteDeadlineSec > 0 {
		writeDeadline = time.Duration(globalConfig.WebSocket.WriteDeadlineSec) * time.Second
	}
	refreshDeadline := func() { c.conn.SetReadDeadline(time.Now().Add(readDeadline)) }
	c.conn.SetReadLimit(readLimit) // 剪贴板图片可能大
	refreshDeadline()
	// 任何入站帧都说明客户端存活，都续期读超时：
	// Pong（服务端主动 Ping 的回应）/ Ping（Mac URLSessionWebSocketTask 每 20s 主动发）/ 数据帧（Windows 应用层心跳）。
	// 否则只靠 Pong 续期时，主动发 Ping 但不回 Pong 的客户端（Mac）会在 readDeadline 后被误杀。
	c.conn.SetPongHandler(func(string) error { refreshDeadline(); return nil })
	c.conn.SetPingHandler(func(appData string) error {
		refreshDeadline()
		// 默认行为：回一帧 Pong（控制帧允许在 readPump 上直接写）
		c.conn.SetWriteDeadline(time.Now().Add(writeDeadline))
		return c.conn.WriteMessage(websocket.PongMessage, []byte(appData))
	})
	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			// 网络断开是正常事件，不作为错误级别打印
			return
		}
		refreshDeadline() // 收到数据帧同样视为存活
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

		// 控制消息处理（心跳、伪造的 presence），不转发也不记录
		// Windows 端会发应用层 {"type":"ping"} 心跳；presence 只允许服务端下发
		switch msg.Type {
		case "pong", TypePresence:
			continue
		case "ping":
			// 回一帧 pong 给发送方：手机端用它判断上行/下行链路是否双向通畅，
			// 用来在 Doze 后台检测"半开连接"（连接看似在线但数据已发不出去）。
			// 旧客户端不认识 pong，JSON 解析失败会安全忽略，无副作用。
			if pong, err := json.Marshal(Message{Type: "pong", TS: time.Now().Unix()}); err == nil {
				select {
				case c.send <- pong:
				default:
					msgLog.Printf("  ⚠ pong 丢弃：%s 队列已满", shortID(c.deviceID))
				}
			}
			continue
		}

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
	// 单个前导【xxx】块（带前后空白），用于循环定位第一个前缀
	reLeadingBracket  = regexp.MustCompile(`^\s*【([^】]*)】\s*`)
	// 从【】里提取号码/服务号（覆盖手机号、服务号、106 短信通道号）
	rePhoneInside     = regexp.MustCompile(`【\s*([^】]*?)\s*】`)
	// 判断是否为号码（包含至少 3 位连续数字）
	reDigitLike       = regexp.MustCompile(`\d{3,}`)
	reMergeCount1     = regexp.MustCompile(`\[\s*\d+\s*条\s*\]`)
	reMergeCount2     = regexp.MustCompile(`(?i)\[x\s*\d+\s*\]`)
	reLeadingEllips   = regexp.MustCompile(`^\s*(?:\.{3,}|…+)\s*`)
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

	// 抽发件人：遍历所有【xxx】块，找第一个包含 3 位以上数字的（可能带 +86 前缀）
	sender := ""
	for _, s := range []string{text, preview} {
		if s == "" {
			continue
		}
		matches := rePhoneInside.FindAllStringSubmatch(s, -1)
		for _, m := range matches {
			if len(m) > 1 {
				candidate := strings.TrimSpace(m[1])
				// 去掉可能的 +86 / 86 前缀
				candidate = strings.TrimLeft(candidate, "+")
				candidate = strings.TrimPrefix(candidate, "86")
				candidate = strings.TrimSpace(candidate)
				// 必须包含至少 3 位数字才视为号码
				if reDigitLike.MatchString(candidate) {
					sender = candidate
					break
				}
			}
		}
		if sender != "" {
			break
		}
	}

	// 清洗文本
	clean := func(s string) string {
		if s == "" {
			return s
		}
		// 只剥离前导里【内容】含号码/服务号的前缀块（保留【招商银行】等签名）
		for {
			m := reLeadingBracket.FindStringSubmatch(s)
			if m == nil {
				break
			}
			content := strings.TrimSpace(m[1])
			// 内容里含 3+ 位连续数字 → 视为号码前缀，删掉；
			// 否则是服务商签名 → 停
			if !reDigitLike.MatchString(content) {
				break
			}
			s = reLeadingBracket.ReplaceAllString(s, "")
		}
		s = reMergeCount1.ReplaceAllString(s, "")
		s = reMergeCount2.ReplaceAllString(s, "")
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
	q := r.URL.Query()
	token := q.Get("token")
	device := q.Get("device")
	role := q.Get("role")
	// role 决定路由，只允许 pc / mobile；兼容旧值 phone
	switch role {
	case RolePC:
	case RoleMobile:
	case "phone": // 向后兼容旧客户端
		role = RoleMobile
	default:
		role = RolePC // 默认 pc（保守：能收到 notify_pc）
	}

	// platform：客户端平台。旧客户端不上报则根据 deviceID 前缀兜底推断。
	platform := q.Get("platform")
	if platform == "" {
		platform = platformFromDeviceID(device)
	}
	// caps：客户端同步能力/开关，逗号分隔，如 "clip_up,sms_in,auto_put"
	caps := parseCaps(q.Get("caps"))
	// 设备名：客户端可以在握手时上报本机自定义名称（如"我的 MacBook"）
	name := strings.TrimSpace(q.Get("name"))

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

	// 设备准入：首次见到的设备自动建档；已被管理员禁用的设备直接拒绝升级。
	clientIP := extractIP(r)
	deviceCtx, deviceCancel := context.WithTimeout(r.Context(), 5*time.Second)
	err = authService.EnsureDeviceAllowed(deviceCtx, userID, device, role, platform, name, clientIP)
	deviceCancel()
	if err != nil {
		logWarn("⚠ 拒绝连接: device=%s user=%d 设备已被禁用", shortID(device), userID)
		http.Error(w, "设备已被管理员禁用", http.StatusForbidden)
		return
	}

	// 设备名优先级：以数据库里的自定义名为权威来源，避免客户端重连时用系统主机名
	// 把用户通过重命名接口设置的名字覆盖回去。
	// - 库里有名字：用库里的（用户已自定义）；
	// - 库里没名字（首次建档）：用客户端上报的 name，作为初始值。
	// 注意：EnsureDeviceAllowed 里的 UpsertDevice 只在 INSERT 时写 name，
	// 已存在记录不会被覆盖，所以这里读到的一定是用户最终设置的名字。
	nameCtx, nameCancel := context.WithTimeout(r.Context(), 5*time.Second)
	dbName, _ := authService.GetDeviceName(nameCtx, userID, device)
	nameCancel()
	if strings.TrimSpace(dbName) != "" {
		name = dbName
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
		ip:       clientIP,
		send:     make(chan []byte, sendQueue),
		userID:   userID,
		onlineAt: time.Now(),
		platform: platform,
		name:     name,
		caps:     caps,
	}

	// Redis 在线登记：登录接口靠它判断"当前用户是否已有客户端登录"
	onlineTTL := onlineTTLDuration()
	regCtx, regCancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := authGate.MarkOnline(regCtx, userID, device, role, onlineTTL); err != nil {
		logWarn("⚠ 在线登记失败（不影响转发）: %v", err)
	}
	regCancel()
	defer func() {
		// 被同设备新连接顶替的旧连接不能执行 MarkOffline：
		// Redis 的在线登记按 deviceID 存储，旧连接 HDel 会把新连接刚写入的
		// 在线字段一并删掉，导致"实际在线但登录接口判断离线"。新连接自己
		// 的 keepOnline 会持续续期，断开时再由它负责清理。
		if client.replaced.Load() {
			return
		}
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
// 用 MarkOnline（HSet+Expire）而非 TouchOnline（仅 Expire），
// 确保初次 MarkOnline 失败时心跳能补上 field，不会出现"内存在线但 Redis 缺失"。
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
		if err := authGate.MarkOnline(ctx, c.userID, c.deviceID, c.role, ttl); err != nil {
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
	dumpConfig := flag.Bool("print-default-config", false, "打印内置默认配置（YAML）并退出，可重定向成 config.yaml")
	flag.Parse()

	if *showVersion {
		fmt.Println("clipsync-server", version)
		return
	}

	// 导出一份带注释的完整配置，方便直接改：
	//   clipsync-server --print-default-config > config.yaml
	if *dumpConfig {
		os.Stdout.Write(DefaultConfigYAML())
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

	// 管理端事件订阅：重置密码 / 封禁用户 / 设备管理时触发
	{
		adminCtx, adminCancel := context.WithCancel(context.Background())
		_ = adminCancel // 进程退出时随 HTTP Server 一起被回收
		go func() {
			backoff := 1 * time.Second
			for {
				err := cache.SubscribeAdminCommands(adminCtx, func(cmd AdminCommand) {
					ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel2()
					switch cmd.Action {
					case AdminActionKickUser:
						// 踢整个账号：清会话 + 关闭所有连接
						invalidateUserSessions(ctx2, cmd.UserID)
						reason := cmd.Reason
						if reason == "" {
							reason = KickReasonPasswordReset
						}
						hub.kickUser(cmd.UserID, reason)
					case AdminActionKickDevice:
						if cmd.DeviceID != "" {
							reason := cmd.Reason
							if reason == "" {
								reason = KickReasonDeviceKicked
							}
							hub.kickDevice(cmd.UserID, cmd.DeviceID, reason)
						}
					case AdminActionDisableDevice:
						if cmd.DeviceID != "" {
							if err := authService.SetDeviceStatus(ctx2, cmd.UserID, cmd.DeviceID, true); err != nil {
								logWarn("⚠ 禁用设备失败 user=%d device=%s: %v",
									cmd.UserID, shortID(cmd.DeviceID), err)
								return
							}
							hub.kickDevice(cmd.UserID, cmd.DeviceID, KickReasonDeviceBanned)
						}
					case AdminActionEnableDevice:
						if cmd.DeviceID != "" {
							if err := authService.SetDeviceStatus(ctx2, cmd.UserID, cmd.DeviceID, false); err != nil {
								logWarn("⚠ 解禁设备失败 user=%d device=%s: %v",
									cmd.UserID, shortID(cmd.DeviceID), err)
							}
						}
					}
				})
				// context 被取消：优雅退出
				if adminCtx.Err() != nil {
					return
				}
				logWarn("⚠ 管理端事件订阅断开: %v，%v 后重连", err, backoff)
				time.Sleep(backoff)
				if backoff < 30*time.Second {
					backoff *= 2
				}
			}
		}()
		logError("📡 管理端事件订阅已启动，频道: %s", cache.KickUserChannel())
	}

	// 6. 注册路由
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", wsHandler)
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/push", pushHandler)
	mux.HandleFunc("/auth/login", loginHandler)
	mux.HandleFunc("/auth/register", registerHandler)
	mux.HandleFunc("/auth/session", sessionHandler)
	mux.HandleFunc("/auth/logout", logoutHandler)
	mux.HandleFunc("/auth/change-password", changePasswordHandler)

	// 管理端 HTTP 接口（走 admin_token 鉴权，与 Redis Pub/Sub 形成双保险）
	mux.HandleFunc("POST /server-admin/users", requireAdminToken(adminCreateUser))
	mux.HandleFunc("GET /server-admin/devices", requireAdminToken(adminListAllDevices))
	mux.HandleFunc("GET /server-admin/users/{id}/devices", requireAdminToken(adminListDevices))
	mux.HandleFunc("PUT /server-admin/users/{id}/devices/{deviceID}/status", requireAdminToken(adminSetDeviceStatus))
	mux.HandleFunc("PUT /server-admin/users/{id}/devices/{deviceID}/name", requireAdminToken(adminRenameDevice))
	mux.HandleFunc("POST /server-admin/kick", requireAdminToken(adminKick))

	// 普通用户 HTTP 接口（走登录 token 鉴权）
	mux.HandleFunc("POST /device/name", requireUserToken(handleRenameDevice))

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
