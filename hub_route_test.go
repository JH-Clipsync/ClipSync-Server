package main

import (
	"io"
	"log"
	"testing"
)

func TestHubRouteExcludesAllConnectionsFromSenderDevice(t *testing.T) {
	oldMsgLog := msgLog
	msgLog = log.New(io.Discard, "", 0)
	t.Cleanup(func() { msgLog = oldMsgLog })

	newClient := func(deviceID string) *Client {
		return &Client{userID: 42, deviceID: deviceID, role: RolePC, send: make(chan []byte, 1)}
	}
	sender := newClient("win-same")
	otherConnectionSameDevice := newClient("win-same")
	otherDevice := newClient("win-other")
	otherUser := newClient("win-other-user")
	otherUser.userID = 43

	h := &Hub{clients: map[int64]map[*Client]bool{
		42: {sender: true, otherConnectionSameDevice: true, otherDevice: true},
		43: {otherUser: true},
	}}
	raw := []byte(`{"id":"clip-local","type":"clipboard","from":"win-same"}`)
	h.route(sender, TypeClipboard, raw)

	assertNoRoutedMessage(t, "发送连接", sender)
	assertNoRoutedMessage(t, "同 deviceID 的另一条连接", otherConnectionSameDevice)
	assertRoutedMessage(t, "同用户其他设备", otherDevice, raw)
	assertNoRoutedMessage(t, "其他用户设备", otherUser)
}

func assertNoRoutedMessage(t *testing.T, label string, c *Client) {
	t.Helper()
	select {
	case raw := <-c.send:
		t.Errorf("%s收到了自己设备发出的消息（本机回环）: %s", label, raw)
	default:
	}
}

func assertRoutedMessage(t *testing.T, label string, c *Client, want []byte) {
	t.Helper()
	select {
	case got := <-c.send:
		if string(got) != string(want) {
			t.Errorf("%s收到的消息 = %s, 期望 %s", label, got, want)
		}
	default:
		t.Errorf("%s没有收到消息，跨设备 fan-out 被破坏", label)
	}
}
