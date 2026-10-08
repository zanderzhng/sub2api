package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// piRoutingAccount 构造一个可调度、可参与资格判定的 OpenAI OAuth 账号，并按需打上 pi 平台标签。
func piRoutingAccount(id int64, platformTag string) *Account {
	extra := map[string]any{}
	if platformTag != "" {
		extra[PiPlatformExtraKey] = platformTag
	}
	return &Account{
		ID:          id,
		Name:        "pi-routing-" + platformTag,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		Credentials: map[string]any{"chatgpt_account_id": "chatgpt-acc"},
		Extra:       extra,
	}
}

// TestPiPlatformRoutingSelectsMatchingCredential 验证平台 → 凭据路由：
// 两个引擎（legacy 资格门与 /responses 实际走的 scheduler 门）都必须只放行
// 与请求平台一致的账号；未标注标签的账号按通配放行（过渡期兼容）。
func TestPiPlatformRoutingSelectsMatchingCredential(t *testing.T) {
	t.Cleanup(func() { SetPiImpersonationSettings(false, "darwin") })

	win := piRoutingAccount(11, "win32")
	mac := piRoutingAccount(12, "darwin")
	untagged := piRoutingAccount(13, "")
	svc := newPiTestService()
	scheduler := &defaultOpenAIAccountScheduler{service: svc}

	SetPiImpersonationSettings(true, "darwin")
	winCtx := SetPiPlatform(context.Background(), PiPlatformWin32)
	macCtx := SetPiPlatform(context.Background(), PiPlatformDarwin)
	request := OpenAIAccountScheduleRequest{Platform: PlatformOpenAI, RequestedModel: "gpt-5.4-codex"}

	// legacy 引擎（DB recheck 与粘性循环共用）。
	require.True(t, isOpenAICompatibleAccountEligibleForRequest(winCtx, win, PlatformOpenAI, "gpt-5.4-codex", false, OpenAIEndpointCapabilityChatCompletions))
	require.False(t, isOpenAICompatibleAccountEligibleForRequest(winCtx, mac, PlatformOpenAI, "gpt-5.4-codex", false, OpenAIEndpointCapabilityChatCompletions))
	require.Equal(t, "pi_platform_mismatch",
		openAICompatibleAccountEligibilityFailureReason(winCtx, mac, PlatformOpenAI, "gpt-5.4-codex", false, OpenAIEndpointCapabilityChatCompletions))
	require.True(t, isOpenAICompatibleAccountEligibleForRequest(macCtx, mac, PlatformOpenAI, "gpt-5.4-codex", false, OpenAIEndpointCapabilityChatCompletions))
	require.False(t, isOpenAICompatibleAccountEligibleForRequest(macCtx, win, PlatformOpenAI, "gpt-5.4-codex", false, OpenAIEndpointCapabilityChatCompletions))

	// scheduler 引擎（/responses 主路径）。
	compatible, reason := scheduler.isAccountRequestCompatibleReason(winCtx, win, request)
	require.True(t, compatible, reason)
	compatible, reason = scheduler.isAccountRequestCompatibleReason(winCtx, mac, request)
	require.False(t, compatible)
	require.Equal(t, "pi_platform_mismatch", reason)
	compatible, reason = scheduler.isAccountRequestCompatibleReason(macCtx, mac, request)
	require.True(t, compatible, reason)
	compatible, reason = scheduler.isAccountRequestCompatibleReason(macCtx, win, request)
	require.False(t, compatible)
	require.Equal(t, "pi_platform_mismatch", reason)

	// 未标注标签的账号在两个平台下都放行。
	require.True(t, isOpenAICompatibleAccountEligibleForRequest(winCtx, untagged, PlatformOpenAI, "gpt-5.4-codex", false, OpenAIEndpointCapabilityChatCompletions))
	require.True(t, isOpenAICompatibleAccountEligibleForRequest(macCtx, untagged, PlatformOpenAI, "gpt-5.4-codex", false, OpenAIEndpointCapabilityChatCompletions))
}

