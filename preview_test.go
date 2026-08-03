package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// withPreviewLimit 临时设置 max_payload_preview，测完还原全局配置
func withPreviewLimit(t *testing.T, limit int) {
	t.Helper()
	orig := globalConfig
	globalConfig = DefaultConfig()
	globalConfig.MessageProtocol.MaxPayloadPreview = limit
	t.Cleanup(func() { globalConfig = orig })
}

func TestPreviewLimitFallsBackTo40(t *testing.T) {
	orig := globalConfig
	globalConfig = nil
	defer func() { globalConfig = orig }()

	if got := previewLimit(); got != 40 {
		t.Errorf("未配置时 previewLimit() = %d, 期望回落 40", got)
	}

	withPreviewLimit(t, 0)
	if got := previewLimit(); got != 40 {
		t.Errorf("配置为 0 时 previewLimit() = %d, 期望回落 40", got)
	}
}

// 配置改小后，截断长度必须跟着变（回归防护：曾经是硬编码 40）
func TestTruncatePreviewHonorsConfig(t *testing.T) {
	withPreviewLimit(t, 5)

	if got := truncatePreview("12345"); got != "12345" {
		t.Errorf("刚好等于上限不该截断，得到 %q", got)
	}
	if got := truncatePreview("123456"); got != "12345…" {
		t.Errorf("超长应截断为 5 字 + 省略号，得到 %q", got)
	}
	// 中文按 rune 计数，不能截出乱码
	if got := truncatePreview("一二三四五六七"); got != "一二三四五…" {
		t.Errorf("中文应按 rune 截断，得到 %q", got)
	}
}

// ensurePreview 用配置长度补 preview，而不是固定 40
func TestEnsurePreviewUsesConfiguredLimit(t *testing.T) {
	withPreviewLimit(t, 4)

	in := json.RawMessage(`{"text":"abcdefghij"}`)
	out := ensurePreview(in)

	var p map[string]any
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatalf("解析结果失败: %v", err)
	}
	if p["preview"] != "abcd…" {
		t.Errorf("preview = %v, 期望 \"abcd…\"", p["preview"])
	}
}

// extractPayloadMeta 的预览同样走配置
func TestExtractPayloadMetaUsesConfiguredLimit(t *testing.T) {
	withPreviewLimit(t, 3)

	_, _, preview := extractPayloadMeta(json.RawMessage(`{"text":"abcdefg","kind":"text"}`))
	if preview != "abc…" {
		t.Errorf("preview = %q, 期望 \"abc…\"", preview)
	}
}

// 已有 preview 时不应被覆盖
func TestEnsurePreviewKeepsExisting(t *testing.T) {
	withPreviewLimit(t, 4)

	out := ensurePreview(json.RawMessage(`{"text":"abcdefghij","preview":"原始预览"}`))
	if !strings.Contains(string(out), "原始预览") {
		t.Errorf("已有 preview 被覆盖了: %s", out)
	}
}
