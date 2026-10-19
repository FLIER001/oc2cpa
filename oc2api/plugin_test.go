package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// ---------------------------------------------------------------------------
// session id shape
// ---------------------------------------------------------------------------

var sessionIDRe = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)

func TestOcSessionIDShape(t *testing.T) {
	for range 32 {
		id := ocSessionID(1_760_000_000_000)
		if !sessionIDRe.MatchString(id) {
			t.Fatalf("session id shape rejected: %q", id)
		}
	}
}

func TestOcSessionIDHexPartDerivesFromTimestamp(t *testing.T) {
	// value = ~(timestamp*0x1000 + 1) & low48 → the first 12 hex chars.
	value := uint64(1_760_000_000)*0x1000 + 1
	low48 := (^value) & 0xFFFF_FFFF_FFFF
	want := ""
	for index := 0; index < 6; index++ {
		want += hex.EncodeToString([]byte{byte(low48 >> (40 - 8*index))})
	}
	id := ocSessionID(1_760_000_000)
	if !strings.HasPrefix(id, "ses_"+want) {
		t.Fatalf("hex part mismatch: %q want prefix ses_%s", id, want)
	}
}

func TestSessionRegistryReusesAndExpires(t *testing.T) {
	registry := newSessionRegistry()
	first := registry.get("holder")
	second := registry.get("holder")
	if first != second {
		t.Fatalf("session should be reused within TTL: %q vs %q", first, second)
	}
	entry := registry.sessions["holder"]
	entry.start = registry.now().Add(-31 * time.Minute)
	registry.sessions["holder"] = entry
	third := registry.get("holder")
	if third == first {
		t.Fatal("session should rotate after TTL")
	}
}

// ---------------------------------------------------------------------------
// model catalog
// ---------------------------------------------------------------------------

func TestIsAllowedModelID(t *testing.T) {
	allowed := []string{"big-pickle", "glm-5.3-flash-free", "deepseek-v4.1-flash-free", "big-pickle-free"}
	for _, id := range allowed {
		if !isAllowedModelID(id) {
			t.Errorf("%q should be allowed", id)
		}
	}
	rejected := []string{"gpt-4o", "mini", "free-", "freex"}
	for _, id := range rejected {
		if isAllowedModelID(id) {
			t.Errorf("%q should be rejected", id)
		}
	}
}

func TestBuildModelInfoDefaults(t *testing.T) {
	info := buildModelInfo("big-pickle")
	if info.ContextLength != 200_000 {
		t.Fatalf("big-pickle context length: %d", info.ContextLength)
	}
	if len(info.SupportedInputModalities) != 1 || info.SupportedInputModalities[0] != "text" {
		t.Fatalf("big-pickle must be text-only: %v", info.SupportedInputModalities)
	}
	image := buildModelInfo("glm-5.3-flash-free")
	if len(image.SupportedInputModalities) != 2 {
		t.Fatalf("glm image modalities: %v", image.SupportedInputModalities)
	}
	unknown := buildModelInfo("newly-discovered-free")
	if unknown.ID != "newly-discovered-free" || unknown.ContextLength == 0 {
		t.Fatalf("unknown model should still get a usable record: %+v", unknown)
	}
}

// ---------------------------------------------------------------------------
// request adapter
// ---------------------------------------------------------------------------

func TestWithGateToolsAliasCollision(t *testing.T) {
	// Different original names can sanitize to the same alias base; the
	// collision loop suffixes with _1, _2… ("a b" and "a.b" both become a_b).
	phase := withGateTools(false, []any{
		map[string]any{"type": "function", "function": map[string]any{"name": "a b"}},
		map[string]any{"type": "function", "function": map[string]any{"name": "a.b"}},
	})
	aliases := []string{}
	for _, raw := range phase.tools {
		name := functionName(asObject(raw))
		if strings.HasPrefix(name, "oc2api_a_b") {
			aliases = append(aliases, name)
		}
	}
	if len(aliases) != 2 || aliases[0] == aliases[1] {
		t.Fatalf("colliding names need distinct aliases: %v", aliases)
	}
	if aliases[0] != "oc2api_a_b" {
		t.Fatalf("first alias must be the plain base: %v", aliases)
	}
	if !strings.HasSuffix(aliases[1], "_1") {
		t.Fatalf("collision suffix expected: %v", aliases)
	}
}