// TestPiPlatformRoutingByRequestShape 覆盖计划 E4 的四条端到端判定：
// UA 判定优先，UA 无法判定时回退 body，都判不出时落默认平台。
func TestPiPlatformRoutingByRequestShape(t *testing.T) {
	t.Cleanup(func() { SetPiImpersonationSettings(false, "darwin") })

	win := piRoutingAccount(11, "win32")
	mac := piRoutingAccount(12, "darwin")
	untagged := piRoutingAccount(13, "")
	svc := newPiTestService()
	scheduler := &defaultOpenAIAccountScheduler{service: svc}
	request := OpenAIAccountScheduleRequest{Platform: PlatformOpenAI, RequestedModel: "gpt-5.4-codex"}

	SetPiImpersonationSettings(true, "darwin")

	bodyWithShell := func(shell string) []byte {
		return []byte(`{"model":"gpt-5.4-codex","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context><shell>` + shell + `</shell></environment_context>"}]}]}`)
	}
	routedAccountIDs := func(ctx context.Context, candidates []*Account) []int64 {
		ids := make([]int64, 0, len(candidates))
		for _, account := range candidates {
			if compatible, _ := scheduler.isAccountRequestCompatibleReason(ctx, account, request); compatible {
				ids = append(ids, account.ID)
			}
		}
		return ids
	}

	testCases := []struct {
		name       string
		userAgent  string
		body       []byte
		expectedID []int64
	}{
		{
			name:       "windows codex UA → win32 账号",
			userAgent:  "codex-tui/0.153.4 (Windows 10.0.22631; x86_64) xterm-256color",
			body:       bodyWithShell("zsh"),
			expectedID: []int64{win.ID, untagged.ID},
		},
		{
			name:       "mac codex UA → darwin 账号",
			userAgent:  "codex_cli_rs/0.155.0 (Mac OS X 14.5; arm64)",
			body:       nil,
			expectedID: []int64{mac.ID, untagged.ID},
		},
		{
			name:       "curl + zsh environment_context → darwin 账号",
			userAgent:  "curl/8.7.1",
			body:       bodyWithShell("zsh"),
			expectedID: []int64{mac.ID, untagged.ID},
		},
		{
			name:       "curl + powershell environment_context → win32 账号",
			userAgent:  "curl/8.7.1",
			body:       bodyWithShell("powershell"),
			expectedID: []int64{win.ID, untagged.ID},
		},
		{
			name:       "curl 无线索 → 默认平台 darwin",
			userAgent:  "curl/8.7.1",
			body:       []byte(`{"model":"gpt-5.4-codex","input":[]}`),
			expectedID: []int64{mac.ID, untagged.ID},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			platform := ResolvePiPlatform(svc.cfg, testCase.userAgent, testCase.body)
			ctx := SetPiPlatform(context.Background(), platform)
			require.Equal(t, testCase.expectedID, routedAccountIDs(ctx, []*Account{win, mac, untagged}))
		})
	}
}

// TestPiPlatformRoutingDisabledKeepsWildcard 证明开关隔离：pi 模式关闭时平台标签不参与路由。
func TestPiPlatformRoutingDisabledKeepsWildcard(t *testing.T) {
	t.Cleanup(func() { SetPiImpersonationSettings(false, "darwin") })
	SetPiImpersonationSettings(false, "darwin")

	win := piRoutingAccount(11, "win32")
	mac := piRoutingAccount(12, "darwin")
	ctx := SetPiPlatform(context.Background(), PiPlatformWin32)
	require.True(t, isOpenAICompatibleAccountEligibleForRequest(ctx, mac, PlatformOpenAI, "gpt-5.4-codex", false, OpenAIEndpointCapabilityChatCompletions))
	require.True(t, isOpenAICompatibleAccountEligibleForRequest(ctx, win, PlatformOpenAI, "gpt-5.4-codex", false, OpenAIEndpointCapabilityChatCompletions))
}
