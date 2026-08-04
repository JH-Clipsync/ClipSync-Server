package main

import (
	"encoding/json"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"golang.org/x/crypto/pbkdf2"
)

// 一个字段完整的加密信封
const validEnvelopeJSON = `{"enc":{"v":1,"alg":"AES-256-GCM","kdf":"PBKDF2-HMAC-SHA256",` +
	`"iter":200000,"salt":"c2FsdHNhbHQ=","iv":"aXZpdml2aXZpdg==","ct":"Y2lwaGVydGV4dA==","fp":"a1b2c3d4e5f6a7b8"}}`

// TestBuiltinSyncPasswordFingerprint 锁住「内置默认同步密码」在三端的一致性。
//
// 用户开了加密但没填密码时，三端都回落到 BuiltinSyncPassword。这里按客户端
// 的派生规则算一遍指纹：任何一端改了常量或 KDF 参数，这个断言就会失败，
// 提醒必须三端同步改（Android BuiltinSyncPasswordTest 有同样的哨兵）。
func TestBuiltinSyncPasswordFingerprint(t *testing.T) {
	const (
		saltSeed    = "clipsync-e2ee-v1"
		iterations  = 200000
		wantPass    = "cs1-louuMZxNFCXgL1AcXjlBCly2E54NeH5T"
		wantFingerp = "6cc296bd5bca6b1d"
	)

	if BuiltinSyncPassword != wantPass {
		t.Fatalf("内置密码常量被改动：想要 %q，实际 %q（三端必须同步）", wantPass, BuiltinSyncPassword)
	}

	salt := sha256.Sum256([]byte(saltSeed))
	key := pbkdf2.Key([]byte(BuiltinSyncPassword), salt[:], iterations, 32, sha256.New)
	sum := sha256.Sum256(key)
	got := hex.EncodeToString(sum[:])[:16]
	if got != wantFingerp {
		t.Fatalf("内置密码指纹不符：想要 %s，实际 %s", wantFingerp, got)
	}
}

func TestParseEnvelopeAcceptsValid(t *testing.T) {
	env, err := parseEnvelope(json.RawMessage(validEnvelopeJSON))
	if err != nil {
		t.Fatalf("合法信封应解析成功，实际 %v", err)
	}
	if env == nil {
		t.Fatal("合法信封不该返回 nil")
	}
	if env.Alg != EncAlg || env.V != EncVersion {
		t.Errorf("信封字段解析错误: %+v", env)
	}
}

func TestParseEnvelopeTreatsPlainAsNil(t *testing.T) {
	env, err := parseEnvelope(json.RawMessage(`{"text":"hello","kind":"text"}`))
	if err != nil {
		t.Fatalf("明文 payload 不该报错，实际 %v", err)
	}
	if env != nil {
		t.Errorf("明文 payload 应返回 nil 信封，实际 %+v", env)
	}
}

func TestParseEnvelopeRejectsBadEnvelope(t *testing.T) {
	cases := map[string]string{
		"版本不支持": `{"enc":{"v":99,"alg":"AES-256-GCM","salt":"cw==","iv":"aXY=","ct":"Y3Q="}}`,
		"算法不支持": `{"enc":{"v":1,"alg":"DES","salt":"cw==","iv":"aXY=","ct":"Y3Q="}}`,
		"缺少 iv": `{"enc":{"v":1,"alg":"AES-256-GCM","salt":"cw==","ct":"Y3Q="}}`,
		"缺少密文":  `{"enc":{"v":1,"alg":"AES-256-GCM","salt":"cw==","iv":"aXY="}}`,
		"缺少盐":   `{"enc":{"v":1,"alg":"AES-256-GCM","iv":"aXY=","ct":"Y3Q="}}`,
	}
	for name, payload := range cases {
		if _, err := parseEnvelope(json.RawMessage(payload)); err == nil {
			t.Errorf("%s：应判为非法信封", name)
		}
	}
}

func TestCheckEncryptionAllowsPlaintextByDefault(t *testing.T) {
	origCfg := globalConfig
	defer func() { globalConfig = origCfg }()
	globalConfig = DefaultConfig() // e2ee.require = false

	encrypted, err := checkEncryption(json.RawMessage(`{"text":"hello"}`))
	if err != nil {
		t.Fatalf("默认配置下明文应放行，实际 %v", err)
	}
	if encrypted {
		t.Error("明文消息不该被判为加密")
	}
}

func TestCheckEncryptionRejectsPlaintextWhenRequired(t *testing.T) {
	origCfg := globalConfig
	defer func() { globalConfig = origCfg }()
	globalConfig = DefaultConfig()
	globalConfig.E2EE.Require = true

	if _, err := checkEncryption(json.RawMessage(`{"text":"hello"}`)); err == nil {
		t.Error("require=true 时明文应被拒绝")
	}

	// 密文照常放行
	encrypted, err := checkEncryption(json.RawMessage(validEnvelopeJSON))
	if err != nil {
		t.Fatalf("require=true 时密文应放行，实际 %v", err)
	}
	if !encrypted {
		t.Error("密文消息应被判为加密")
	}
}

// 日志占位文案只能露出密钥指纹，绝不能带密文
func TestEncPreviewHidesCiphertext(t *testing.T) {
	env, _ := parseEnvelope(json.RawMessage(validEnvelopeJSON))
	preview := encPreview(env)
	if preview == "" {
		t.Fatal("预览不该为空")
	}
	if strings.Contains(preview, env.CT) {
		t.Errorf("预览泄漏了密文: %q", preview)
	}
	if !strings.Contains(preview, "a1b2c3d4") {
		t.Errorf("预览应包含密钥指纹前缀，实际 %q", preview)
	}
	if got := encPreview(nil); got == "" {
		t.Error("nil 信封也应有占位文案")
	}
}
