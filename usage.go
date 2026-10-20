package main

// usage.go implements the UsagePlugin capability (see sdk/pluginabi
// MethodUsageHandle). After every request the host pushes the canonical
// pluginapi.UsageRecord here — it already persists the token counters into its
// own usage manager for the management UI, so this plugin only emits one
// compact forwarding-log line (model, latency, tokens, failure) to stderr,
// which the host captures into its log stream. The record shape mirrors the
// host's wire format: Go field names, nested Detail and Failure objects.

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// usageLogLine is the stable marker that lets users grep forwarding logs out
// of the host log stream.
const usageLogLine = "[oc2cpa-usage]"

// usageLogEvent is the JSON shape of one forwarding-log line. Lowercase keys
// keep the console output human-friendly; counters are always present so
// log scrapers can rely on the schema.
type usageLogEvent struct {
	Time          string `json:"time"`
	RequestID     string `json:"request_id,omitempty"`
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	ResponseModel string `json:"response_model,omitempty"`
	AuthType      string `json:"auth_type,omitempty"`
	APIKey        string `json:"api_key,omitempty"`
	Stream        bool   `json:"stream"`
	Failed        bool   `json:"failed"`
	Status        int    `json:"status,omitempty"`
	LatencyMS     int64  `json:"latency_ms"`
	TTFTMS        int64  `json:"ttft_ms,omitempty"`
	InputTokens   int64  `json:"input_tokens"`
	OutputTokens  int64  `json:"output_tokens"`
	Reasoning     int64  `json:"reasoning_tokens,omitempty"`
	Cached        int64  `json:"cached_tokens,omitempty"`
	TotalTokens   int64  `json:"total_tokens"`
}

// handleUsage renders and prints the forwarding-log line for one record.
// Logging is strictly best-effort: a malformed record or a disabled switch
// simply no-ops, because the host logs a plugin fault if this RPC errors and
// usage telemetry must never break request handling.
func handleUsage(requestJSON []byte) error {
	var record pluginapi.UsageRecord
	if err := json.Unmarshal(requestJSON, &record); err != nil {
		return nil
	}
	// Only log this plugin's own records; the host routes other plugins' usage
	// to their owners, but defensive filtering keeps the log honest if a host
	// ever broadcasts globally.
	if record.Provider != "" && record.Provider != providerName {
		return nil
	}
	if !readSettings().UsageLog {
		return nil
	}
	fmt.Fprintln(os.Stderr, usageLogLine+" "+formatUsageLog(record))
	return nil
}

// formatUsageLog renders one record as a compact single-line JSON event.
func formatUsageLog(record pluginapi.UsageRecord) string {
	event := usageLogEvent{
		Time:          time.Now().Format(time.RFC3339),
		RequestID:     record.RequestID,
		Provider:      record.Provider,
		Model:         record.Model,
		ResponseModel: record.ResponseModel,
		AuthType:      record.AuthType,
		APIKey:        redactKey(record.APIKey),
		Stream:        record.Stream,
		Failed:        record.Failed,
		LatencyMS:     record.Latency.Milliseconds(),
		InputTokens:   record.Detail.InputTokens,
		OutputTokens:  record.Detail.OutputTokens,
		Reasoning:     record.Detail.ReasoningTokens,
		Cached:        record.Detail.CachedTokens + record.Detail.CacheReadTokens,
		TotalTokens:   record.Detail.TotalTokens,
	}
	if record.TTFT > 0 {
		event.TTFTMS = record.TTFT.Milliseconds()
	}
	if record.Failed {
		event.Status = record.Failure.StatusCode
	}
	out, err := json.Marshal(event)
	if err != nil {
		return fmt.Sprintf(`{"provider":%q,"model":%q,"failed":%v}`,
			record.Provider, record.Model, record.Failed)
	}
	return string(out)
}

// redactKey keeps log scrapes from leaking client keys: only a short prefix
// of the API key identifier survives into the usage log.
func redactKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	if len(key) <= 8 {
		return strings.Repeat("*", len(key))
	}
	return key[:8] + "..."
}
