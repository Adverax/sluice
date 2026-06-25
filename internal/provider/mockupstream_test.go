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

// TestMockUpstreamHandler_Unary covers the JSON (non-stream) endpoint: defaults,
// custom content/usage, and the configured error status.
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
				return
			}
			var wr wireResponse
			if err := json.NewDecoder(resp.Body).Decode(&wr); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if wr.Content != tc.wantContent {
				t.Fatalf("content = %q, want %q", wr.Content, tc.wantContent)
			}
			if wr.Model != "mock" {
				t.Fatalf("model = %q, want %q (echoed from request)", wr.Model, "mock")
			}
			if tc.opts.Usage.TotalTokens != 0 && wr.Usage.TotalTokens != tc.opts.Usage.TotalTokens {
				t.Fatalf("usage.TotalTokens = %d, want %d", wr.Usage.TotalTokens, tc.opts.Usage.TotalTokens)
			}
		})
	}
}

// TestMockUpstreamHandler_Stream covers the SSE endpoint: it emits the requested
// number of delta events, a terminal Done event with usage, and a [DONE]
// sentinel.
func TestMockUpstreamHandler_Stream(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(MockUpstreamHandler(MockUpstreamOptions{
		Content:      "abcd",
		StreamChunks: 4,
		Usage:        Usage{TotalTokens: 4},
	}))
	t.Cleanup(srv.Close)

	body := strings.NewReader(`{"model":"mock","stream":true,"messages":[]}`)
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", body)
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	var deltas int
	var sawDone, sawSentinel bool
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
		var ev wireStreamEvent
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			t.Fatalf("decode event %q: %v", payload, err)
		}
		if ev.Done {
			sawDone = true
			if ev.Usage.TotalTokens != 4 {
				t.Fatalf("terminal usage.TotalTokens = %d, want 4", ev.Usage.TotalTokens)
			}
			continue
		}
		deltas++
		content += ev.Content
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
	if !sawDone {
		t.Fatal("missing terminal Done event")
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