func TestWithGateToolsSkipsMalformed(t *testing.T) {
	phase := withGateTools(false, []any{
		"not-an-object",                    // dropped
		map[string]any{"type": "function"}, // name-less: passed through unrenamed
		map[string]any{"type": "function", "function": map[string]any{"name": "read"}},
	})
	if len(phase.tools) != 3 { // nameless + read + gate
		t.Fatalf("string tools dropped, objects kept: %v", phase.tools)
	}
	if functionName(asObject(phase.tools[0])) != "" {
		t.Fatalf("name-less tool must keep its shape: %v", phase.tools[0])
	}
	if functionName(asObject(phase.tools[1])) != "oc2api_read" {
		t.Fatalf("named tool must be renamed: %v", phase.tools[1])
	}
	if functionName(asObject(phase.tools[2])) != gateToolName {
		t.Fatalf("gate tool must be last: %v", phase.tools[2])
	}
}

func TestBuildUpstreamRequestCore(t *testing.T) {
	raw := `{"model":"glm-5.3-flash-free","stream":true,"messages":[{"role":"user","content":"hi"}],"__proto__":{"x":1},"constructor":1,"prototype":2}`
	request, err := buildUpstreamRequest(context.Background(), []byte(raw), "glm-5.3-flash-free", ocAuthView{Holder: "t"})
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(request.Body, &body); err != nil {
		t.Fatal(err)
	}
	for _, key := range dangerousKeys {
		if _, has := body[key]; has {
			t.Fatalf("dangerous key %q must be stripped", key)
		}
	}
	if body["stream"] != true {
		t.Fatalf("stream must be forced true, got %v", body["stream"])
	}
	if _, has := body["stream_options"].(map[string]any); !has {
		t.Fatal("stream_options.include_usage must be requested")
	}
	if _, has := body["max_tokens"].(float64); !has {
		t.Fatal("max_tokens must default to a number")
	}
	if request.SessionID == "" {
		t.Fatal("session id must be attached")
	}
	if !request.ThinkingEnabled {
		t.Fatal("reasoning defaults to enabled")
	}
	if !request.StreamRequested {
		t.Fatal("stream detection")
	}
}

func TestBuildUpstreamRequestRejectsNonWhitelistedModel(t *testing.T) {
	_, err := buildUpstreamRequest(context.Background(), []byte(`{"messages":[{"role":"user","content":"hi"}]}`), "gpt-5", ocAuthView{})
	if err == nil {
		t.Fatal("non-whitelisted model must be rejected")
	}
}

func TestBuildUpstreamRequestReasoningNone(t *testing.T) {
	request, err := buildUpstreamRequest(context.Background(), []byte(`{"model":"big-pickle","messages":[{"role":"user","content":"x"}],"reasoning_effort":"none"}`), "big-pickle", ocAuthView{})
	if err != nil {
		t.Fatal(err)
	}
	if request.ThinkingEnabled {
		t.Fatal("reasoning_effort none must disable thinking")
	}
	var body map[string]any
	_ = json.Unmarshal(request.Body, &body)
	// JS parity: "none" is forwarded as-is so the upstream drops thinking.
	if body["reasoning_effort"] != "none" {
		t.Fatalf("reasoning_effort none must stay upstream: %v", body["reasoning_effort"])
	}
}

func TestRenameMessageToolsStripsNameOnToolRole(t *testing.T) {
	message := map[string]any{
		"role":    "tool",
		"name":    "read",
		"content": "ok",
	}
	renamed := renameMessageTools(message, map[string]string{})
	object := asObject(renamed)
	if _, has := object["name"]; has {
		t.Fatalf("role:tool name must be stripped unconditionally: %v", object)
	}
}

func TestRenameMessageToolsRenamesToolCallNames(t *testing.T) {
	message := map[string]any{
		"role": "assistant",
		"tool_calls": []any{
			map[string]any{
				"id":   "call_1",
				"type": "function",
				"function": map[string]any{
					"name":      "read",
					"arguments": "{}",
				},
			},
		},
	}
	renamed := renameMessageTools(message, map[string]string{"read": "oc2api_read"})
	object := asObject(renamed)
	calls := fromAnySlice(object["tool_calls"])
	fn := asObject(asObject(calls[0])["function"])
	if fn["name"] != "oc2api_read" {
		t.Fatalf("tool_call name must be renamed: %v", fn)
	}
}

