package service

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
)

// pi 出站伪装（OpenAI OAuth 面）。
//
// 线上契约来自 earendil-works/pi 的 packages/ai/src/api/openai-codex-responses.ts
// 与 utils/pi-user-agent.ts：
//   - originator: pi，User-Agent: `pi (<platform> <release>; <arch>)`（不含版本号）
//   - 不发 version 头，不发任何 x-codex-* 头，不发 body 的 client_metadata
//   - SSE 请求体 zstd(level 3) 压缩并带 content-encoding: zstd；WS 帧不压缩
//   - 走 Node 自带 OpenSSL（ALPN 仅 http/1.1、无 GREASE）
const (
	// PiOriginator 是 pi 出站请求的 originator 头值。
	PiOriginator = "pi"

	// PiPlatformExtraKey 是账号 extra 中标注 pi 平台的键（darwin | win32）。
	PiPlatformExtraKey = "pi_platform"

	// piResponsesLiteHeaderKey 是 Codex 专有的 responses-lite 头，pi 不发送。
	piResponsesLiteHeaderKey = "x-openai-internal-codex-responses-lite"

	// piCodexHeaderPrefix 覆盖所有 Codex 专有头（x-codex-*），pi 一个都不发。
	piCodexHeaderPrefix = "x-codex-"
)

// PiPlatform 是 pi 的进程平台名（node:os 的 platform()）。
type PiPlatform string

const (
	PiPlatformDarwin PiPlatform = "darwin"
	PiPlatformWin32  PiPlatform = "win32"
)

// piPlatformTuple 是 UA 里的 (<release>; <arch>) 部分。
// release 取 Node 语义的固定值：macOS 用 Darwin 内核版本（不是营销版本），
// Windows 用 NT 版本。请求内容里没有内核版本，因此不会被交叉核对。
var piPlatformTuple = map[PiPlatform]struct{ Release, Arch string }{
	PiPlatformDarwin: {"24.5.0", "arm64"},
	PiPlatformWin32:  {"10.0.22631", "x64"},
}

// PiUserAgent 返回 pi 的 User-Agent：`pi (<platform> <release>; <arch>)`。
func PiUserAgent(platform PiPlatform) string {
	tuple, ok := piPlatformTuple[platform]
	if !ok {
		tuple = piPlatformTuple[PiPlatformDarwin]
		platform = PiPlatformDarwin
	}
	return "pi (" + string(platform) + " " + tuple.Release + "; " + tuple.Arch + ")"
}

// NormalizePiPlatform 归一化平台字符串：只接受 darwin/win32（大小写与空白不敏感），
// 其他值（含空串）返回 false。
func NormalizePiPlatform(raw string) (PiPlatform, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case string(PiPlatformDarwin):
		return PiPlatformDarwin, true
	case string(PiPlatformWin32):
		return PiPlatformWin32, true
	default:
		return "", false
	}
}

// piUAMarker 是 UA 里可判定平台的标记与其平台。
// 覆盖真实下游形态：pi 自身、codex-tui / codex_cli_rs / Codex Desktop / opencode。
var piUAMarker = []struct {
	marker   string
	platform PiPlatform
}{
	{"win32", PiPlatformWin32},
	{"win64", PiPlatformWin32},
	{"windows", PiPlatformWin32},
	{"winnt", PiPlatformWin32},
	{"macintosh", PiPlatformDarwin},
	{"mac os", PiPlatformDarwin},
	{"darwin", PiPlatformDarwin},
}

// ParsePiPlatformFromUA 从下游 User-Agent 解析平台；识别不出来返回 ("", false)。
// 取出现位置最靠前的标记，保证同一 UA 里多标记时结果稳定。
func ParsePiPlatformFromUA(ua string) (PiPlatform, bool) {
	lowered := strings.ToLower(ua)
	if strings.TrimSpace(lowered) == "" {
		return "", false
	}
	bestIndex := -1
	var best PiPlatform
	for _, candidate := range piUAMarker {
		index := strings.Index(lowered, candidate.marker)
		if index < 0 {
			continue
		}
		if bestIndex < 0 || index < bestIndex {
			bestIndex = index
			best = candidate.platform
		}
	}
	if bestIndex < 0 {
		return "", false
	}
	return best, true
}

