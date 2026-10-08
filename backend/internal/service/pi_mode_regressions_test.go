package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// TestPiModeWSHeadersCarryNoCodexHeaders 覆盖「收口之后再补回 x-codex-*」这类顺序缺陷：
// WS 握手在 pi 模式下不得出现任何 x-codex-*（含 routing hint）。
func TestPiModeWSHeadersCarryNoCodexHeaders(t *testing.T) {
	t.Cleanup(func() { SetPiImpersonationSettings(false, "darwin") })
	SetPiImpersonationSettings(true, "win32")

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	svc := &OpenAIGatewayService{}
	account := &Account{
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"chatgpt_account_id": "chatgpt-acc"},
		Extra:       map[string]any{PiPlatformExtraKey: "win32"},
	}
	headers, _, err := svc.buildOpenAIWSHeaders(
		context.Background(),
		c,
		account,
		"test-token",
		OpenAIWSProtocolDecision{Transport: OpenAIUpstreamTransportResponsesWebsocketV2},
		true,
		"",
		"",
		"",
		"gpt-5.6-codex",
		"priority",
	)
	require.NoError(t, err)
	require.Equal(t, PiOriginator, headers.Get("originator"))
	for name := range headers {
		require.NotContains(t, strings.ToLower(name), "x-codex-", "WS 握手在 pi 模式下不得带 x-codex-*")
	}
}

// TestPiModeCompactBodyStaysUnreshaped 覆盖 /responses/compact：pi 不调该端点，它的请求体
// 经过 compact 归一化（上游会拒绝 tool_choice 等字段）。pi 模式必须只改身份，不改请求体、
// 不压缩——否则会把 compact 请求打成 400 unknown_parameter。
func TestPiModeCompactBodyStaysUnreshaped(t *testing.T) {
	t.Cleanup(func() { SetPiImpersonationSettings(false, "darwin") })
	SetPiImpersonationSettings(true, "darwin")

	const compactBody = `{"model":"gpt-5.4-codex","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"summarize"}]}],"instructions":"You are a helpful assistant."}`
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewReader([]byte(compactBody)))
	c.Request.Header.Set("User-Agent", "codex-tui/0.153.4 (Mac OS X 14.5; arm64)")

	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	account := piTestOAuthAccount()
	svc.ApplyPiPlatformToRequest(c, []byte(compactBody))

	req, err := svc.buildUpstreamRequest(c.Request.Context(), c, account, []byte(compactBody), "token", false, "", true)
	require.NoError(t, err)

	// 身份仍是 pi。
	require.Equal(t, PiOriginator, req.Header.Get("originator"))
	require.Equal(t, "pi (darwin 24.5.0; arm64)", req.Header.Get("user-agent"))
	require.Empty(t, req.Header.Get("version"))

	// 请求体未被重塑、未被压缩。
	require.Empty(t, req.Header.Get("Content-Encoding"))
	body, err := req.GetBody()
	require.NoError(t, err)
	defer func() { _ = body.Close() }()
	raw, err := io.ReadAll(body)
	require.NoError(t, err)
	require.JSONEq(t, compactBody, string(raw))
	require.NotContains(t, string(raw), `"tool_choice"`)
	require.NotContains(t, string(raw), `"client_metadata"`)
}
