package service

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// newPiTestHeader 构造一份 Codex 形态的出站头，用于断言 pi 改写是否彻底。
func newPiTestHeader() http.Header {
	headers := http.Header{}
	headers.Set("originator", "codex-tui")
	headers.Set("user-agent", "codex-tui/0.146.0 (Ubuntu 22.4.0; x86_64) xterm-256color")
	headers.Set("version", "0.146.0")
	headers.Set("OpenAI-Beta", "responses=experimental")
	headers.Set("session_id", "sess-1")
	headers.Set("x-client-request-id", "sess-1")
	headers.Set("x-codex-window-id", "win-1")
	headers.Set("x-codex-installation-id", "inst-1")
	headers.Set("x-codex-turn-metadata", `{"turn_id":"t1"}`)
	headers.Set("x-codex-beta-features", "remote_compaction_v2")
	headers.Set("x-openai-internal-codex-responses-lite", "1")
	return headers
}

func piTestConfig(enabled bool, defaultPlatform string) *config.Config {
	cfg := &config.Config{}
	cfg.Gateway.PiImpersonation.Enabled = enabled
	cfg.Gateway.PiImpersonation.DefaultPlatform = defaultPlatform
	return cfg
}

func TestPiUserAgent(t *testing.T) {
	require.Equal(t, "pi (darwin 24.5.0; arm64)", PiUserAgent(PiPlatformDarwin))
	require.Equal(t, "pi (win32 10.0.22631; x64)", PiUserAgent(PiPlatformWin32))
	// 未知平台回退 darwin，避免产出 "pi (  ; )" 这种一眼假的 UA。
	require.Equal(t, "pi (darwin 24.5.0; arm64)", PiUserAgent(PiPlatform("freebsd")))
}

func TestParsePiPlatformFromUA(t *testing.T) {
	testCases := []struct {
		name     string
		agent    string
		platform PiPlatform
		ok       bool
	}{
		{"pi win32", "pi (win32 10.0.22631; x64)", PiPlatformWin32, true},
		{"pi darwin", "pi (darwin 24.5.0; arm64)", PiPlatformDarwin, true},
		{"pi browser", "pi (browser)", "", false},
		{"codex-tui windows", "codex-tui/0.153.4 (Windows 10.0.22631; x86_64) xterm-256color", PiPlatformWin32, true},
		{"codex-tui mac", "codex-tui/0.153.4 (Mac OS X 14.5; arm64) iTerm", PiPlatformDarwin, true},
		{"codex_cli mac os", "codex_cli_rs/0.155.0 (Mac OS X 14.5; arm64)", PiPlatformDarwin, true},
		{"codex_cli windows", "codex_cli_rs/0.155.0 (Windows 11; x86_64)", PiPlatformWin32, true},
		{"desktop macintosh", "Codex Desktop/0.155.0 (Macintosh; Intel Mac OS X 14.5)", PiPlatformDarwin, true},
		{"desktop windows", "Codex Desktop/0.155.0 (Windows NT 10.0; Win64; x64)", PiPlatformWin32, true},
		{"opencode darwin", "opencode/1.18.32 (darwin 24.5.0; arm64)", PiPlatformDarwin, true},
		{"opencode win32", "opencode/1.18.32 (win32 10.0.22631; x64)", PiPlatformWin32, true},
		{"curl", "curl/8.7.1", "", false},
		{"go http client", "Go-http-client/1.1", "", false},
		{"openai python", "OpenAI/Python 1.0.0", "", false},
		{"node", "node", "", false},
		{"empty", "", "", false},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			platform, ok := ParsePiPlatformFromUA(testCase.agent)
			require.Equal(t, testCase.ok, ok)
			require.Equal(t, testCase.platform, platform)
		})
	}
}