// ParsePiPlatformFromBody 从 body 的 <environment_context> 块解析平台；
// 无该块或线索互相矛盾时返回 ("", false)。
//
// 判定标记（来自 pi / Codex 的 environment_context）：
//   - win32：<shell> 为 powershell/cmd/pwsh，或 <cwd> 是盘符路径（C:\...）
//   - darwin：<shell> 为 zsh/bash/fish，或 <cwd> 以 /Users/ 开头
func ParsePiPlatformFromBody(body []byte) (PiPlatform, bool) {
	if len(body) == 0 {
		return "", false
	}
	win32Hits, darwinHits := 0, 0
	gjson.GetBytes(body, "input").ForEach(func(_, item gjson.Result) bool {
		item.Get("content").ForEach(func(_, part gjson.Result) bool {
			text := part.Get("text").String()
			if text == "" {
				text = part.String()
			}
			if !strings.Contains(text, "<environment_context") {
				return true
			}
			switch normalizePiShell(piEnvironmentTag(text, "shell")) {
			case "powershell", "cmd", "pwsh", "cmd.exe", "powershell.exe":
				win32Hits++
			case "zsh", "bash", "fish", "sh":
				darwinHits++
			}
			if cwd := piEnvironmentTag(text, "cwd"); cwd != "" {
				switch {
				case piLooksLikeWindowsPath(cwd):
					win32Hits++
				case strings.HasPrefix(cwd, "/Users/"), strings.HasPrefix(cwd, "/System/"), strings.HasPrefix(cwd, "/Volumes/"):
					darwinHits++
				}
			}
			return true
		})
		return true
	})
	switch {
	case win32Hits > 0 && darwinHits == 0:
		return PiPlatformWin32, true
	case darwinHits > 0 && win32Hits == 0:
		return PiPlatformDarwin, true
	default:
		return "", false
	}
}

// piEnvironmentTag 取 environment_context 里形如 <tag>value</tag> 的文本内容。
func piEnvironmentTag(text, tag string) string {
	open := "<" + tag + ">"
	start := strings.Index(text, open)
	if start < 0 {
		return ""
	}
	rest := text[start+len(open):]
	end := strings.Index(rest, "</"+tag+">")
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:end])
}

