package vision_relay

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// 递归保护认证 marker（审核 P0-2）。
//
// 旧实现信任固定字面值头 X-NewAPI-Vision-Relay: 1——任何外部调用者都可以
// 自行设置该头直接绕过 Vision Relay。现改为 HMAC 认证 marker：
//
//	格式: vr:<unix_ts>:<hmac_hex>
//	hmac = HMAC-SHA256(sidecall_token, "newapi-vision-relay:<unix_ts>")
//
// - 共享 secret（vision_relay.sidecall_token）来自 DB options，多实例读同一
//   配置 → 跨节点校验一致（sidecall 可能被反代路由到任意实例）。
// - 时间戳窗口 ±5min 防重放（sidecall 为秒级即时调用，窗口充裕）。
// - token 未配置时任何 header 都不被信任（返回 false），继续正常 Enhance；
//   只有认证 marker 匹配才允许 bypass。
// - marker 值不落日志、错误响应或状态 API（调用方保证）。

const (
	markerPrefix = "vr"
	markerMsg    = "newapi-vision-relay"
	markerTTL    = 5 * time.Minute
)

// BuildMarker 构建认证 marker；token 为空返回空串（调用方不携带该头）。
func BuildMarker(token string, now time.Time) string {
	if token == "" {
		return ""
	}
	ts := now.Unix()
	mac := hmac.New(sha256.New, []byte(token))
	_, _ = mac.Write([]byte(fmt.Sprintf("%s:%d", markerMsg, ts)))
	return fmt.Sprintf("%s:%d:%s", markerPrefix, ts, hex.EncodeToString(mac.Sum(nil)))
}

// ValidateMarker 校验请求头是否为合法认证 marker。token 空 / header 空 /
// 格式错 / 时间窗口外 / HMAC 不匹配 → false（调用方忽略该头，继续正常流程）。
func ValidateMarker(token, header string, now time.Time) bool {
	if token == "" || header == "" {
		return false
	}
	parts := strings.Split(header, ":")
	if len(parts) != 3 || parts[0] != markerPrefix {
		return false
	}
	ts, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return false
	}
	if d := now.Sub(time.Unix(ts, 0)); d < -markerTTL || d > markerTTL {
		return false
	}
	mac := hmac.New(sha256.New, []byte(token))
	_, _ = mac.Write([]byte(fmt.Sprintf("%s:%d", markerMsg, ts)))
	want := mac.Sum(nil)
	got, err := hex.DecodeString(parts[2])
	if err != nil {
		return false
	}
	return hmac.Equal(want, got)
}
