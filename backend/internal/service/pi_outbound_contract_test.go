package service

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// piOutboundTestBody 是一份真实形状的 pi/Codex 请求体：含 <environment_context>
// （用于验证 HTML 不转义与平台判定）、tools、reasoning 与下游特有的 client_metadata。
func piOutboundTestBody() []byte {
	return []byte(`{"model":"gpt-5.4-codex","stream":true,"store":true,` +
		`"instructions":"You are a helpful assistant.","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>\n  <cwd>C:\\Users\\zz\\proj</cwd>\n  <shell>powershell</shell>\n</environment_context>"}]}],` +
		`"text":{"verbosity":"high"},"prompt_cache_key":"cache-1","tool_choice":"auto","parallel_tool_calls":false,` +
		`"service_tier":"priority","tools":[{"type":"function","name":"shell"}],"reasoning":{"effort":"high","summary":"auto"},` +
		`"previous_response_id":"resp_keep_dropped","client_metadata":{"x-codex-window-id":"win-1"}}`)
}

// capturedUpstream 是桩上游收到的请求快照。
type capturedUpstream struct {
	header     http.Header
	body       []byte
	contentLen int64
}

func newCapturedUpstream(t *testing.T) (*httptest.Server, *capturedUpstream) {
	t.Helper()
	captured := &capturedUpstream{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.header = r.Header.Clone()
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		captured.body = body
		captured.contentLen = r.ContentLength
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\"}\n\n"))
	}))
	t.Cleanup(server.Close)
	return server, captured
}

// sendToUpstream 把已构造好的出站请求真发到桩上游，断言桩看到的原始字节。
func sendToUpstream(t *testing.T, req *http.Request, server *httptest.Server) {
	t.Helper()
	target, err := url.Parse(server.URL)
	require.NoError(t, err)
	req.URL.Scheme = target.Scheme
	req.URL.Host = target.Host
	req.Host = target.Host
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func piTestGinContext(t *testing.T, userAgent string, body []byte) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("User-Agent", userAgent)
	return c
}

func piTestOAuthAccount() *Account {
	return &Account{
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"chatgpt_account_id": "chatgpt-acc"},
		Extra:       map[string]any{PiPlatformExtraKey: "win32"},
	}
}

func newPiTestService() *OpenAIGatewayService {
	return &OpenAIGatewayService{cfg: &config.Config{}}
}

// decodeZstdBody 解出 zstd 请求体，证明上游能按 content-encoding: zstd 解码。
func decodeZstdBody(t *testing.T, raw []byte) []byte {
	t.Helper()
	reader, err := zstd.NewReader(nil)
	require.NoError(t, err)
	defer reader.Close()
	decoded, err := reader.DecodeAll(raw, nil)
	require.NoError(t, err)
	return decoded
}

func TestPiOutboundContractOnTheWire(t *testing.T) {
	t.Cleanup(func() { SetPiImpersonationSettings(false, "darwin") })
	SetPiImpersonationSettings(true, "darwin")

	body := piOutboundTestBody()
	// UA 形如 Windows 的 codex 客户端：平台判定必须落到 win32。
	c := piTestGinContext(t, "codex-tui/0.153.4 (Windows 10.0.22631; x86_64) xterm-256color", body)
	svc := newPiTestService()
	account := piTestOAuthAccount()

	// 与 handler 一致：先判定平台写进 ctx，再构造出站请求。
	svc.ApplyPiPlatformToRequest(c, body)
	platform, ok := PiPlatformFromContext(c.Request.Context())
	require.True(t, ok)
	require.Equal(t, PiPlatformWin32, platform)

	req, err := svc.buildUpstreamRequest(c.Request.Context(), c, account, body, "token", true, "cache-1", true)
	require.NoError(t, err)

	// 身份：originator/UA 是 pi，且不发 version、不发任何 x-codex-*。
	require.Equal(t, "pi", req.Header.Get("originator"))
	require.Equal(t, "pi (win32 10.0.22631; x64)", req.Header.Get("user-agent"))
	require.Equal(t, "responses=experimental", req.Header.Get("OpenAI-Beta"))
	require.Equal(t, "text/event-stream", req.Header.Get("accept"))
	require.NotEmpty(t, req.Header.Get("session_id"))
	require.NotEmpty(t, req.Header.Get("x-client-request-id"))
	require.Empty(t, req.Header.Get("version"))
	for name := range req.Header {
		require.NotContains(t, strings.ToLower(name), "x-codex-", "pi 不发 x-codex-* 头")
	}

	server, captured := newCapturedUpstream(t)
	sendToUpstream(t, req, server)

	require.Equal(t, "pi", captured.header.Get("originator"))
	require.Equal(t, "pi (win32 10.0.22631; x64)", captured.header.Get("User-Agent"))
	require.Empty(t, captured.header.Get("version"))
	require.Empty(t, captured.header.Get("x-openai-internal-codex-responses-lite"))
	for name := range captured.header {
		require.NotContains(t, strings.ToLower(name), "x-codex-")
	}

	// 压缩：content-encoding 是 zstd，且是真能解开的 zstd 帧。
	require.Equal(t, "zstd", captured.header.Get("Content-Encoding"))
	require.Equal(t, int64(len(captured.body)), captured.contentLen)
	decoded := decodeZstdBody(t, captured.body)

	// 键序必须是 pi 的字段顺序。
	order := []string{
		`"model"`, `"store"`, `"stream"`, `"instructions"`, `"input"`, `"text"`, `"include"`,
		`"prompt_cache_key"`, `"tool_choice"`, `"parallel_tool_calls"`, `"service_tier"`, `"tools"`, `"reasoning"`,
	}
	cursor := -1
	for _, key := range order {
		index := bytes.Index(decoded, []byte(key))
		require.Greater(t, index, cursor, "字段 %s 的位置必须晚于前一个字段（pi 的键序）", key)
		cursor = index
	}

	// 固定值来自 pi。
	require.Contains(t, string(decoded), `"store":false`)
	require.Contains(t, string(decoded), `"stream":true`)
	require.Contains(t, string(decoded), `"parallel_tool_calls":true`)
	require.Contains(t, string(decoded), `"include":["reasoning.encrypted_content"]`)
	require.Contains(t, string(decoded), `"verbosity":"high"`)

	// 不转义 < > &：否则含 <environment_context> 的请求每个都会被识别出来。
	require.NotContains(t, string(decoded), `\u003c`)
	require.NotContains(t, string(decoded), `\u003e`)
	require.NotContains(t, string(decoded), `\u0026`)
	require.Contains(t, string(decoded), "<environment_context>")

	// 不发 client_metadata，也不发 pi 没有的字段。
	require.NotContains(t, string(decoded), "client_metadata")
	require.NotContains(t, string(decoded), "previous_response_id")

	var payload map[string]any
	require.NoError(t, json.Unmarshal(decoded, &payload))
	require.Equal(t, false, payload["store"])
	require.Equal(t, true, payload["stream"])
	require.Equal(t, "priority", payload["service_tier"])
	require.NotNil(t, payload["tools"])
	require.Equal(t, "high", payload["reasoning"].(map[string]any)["effort"])
}