// normalizePiShell 归一化 shell 名：去掉路径与 .exe 后缀，转小写。
func normalizePiShell(shell string) string {
	normalized := strings.ToLower(strings.TrimSpace(shell))
	if index := strings.LastIndexAny(normalized, `/\`); index >= 0 {
		normalized = normalized[index+1:]
	}
	return strings.TrimSuffix(normalized, ".exe")
}

// piLooksLikeWindowsPath 判断 <cwd> 是否为 Windows 盘符路径（C:\ 或 C:/，含 UNC）。
func piLooksLikeWindowsPath(cwd string) bool {
	if strings.HasPrefix(cwd, `\\`) {
		return true
	}
	if len(cwd) < 3 {
		return false
	}
	drive := cwd[0]
	if !((drive >= 'a' && drive <= 'z') || (drive >= 'A' && drive <= 'Z')) {
		return false
	}
	return cwd[1] == ':' && (cwd[2] == '\\' || cwd[2] == '/')
}

// ResolvePiPlatform 分层判定平台：UA -> body -> cfg.DefaultPlatform。
// 永不返回空平台：DefaultPlatform 非法或缺失时落到 piImpersonationDefaultPlatform()（darwin）。
func ResolvePiPlatform(cfg *config.Config, ua string, body []byte) PiPlatform {
	if platform, ok := ParsePiPlatformFromUA(ua); ok {
		return platform
	}
	if platform, ok := ParsePiPlatformFromBody(body); ok {
		return platform
	}
	if cfg != nil {
		if platform, ok := NormalizePiPlatform(cfg.Gateway.PiImpersonation.DefaultPlatform); ok {
			return platform
		}
	}
	return piImpersonationDefaultPlatform()
}

// SetPiPlatform 把判定结果沿 request context 传给账号选择与出站身份改写。
func SetPiPlatform(ctx context.Context, platform PiPlatform) context.Context {
	if ctx == nil {
		return ctx
	}
	if _, ok := NormalizePiPlatform(string(platform)); !ok {
		return ctx
	}
	return context.WithValue(ctx, ctxkey.PiPlatform, platform)
}

// PiPlatformFromContext 读取请求 context 中的平台；未判定过返回 false。
func PiPlatformFromContext(ctx context.Context) (PiPlatform, bool) {
	if ctx == nil {
		return "", false
	}
	platform, ok := ctx.Value(ctxkey.PiPlatform).(PiPlatform)
	if !ok {
		return "", false
	}
	return NormalizePiPlatform(string(platform))
}

// piImpersonationSettings 是进程级配置快照。
// enforceCodexIdentityHeadersWithUA 是所有 Codex 出站路径共用的纯函数收口点，
// 拿不到配置对象，故与 codexIdentityEnforcement 同样由服务构造时发布快照。
type piImpersonationSettings struct {
	enabled         bool
	defaultPlatform PiPlatform
}

var piImpersonation = func() *atomic.Pointer[piImpersonationSettings] {
	pointer := &atomic.Pointer[piImpersonationSettings]{}
	pointer.Store(&piImpersonationSettings{defaultPlatform: PiPlatformDarwin})
	return pointer
}()

// SetPiImpersonationSettings 发布 gateway.pi_impersonation 的进程级快照。
func SetPiImpersonationSettings(enabled bool, defaultPlatform string) {
	platform, ok := NormalizePiPlatform(defaultPlatform)
	if !ok {
		platform = PiPlatformDarwin
	}
	piImpersonation.Store(&piImpersonationSettings{enabled: enabled, defaultPlatform: platform})
}

// PiImpersonationEnabled 返回 pi 出站伪装是否启用。
func PiImpersonationEnabled() bool {
	settings := piImpersonation.Load()
	return settings != nil && settings.enabled
}

// piImpersonationDefaultPlatform 返回快照里的默认平台（快照未发布时为 darwin）。
func piImpersonationDefaultPlatform() PiPlatform {
	settings := piImpersonation.Load()
	if settings == nil || settings.defaultPlatform == "" {
		return PiPlatformDarwin
	}
	return settings.defaultPlatform
}

// PiRequestPlatform 返回本次请求的平台：context 已判定则用之，否则用默认平台。
func PiRequestPlatform(ctx context.Context) PiPlatform {
	if platform, ok := PiPlatformFromContext(ctx); ok {
		return platform
	}
	return piImpersonationDefaultPlatform()
}

// ApplyPiOutboundIdentity 在 pi 模式启用时把出站头改写为 pi 的身份，返回是否已改写。
// 顺序固定：先写 originator 与 User-Agent，再删 Codex 专有头（version / x-codex-*）。
func ApplyPiOutboundIdentity(ctx context.Context, h http.Header) bool {
	if h == nil || !PiImpersonationEnabled() {
		return false
	}
	h.Set("originator", PiOriginator)
	h.Set("user-agent", PiUserAgent(PiRequestPlatform(ctx)))
	h.Del("version")
	deletePiForbiddenHeaders(h)
	return true
}

// deletePiForbiddenHeaders 删除 pi 不发送的 Codex 专有请求头。
func deletePiForbiddenHeaders(h http.Header) {
	for key := range h {
		if strings.HasPrefix(strings.ToLower(key), piCodexHeaderPrefix) {
			h.Del(key)
		}
	}
	h.Del(piResponsesLiteHeaderKey)
}

// piTLSProfile 是 pi 的 TLS 指纹：Node 自带 OpenSSL 的内置 ClientHello 形态
// （tlsfingerprint 的 Built-in Default，Node.js 24.x），ALPN 只声明 http/1.1。
// 显式写出 ALPN 而不是依赖 dialer 默认值，避免默认值变化后悄悄协商到 h2。
var piTLSProfile = &tlsfingerprint.Profile{
	Name:          "pi built-in (Node.js 24.x, HTTP/1.1)",
	ALPNProtocols: []string{"http/1.1"},
}

// PiUpstreamTLSProfile 返回 pi 出站应使用的 TLS 指纹 profile。
func PiUpstreamTLSProfile() *tlsfingerprint.Profile {
	return piTLSProfile
}

// piImpersonationEnabled 返回本服务是否启用 pi 出站伪装。
// 读进程级快照而不是 s.cfg：判定候选资格的函数拿不到服务句柄，两条路径必须同源，
// 否则会出现「出站按 pi 改写、选号仍按通配」这类只有线上才暴露的分裂。
func (s *OpenAIGatewayService) piImpersonationEnabled() bool {
	return PiImpersonationEnabled()
}

// piPlatformVetoReason 返回账号在 pi 平台维度上的否决原因；兼容（或 pi 模式未启用、
// 请求未判定平台、账号未标注标签）时返回空串。
//
// 放在候选资格门里而不是候选列表过滤里：粘性会话命中、previous_response_id 复用与
// guardian 母账号回退都可能绕过候选列表，只有资格门能保证「一个凭据只服务一个平台」。
// 未标注标签的账号按通配放行（过渡期兼容，见计划 A6）。
func piPlatformVetoReason(ctx context.Context, account *Account) string {
	if !PiImpersonationEnabled() || account == nil {
		return ""
	}
	platform, ok := PiPlatformFromContext(ctx)
	if !ok {
		return ""
	}
	tag := account.PiPlatform()
	if tag == "" || tag == string(platform) {
		return ""
	}
	return "pi_platform_mismatch"
}

// ApplyPiPlatformToRequest 在 pi 模式启用时按下游 UA 与请求体判定本次请求平台，
// 写回 c.Request 的 context：账号选择（平台 → 凭据）与出站身份（pi 的 UA）都读这一个值。
// 已判定过的请求不重复判定（同一请求链路只判一次）。
func (s *OpenAIGatewayService) ApplyPiPlatformToRequest(c *gin.Context, body []byte) {
	if s == nil || c == nil || c.Request == nil || !s.piImpersonationEnabled() {
		return
	}
	ctx := c.Request.Context()
	if _, ok := PiPlatformFromContext(ctx); ok {
		return
	}
	platform := ResolvePiPlatform(s.cfg, c.Request.UserAgent(), body)
	c.Request = c.Request.WithContext(SetPiPlatform(ctx, platform))
}

// piImpersonationActiveFor 报告本次出站是否要走 pi 契约：pi 模式开启，且账号确实在走
// ChatGPT 内部接口的 OpenAI 路径（写 pi 身份与请求体到非 Codex 协议上游没有意义）。
func (s *OpenAIGatewayService) piImpersonationActiveFor(account *Account) bool {
	return s.piImpersonationEnabled() && account != nil &&
		account.Platform == PlatformOpenAI && account.UsesOpenAICodexProtocol()
}
