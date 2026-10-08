package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// pi 请求体形状（见 pi_identity.go 顶部契约说明）。
//
// 为什么必须重建而不是原地改：现有路径用 sjson 在原 body 上改键，保留的是**下游**的键序；
// pi 的请求体键序来自它自己的 builder，两者不同。重建同时保证 Go 的 JSON 编码器
// 不把 < > & 转义成 \u003c 等（pi 是 JSON.stringify，不转义）。
const (
	piDefaultInstructions      = "You are a helpful assistant."
	piDefaultTextVerbosity     = "low"
	piDefaultToolChoice        = "auto"
	piIncludeReasoningWithText = "reasoning.encrypted_content"
)

// piText 是 pi 的 text 字段。
type piText struct {
	Verbosity string `json:"verbosity,omitempty"`
}

// piReasoning 是 pi 的 reasoning 字段。
type piReasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

// piRequestBody 的字段声明顺序就是出站 JSON 的键序，必须与 pi 的 builder 一致：
// model, store, stream, instructions, input, text, include, prompt_cache_key,
// tool_choice, parallel_tool_calls, temperature, service_tier, tools, reasoning。
type piRequestBody struct {
	Model             string       `json:"model"`
	Store             bool         `json:"store"`
	Stream            bool         `json:"stream"`
	Instructions      string       `json:"instructions"`
	Input             []any        `json:"input"`
	Text              *piText      `json:"text,omitempty"`
	Include           []string     `json:"include"`
	PromptCacheKey    string       `json:"prompt_cache_key,omitempty"`
	ToolChoice        string       `json:"tool_choice"`
	ParallelToolCalls bool         `json:"parallel_tool_calls"`
	Temperature       *float64     `json:"temperature,omitempty"`
	ServiceTier       *string      `json:"service_tier,omitempty"`
	Tools             []any        `json:"tools,omitempty"`
	Reasoning         *piReasoning `json:"reasoning,omitempty"`
}

// BuildPiRequestBody 把下游 Responses body 归一为 pi 的字段集与键序。
//
// 固定值来自 pi：store=false、stream=true、include=["reasoning.encrypted_content"]、
// parallel_tool_calls=true；instructions 缺省为 "You are a helpful assistant."，
// text.verbosity 缺省 low，tool_choice 缺省 auto。
// input 原样透传：条目内部形状由下游客户端产生，不属于本次伪装范围。
//
// 重要语义：pi 的 SSE builder 从不发送 previous_response_id（pi 客户端在本地保留完整
// 上下文，每个回合都把 transcript 放进 input），因此这里也不输出该字段。凡是依赖
// previous_response_id 续接、而不自带完整 transcript 的下游请求，在 pi 模式下都会
// 变成一次新的响应——这是「出站与 pi 一致」的必然代价，不是缺陷。
// 会话连续性由 sub2api 自己的 session_id / conversation_id（已隔离）保证。
func BuildPiRequestBody(body []byte) ([]byte, error) {
	decoded := map[string]any{}
	if err := decodeOpenAIJSONUseNumber(body, &decoded); err != nil {
		return nil, fmt.Errorf("decode responses body for pi: %w", err)
	}
	model := piStringValue(decoded["model"])
	if model == "" {
		return nil, fmt.Errorf("pi request body: model is required")
	}
	request := piRequestBody{
		Model:             model,
		Store:             false,
		Stream:            true,
		Instructions:      piStringValue(decoded["instructions"]),
		Input:             piArrayValue(decoded["input"]),
		Text:              piTextValue(decoded["text"]),
		Include:           []string{piIncludeReasoningWithText},
		PromptCacheKey:    piStringValue(decoded["prompt_cache_key"]),
		ToolChoice:        piStringValue(decoded["tool_choice"]),
		ParallelToolCalls: true,
		Temperature:       piFloatValue(decoded["temperature"]),
		ServiceTier:       piOptionalStringValue(decoded["service_tier"]),
		Tools:             piArrayValue(decoded["tools"]),
		Reasoning:         piReasoningValue(decoded["reasoning"]),
	}
	if request.Instructions == "" {
		request.Instructions = piDefaultInstructions
	}
	if request.ToolChoice == "" {
		request.ToolChoice = piDefaultToolChoice
	}
	// marshalOpenAIUpstreamJSON 用 SetEscapeHTML(false)，与 pi 的 JSON.stringify 一致。
	return marshalOpenAIUpstreamJSON(request)
}