func TestRenameToolChoice(t *testing.T) {
	// JS parity: a legacy string tool_choice is NOT renamed (renameToolCall
	// reads call?.function?.name, undefined on strings).
	renamed := renameToolChoice("read", map[string]string{"read": "oc2api_read"})
	if renamed != "read" {
		t.Fatalf("legacy string tool_choice must stay unchanged: %v", renamed)
	}
	object := map[string]any{"type": "function", "function": map[string]any{"name": "read"}}
	out := renameToolChoice(object, map[string]string{"read": "oc2api_read"})
	converted := asObject(out)
	if asObject(converted["function"])["name"] != "oc2api_read" {
		t.Fatalf("tool_choice object rename: %v", converted)
	}
}

func fromAnySlice(values any) []map[string]any {
	slice, _ := values.([]any)
	out := make([]map[string]any, 0, len(slice))
	for _, item := range slice {
		out = append(out, asObject(item))
	}
	return out
}

// ---------------------------------------------------------------------------
// SSE response pipeline: non-stream aggregation
// ---------------------------------------------------------------------------

// fakeBody streams pre-chunked byte slices; each Read returns at most one
// chunk to force the parser through its line-assembly paths.
type fakeBody struct {
	chunks [][]byte
	index  int
	buf    []byte
}

func (f *fakeBody) Read(p []byte) (int, error) {
	if len(f.buf) == 0 {
		if f.index >= len(f.chunks) {
			return 0, io.EOF
		}
		f.buf = f.chunks[f.index]
		f.index++
	}
	n := copy(p, f.buf)
	f.buf = f.buf[n:]
	return n, nil
}

func (f *fakeBody) Close() error { return nil }

func sse(data string) string {
	return "data: " + data + "\n\n"
}

func aggregateWith(t *testing.T, ctx *streamCtx, chunks ...[]byte) (map[string]any, error) {
	t.Helper()
	body := &fakeBody{chunks: chunks}
	return aggregateUpstreamJSON(body, ctx)
}

func aggregate(t *testing.T, chunks ...string) (map[string]any, error) {
	t.Helper()
	parts := make([][]byte, 0, len(chunks))
	for _, chunk := range chunks {
		parts = append(parts, []byte(chunk))
	}
	return aggregateWith(t, baseTestCtx(), parts...)
}

func TestAggregateSimpleText(t *testing.T) {
	result, err := aggregate(t,
		sse(`{"id":"c1","object":"chat.completion.chunk","created":1700000000,"model":"x","choices":[{"index":0,"delta":{"role":"assistant","content":"He"},"finish_reason":null}]}`),
		sse(`{"choices":[{"index":0,"delta":{"content":"llo"},"finish_reason":null}]}`),
		sse(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`),
		"data: [DONE]\n\n",
	)
	if err != nil {
		t.Fatal(err)
	}
	choice := asObject(result["choices"].([]any)[0])
	message := asObject(choice["message"])
	if message["content"] != "Hello" {
		t.Fatalf("content: %v", message["content"])
	}
	if message["role"] != "assistant" {
		t.Fatalf("role: %v", message["role"])
	}
	if choice["finish_reason"] != "stop" {
		t.Fatalf("finish_reason: %v", choice["finish_reason"])
	}
	if result["model"] != "big-pickle" {
		t.Fatalf("model must be forced to the client model: %v", result["model"])
	}
	if result["id"] != "c1" {
		t.Fatalf("id: %v", result["id"])
	}
}

func TestAggregateToolCallsByIndex(t *testing.T) {
	// The client's read tool travels upstream as the oc2api_read alias and is
	// restored on the response side.
	ctx := streamCtxFrom(&upstreamRequest{
		Model:             "big-pickle",
		RequestID:         "req_alias",
		ThinkingEnabled:   true,
		ChoiceCount:       1,
		Aliases:           map[string]string{"oc2api_read": "read"},
		ReservedToolNames: []string{"read"},
		ClientToolNames:   []string{"oc2api_read"},
	})
	result, err := aggregateWith(t, ctx,
		[]byte(sse(`{"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_a","function":{"name":"oc2api_read","arguments":""}}]}}]}`)),
		[]byte(sse(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"p\":1}"}}]}}]}`)),
		[]byte(sse(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)),
		[]byte("data: [DONE]\n\n"),
	)
	if err != nil {
		t.Fatal(err)
	}
	choice := asObject(result["choices"].([]any)[0])
	message := asObject(choice["message"])
	call := asObject(message["tool_calls"].([]any)[0])
	if call["id"] != "call_a" {
		t.Fatalf("id: %v", call["id"])
	}
	fn := asObject(call["function"])
	if fn["name"] != "read" || fn["arguments"] != "{\"p\":1}" {
		t.Fatalf("name/arguments: %v %v", fn["name"], fn["arguments"])
	}
	// finish stop → tool_calls rewrite (upstream mislabels ~40% of tool answers)
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish stop must rewrite to tool_calls: %v", choice["finish_reason"])
	}
}

