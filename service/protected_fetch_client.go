package service

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/system_setting"
)

type ssrfResolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

type protectedFetchDialer struct {
	resolver      ssrfResolver
	dialContext   func(ctx context.Context, network, address string) (net.Conn, error)
	getProtection func() (*common.SSRFProtection, bool, error)
}

type ssrfProtectedRoundTripper struct {
	resolver      ssrfResolver
	dialContext   func(ctx context.Context, network, address string) (net.Conn, error)
	getProtection func() (*common.SSRFProtection, bool, error)
	proxy         func(*http.Request) (*url.URL, error)
	// validateURL 覆盖 URL 层校验（nil = ValidateSSRFProtectedFetchURL）。
	// 专用客户端用它强制 ApplyIPFilterForDomain，不 AND 全局 operator 开关。
	validateURL func(urlStr string) error

	mutex      sync.Mutex
	transports map[string]*http.Transport
}

func currentFetchProtection() (*common.SSRFProtection, bool, error) {
	fetchSetting := system_setting.GetFetchSetting()
	if !fetchSetting.EnableSSRFProtection {
		return nil, false, nil
	}

	protection, err := common.NewSSRFProtectionFromFetchSetting(
		fetchSetting.AllowPrivateIp,
		fetchSetting.DomainFilterMode,
		fetchSetting.IpFilterMode,
		fetchSetting.DomainList,
		fetchSetting.IpList,
		fetchSetting.AllowedPorts,
		fetchSetting.ApplyIPFilterForDomain,
	)
	if err != nil {
		return nil, true, err
	}
	return protection, true, nil
}

func newProtectedFetchHTTPClient() *http.Client {
	return newProtectedFetchHTTPClientWithDialer(nil, nil, nil)
}

func newProtectedFetchHTTPClientWithDialer(resolver ssrfResolver, dialContext func(ctx context.Context, network, address string) (net.Conn, error), getProtection func() (*common.SSRFProtection, bool, error)) *http.Client {
	return newProtectedFetchHTTPClientWithProxy(resolver, dialContext, getProtection, http.ProxyFromEnvironment)
}

func newProtectedFetchHTTPClientWithProxy(resolver ssrfResolver, dialContext func(ctx context.Context, network, address string) (net.Conn, error), getProtection func() (*common.SSRFProtection, bool, error), proxy func(*http.Request) (*url.URL, error)) *http.Client {
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	if dialContext == nil {
		netDialer := &net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}
		dialContext = netDialer.DialContext
	}
	if getProtection == nil {
		getProtection = currentFetchProtection
	}
	if proxy == nil {
		proxy = http.ProxyFromEnvironment
	}

	client := &http.Client{
		Transport: &ssrfProtectedRoundTripper{
			resolver:      resolver,
			dialContext:   dialContext,
			getProtection: getProtection,
			proxy:         proxy,
			transports:    make(map[string]*http.Transport),
		},
		CheckRedirect: checkProtectedFetchRedirect,
	}
	if common.RelayTimeout != 0 {
		client.Timeout = time.Duration(common.RelayTimeout) * time.Second
	}
	return client
}

func (t *ssrfProtectedRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil || req.URL == nil {
		return nil, fmt.Errorf("invalid request")
	}
	validate := t.validateURL
	if validate == nil {
		validate = ValidateSSRFProtectedFetchURL
	}
	if err := validate(req.URL.String()); err != nil {
		return nil, err
	}

	proxyURL, err := t.proxy(req)
	if err != nil {
		return nil, err
	}
	return t.transportFor(proxyURL).RoundTrip(req)
}

func (t *ssrfProtectedRoundTripper) CloseIdleConnections() {
	t.mutex.Lock()
	defer t.mutex.Unlock()

	for _, transport := range t.transports {
		transport.CloseIdleConnections()
	}
}

