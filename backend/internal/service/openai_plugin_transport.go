package service

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

func (s *OpenAIGatewayService) SetPluginManager(manager *PluginManager) {
	s.pluginManager = manager
}

// doOpenAIUpstream 只在 OpenAI OAuth 能力绑定已启用时把真实请求交给插件。
// 插件返回标准 http.Response，响应解析、错误映射、SSE 和计费仍由现有核心链处理。
func (s *OpenAIGatewayService) doOpenAIUpstream(request *http.Request, proxyURL string, account *Account, contexts ...*gin.Context) (*http.Response, error) {
	var c *gin.Context
	if len(contexts) > 0 {
		c = contexts[0]
	}
	if c != nil {
		c.Set(opsProxyDirectFallbackAccountKey, int64(0))
	}
	if s.pluginManager != nil {
		response, handled, err := s.pluginManager.RoundTripOpenAIOAuth(request.Context(), request, proxyURL, account)
		if handled {
			return response, err
		}
	}
	return doOpenAIProxyFallback(request, proxyURL, account, s.httpUpstream.Do, func() {
		if c == nil {
			return
		}
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			ProxyID: opsUpstreamProxyID(account), ProxyName: opsUpstreamProxyName(account),
			Platform: account.Platform, AccountID: account.ID,
			Kind: "request_error", Stage: "proxy_connect", Scope: "proxy",
			Reason:  "proxy_connection_failed_fallback_direct",
			Message: "Account proxy unavailable; falling back to server egress before sending request",
		})
		c.Set(opsProxyDirectFallbackAccountKey, account.ID)
	})
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
		profile := s.tlsFPProfileService.ResolveTLSProfile(account)
		return doOpenAIProxyFallback(request, proxyURL, account, func(req *http.Request, proxy string, id int64, concurrency int) (*http.Response, error) {
			return s.httpUpstream.DoWithTLS(req, proxy, id, concurrency, profile)
		}, nil)
	}
	return doOpenAIProxyFallback(request, proxyURL, account, s.httpUpstream.Do, nil)
}