func TestPiOutboundContractDisabledKeepsCodexIdentity(t *testing.T) {
	t.Cleanup(func() { SetPiImpersonationSettings(false, "darwin") })
	SetPiImpersonationSettings(false, "darwin")

	body := piOutboundTestBody()
	c := piTestGinContext(t, "codex-tui/0.153.4 (Windows 10.0.22631; x86_64) xterm-256color", body)
	svc := newPiTestService()
	account := piTestOAuthAccount()

	svc.ApplyPiPlatformToRequest(c, body)
	_, ok := PiPlatformFromContext(c.Request.Context())
	require.False(t, ok, "pi 模式关闭时不写平台")

	req, err := svc.buildUpstreamRequest(c.Request.Context(), c, account, body, "token", true, "cache-1", true)
	require.NoError(t, err)

	// 对照：pi 模式关闭时必须仍产出旧的 Codex 身份与未压缩请求体。
	require.Equal(t, "codex-tui", req.Header.Get("originator"))
	require.NotEmpty(t, req.Header.Get("version"))
	require.Empty(t, req.Header.Get("Content-Encoding"))
	require.Contains(t, strings.ToLower(req.Header.Get("user-agent")), "codex-tui")

	server, captured := newCapturedUpstream(t)
	sendToUpstream(t, req, server)

	require.NotEqual(t, "pi", captured.header.Get("originator"))
	require.Empty(t, captured.header.Get("Content-Encoding"))
	// 未压缩时桩收到的就是 pi 模式不会有的 Codex 形状（client_metadata 存在）。
	require.Contains(t, string(captured.body), "client_metadata")
}

// TestPiCompressZstdRoundTrip 保证压缩结果可被标准 zstd 解出（上游按 content-encoding: zstd 解码）。
func TestPiCompressZstdRoundTrip(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4-codex","input":[]}`)
	compressed, ok := PiCompressZstd(body)
	require.True(t, ok)
	require.NotEqual(t, body, compressed)
	require.Equal(t, body, decodeZstdBody(t, compressed))
}

// TestPiCompressZstdMatchesLibzstdLevel3 是 E1 的常驻回归：帧必须与
// `zstd -3 --no-check` 逐字节一致，否则线上帧形状与真实 pi 不同。
func TestPiCompressZstdMatchesLibzstdLevel3(t *testing.T) {
	fixture, err := os.ReadFile("testdata/pi_zstd_level3_fixture.json")
	if err != nil {
		t.Skipf("fixture missing: %v", err)
	}
	expected, err := os.ReadFile("testdata/pi_zstd_level3_fixture.expected.zst")
	if err != nil {
		t.Skipf("expected frame missing: %v", err)
	}
	compressed, ok := PiCompressZstd(fixture)
	require.True(t, ok)
	require.Equal(t, expected, compressed)
}
