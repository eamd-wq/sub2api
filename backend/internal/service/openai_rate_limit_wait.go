package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"go.uber.org/zap"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

type openAIRateLimitWaitContextKey struct{}

// OpenAIRateLimitWaitState 只属于一个 HTTP 请求；各次换号共用等待预算和可恢复的排除项。
// 只在尚未输出语义内容的 failover 分支记录，不能用于重放已经开始的模型回复。
type OpenAIRateLimitWaitState struct {
	timeout      time.Duration
	deadline     time.Time
	transientIDs map[int64]int // 记录原始状态码，保留路由故障与真实限流的区别。
}

// OpenAIRateLimitWaitError 保留选号错误和实际冷却时间，供 HTTP 入口返回明确的 429。
type OpenAIRateLimitWaitError struct {
	RetryAfter          time.Duration
	QueueFull           bool
	UpstreamUnavailable bool // 等待的原因为上游路由故障时，对外保留 503 语义。
	cause               error
}

func (e *OpenAIRateLimitWaitError) Error() string {
	return fmt.Sprintf("%v (RPM/TPM wait exhausted, queue_full=%t)", e.cause, e.QueueFull)
}

func (e *OpenAIRateLimitWaitError) Unwrap() error { return e.cause }

// WithOpenAIRateLimitWait 由 HTTP 入口显式启用，避免改变 WebSocket、媒体和后台探测的时序。
func (s *OpenAIGatewayService) WithOpenAIRateLimitWait(ctx context.Context) (context.Context, *OpenAIRateLimitWaitState) {
	if s == nil || s.cfg == nil || s.cfg.Gateway.Scheduling.RateLimitWaitTimeout <= 0 || s.cfg.Gateway.Scheduling.RateLimitMaxWaiting <= 0 {
		return ctx, nil
	}
	state := &OpenAIRateLimitWaitState{timeout: s.cfg.Gateway.Scheduling.RateLimitWaitTimeout}
	return context.WithValue(ctx, openAIRateLimitWaitContextKey{}, state), state
}

// RecordFailover 对明确的分钟级限流直接换号，不在同一冷却账号上浪费池模式重试。
// 明确的暂时模型路由故障也复用同一等待预算，不改变权限错误或真实模型缺失的行为。
// 同一账号后续出现其他错误时必须撤销恢复资格，不能误清权限失败或利润门的排除项。
func (s *OpenAIRateLimitWaitState) RecordFailover(account *Account, err *UpstreamFailoverError) bool {
	if s == nil || account == nil {
		return false
	}
	delete(s.transientIDs, account.ID)
	if !account.IsOpenAIApiKey() || err == nil || !err.ShouldRetryNextAccount() || !isOpenAIWaitableModelError(err.StatusCode, err.ResponseBody) {
		return false
	}
	if s.transientIDs == nil {
		s.transientIDs = make(map[int64]int)
	}
	if s.deadline.IsZero() {
		s.deadline = time.Now().Add(s.timeout)
	}
	s.transientIDs[account.ID] = err.StatusCode
	return true
}

func (s *OpenAIRateLimitWaitState) upstreamUnavailable(err error) bool {
	if err != nil && strings.Contains(err.Error(), "upstream_model_unavailable=") {
		return true
	}
	for _, status := range s.transientIDs {
		if status == http.StatusNotFound {
			return true
		}
	}
	return false
}

func isOpenAIWaitableModelError(status int, body []byte) bool {
	return (status == http.StatusTooManyRequests && isOpenAIRPMTPMRateLimit(body)) ||
		isUpstreamModelRouteUnavailableError(status, body)
}

func isOpenAIRPMTPMRateLimit(body []byte) bool {
	code := strings.ToLower(gjson.GetBytes(body, "error.code").String())
	if code == "" {
		code = strings.ToLower(gjson.GetBytes(body, "code").String())
	}
	if strings.Contains(code, "rpm") || strings.Contains(code, "tpm") || strings.Contains(code, "requestsperminute") || strings.Contains(code, "tokensperminute") {
		return true
	}
	message := strings.ToLower(gjson.GetBytes(body, "error.message").String())
	return strings.Contains(message, "requests per minute") || strings.Contains(message, "tokens per minute") ||
		strings.Contains(message, "rpm limit") || strings.Contains(message, "tpm limit")
}