func TestAggregateToolCallsByIdContinuation(t *testing.T) {
	result, err := aggregate(t,
		sse(`{"choices":[{"index":0,"delta":{"tool_calls":[{"id":"call_x","function":{"name":"toolA","arguments":"{\"a"}}]}}]}`),
		sse(`{"choices":[{"index":0,"delta":{"tool_calls":[{"id":"call_x","function":{"arguments":":1}"}}]}}]}`),
		sse(`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`),
		"data: [DONE]\n\n",
	)
	if err != nil {
		t.Fatal(err)
	}
	message := asObject(asObject(result["choices"].([]any)[0])["message"])
	call := asObject(message["tool_calls"].([]any)[0])
	fn := asObject(call["function"])
	if fn["arguments"] != "{\"a:1}" {
		t.Fatalf("arguments must merge by id: %q", fn["arguments"])
	}
}

func TestAggregateAutoSlotContinuationWithoutId(t *testing.T) {
	// Interleaved parallel calls without ids: a bare-argument frame continues
	// the currently accumulating call.
	result, err := aggregate(t,
		sse(`{"choices":[{"index":0,"delta":{"tool_calls":[{"id":"c1","function":{"name":"t1","arguments":"1"}}]}}]}`),
		sse(`{"choices":[{"index":0,"delta":{"tool_calls":[{"id":"c2","function":{"name":"t2","arguments":"2"}}]}}]}`),
		sse(`{"choices":[{"index":0,"delta":{"tool_calls":[{"function":{"arguments":"x"}}]}}]}`),
		sse(`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`),
		"data: [DONE]\n\n",
	)
	if err != nil {
		t.Fatal(err)
	}
	message := asObject(asObject(result["choices"].([]any)[0])["message"])
	calls := message["tool_calls"].([]any)
	if len(calls) != 2 {
		t.Fatalf("expect 2 calls, got %v", calls)
	}
	last := asObject(calls[1])
	if asObject(last["function"])["arguments"] != "2x" {
		t.Fatalf("continuation must append to the latest call: %v", last)
	}
}