func TestParsePiPlatformFromBody(t *testing.T) {
	// wrap 用 json.Marshal 生成 text 字段，避免手写 JSON 里 Windows 路径的反斜杠
	// 变成非法转义（那样测的就不是判定逻辑，而是夹具本身）。
	wrap := func(text string) []byte {
		encoded, err := json.Marshal(text)
		require.NoError(t, err)
		return []byte(`{"model":"gpt-5.4","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":` + string(encoded) + `}]}]}`)
	}
	wrapPlainContent := func(text string) []byte {
		encoded, err := json.Marshal(text)
		require.NoError(t, err)
		return []byte(`{"model":"gpt-5.4","input":[{"type":"message","role":"user","content":` + string(encoded) + `}]}`)
	}
	environment := func(inner string) string {
		return "<environment_context>\n  " + inner + "\n</environment_context>"
	}
	testCases := []struct {
		name     string
		body     []byte
		platform PiPlatform
		ok       bool
	}{
		{"powershell", wrap(environment("<shell>powershell</shell>")), PiPlatformWin32, true},
		{"cmd", wrap(environment("<shell>cmd.exe</shell>")), PiPlatformWin32, true},
		{"pwsh", wrap(environment("<shell>pwsh</shell>")), PiPlatformWin32, true},
		{"windows cwd", wrap(environment(`<cwd>C:\Users\a\project</cwd>`)), PiPlatformWin32, true},
		{"windows cwd forward slash", wrap(environment(`<cwd>C:/Users/a/project</cwd>`)), PiPlatformWin32, true},
		{"zsh", wrap(environment("<shell>zsh</shell>")), PiPlatformDarwin, true},
		{"bash", wrap(environment("<shell>bash</shell>")), PiPlatformDarwin, true},
		{"fish", wrap(environment("<shell>fish</shell>")), PiPlatformDarwin, true},
		{"darwin cwd", wrap(environment("<cwd>/Users/a/project</cwd>")), PiPlatformDarwin, true},
		{"no environment block", wrap("hello"), "", false},
		{"empty body", nil, "", false},
		{"plain content item", wrapPlainContent(environment("<shell>zsh</shell>")), PiPlatformDarwin, true},
		// 同一块里两种平台线索同时出现时判不出，交给下一层。
		{"conflicting", wrap(environment(`<shell>zsh</shell><cwd>C:\Users\a</cwd>`)), "", false},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			platform, ok := ParsePiPlatformFromBody(testCase.body)
			require.Equal(t, testCase.ok, ok)
			require.Equal(t, testCase.platform, platform)
		})
	}
}

func TestResolvePiPlatformLayers(t *testing.T) {
	cfg := piTestConfig(true, "win32")
	body := []byte(`{"model":"gpt-5.4","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context><shell>zsh</shell></environment_context>"}]}]}`)

	// UA 优先：UA 判 win32、body 判 darwin 时以 UA 为准。
	require.Equal(t, PiPlatformWin32, ResolvePiPlatform(cfg, "codex-tui/0.153.4 (Windows 10.0.22631; x86_64)", body))
	// UA 判不出时用 body。
	require.Equal(t, PiPlatformDarwin, ResolvePiPlatform(cfg, "curl/8.7.1", body))
	// 都判不出时用配置默认平台。
	require.Equal(t, PiPlatformWin32, ResolvePiPlatform(cfg, "curl/8.7.1", nil))
	// 配置非法时落到 darwin，绝不返回空平台。
	require.Equal(t, PiPlatformDarwin, ResolvePiPlatform(piTestConfig(true, "plan9"), "curl/8.7.1", nil))
	require.Equal(t, PiPlatformDarwin, ResolvePiPlatform(nil, "", nil))
}

func TestSetPiPlatformContextRoundTrip(t *testing.T) {
	ctx := SetPiPlatform(context.Background(), PiPlatformWin32)
	platform, ok := PiPlatformFromContext(ctx)
	require.True(t, ok)
	require.Equal(t, PiPlatformWin32, platform)

	_, ok = PiPlatformFromContext(context.Background())
	require.False(t, ok)

	// 非法平台不写入，避免下游读到 "freebsd" 这类值。
	ctx = SetPiPlatform(context.Background(), PiPlatform("freebsd"))
	_, ok = PiPlatformFromContext(ctx)
	require.False(t, ok)

	// 未判定过平台时回退默认平台（快照）。
	SetPiImpersonationSettings(true, "win32")
	t.Cleanup(func() { SetPiImpersonationSettings(false, "darwin") })
	require.Equal(t, PiPlatformWin32, PiRequestPlatform(context.Background()))
}

func TestPiImpersonationSettingsSnapshot(t *testing.T) {
	t.Cleanup(func() { SetPiImpersonationSettings(false, "darwin") })

	SetPiImpersonationSettings(false, "win32")
	require.False(t, PiImpersonationEnabled())
	require.Equal(t, PiPlatformWin32, piImpersonationDefaultPlatform())

	SetPiImpersonationSettings(true, "")
	require.True(t, PiImpersonationEnabled())
	require.Equal(t, PiPlatformDarwin, piImpersonationDefaultPlatform())

	SetPiImpersonationSettings(true, "nonsense")
	require.Equal(t, PiPlatformDarwin, piImpersonationDefaultPlatform())
}

