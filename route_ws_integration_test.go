package main

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestWebSocketRouteDoesNotLoopClipboardBackToSenderDevice 用真实 WebSocket
// readPump/writePump 验证 route 的线上行为。这里有意绕开 register 的同设备顶替，
// 构造旧连接尚未退出时可能短暂存在的双连接窗口；它不是对正常 register 结果的断言。
func TestWebSocketRouteDoesNotLoopClipboardBackToSenderDevice(t *testing.T) {
	oldHub, oldConfig, oldMsgLog := hub, globalConfig, msgLog
	oldLogWriter := log.Writer()
	h := &Hub{clients: map[int64]map[*Client]bool{42: {}}}
	hub = h
	globalConfig = DefaultConfig()
	msgLog = log.New(io.Discard, "", 0)
	log.SetOutput(io.Discard)

	var handlers sync.WaitGroup
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	registered := make(chan struct{}, 3)
	handlers.Add(3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer handlers.Done()
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		c := &Client{
			conn: conn, userID: 42, deviceID: r.URL.Query().Get("device"),
			role: RolePC, send: make(chan []byte, 32), onlineAt: time.Now(),
		}
		h.mu.Lock()
		h.clients[c.userID][c] = true // 刻意保留同 deviceID 的两条活跃连接
		h.mu.Unlock()
		registered <- struct{}{}
		go c.writePump()
		c.readPump()
	}))

	var connections []*websocket.Conn
	dial := func(deviceID string) *websocket.Conn {
		t.Helper()
		wsURL, err := url.Parse(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		wsURL.Scheme = "ws"
		wsURL.Path = "/ws"
		wsURL.RawQuery = "device=" + url.QueryEscape(deviceID)
		conn, _, err := websocket.DefaultDialer.Dial(wsURL.String(), nil)
		if err != nil {
			t.Fatalf("连接 %s 失败: %v", deviceID, err)
		}
		connections = append(connections, conn)
		select {
		case <-registered:
		case <-time.After(2 * time.Second):
			t.Fatalf("等待 %s 加入 Hub 超时", deviceID)
		}
		return conn
	}

	sender := dial("win-same")
	otherConnectionSameDevice := dial("win-same")
	otherDevice := dial("win-other")
	t.Cleanup(func() {
		for _, conn := range connections {
			_ = conn.Close()
		}
		server.Close()
		handlers.Wait()
		hub, globalConfig, msgLog = oldHub, oldConfig, oldMsgLog
		log.SetOutput(oldLogWriter)
	})

	want := Message{
		ID: "clip-local-integration", Type: TypeClipboard, From: "client-cannot-spoof-this",
		TS: time.Now().UnixMilli(), Payload: json.RawMessage(`{"kind":"text","text":"local clipboard"}`),
	}
	if err := sender.WriteJSON(want); err != nil {
		t.Fatalf("发送剪贴板消息失败: %v", err)
	}

	// 先收到其他设备的 fan-out，证明发送帧已经经过完整 readPump → route → writePump。
	_ = otherDevice.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, raw, err := otherDevice.ReadMessage()
	if err != nil {
		t.Fatalf("其他设备未收到 fan-out: %v", err)
	}
	var got Message
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("其他设备收到非法消息: %v", err)
	}
	if got.ID != want.ID || got.From != "win-same" || got.Type != TypeClipboard {
		t.Errorf("其他设备收到消息 = %+v, 期望 id=%q from=win-same type=clipboard", got, want.ID)
	}

	assertNoWebSocketMessage(t, "同 deviceID 的另一条连接", otherConnectionSameDevice)
	assertNoWebSocketMessage(t, "发送连接", sender)
}

func assertNoWebSocketMessage(t *testing.T, label string, conn *websocket.Conn) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	_, raw, err := conn.ReadMessage()
	if err == nil {
		t.Errorf("%s通过真实 WebSocket 收到了本机回环消息: %s", label, raw)
		return
	}
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("%s读取回环断言时发生非超时错误: %v", label, err)
	}
}