func (t *ssrfProtectedRoundTripper) transportFor(proxyURL *url.URL) *http.Transport {
	// 只按代理地址分组：代理来自环境变量，取值有限，map 有界；
	// 目标 origin 是用户可控输入，不能作为缓存 key。
	key := "direct"
	if proxyURL != nil {
		key = proxyURL.String()
	}
	t.mutex.Lock()
	defer t.mutex.Unlock()

	if transport, ok := t.transports[key]; ok {
		return transport
	}

	transport := t.newTransport(proxyURL)
	t.transports[key] = transport
	return transport
}

func (t *ssrfProtectedRoundTripper) newTransport(proxyURL *url.URL) *http.Transport {
	dialContext := t.dialContext
	proxyFunc := http.ProxyURL(proxyURL)
	if proxyURL == nil {
		protectedDialer := &protectedFetchDialer{
			resolver:      t.resolver,
			dialContext:   t.dialContext,
			getProtection: t.getProtection,
		}
		dialContext = protectedDialer.DialContext
		proxyFunc = nil
	}

	transport := &http.Transport{
		MaxIdleConns:        common.RelayMaxIdleConns,
		MaxIdleConnsPerHost: common.RelayMaxIdleConnsPerHost,
		IdleConnTimeout:     time.Duration(common.RelayIdleConnTimeout) * time.Second,
		ForceAttemptHTTP2:   true,
		Proxy:               proxyFunc,
		DialContext:         dialContext,
	}
	if common.TLSInsecureSkipVerify {
		transport.TLSClientConfig = common.InsecureTLSConfig
	}
	return transport
}

func (d *protectedFetchDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	protection, enabled, err := d.getProtection()
	if err != nil {
		return nil, err
	}
	if !enabled {
		return d.dialContext(ctx, network, addr)
	}

	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("invalid dial address %s: %w", addr, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return nil, fmt.Errorf("invalid port: %s", portText)
	}
	if err := protection.ValidateNetworkTarget(host, port); err != nil {
		return nil, err
	}

	if ip := net.ParseIP(host); ip != nil {
		return d.dialContext(ctx, network, net.JoinHostPort(ip.String(), portText))
	}
	if !protection.ApplyIPFilterForDomain {
		return d.dialContext(ctx, network, addr)
	}

	resolved, err := d.resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("DNS resolution failed for %s: %v", host, err)
	}

	var candidateIPs []net.IP
	for _, ipAddr := range resolved {
		ip := ipAddr.IP
		if ip == nil || !networkAllowsIP(network, ip) {
			continue
		}
		if err := protection.ValidateResolvedIP(host, ip); err != nil {
			return nil, err
		}
		candidateIPs = append(candidateIPs, ip)
	}

	var lastDialErr error
	for _, ip := range candidateIPs {
		conn, err := d.dialContext(ctx, network, net.JoinHostPort(ip.String(), portText))
		if err == nil {
			return conn, nil
		}
		lastDialErr = err
	}

	if lastDialErr != nil {
		return nil, lastDialErr
	}
	return nil, fmt.Errorf("DNS resolution for %s returned no usable IP addresses", host)
}

func networkAllowsIP(network string, ip net.IP) bool {
	switch network {
	case "tcp4":
		return ip.To4() != nil
	case "tcp6":
		return ip.To4() == nil
	default:
		return true
	}
}

// validateVisionRelayFetchURL 校验 vision relay 抓取的用户图片 URL：与全局 SSRF
// 校验同源，但强制 ApplyIPFilterForDomain=true，不 AND 全局 operator 开关
// （validateURLWithCurrentFetchSetting 里是 applyDomainIPFilter && setting.ApplyIPFilterForDomain）。
// 用户提供的图片 URL 必须对域名做 IP 过滤，防止 DNS rebinding（B6）。
func validateVisionRelayFetchURL(urlStr string) error {
	fetchSetting := system_setting.GetFetchSetting()
	return common.ValidateURLWithFetchSetting(urlStr, fetchSetting.EnableSSRFProtection, fetchSetting.AllowPrivateIp, fetchSetting.DomainFilterMode, fetchSetting.IpFilterMode, fetchSetting.DomainList, fetchSetting.IpList, fetchSetting.AllowedPorts, true)
}