// openAIRPMTPMResetAt 提取已落库的 RPM/TPM 与明确路由故障短冷却；余额、日配额和未知 429 不进入短队列。
// 用副本移除这些冷却后复用完整的兼容性校验，绝不修改共享快照或放宽其他调度门。
// 同时接受带明确 404 路由故障原因的短冷却，永久模型缺失仍不具备恢复资格。
func openAIRPMTPMResetAt(account *Account, ctx context.Context, model string) (time.Time, *Account) {
	var resetAt time.Time
	var candidate *Account
	for _, key := range account.modelRateLimitKeysForRequest(ctx, model) {
		until := account.modelRateLimitResetAt(key)
		var reason TempUnschedState
		if until == nil || json.Unmarshal([]byte(account.modelRateLimitReason(key)), &reason) != nil || !isOpenAIWaitableModelError(reason.StatusCode, []byte(reason.ErrorMessage)) {
			continue
		}
		if until.After(resetAt) {
			resetAt = *until
		}
		if candidate == nil {
			copy := *account
			copy.Extra = maps.Clone(account.Extra)
			limits, _ := account.Extra[modelRateLimitsKey].(map[string]any)
			copy.Extra[modelRateLimitsKey] = maps.Clone(limits)
			candidate = &copy
		}
		delete(candidate.Extra[modelRateLimitsKey].(map[string]any), key)
	}
	return resetAt, candidate
}

func (s *OpenAIGatewayService) openAIRateLimitWaitCandidates(ctx context.Context, req OpenAIAccountScheduleRequest, state *OpenAIRateLimitWaitState) (map[int64]time.Time, error) {
	// 带服务端续链状态的请求不能任意迁移；保留原有粘性约束。
	if s.accountRepo == nil || req.Platform != PlatformOpenAI || req.PreviousResponseID != "" || s.checkChannelPricingRestriction(ctx, req.GroupID, req.RequestedModel) {
		return nil, nil
	}
	// 只在整池选号失败后回源一次，读取最新冷却与分组状态，避免快照滞后造成漏等。
	// 复用持久候选池查询，再应用全部运行期校验，不为每个账号各发一条 SQL。
	queryGroupID := req.GroupID
	includeGrouped := s.cfg != nil && s.cfg.RunMode == config.RunModeSimple
	if includeGrouped {
		queryGroupID = nil
	}
	accounts, err := s.accountRepo.ListModelAvailabilityCandidates(ctx, queryGroupID, []string{req.Platform}, includeGrouped)
	if err != nil {
		return nil, err
	}
	ctx = s.withOpenAIQuotaAutoPauseContext(ctx)
	ctx = s.withOpenAIProfitControlGate(ctx, req.GroupID)
	req.RequirePrivacySet = s.openAIGroupRequiresPrivacySet(ctx, req.GroupID)
	scheduler := &defaultOpenAIAccountScheduler{service: s}
	candidates := make(map[int64]time.Time)
	for i := range accounts {
		account := &accounts[i]
		_, excluded := req.ExcludedIDs[account.ID]
		_, transient := state.transientIDs[account.ID]
		if excluded && !transient {
			continue
		}
		if !account.IsOpenAIApiKey() || !account.IsSchedulable() || !s.openAIAccountMatchesSchedulingGroup(account, req.GroupID) || s.isOpenAIAccountBlockedBySchedulingThreshold(ctx, account) {
			continue
		}
		resetAt, candidate := openAIRPMTPMResetAt(account, ctx, req.RequestedModel)
		if candidate == nil || (!excluded && !resetAt.After(time.Now())) {
			continue
		}
		if compatible, _ := scheduler.isAccountRequestCompatibleReason(ctx, candidate, req); !compatible || !scheduler.isAccountTransportCompatible(candidate, req.RequiredTransport) {
			continue
		}
		candidates[account.ID] = resetAt
	}
	return candidates, nil
}