func piStringValue(value any) string {
	text, _ := value.(string)
	return text
}

// piOptionalStringValue 只在值非空时返回指针：omitempty 对 *string 无效，
// 空字符串会被编码成 ""，而 pi 在未设置 service_tier 时整个键都不出现。
func piOptionalStringValue(value any) *string {
	text := piStringValue(value)
	if text == "" {
		return nil
	}
	return &text
}

func piFloatValue(value any) *float64 {
	switch typed := value.(type) {
	case json.Number:
		if parsed, err := typed.Float64(); err == nil {
			return &parsed
		}
	case float64:
		return &typed
	case int:
		converted := float64(typed)
		return &converted
	}
	return nil
}

func piArrayValue(value any) []any {
	if items, ok := value.([]any); ok {
		return items
	}
	return []any{}
}

func piTextValue(value any) *piText {
	text := &piText{Verbosity: piDefaultTextVerbosity}
	object, ok := value.(map[string]any)
	if !ok {
		return text
	}
	if verbosity := piStringValue(object["verbosity"]); verbosity != "" {
		text.Verbosity = verbosity
	}
	return text
}

func piReasoningValue(value any) *piReasoning {
	object, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	reasoning := &piReasoning{
		Effort:  piStringValue(object["effort"]),
		Summary: piStringValue(object["summary"]),
	}
	if reasoning.Effort == "" && reasoning.Summary == "" {
		// pi 不会发空对象：没有 effort/summary 时整个 reasoning 键都不出现。
		return nil
	}
	return reasoning
}

// applyPiOutboundBody 把 pi 模式下的出站请求体换成 pi 的形状，并在 SSE 面按 pi 的方式压缩。
//
// 压缩只在 SSE 面：pi 的 WS 帧发未压缩的 JSON，而 /responses/compact 是 unary JSON，
// pi 不走这条端点。
//
// 同时记录 (account_id, pi_platform)：平台与凭据的 1:1 不变式靠这条日志巡检
// （按账号聚合 distinct pi_platform 必须恒为 1），因此是 Info 级。
func (s *OpenAIGatewayService) applyPiOutboundBody(req *http.Request, c *gin.Context, account *Account, body []byte) error {
	// /responses/compact 是 unary JSON 端点，pi 从不调用它，且它的请求体由
	// normalizeOpenAICompactRequestBody 收敛过：上游对 compact 会拒绝 tool_choice 等字段
	// （400 unknown_parameter）。因此这里只保留 pi 身份，请求体与压缩都不动。
	if isOpenAIResponsesCompactPath(c) {
		return nil
	}
	reshaped, err := BuildPiRequestBody(body)
	if err != nil {
		return err
	}
	content := reshaped
	if compressed, ok := PiCompressZstd(reshaped); ok {
		content = compressed
		req.Header.Set("content-encoding", "zstd")
	}
	req.Body = io.NopCloser(bytes.NewReader(content))
	req.ContentLength = int64(len(content))
	saved := content
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(saved)), nil }
	// pi 的 SSE/WS 头里 session-id 与 x-client-request-id 是同一个值
	// （openai-codex-responses.ts 的 buildSSEHeaders）。会话标识已由上游路径隔离，
	// 这里只做同值复制；客户端自己带了就不覆盖。
	if sessionID := strings.TrimSpace(req.Header.Get("session_id")); sessionID != "" &&
		strings.TrimSpace(req.Header.Get("x-client-request-id")) == "" {
		req.Header.Set("x-client-request-id", sessionID)
	}
	accountID := int64(0)
	if account != nil {
		accountID = account.ID
	}
	logger.FromContext(req.Context()).Info("pi_outbound_request",
		zap.Int64("account_id", accountID),
		zap.String("pi_platform", string(PiRequestPlatform(req.Context()))),
		zap.Int("raw_bytes", len(reshaped)),
		zap.Int("sent_bytes", len(content)),
	)
	return nil
}
