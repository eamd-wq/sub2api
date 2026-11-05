package service

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync/atomic"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

const openAIProxyFallbackDirectKey = "openai_proxy_fallback_to_direct"
const opsProxyDirectFallbackAccountKey = "ops_proxy_direct_fallback_account"

type openAIProxyRequestFunc func(*http.Request, string, int64, int) (*http.Response, error)

// doOpenAIProxyFallback 仅为显式允许的 API Key 账号处理代理建连失败。
// 目标连接尚未交给 HTTP 层、请求体可重建时才回退一次，避免重复生成或重复计费。
func doOpenAIProxyFallback(request *http.Request, proxyURL string, account *Account, do openAIProxyRequestFunc, onFallback func()) (*http.Response, error) {
	if account == nil {
		return nil, errors.New("OpenAI upstream account is required")
	}
	allowed, _ := account.Extra[openAIProxyFallbackDirectKey].(bool)
	if request == nil || proxyURL == "" || !account.IsOpenAIApiKey() || !allowed {
		return do(request, proxyURL, account.ID, account.Concurrency)
	}

	// GetConn 证明实际使用了支持跟踪的 HTTP 传输；未知自定义传输不自动重放。
	var connectionStarted atomic.Bool
	// GotConn 后即可能发送部分请求；任何写入信号同样禁止出口回退。
	var connectionAcquired atomic.Bool
	trace := &httptrace.ClientTrace{
		GetConn:      func(string) { connectionStarted.Store(true) },
		GotConn:      func(httptrace.GotConnInfo) { connectionAcquired.Store(true) },
		WroteHeaders: func() { connectionAcquired.Store(true) },
		WroteRequest: func(httptrace.WroteRequestInfo) { connectionAcquired.Store(true) },
	}
	attempt := request.WithContext(httptrace.WithClientTrace(request.Context(), trace))
	response, err := do(attempt, proxyURL, account.ID, account.Concurrency)
	if err == nil || response != nil || !connectionStarted.Load() || connectionAcquired.Load() || request.Context().Err() != nil || !isOpenAIProxyConnectionError(err) {
		return response, err
	}

	fallback := request.Clone(request.Context())
	if request.Body != nil && request.Body != http.NoBody {
		if request.GetBody == nil {
			return response, err
		}
		body, replayErr := request.GetBody()
		if replayErr != nil {
			return response, err
		}
		fallback.Body = body
	}
	if onFallback != nil {
		onFallback()
	}
	requestID, _ := request.Context().Value(ctxkey.RequestID).(string)
	logger.L().Warn("openai.proxy_fallback_direct",
		zap.Int64("account_id", account.ID), zap.Any("configured_proxy_id", account.ProxyID),
		zap.String("request_id", requestID), zap.String("reason", "proxy_connection_failed_before_request"),
		zap.String("actual_route", "direct/no_proxy"))
	return do(fallback, "", account.ID, account.Concurrency)
}

func isOpenAIProxyConnectionError(err error) bool {
	var networkError net.Error
	return errors.As(err, &networkError) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || classifyUpstreamTransportError(err).Persistent
}

func isOpenAIProxyDirectFallback(c *gin.Context, accountID int64) bool {
	if c == nil || accountID <= 0 {
		return false
	}
	value, _ := c.Get(opsProxyDirectFallbackAccountKey)
	actualAccountID, _ := value.(int64)
	return actualAccountID == accountID
}
