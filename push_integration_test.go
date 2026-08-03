package main

import (
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 端到端验证三项配置在真实请求路径上的效果：
// logs.level 过滤通用日志、max_payload_preview 决定预览截断、
// 消息流水日志不受级别影响。
func TestPushHandlerRespectsLogLevelAndPreviewLimit(t *testing.T) {
	dir := t.TempDir()

	// /push 现在要求有效 token，用替身把 test123 映射成 user 1
	defer withFakeGate(map[string]int64{"test123": 1})()

	// 通用日志 → clipsync.log，级别设为 error
	genWriter, genCloser, err := setupLogWriter(dir, "clipsync", false, 0)
	if err != nil {
		t.Fatalf("初始化通用日志失败: %v", err)
	}
	defer genCloser.Close()

	// 消息流水 → message.log
	ml, mlCloser, err := newCategoryLogger(dir, "message", 0)
	if err != nil {
		t.Fatalf("初始化消息日志失败: %v", err)
	}
	defer mlCloser.Close()

	origLevel, origCfg, origMsgLog, origFlags := currentLevel, globalConfig, msgLog, log.Flags()
	defer func() {
		SetLogLevel(origLevel)
		globalConfig = origCfg
		msgLog = origMsgLog
		log.SetOutput(os.Stderr)
		log.SetFlags(origFlags)
	}()

	globalConfig = DefaultConfig()
	globalConfig.MessageProtocol.MaxPayloadPreview = 8
	msgLog = ml
	log.SetOutput(genWriter)
	SetLogLevel(LevelError)

	body := `{"type":"notify_pc","kind":"sms_code","text":"【+8613800138000】您的验证码是 314159，5分钟内有效，请勿泄露给他人"}`
	req := httptest.NewRequest(http.MethodPost, "/push?token=test123", strings.NewReader(body))
	rec := httptest.NewRecorder()
	pushHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应解析失败: %v", err)
	}
	if resp["ok"] != true {
		t.Errorf("响应 ok = %v, 期望 true", resp["ok"])
	}

	genLog := readFileString(t, filepath.Join(dir, "clipsync.log"))
	if strings.Contains(genLog, "↑ /push") {
		t.Errorf("level=error 时 info 级别的 /push 日志不该写入通用日志:\n%s", genLog)
	}

	// 业务流水日志是审计用途，不参与级别过滤，必须照常落盘
	msgLogContent := readFileString(t, filepath.Join(dir, "message.log"))
	if !strings.Contains(msgLogContent, "收到") {
		t.Errorf("消息流水日志缺失，不该被级别过滤:\n%s", msgLogContent)
	}
	// 手机号前缀应被 sanitize 剥掉
	if strings.Contains(msgLogContent, "13800138000") {
		t.Errorf("短信清洗未剥离手机号前缀:\n%s", msgLogContent)
	}
}

// 预览长度由配置决定：同一段文本在不同 limit 下截断长度不同
func TestPushHandlerPreviewLength(t *testing.T) {
	origCfg := globalConfig
	defer func() { globalConfig = origCfg }()

	text := "您的验证码是 314159，5分钟内有效，请勿泄露给他人"

	globalConfig = DefaultConfig()
	globalConfig.MessageProtocol.MaxPayloadPreview = 6
	short := previewOf(t, text)

	globalConfig = DefaultConfig()
	globalConfig.MessageProtocol.MaxPayloadPreview = 20
	long := previewOf(t, text)

	if len([]rune(short)) != 7 { // 6 字 + 省略号
		t.Errorf("limit=6 时预览 = %q（%d rune），期望 7 rune", short, len([]rune(short)))
	}
	if len([]rune(long)) != 21 { // 20 字 + 省略号
		t.Errorf("limit=20 时预览 = %q（%d rune），期望 21 rune", long, len([]rune(long)))
	}
	if short == long {
		t.Error("不同 max_payload_preview 下预览长度应不同（配置未生效）")
	}
}

// previewOf 走 ensurePreview 拿到自动填充的 preview
func previewOf(t *testing.T, text string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"text": text, "kind": "sms_code"})
	if err != nil {
		t.Fatal(err)
	}
	out := ensurePreview(raw)
	var p map[string]any
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatal(err)
	}
	s, _ := p["preview"].(string)
	return s
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	return string(data)
}
