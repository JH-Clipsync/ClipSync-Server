package main

import (
	"encoding/json"
	"errors"
)

// ===== 端到端隧道加密（服务端视角） =====
//
// 设计原则：**服务端永远拿不到明文，也拿不到密钥**。
//
// 客户端用「用户在本地设置的同步密码」派生对称密钥（PBKDF2-HMAC-SHA256），
// 用 AES-256-GCM 加密整个业务 payload，把结果放进 payload.enc：
//
//	{
//	  "id": "...", "type": "clipboard", "from": "...", "ts": 0,
//	  "payload": {
//	    "enc": {
//	      "v": 1,                  // 协议版本
//	      "alg": "AES-256-GCM",
//	      "kdf": "PBKDF2-HMAC-SHA256",
//	      "iter": 200000,          // 迭代次数
//	      "salt": "<base64>",      // KDF 盐（每个用户固定，取自密码指纹）
//	      "iv": "<base64>",        // 12 字节随机 nonce，每条消息不同
//	      "ct": "<base64>",        // 密文 = AES-GCM(明文 payload JSON)
//	      "fp": "<hex 前 16 位>"   // 密钥指纹，收端用来判断"密码是否一致"
//	    },
//	    "preview": "🔒 加密消息"   // 可选：仅用于日志/占位，绝不含真实内容
//	  }
//	}
//
// 服务端只做三件事：
//  1. 识别这是不是加密消息（payload.enc 存在且字段完整）
//  2. e2ee.require = true 时拒绝明文消息
//  3. 原样转发，日志里不打印任何密文内容
//
// 短信清洗（sanitizeSmsPayload）对加密消息自然失效——服务端看不到 text，
// 所以清洗职责在加密链路下由发送端（Android）在加密前完成。

// EncEnvelope 加密信封，跟客户端字段一一对应。
type EncEnvelope struct {
	V    int    `json:"v"`
	Alg  string `json:"alg"`
	KDF  string `json:"kdf"`
	Iter int    `json:"iter"`
	Salt string `json:"salt"`
	IV   string `json:"iv"`
	CT   string `json:"ct"`
	FP   string `json:"fp"`
}

// 当前支持的协议版本与算法
const (
	EncVersion = 1
	EncAlg     = "AES-256-GCM"
	EncKDF     = "PBKDF2-HMAC-SHA256"
)

// ErrPlaintextRejected e2ee.require 开启时收到明文消息
var ErrPlaintextRejected = errors.New("服务端要求端到端加密，拒绝明文消息")

// ErrBadEnvelope 加密信封字段不完整或版本不支持
var ErrBadEnvelope = errors.New("加密信封不合法")

// parseEnvelope 从 payload 里取出加密信封。
// 返回 (nil, nil) 表示这是一条明文消息。
func parseEnvelope(payload json.RawMessage) (*EncEnvelope, error) {
	var p struct {
		Enc *EncEnvelope `json:"enc"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil, nil // 解析不了当明文处理，后续逻辑照旧
	}
	if p.Enc == nil {
		return nil, nil
	}
	e := p.Enc
	if e.V != EncVersion || e.Alg != EncAlg {
		return nil, ErrBadEnvelope
	}
	if e.IV == "" || e.CT == "" || e.Salt == "" {
		return nil, ErrBadEnvelope
	}
	return e, nil
}

// checkEncryption 是消息进入路由前的加密策略闸门。
//
// 返回 encrypted 表示这条消息是密文；err != nil 时调用方应拒绝转发。
func checkEncryption(payload json.RawMessage) (encrypted bool, err error) {
	env, err := parseEnvelope(payload)
	if err != nil {
		return false, err
	}
	if env != nil {
		return true, nil
	}
	if globalConfig != nil && globalConfig.E2EE.Require {
		return false, ErrPlaintextRejected
	}
	return false, nil
}

// encPreview 加密消息在日志里的占位文案。
// 只暴露密钥指纹（用于排查"两端密码不一致"），不暴露任何密文字节。
func encPreview(env *EncEnvelope) string {
	if env == nil {
		return "🔒 加密消息"
	}
	fp := env.FP
	if len(fp) > 8 {
		fp = fp[:8]
	}
	if fp == "" {
		return "🔒 加密消息"
	}
	return "🔒 加密消息 (key=" + fp + ")"
}
