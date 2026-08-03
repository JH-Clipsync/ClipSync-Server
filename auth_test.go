package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHashAndVerifyPassword(t *testing.T) {
	hash, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatalf("哈希失败: %v", err)
	}
	if !strings.HasPrefix(hash, "scrypt$") {
		t.Errorf("哈希串应带算法前缀，实际 = %q", hash)
	}
	if !VerifyPassword(hash, "correct horse battery") {
		t.Error("正确密码校验应通过")
	}
	if VerifyPassword(hash, "wrong password") {
		t.Error("错误密码不该通过")
	}
	// 同一密码两次哈希必须不同（盐随机）
	hash2, _ := HashPassword("correct horse battery")
	if hash == hash2 {
		t.Error("两次哈希结果相同，说明盐没有随机化")
	}
}

func TestVerifyPasswordRejectsMalformedHash(t *testing.T) {
	for _, bad := range []string{
		"", "plain", "scrypt$abc", "bcrypt$1$2$3$c2FsdA$ZGs",
		"scrypt$32768$8$1$notbase64!!$ZGs",
	} {
		if VerifyPassword(bad, "whatever") {
			t.Errorf("非法哈希串 %q 不该通过校验", bad)
		}
	}
}

func TestTokenGeneration(t *testing.T) {
	a, err := NewToken()
	if err != nil {
		t.Fatalf("生成 token 失败: %v", err)
	}
	if len(a) != 64 {
		t.Errorf("token 长度 = %d, 期望 64 (32 字节 hex)", len(a))
	}
	b, _ := NewToken()
	if a == b {
		t.Error("两次生成的 token 相同，随机源有问题")
	}
	if TokenHash(a) == a {
		t.Error("TokenHash 不该返回原文")
	}
	if TokenHash(a) != TokenHash(a) {
		t.Error("TokenHash 应该是确定性的")
	}
}

func TestValidateUsername(t *testing.T) {
	ok := []string{"alice", "bob_99", "a-b-c", "ABC123"}
	for _, n := range ok {
		if err := ValidateUsername(n); err != nil {
			t.Errorf("用户名 %q 应合法，却报 %v", n, err)
		}
	}
	bad := []string{"ab", strings.Repeat("x", 33), "有中文", "with space", "a@b"}
	for _, n := range bad {
		if err := ValidateUsername(n); err == nil {
			t.Errorf("用户名 %q 应被拒绝", n)
		}
	}
}

func TestLoginLimiter(t *testing.T) {
	l := newLoginLimiter(3)
	for i := 0; i < 3; i++ {
		if !l.allow("1.2.3.4") {
			t.Fatalf("第 %d 次尝试不该被限流", i+1)
		}
	}
	if l.allow("1.2.3.4") {
		t.Error("超过上限后应被限流")
	}
	// 换 IP 不受影响
	if !l.allow("5.6.7.8") {
		t.Error("其它 IP 不该被连带限流")
	}
	// limit=0 表示不限流
	if !newLoginLimiter(0).allow("1.2.3.4") {
		t.Error("limit=0 时不该限流")
	}
}

// /push 是明文通道：token 无效必须 401，不能把消息投出去
func TestPushHandlerRejectsInvalidToken(t *testing.T) {
	defer withFakeGate(map[string]int64{"good": 7})()
	origCfg := globalConfig
	defer func() { globalConfig = origCfg }()
	globalConfig = DefaultConfig()

	req := httptest.NewRequest(http.MethodPost, "/push?token=bad",
		strings.NewReader(`{"type":"notify_pc","text":"hi"}`))
	rec := httptest.NewRecorder()
	pushHandler(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("状态码 = %d, 期望 401", rec.Code)
	}
}

// e2ee.require = true 时，明文 /push 必须关闭，否则就成了绕过加密的后门
func TestPushHandlerDisabledWhenE2EERequired(t *testing.T) {
	defer withFakeGate(map[string]int64{"good": 7})()
	origCfg := globalConfig
	defer func() { globalConfig = origCfg }()
	globalConfig = DefaultConfig()
	globalConfig.E2EE.Require = true

	req := httptest.NewRequest(http.MethodPost, "/push?token=good",
		strings.NewReader(`{"type":"notify_pc","text":"hi"}`))
	rec := httptest.NewRecorder()
	pushHandler(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("状态码 = %d, 期望 403", rec.Code)
	}
}

// Bearer 头和 ?token= 两种传法都要认
func TestBearerToken(t *testing.T) {
	r1 := httptest.NewRequest(http.MethodGet, "/auth/session", nil)
	r1.Header.Set("Authorization", "Bearer abc123")
	if got := bearerToken(r1); got != "abc123" {
		t.Errorf("Bearer 头解析 = %q, 期望 abc123", got)
	}

	r2 := httptest.NewRequest(http.MethodGet, "/auth/session?token=xyz", nil)
	if got := bearerToken(r2); got != "xyz" {
		t.Errorf("query 解析 = %q, 期望 xyz", got)
	}

	r3 := httptest.NewRequest(http.MethodGet, "/auth/session", nil)
	if got := bearerToken(r3); got != "" {
		t.Errorf("无 token 时应返回空，实际 %q", got)
	}
}

func TestWriteErrShape(t *testing.T) {
	rec := httptest.NewRecorder()
	writeErr(rec, http.StatusBadRequest, "参数错误")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("状态码 = %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if body["ok"] != false || body["error"] != "参数错误" {
		t.Errorf("响应体 = %v", body)
	}
}