func TestApplyPiOutboundIdentity(t *testing.T) {
	t.Cleanup(func() { SetPiImpersonationSettings(false, "darwin") })

	header := func() http.Header {
		return newPiTestHeader()
	}

	// 关闭时不改写，Codex 身份原样保留。
	SetPiImpersonationSettings(false, "darwin")
	headers := header()
	require.False(t, ApplyPiOutboundIdentity(context.Background(), headers))
	require.Equal(t, "codex-tui", headers.Get("originator"))
	require.Equal(t, "0.146.0", headers.Get("version"))

	// 开启时整体换成 pi：originator/UA 改写，version 与所有 x-codex-* 删除。
	SetPiImpersonationSettings(true, "darwin")
	headers = header()
	ctx := SetPiPlatform(context.Background(), PiPlatformWin32)
	require.True(t, ApplyPiOutboundIdentity(ctx, headers))
	require.Equal(t, "pi", headers.Get("originator"))
	require.Equal(t, "pi (win32 10.0.22631; x64)", headers.Get("user-agent"))
	require.Empty(t, headers.Get("version"))
	for name := range headers {
		require.NotContains(t, strings.ToLower(name), "x-codex-")
	}
	require.Empty(t, headers.Get("x-openai-internal-codex-responses-lite"))
	// pi 会发的头必须保留。
	require.Equal(t, "sess-1", headers.Get("session_id"))
	require.Equal(t, "sess-1", headers.Get("x-client-request-id"))
	require.Equal(t, "responses=experimental", headers.Get("OpenAI-Beta"))
}

func TestPiPlatformVetoReason(t *testing.T) {
	t.Cleanup(func() { SetPiImpersonationSettings(false, "darwin") })

	win := &Account{Platform: PlatformOpenAI, Extra: map[string]any{PiPlatformExtraKey: "win32"}}
	mac := &Account{Platform: PlatformOpenAI, Extra: map[string]any{PiPlatformExtraKey: "darwin"}}
	untagged := &Account{Platform: PlatformOpenAI}
	badTag := &Account{Platform: PlatformOpenAI, Extra: map[string]any{PiPlatformExtraKey: "plan9"}}

	SetPiImpersonationSettings(false, "darwin")
	require.Empty(t, piPlatformVetoReason(SetPiPlatform(context.Background(), PiPlatformWin32), mac))

	SetPiImpersonationSettings(true, "darwin")
	winCtx := SetPiPlatform(context.Background(), PiPlatformWin32)
	macCtx := SetPiPlatform(context.Background(), PiPlatformDarwin)

	require.Empty(t, piPlatformVetoReason(winCtx, win))
	require.Equal(t, "pi_platform_mismatch", piPlatformVetoReason(winCtx, mac))
	require.Empty(t, piPlatformVetoReason(macCtx, mac))
	require.Equal(t, "pi_platform_mismatch", piPlatformVetoReason(macCtx, win))
	// 未标注与标注错误都按通配处理（过渡期兼容）。
	require.Empty(t, piPlatformVetoReason(winCtx, untagged))
	require.Empty(t, piPlatformVetoReason(winCtx, badTag))
	require.Empty(t, piPlatformVetoReason(winCtx, nil))
	// 请求未判定平台时不否决。
	require.Empty(t, piPlatformVetoReason(context.Background(), mac))
}

func TestAccountPiPlatform(t *testing.T) {
	require.Equal(t, "win32", (&Account{Extra: map[string]any{PiPlatformExtraKey: "win32"}}).PiPlatform())
	require.Equal(t, "darwin", (&Account{Extra: map[string]any{PiPlatformExtraKey: " Darwin "}}).PiPlatform())
	require.Empty(t, (&Account{Extra: map[string]any{PiPlatformExtraKey: "plan9"}}).PiPlatform())
	require.Empty(t, (&Account{Extra: map[string]any{PiPlatformExtraKey: 32}}).PiPlatform())
	require.Empty(t, (&Account{}).PiPlatform())
	require.Empty(t, (*Account)(nil).PiPlatform())
}

func TestPiUpstreamTLSProfileIsHTTP11WithoutGREASE(t *testing.T) {
	profile := PiUpstreamTLSProfile()
	require.NotNil(t, profile)
	require.Equal(t, []string{"http/1.1"}, profile.ALPNProtocols)
	require.False(t, profile.EnableGREASE)
	// Extensions 为空表示使用 dialer 的内置 Node.js 24.x ClientHello。
	require.Empty(t, profile.Extensions)
}
