package service

import "net/http"

func (s *OpenAIGatewayService) SetPluginManager(manager *PluginManager) {
	s.pluginManager = manager
}

// doOpenAIUpstream 只在 OpenAI OAuth 能力绑定已启用时把真实请求交给插件。
// 插件返回标准 http.Response，响应解析、错误映射、SSE 和计费仍由现有核心链处理。
func (s *OpenAIGatewayService) doOpenAIUpstream(request *http.Request, proxyURL string, account *Account) (*http.Response, error) {
	if s.pluginManager != nil {
		response, handled, err := s.pluginManager.RoundTripOpenAIOAuth(request.Context(), request, proxyURL, account)
		if handled {
			return response, err
		}
	}
	// pi 模式：真实推理流量改走 TLS 指纹 client（Node 内置 ClientHello、ALPN 仅
	// http/1.1），非 pi 模式保持原有裸 Do，不影响其他 provider 与 API-key 账号。
	if s.piImpersonationActiveFor(account) {
		return s.doOpenAIUpstreamForPi(request, proxyURL, account)
	}
	return s.httpUpstream.Do(request, proxyURL, account.ID, account.Concurrency)
}

// doOpenAIUpstreamForPi 走 pi 的 TLS 形状：Node 内置 ClientHello（无 GREASE、ALPN 仅
// http/1.1）与 HTTP/1.1 连接池。pi 走 globalThis.fetch / undici，从不协商 h2。
//
// 为什么不用账号绑定的 TLS 指纹模板：ResolveTLSProfile 的门是 Anthropic OAuth/SetupToken，
// OpenAI 账号拿到 nil；pi 要的是 Node 内建形态（E1/A1 的标定目标），固定用内置 profile。
func (s *OpenAIGatewayService) doOpenAIUpstreamForPi(request *http.Request, proxyURL string, account *Account) (*http.Response, error) {
	return s.httpUpstream.DoWithTLS(request, proxyURL, account.ID, account.Concurrency, PiUpstreamTLSProfile())
}

// doOpenAIAccountTestUpstream 让 OpenAI OAuth 账号测试与真实转发使用同一插件路径。
// API Key 和未命中插件的账号保持各自原有的 HTTPUpstream 行为。
func (s *AccountTestService) doOpenAIAccountTestUpstream(
	request *http.Request,
	proxyURL string,
	account *Account,
	useTLSFallback bool,
) (*http.Response, error) {
	if s.pluginManager != nil {
		response, handled, err := s.pluginManager.RoundTripOpenAIOAuth(request.Context(), request, proxyURL, account)
		if handled {
			return response, err
		}
	}
	if useTLSFallback {
		return s.httpUpstream.DoWithTLS(
			request,
			proxyURL,
			account.ID,
			account.Concurrency,
			s.tlsFPProfileService.ResolveTLSProfile(account),
		)
	}
	return s.httpUpstream.Do(request, proxyURL, account.ID, account.Concurrency)
}