func TestAggregateTrailingUsageArrives(t *testing.T) {
	result, err := aggregate(t,
		sse(`{"choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`),
		"data: [DONE]\n\n",
		`data: {"choices":[],"usage":{"total_tokens":42}}`+"\n\n",
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, has := result["usage"]; !has {
		t.Fatalf("usage after [DONE] must be captured: %v", result)
	}
}

func TestAggregateWithoutDoneButAllFinished(t *testing.T) {
	result, err := aggregate(t,
		sse(`{"choices":[{"index":0,"delta":{"content":"x"},"finish_reason":"stop"}]}`),
	)
	if err != nil {
		t.Fatalf("all-finished without [DONE] is complete: %v", err)
	}
	if asObject(result["choices"].([]any)[0])["finish_reason"] != "stop" {
		t.Fatal("finish kept")
	}
}

func TestAggregateIncompleteFails(t *testing.T) {
	_, err := aggregate(t,
		sse(`{"choices":[{"index":0,"delta":{"content":"x"},"finish_reason":null}]}`),
	)
	if err == nil {
		t.Fatal("stream without a completion signal must fail")
	}
}

func TestAggregateJSONErrorBodyWith200(t *testing.T) {
	// Free tier can serve HTTP 200 with a raw JSON error body.
	body := &fakeBody{chunks: [][]byte{[]byte(`{"error":{"message":"tool not supported"},"usage":null}`)}}
	_, err := aggregateUpstreamJSON(body, baseTestCtx())
	if err == nil || !strings.Contains(err.Error(), "tool not supported") {
		t.Fatalf("200-with-JSON-error must fail the stream: %v", err)
	}
}

// ---------------------------------------------------------------------------
// SSE response pipeline: streaming writer
// ---------------------------------------------------------------------------

func TestStreamSimple(t *testing.T) {
	ctx := baseTestCtx()
	encoder := newStreamEncoder(ctx)
	first, err := encoder.encode(parseFrame(t, `{"id":"c1","created":1700000000,"choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":null}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(first) == 0 {
		t.Fatal("content frame must be emitted immediately")
	}
	// Native chat-completions entry: the host writer adds "data: ...\n\n"
	// itself, so the plugin must emit bare JSON objects.
	if bytes.HasPrefix(first, []byte("data: ")) {
		t.Fatalf("chat-completions chunks must be bare JSON: %q", first)
	}
	var sent map[string]any
	if err := json.Unmarshal(first, &sent); err != nil {
		t.Fatalf("bare payload must be valid JSON: %v", err)
	}
	if sent["model"] != "big-pickle" {
		t.Fatalf("chunk must carry the client model: %v", sent["model"])
	}
	second, err := encoder.encode(parseFrame(t, `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(second) == 0 {
		t.Fatal("finish frame must be emitted")
	}
	done, err := encoder.finish()
	if err != nil {
		t.Fatal(err)
	}
	if len(done) != 0 {
		t.Fatalf("the host appends [DONE]; plugin must not: %q", done)
	}
}

func TestStreamSSEFramedForNonChatEntry(t *testing.T) {
	for path, want := range map[string]bool{
		"/v1/chat/completions":  false,
		"/v1/completions":       false,
		"/v1/responses":         true,
		"/v1/messages":          true,
		"":                      true,
		"/v1/chat/completions/": true,
	} {
		if got := clientNeedsSSEFrame(map[string]any{"request_path": path}); got != want {
			t.Fatalf("request_path %q: framing = %v, want %v", path, got, want)
		}
	}
	ctx := baseTestCtx()
	ctx.sseFrame = true
	encoder := newStreamEncoder(ctx)
	chunk, err := encoder.encode(parseFrame(t, `{"choices":[{"index":0,"delta":{"content":"hi"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(chunk), "data: ") || strings.HasSuffix(string(chunk), "\n\n") {
		t.Fatalf("cross-format chunks must be data-framed lines: %q", chunk)
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(string(chunk), "data: ")), &map[string]any{}); err != nil {
		t.Fatalf("framed payload must be valid JSON: %v", err)
	}
}

func TestStreamToolIndexNormalization(t *testing.T) {
	ctx := baseTestCtx()
	encoder := newStreamEncoder(ctx)
	chunk1, err := encoder.encode(parseFrame(t, `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":5,"id":"a","function":{"name":"t","arguments":""}}]}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(chunk1) == 0 {
		t.Fatal("tool call frame carries content and must be emitted")
	}
	var first map[string]any
	_ = json.Unmarshal([]byte(strings.TrimPrefix(string(chunk1), "data: ")), &first)
	calls := asObject(asObject(first["choices"].([]any)[0])["delta"])["tool_calls"].([]any)
	call := asObject(calls[0])
	// JS parity: a valid, unoccupied upstream index is kept as-is.
	if index, ok := asInt(call["index"]); !ok || index != 5 {
		t.Fatalf("unoccupied upstream index must be kept: %v", call["index"])
	}
	// A second, index-less fragment lands on the next free slot (0 here).
	encoder2 := newStreamEncoder(newToolCallCtx())
	chunk2, err := encoder2.encode(parseFrame(t, `{"choices":[{"index":0,"delta":{"tool_calls":[{"id":"z","function":{"name":"t","arguments":""}}]}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var second map[string]any
	_ = json.Unmarshal([]byte(strings.TrimPrefix(string(chunk2), "data: ")), &second)
	call2 := asObject(asObject(asObject(second["choices"].([]any)[0])["delta"])["tool_calls"].([]any)[0])
	if index, ok := asInt(call2["index"]); !ok || index != 0 {
		t.Fatalf("index-less fragment must get the free slot 0: %v", call2["index"])
	}
}

func newToolCallCtx() *streamCtx {
	return baseTestCtx()
}

func TestStreamRewritesFinishStopWithTools(t *testing.T) {
	ctx := baseTestCtx()
	encoder := newStreamEncoder(ctx)
	if _, err := encoder.encode(parseFrame(t, `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"t","arguments":"{}"}}]}}]}`)); err != nil {
		t.Fatal(err)
	}
	second, err := encoder.encode(parseFrame(t, `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var finish map[string]any
	_ = json.Unmarshal([]byte(strings.TrimPrefix(string(second), "data: ")), &finish)
	choice := asObject(finish["choices"].([]any)[0])
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("stream finish stop must rewrite to tool_calls: %v", choice["finish_reason"])
	}
}