func (s *OpenAIGatewayService) selectAccountWithRateLimitWait(
	ctx context.Context,
	req OpenAIAccountScheduleRequest,
	selectAccount func() (*AccountSelectionResult, OpenAIAccountScheduleDecision, error),
) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error) {
	state, _ := ctx.Value(openAIRateLimitWaitContextKey{}).(*OpenAIRateLimitWaitState)
	queued := false
	var lastSelectionErr error
	defer func() {
		if queued {
			s.openaiRateLimitWaiters.Add(-1)
		}
	}()
	for {
		if err := ctx.Err(); err != nil {
			return nil, OpenAIAccountScheduleDecision{}, err
		}
		var selection *AccountSelectionResult
		var decision OpenAIAccountScheduleDecision
		var err error = ErrNoAvailableAccounts
		expired := state != nil && !state.deadline.IsZero() && !time.Now().Before(state.deadline)
		// 预算从首次 RPM/TPM 失败开始，换号耗时也计入；到期后不能再发起新尝试。
		if !expired {
			selection, decision, err = selectAccount()
			lastSelectionErr = err
		} else if lastSelectionErr != nil {
			// 到期后跳过新尝试，但不能丢掉上一次真实的选号失败原因。
			err = lastSelectionErr
		}
		if err == nil || state == nil || !errors.Is(err, ErrNoAvailableAccounts) {
			return selection, decision, err
		}
		candidates, planErr := s.openAIRateLimitWaitCandidates(ctx, req, state)
		if planErr != nil {
			return nil, decision, planErr
		}
		if len(candidates) == 0 {
			if expired {
				return nil, decision, &OpenAIRateLimitWaitError{RetryAfter: time.Second, UpstreamUnavailable: state.upstreamUnavailable(err), cause: err}
			}
			return selection, decision, err
		}
		now := time.Now()
		var earliest time.Time
		for _, until := range candidates {
			if earliest.IsZero() || until.Before(earliest) {
				earliest = until
			}
		}
		retryAfter := time.Until(earliest)
		if retryAfter < time.Second {
			retryAfter = time.Second
		}
		waitErr := &OpenAIRateLimitWaitError{RetryAfter: retryAfter, UpstreamUnavailable: state.upstreamUnavailable(err), cause: err}
		if state.deadline.IsZero() {
			state.deadline = now.Add(s.cfg.Gateway.Scheduling.RateLimitWaitTimeout)
		}
		remaining := time.Until(state.deadline)
		if remaining <= 0 {
			return nil, decision, waitErr
		}
		if !queued {
			if s.openaiRateLimitWaiters.Add(1) > int64(s.cfg.Gateway.Scheduling.RateLimitMaxWaiting) {
				s.openaiRateLimitWaiters.Add(-1)
				waitErr.QueueFull = true
				return nil, decision, waitErr
			}
			queued = true
		}
		// 使用真实最早重置点；少量随机错峰，避免同一秒恢复时再次形成 RPM/TPM 洪峰。
		// 不抢占账号槽位，不写出模型内容；总预算跨多轮换号累计，客户端取消可立即打断。
		delay := time.Until(earliest)
		if delay < 0 {
			delay = 0
		}
		delay += time.Duration(100+rand.Intn(401)) * time.Millisecond
		delay = min(delay, remaining)
		logger.FromContext(ctx).Info("openai.rpm_tpm_wait",
			zap.Int("candidate_count", len(candidates)), zap.Duration("delay", delay),
			zap.Duration("remaining", remaining), zap.Int64("waiting", s.openaiRateLimitWaiters.Load()))
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, decision, ctx.Err()
		case <-timer.C:
		}
		for id, until := range candidates {
			if _, transient := state.transientIDs[id]; transient && !time.Now().Before(until) {
				delete(req.ExcludedIDs, id)
				delete(state.transientIDs, id)
			}
		}
	}
}
