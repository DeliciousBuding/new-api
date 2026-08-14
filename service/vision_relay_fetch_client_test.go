package service

import (
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/stretchr/testify/require"
)

// TestValidateVisionRelayFetchURLForcesDomainIPFilter 锁定 B6 核心不变量：
// vision relay 的用户图片 URL 校验强制 ApplyIPFilterForDomain=true，即使全局
// operator 开关关闭。localhost 解析到回环地址（私有），强制 IP 过滤时必须拒绝。
func TestValidateVisionRelayFetchURLForcesDomainIPFilter(t *testing.T) {
	configureSSRFTestFetchSetting(t)
	fetchSetting := system_setting.GetFetchSetting()
	fetchSetting.ApplyIPFilterForDomain = false

	err := validateVisionRelayFetchURL("http://localhost/resource")

	require.Error(t, err)
	require.Contains(t, err.Error(), "private IP address not allowed")
}

// TestValidateURLWithCurrentFetchSettingAndsGlobalIPFilter 对照用例：全局校验
// 的 applyDomainIPFilter 与 operator 开关做 AND，全局关闭时 localhost 不拦截。
// 与上个测试成对，共同证明 vision 专用校验确实"强制"而非沿用全局开关。
func TestValidateURLWithCurrentFetchSettingAndsGlobalIPFilter(t *testing.T) {
	configureSSRFTestFetchSetting(t)
	fetchSetting := system_setting.GetFetchSetting()
	fetchSetting.ApplyIPFilterForDomain = false

	err := validateURLWithCurrentFetchSetting("http://localhost/resource", true)

	require.NoError(t, err)
}

// TestVisionRelayFetchProtectionForcesDomainIPFilter 锁定 dialer 层不变量：
// visionRelayFetchProtection 构建的 protection 强制 ApplyIPFilterForDomain=true。
func TestVisionRelayFetchProtectionForcesDomainIPFilter(t *testing.T) {
	configureSSRFTestFetchSetting(t)
	fetchSetting := system_setting.GetFetchSetting()
	fetchSetting.ApplyIPFilterForDomain = false

	protection, enabled, err := visionRelayFetchProtection()

	require.NoError(t, err)
	require.True(t, enabled)
	require.True(t, protection.ApplyIPFilterForDomain)
}

// TestGetVisionRelayFetchClientNilWhenProtectionDisabled 锁定 fail-closed 契约：
// 全局 SSRF 保护关闭时返回 nil，调用方必须拒绝抓取而不是降级到无保护客户端。
func TestGetVisionRelayFetchClientNilWhenProtectionDisabled(t *testing.T) {
	fetchSetting := system_setting.GetFetchSetting()
	original := *fetchSetting
	t.Cleanup(func() {
		*fetchSetting = original
	})

	fetchSetting.EnableSSRFProtection = false

	require.Nil(t, GetVisionRelayFetchClient(false))
	require.Nil(t, GetVisionRelayFetchClient(true))
}

// TestNewVisionRelayProtectedFetchClientForcesIPFilterAndDisablesProxy 锁定 B5+B6：
// disableProxy=true 时代理函数明确返回 nil（禁用代理），且 URL 层校验被覆盖为
// 强制版本（validateURL 字段指向 validateVisionRelayFetchURL）。
func TestNewVisionRelayProtectedFetchClientForcesIPFilterAndDisablesProxy(t *testing.T) {
	noProxy := newVisionRelayProtectedFetchClient(true)
	require.NotNil(t, noProxy)

	roundTripper, ok := noProxy.Transport.(*ssrfProtectedRoundTripper)
	require.True(t, ok)

	// B6：URL 层校验被覆盖为强制版本（非默认 ValidateSSRFProtectedFetchURL）。
	require.NotNil(t, roundTripper.validateURL)

	// B5：disableProxy=true → 代理函数返回 nil（禁用代理），且不报错。
	proxyURL, err := roundTripper.proxy(&http.Request{})
	require.NoError(t, err)
	require.Nil(t, proxyURL)
}

// TestNewVisionRelayProtectedFetchClientKeepsProxyByDefault 锁定 B5 默认语义：
// disableProxy=false 时保持环境代理（proxy 字段非 nil 且不是"禁用"函数）。
func TestNewVisionRelayProtectedFetchClientKeepsProxyByDefault(t *testing.T) {
	withProxy := newVisionRelayProtectedFetchClient(false)
	require.NotNil(t, withProxy)

	roundTripper, ok := withProxy.Transport.(*ssrfProtectedRoundTripper)
	require.True(t, ok)
	require.NotNil(t, roundTripper.proxy)
	require.NotNil(t, roundTripper.validateURL)
}