func TestStreamBuffersPendingFrames(t *testing.T) {
	ctx := baseTestCtx()
	encoder := newStreamEncoder(ctx)
	first, err := encoder.encode(parseFrame(t, `{"id":"c1","choices":[{"index":0,"delta":{"role":"assistant"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 0 {
		t.Fatalf("role-only frame must be buffered: %q", first)
	}
	second, err := encoder.encode(parseFrame(t, `{"choices":[{"index":0,"delta":{"content":"hi"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(second), "\"role\":\"assistant\"") {
		t.Fatalf("buffered role frame must flush with the first content frame: %q", second)
	}
}

func TestHostProxyCaptureAndFallback(t *testing.T) {
	hostProxyMu.Lock()
	old := hostProxyURL
	hostProxyMu.Unlock()
	defer func() {
		hostProxyMu.Lock()
		hostProxyURL = old
		hostProxyMu.Unlock()
	}()

	noteHostProxy(pluginapi.HostConfigSummary{})
	if got := readHostProxy(); got != "" {
		t.Fatalf("empty host summary must not set a proxy: %q", got)
	}
	noteHostProxy(pluginapi.HostConfigSummary{ProxyURL: " socks5://127.0.0.1:1080 "})
	if got := readHostProxy(); got != "socks5://127.0.0.1:1080" {
		t.Fatalf("expected trimmed proxy, got %q", got)
	}
}

func TestOpenUpstreamChatRetriesTransientFailures(t *testing.T) {
	oldBackoff := upstreamRetryBackoff
	upstreamRetryBackoff = []time.Duration{0, 0}
	defer func() { upstreamRetryBackoff = oldBackoff }()

	var attempts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	settingsMu.Lock()
	old := settings
	settings = pluginSettings{UpstreamBaseURL: srv.URL, ConnectTimeoutMS: 60000, DynamicModels: false}
	settingsMu.Unlock()
	defer func() {
		settingsMu.Lock()
		settings = old
		settingsMu.Unlock()
	}()

	built, err := buildUpstreamRequest(context.Background(), []byte(`{"model":"big-pickle","stream":true,"messages":[{"role":"user","content":"hi"}]}`), "big-pickle", ocAuthView{})
	if err != nil {
		t.Fatal(err)
	}
	response, err := openUpstreamChat(built)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("want 200 after retries, got %d", response.StatusCode)
	}
	if attempts.Load() != 3 {
		t.Fatalf("want 3 attempts before success, got %d", attempts.Load())
	}
}

func parseFrame(t *testing.T, data string) map[string]any {
	t.Helper()
	out, ok := safeParseAnyToAny(data).(map[string]any)
	if !ok {
		t.Fatalf("bad test JSON: %s", data)
	}
	return out
}

// ---------------------------------------------------------------------------
// parse layer: name restoration & reserved tools
// ---------------------------------------------------------------------------

func TestRestoreFunctionNameDeferredWhilePrefix(t *testing.T) {
	ctx := streamCtxFrom(&upstreamRequest{
		Model:             "big-pickle",
		RequestID:         "r",
		Aliases:           map[string]string{"oc2api_read": "read", "oc2api_read_1": "readdoc"},
		ReservedToolNames: []string{"read"},
		ClientToolNames:   []string{"oc2api_read", "oc2api_read_1"},
	})
	parser := newEventParser(&fakeBody{}, ctx)
	book := newToolCallBook()
	target := toolCallTarget(book, map[string]any{})
	frame, err := parser.restoreFunctionName(map[string]any{"name": "oc2api_read"}, target)
	if err != nil {
		t.Fatal(err)
	}
	if _, has := frame["name"]; has {
		t.Fatalf("name must be deferred while a longer candidate exists: %v", frame)
	}
	if !target.pendingName {
		t.Fatal("pendingName must be set")
	}
	frame, err = parser.restoreFunctionName(map[string]any{"arguments": "{}"}, target)
	if err != nil {
		t.Fatal(err)
	}
	if frame["name"] != "read" {
		t.Fatalf("restored name must be `read`: %v", frame)
	}
}

func TestRestoreFunctionNameAliasRestored(t *testing.T) {
	ctx := streamCtxFrom(&upstreamRequest{
		Model:             "big-pickle",
		RequestID:         "r",
		Aliases:           map[string]string{"oc2api_lookup": "lookup"},
		ReservedToolNames: []string{"read"},
		ClientToolNames:   []string{"oc2api_lookup"},
	})
	parser := newEventParser(&fakeBody{}, ctx)
	book := newToolCallBook()
	target := toolCallTarget(book, map[string]any{})
	frame, err := parser.restoreFunctionName(map[string]any{"name": "oc2api_lookup"}, target)
	if err != nil {
		t.Fatal(err)
	}
	if frame["name"] != "lookup" {
		t.Fatalf("alias must restore the original name: %v", frame)
	}
}

func TestRestoreFunctionNameReservedCallFails(t *testing.T) {
	ctx := streamCtxFrom(&upstreamRequest{
		Model:             "big-pickle",
		RequestID:         "r",
		Aliases:           map[string]string{},
		ReservedToolNames: []string{"read"},
		ClientToolNames:   []string{"write"},
	})
	parser := newEventParser(&fakeBody{}, ctx)
	book := newToolCallBook()
	target := toolCallTarget(book, map[string]any{})
	if _, err := parser.restoreFunctionName(map[string]any{"name": "read", "arguments": "{}"}, target); err == nil {
		t.Fatal("calling the reserved gate tool must fail the stream")
	}
}

func TestReservedToolNameInStreamFrames(t *testing.T) {
	// The upstream emits the reserved gate tool name directly → stream error.
	ctx := streamCtxFrom(&upstreamRequest{
		Model:             "big-pickle",
		RequestID:         "r",
		Aliases:           map[string]string{"oc2api_write": "write"},
		ReservedToolNames: []string{"read"},
		ClientToolNames:   []string{"oc2api_write"},
	})
	body := &fakeBody{chunks: [][]byte{
		[]byte(`data: {"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"a","function":{"name":"read","arguments":"{}"}}]}}]}` + "\n\n"),
	}}
	_, err := aggregateUpstreamJSON(body, ctx)
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("reserved tool in stream must fail: %v", err)
	}
}

// ---------------------------------------------------------------------------
// toolsDisabled enforcement
// ---------------------------------------------------------------------------

func TestToolsDisabledRejectsToolCallFrames(t *testing.T) {
	base := &upstreamRequest{
		Model:         "big-pickle",
		RequestID:     "r",
		ToolsDisabled: true,
		Aliases:       map[string]string{},
	}
	body := &fakeBody{chunks: [][]byte{
		[]byte(`data: {"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"a","function":{"name":"whatever","arguments":"{}"}}]}}]}` + "\n\n"),
	}}
	_, err := aggregateUpstreamJSON(body, streamCtxFrom(base))
	if err == nil || !strings.Contains(err.Error(), "tools are disabled") {
		t.Fatalf("tool call under tool_choice:none must be rejected: %v", err)
	}
}

func TestToolsDisabledRejectsFinishReasonToolCalls(t *testing.T) {
	base := &upstreamRequest{
		Model:         "big-pickle",
		RequestID:     "r",
		ToolsDisabled: true,
		Aliases:       map[string]string{},
	}
	body := &fakeBody{chunks: [][]byte{
		[]byte(`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n"),
	}}
	_, err := aggregateUpstreamJSON(body, streamCtxFrom(base))
	if err == nil {
		t.Fatal("tool_calls finish under disabled tools must be rejected")
	}
}

// ---------------------------------------------------------------------------
// normalizeDelta / reasoning
// ---------------------------------------------------------------------------

func TestNormalizeDeltaReasoning(t *testing.T) {
	enabled := normalizeDelta(map[string]any{"reasoning": "thinking", "content": "x"}, true)
	if enabled["reasoning_content"] != "thinking" || enabled["content"] != "x" {
		t.Fatalf("reasoning normalization (enabled): %v", enabled)
	}
	if _, has := enabled["reasoning"]; has {
		t.Fatal("private reasoning field must be removed")
	}
	disabled := normalizeDelta(map[string]any{"reasoning": "thinking", "content": "x"}, false)
	if _, has := disabled["reasoning_content"]; has {
		t.Fatal("reasoning must be dropped when reasoning_effort=none")
	}
	mirrored := normalizeDelta(map[string]any{"reasoning_content": "b", "reasoning_details": "same"}, true)
	if mirrored["reasoning_content"] != "b" {
		t.Fatalf("reasoning_content passthrough: %v", mirrored)
	}
	if _, has := mirrored["reasoning_details"]; has {
		t.Fatal("reasoning_details mirror must be removed")
	}
	emptyReasoning := normalizeDelta(map[string]any{"reasoning": ""}, true)
	if _, has := emptyReasoning["reasoning_content"]; has {
		t.Fatal("empty reasoning must not be added")
	}
}

func TestNormalizeDeltaToolCallsFilter(t *testing.T) {
	out := normalizeDelta(map[string]any{"tool_calls": []any{1, map[string]any{"index": 0}}, "name": "x", "cost": 2}, true)
	calls := out["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("non-object tool_calls must be filtered: %v", calls)
	}
	if _, has := out["name"]; has {
		t.Fatal("delta.name must be dropped")
	}
	if _, has := out["cost"]; has {
		t.Fatal("delta.cost must be dropped")
	}
	// tool_calls: null stays (JS semantics: the outer `!= null` check skips it)
	kept := normalizeDelta(map[string]any{"tool_calls": nil}, true)
	if _, has := kept["tool_calls"]; !has {
		t.Fatal("tool_calls:null must be kept as-is (JS parity)")
	}
	removed := normalizeDelta(map[string]any{"tool_calls": 5}, true)
	if _, has := removed["tool_calls"]; has {
		t.Fatal("non-array non-null tool_calls must be deleted")
	}
}

// ---------------------------------------------------------------------------
// misc helpers
// ---------------------------------------------------------------------------

func TestNormalizeFinish(t *testing.T) {
	if normalizeFinish("stop", 1) != "tool_calls" {
		t.Fatal("stop with tool calls must rewrite")
	}
	if normalizeFinish("tool_calls", 0) != "tool_calls" {
		t.Fatal("explicit tool_calls stays")
	}
	if normalizeFinish("stop", 0) != "stop" {
		t.Fatal("plain stop stays")
	}
}

func TestUpstreamErrorOfShapes(t *testing.T) {
	if _, ok := upstreamErrorOf(map[string]any{"choices": []any{}}, false); ok {
		t.Fatal("normal chunk must not be a failure")
	}
	if message, ok := upstreamErrorOf(parseFrame(t, `{"error":{"message":"rate limited"}}`), false); !ok || message != "rate limited" {
		t.Fatalf("error object message: %v %v", message, ok)
	}
	if message, ok := upstreamErrorOf(parseFrame(t, `{"type":"error","message":"oops"}`), false); !ok || message != "oops" {
		t.Fatalf("typed error message: %v %v", message, ok)
	}
	if message, ok := upstreamErrorOf(parseFrame(t, `{"error":"auth"}`), false); !ok || message != "auth" {
		t.Fatalf("string error: %v %v", message, ok)
	}
	if message, ok := upstreamErrorOf(parseFrame(t, `{}`), true); !ok || message == "" {
		t.Fatalf("forced parse must produce a fallback: %v %v", message, ok)
	}
}

func TestMapUpstreamFailure(t *testing.T) {
	if f := mapUpstreamFailure(429, "quota"); f.Type != "rate_limit_error" || f.Status != 429 {
		t.Fatalf("429 mapping: %+v", f)
	}
	if f := mapUpstreamFailure(401, "bad key"); f.Type != "authentication_error" {
		t.Fatalf("401 mapping: %+v", f)
	}
	if f := mapUpstreamFailure(400, "bad request"); f.Type != "invalid_request_error" || f.Status != 400 {
		t.Fatalf("400 mapping: %+v", f)
	}
	if f := mapUpstreamFailure(500, "boom"); f.Type != "upstream_error" {
		t.Fatalf("5xx mapping: %+v", f)
	}
	if f := mapUpstreamFailure(429, ""); f.Message != "Upstream returned HTTP 429 (free model rate limit)" {
		t.Fatalf("default message: %q", f.Message)
	}
}

func TestMarshalErrorEnvelopeFrom(t *testing.T) {
	raw := marshalErrorEnvelopeFrom(&ocError{Code: "rate_limit_error", Message: "m", HTTPStatus: 429})
	var env struct {
		OK    bool           `json:"ok"`
		Error *envelopeError `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if env.OK || env.Error == nil || env.Error.Code != "rate_limit_error" || env.Error.HTTPStatus != 429 {
		t.Fatalf("envelope: %s", raw)
	}
}

func TestFilterExcluded(t *testing.T) {
	request := pluginapi.StaticModelRequest{
		Host: pluginapi.HostConfigSummary{
			ExcludedModels: map[string][]string{"oc2api": {"big-pickle"}},
		},
	}
	models := filterExcluded(staticCatalog(), request.Host)
	for _, model := range models {
		if model.ID == "big-pickle" {
			t.Fatal("excluded model must be filtered")
		}
	}
	if len(models) != len(staticCatalog())-1 {
		t.Fatalf("unexpected model count: %d", len(models))
	}
}

// ---------------------------------------------------------------------------

func baseTestCtx() *streamCtx {
	return streamCtxFrom(&upstreamRequest{
		Model:             "big-pickle",
		RequestID:         "req_test_1",
		ThinkingEnabled:   true,
		ChoiceCount:       1,
		Aliases:           map[string]string{},
		ReservedToolNames: []string{"read"},
		ClientToolNames:   []string{},
	})
}
