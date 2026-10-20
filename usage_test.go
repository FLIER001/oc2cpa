package main

// usage_test.go covers the forwarding-log rendering and the UsagePlugin
// dispatch gate. All verification is offline; no host process is needed.

import (
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestFormatUsageLogStreamingRecord(t *testing.T) {
	record := pluginapi.UsageRecord{
		Provider:      providerName,
		RequestID:     "b5db448b-3d6d-495c-9c71-f925b68926cb",
		Model:         "glm-5.3-flash-free",
		ResponseModel: "glm-5.3-flash-free-2026-05-13",
		AuthType:      "oc2cpa",
		APIKey:        "sk-mE279SjZ",
		Stream:        true,
		RequestedAt:   time.Unix(1700000000, 0),
		Latency:       1500 * time.Millisecond,
		TTFT:          240 * time.Millisecond,
		Detail: pluginapi.UsageDetail{
			InputTokens:  241,
			OutputTokens: 11,
			TotalTokens:  252,
		},
	}
	line := formatUsageLog(record)
	body := strings.TrimPrefix(line, usageLogLine+" ")
	if !strings.Contains(body, `"provider":"oc2cpa"`) {
		t.Fatalf("provider missing: %s", body)
	}
	if !strings.Contains(body, `"latency_ms":1500`) {
		t.Fatalf("latency missing: %s", body)
	}
	if !strings.Contains(body, `"ttft_ms":240`) {
		t.Fatalf("ttft missing: %s", body)
	}
	if !strings.Contains(body, `"input_tokens":241`) || !strings.Contains(body, `"total_tokens":252`) {
		t.Fatalf("token counters missing: %s", body)
	}
	if strings.Contains(body, `"failed":true`) || strings.Contains(body, `"status"`) {
		t.Fatalf("success record must not carry failure fields: %s", body)
	}
	if strings.Contains(body, "sk-mE279SjZ") {
		t.Fatalf("api key must be redacted: %s", body)
	}
	if strings.Contains(body, "\n") {
		t.Fatalf("log line must stay single-line: %s", body)
	}
}

func TestFormatUsageLogFailureRecord(t *testing.T) {
	record := pluginapi.UsageRecord{
		Provider: providerName,
		Model:    "big-pickle",
		Stream:   false,
		Failed:   true,
		Failure: pluginapi.UsageFailure{
			StatusCode: 500,
			Body:       `{"error":"upstream"}`,
		},
		Detail: pluginapi.UsageDetail{InputTokens: 3, OutputTokens: 0, TotalTokens: 3},
	}
	line := formatUsageLog(record)
	if !strings.Contains(line, `"failed":true`) || !strings.Contains(line, `"status":500`) {
		t.Fatalf("failure fields missing: %s", line)
	}
}

func TestHandleUsageMethodDispatch(t *testing.T) {
	// usage.handle must resolve to the OK envelope so the host never fuses
	// the plugin over usage telemetry.
	raw := []byte(`{"Provider":"` + providerName + `","Stream":true}`)
	out, err := handleMethod(pluginabi.MethodUsageHandle, raw)
	if err != nil {
		t.Fatalf("handleMethod usage.handle: %v", err)
	}
	if !strings.Contains(string(out), `"ok":true`) {
		t.Fatalf("expected OK envelope, got: %s", out)
	}
}