// checkVisionRelayFetchRedirect 重定向时同样强制 ApplyIPFilterForDomain=true。
func checkVisionRelayFetchRedirect(req *http.Request, via []*http.Request) error {
	urlStr := req.URL.String()
	if err := validateVisionRelayFetchURL(urlStr); err != nil {
		return fmt.Errorf("redirect to %s blocked: %v", urlStr, err)
	}
	if len(via) >= 10 {
		return fmt.Errorf("stopped after 10 redirects")
	}
	return nil
}

// visionRelayFetchProtection 构建 dialer 层保护：与 currentFetchProtection 同源，
// 但强制 ApplyIPFilterForDomain=true（direct 路径最终 dial 前必须解析并校验 IP）。
func visionRelayFetchProtection() (*common.SSRFProtection, bool, error) {
	fetchSetting := system_setting.GetFetchSetting()
	if !fetchSetting.EnableSSRFProtection {
		return nil, false, nil
	}
	protection, err := common.NewSSRFProtectionFromFetchSetting(
		fetchSetting.AllowPrivateIp,
		fetchSetting.DomainFilterMode,
		fetchSetting.IpFilterMode,
		fetchSetting.DomainList,
		fetchSetting.IpList,
		fetchSetting.AllowedPorts,
		true,
	)
	if err != nil {
		return nil, true, err
	}
	return protection, true, nil
}

// newVisionRelayProtectedFetchClient 构建 vision relay 专用 SSRF 保护客户端：
// 三层（URL/dialer/redirect）都强制 ApplyIPFilterForDomain=true；代理策略由
// disableProxy 控制（B5：proxy-only 出口部署时禁用环境代理，默认走环境代理）。
func newVisionRelayProtectedFetchClient(disableProxy bool) *http.Client {
	proxyFunc := http.ProxyFromEnvironment
	if disableProxy {
		// 传非 nil 函数返回 nil：禁用代理（nil proxyFunc 会被构造函数替换回
		// http.ProxyFromEnvironment，无法表达"明确禁用"）。
		proxyFunc = func(*http.Request) (*url.URL, error) { return nil, nil }
	}

	// 复用通用构造函数以继承 resolver/dialContext 默认值；getProtection 强制
	// ApplyIPFilterForDomain=true。随后覆盖 URL 层校验与 redirect 校验为强制版本。
	client := newProtectedFetchHTTPClientWithProxy(nil, nil, visionRelayFetchProtection, proxyFunc)
	if rt, ok := client.Transport.(*ssrfProtectedRoundTripper); ok {
		rt.validateURL = validateVisionRelayFetchURL
	}
	client.CheckRedirect = checkVisionRelayFetchRedirect
	return client
}

var (
	visionRelayFetchClientNoProxy *http.Client
	visionRelayFetchClientProxy   *http.Client
	visionRelayFetchClientOnce    sync.Once
)

func initVisionRelayFetchClients() {
	visionRelayFetchClientNoProxy = newVisionRelayProtectedFetchClient(true)
	visionRelayFetchClientProxy = newVisionRelayProtectedFetchClient(false)
}

// GetVisionRelayFetchClient 返回 vision relay 专用的 SSRF 保护客户端。
// 与 GetSSRFProtectedHTTPClientForUserInput 相同，永不 fallback 到 general
// client（用户输入必须受保护）；额外强制三层 ApplyIPFilterForDomain=true。
// disableProxy 控制代理策略（B5，默认 false=走环境代理）。返回 nil 表示保护
// 不可用（全局 EnableSSRFProtection 关闭），调用方必须 fail closed。
func GetVisionRelayFetchClient(disableProxy bool) *http.Client {
	if fetchSetting := system_setting.GetFetchSetting(); fetchSetting != nil && !fetchSetting.EnableSSRFProtection {
		return nil
	}
	visionRelayFetchClientOnce.Do(initVisionRelayFetchClients)
	if disableProxy {
		return visionRelayFetchClientNoProxy
	}
	return visionRelayFetchClientProxy
}
