package vision_relay

import (
	"strings"
	"testing"
	"time"
)

// 正常往返：同一 token 构建 → 校验通过（多实例语义：sidecall 实例构建、
// 目标实例校验——Build/Validate 分离即跨节点验证，只要共享同一 token）
func TestMarkerRoundTrip(t *testing.T) {
	const token = "shared-secret-token"
	now := time.Now()
	marker := BuildMarker(token, now)
	if marker == "" {
		t.Fatal("marker must not be empty")
	}
	if !ValidateMarker(token, marker, now.Add(2*time.Second)) {
		t.Fatal("valid marker must validate")
	}
	if !ValidateMarker(token, marker, now.Add(-2*time.Second)) {
		t.Fatal("valid marker must validate within window (slight clock skew)")
	}
}

// 外部伪造：旧字面值 "1"、任意随机串、格式错、篡改 HMAC → 全部拒绝
func TestValidateMarkerForged(t *testing.T) {
	const token = "shared-secret-token"
	now := time.Now()
	valid := BuildMarker(token, now)

	cases := []string{
		"1",                                  // 旧版字面值（P0-2 主修复对象）
		"",                                   // 空头
		"vr",                                 // 缺段
		"vr:123",                             // 缺 HMAC 段
		"vr:abc:deadbeef",                    // 时间戳非数字
		"vr:" + "9999999999" + ":" + "abcd",  // 过期时间戳（1970 年）
		valid[:len(valid)-1] + "0",           // 篡改最后一个 hex 字符
		"vr:99999999999999999999:abcd",       // 时间戳溢出
		"other:123:abcd",                     // 错误前缀
	}
	for _, h := range cases {
		if ValidateMarker(token, h, now) {
			t.Fatalf("forged header %q must be rejected", h)
		}
	}
	// 不同 token 构建的 marker → 拒绝（跨实例共享同一 token 才有效）
	other := BuildMarker("other-token", now)
	if ValidateMarker(token, other, now) {
		t.Fatal("marker from different token must be rejected")
	}
}

// 时间窗口：±5min 外拒绝（防重放）
func TestValidateMarkerExpired(t *testing.T) {
	const token = "shared-secret-token"
	now := time.Now()
	marker := BuildMarker(token, now)
	if ValidateMarker(token, marker, now.Add(6*time.Minute)) {
		t.Fatal("expired marker (future) must be rejected")
	}
	if ValidateMarker(token, marker, now.Add(-6*time.Minute)) {
		t.Fatal("expired marker (past) must be rejected")
	}
}

// token 未配置：任何 header 都不被信任（不 bypass）
func TestValidateMarkerEmptyToken(t *testing.T) {
	now := time.Now()
	if ValidateMarker("", "1", now) {
		t.Fatal("empty token must never trust any header")
	}
	if BuildMarker("", now) != "" {
		t.Fatal("empty token must not build a marker (caller omits header)")
	}
	if strings.Contains(BuildMarker("t", now), "\n") || strings.Contains(BuildMarker("t", now), ":") == false {
		t.Fatal("marker must be single-line header-safe value")
	}
}

// marker 不携带 token 明文（日志安全：值本身不泄露 secret）
func TestMarkerDoesNotLeakToken(t *testing.T) {
	const token = "super-secret-token-value"
	marker := BuildMarker(token, time.Now())
	if strings.Contains(marker, token) {
		t.Fatal("marker must not contain the token itself")
	}
}
