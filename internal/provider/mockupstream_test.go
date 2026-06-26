package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// decodeUnary parses an OpenAI unary chat.completion body into the adapter's
// private oaiResponse for assertions.
func decodeUnary(t *testing.T, body *bufio.Reader) oaiResponse {
	t.Helper()
	var or oaiResponse
	if err := json.NewDecoder(body).Decode(&or); err != nil {
		t.Fatalf("decode unary: %v", err)
	}
	return or
}

// TestMockUpstreamHandler_Unary covers the OpenAI JSON (non-stream) endpoint:
// defaults, custom content/usage, and the configured error status.
func TestMockUpstreamHandler_Unary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		opts        MockUpstreamOptions
		wantStatus  int
		wantContent string
	}{
		{
			name:        "defaults",
			opts:        MockUpstreamOptions{},
			wantStatus:  http.StatusOK,
			wantContent: defaultMockUpstreamContent,
		},
		{
			name:        "custom content and usage",
			opts:        MockUpstreamOptions{Content: "custom", Usage: Usage{TotalTokens: 9}},
			wantStatus:  http.StatusOK,
			wantContent: "custom",
		},
		{
			name:       "configured failure status",
			opts:       MockUpstreamOptions{FailStatus: http.StatusServiceUnavailable},
			wantStatus: http.StatusServiceUnavailable,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(MockUpstreamHandler(tc.opts))
			t.Cleanup(srv.Close)

			body := strings.NewReader(`{"model":"mock","messages":[{"role":"user","content":"hi"}]}`)
			resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", body)
			if err != nil {
				t.Fatalf("POST failed: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			if tc.wantStatus != http.StatusOK {
				// Error path must carry the OpenAI error envelope.
				var env oaiErrorEnvelope
				if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
					t.Fatalf("decode error envelope: %v", err)
				}
				if env.Error.Message == "" {
					t.Fatal("error envelope missing error.message")
				}
				return
			}

			or := decodeUnary(t, bufio.NewReader(resp.Body))
			if len(or.Choices) == 0 {
				t.Fatal("response has no choices")
			}
			if got := or.Choices[0].Message.Content; got != tc.wantContent {
				t.Fatalf("content = %q, want %q", got, tc.wantContent)
			}
			if or.Choices[0].Message.Role != "assistant" {
				t.Fatalf("role = %q, want assistant", or.Choices[0].Message.Role)
			}
			if or.Choices[0].FinishReason != defaultMockFinishReason {
				t.Fatalf("finish_reason = %q, want %q", or.Choices[0].FinishReason, defaultMockFinishReason)
			}
			if or.Model != "mock" {
				t.Fatalf("model = %q, want %q (echoed from request)", or.Model, "mock")
			}
			if tc.opts.Usage.TotalTokens != 0 && or.Usage.TotalTokens != tc.opts.Usage.TotalTokens {
				t.Fatalf("usage.TotalTokens = %d, want %d", or.Usage.TotalTokens, tc.opts.Usage.TotalTokens)
			}
		})
	}
}

// TestMockUpstreamHandler_Stream covers the OpenAI SSE endpoint: it emits the
// requested number of chat.completion.chunk delta events, a final chunk with a
// finish_reason, a trailing usage chunk (when include_usage is requested), and
// the [DONE] sentinel.
func TestMockUpstreamHandler_Stream(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(MockUpstreamHandler(MockUpstreamOptions{
		Content:      "abcd",
		StreamChunks: 4,
		Usage:        Usage{TotalTokens: 4},
	}))
	t.Cleanup(srv.Close)

	body := strings.NewReader(`{"model":"mock","stream":true,"stream_options":{"include_usage":true},"messages":[]}`)
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", body)
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	var deltas int
	var sawFinish, sawUsage, sawSentinel bool
	var content string
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			sawSentinel = true
			continue
		}
		var ev oaiStreamEvent
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			t.Fatalf("decode event %q: %v", payload, err)
		}
		if ev.Usage != nil {
			sawUsage = true
			if ev.Usage.TotalTokens != 4 {
				t.Fatalf("trailing usage.TotalTokens = %d, want 4", ev.Usage.TotalTokens)
			}
		}
		if len(ev.Choices) == 0 {
			continue
		}
		if ev.Choices[0].FinishReason != "" {
			sawFinish = true
			continue
		}
		if d := ev.Choices[0].Delta.Content; d != "" {
			deltas++
			content += d
		}
	}
	if scanner.Err() != nil {
		t.Fatalf("scan error: %v", scanner.Err())
	}
	if deltas != 4 {
		t.Fatalf("delta events = %d, want 4", deltas)
	}
	if content != "abcd" {
		t.Fatalf("reassembled content = %q, want %q", content, "abcd")
	}
	if !sawFinish {
		t.Fatal("missing final chunk with finish_reason")
	}
	if !sawUsage {
		t.Fatal("missing trailing usage chunk")
	}
	if !sawSentinel {
		t.Fatal("missing [DONE] sentinel")
	}
}

// TestMockUpstreamHandler_Latency asserts the configured per-request latency is
// honoured (and bounded against the client context).
func TestMockUpstreamHandler_Latency(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(MockUpstreamHandler(MockUpstreamOptions{Latency: 80 * time.Millisecond}))
	t.Cleanup(srv.Close)

	start := time.Now()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost,
		srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"mock"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()

	if elapsed := time.Since(start); elapsed < 70*time.Millisecond {
		t.Fatalf("response returned in %s, want >= ~80ms (latency not applied)", elapsed)
	}
}
