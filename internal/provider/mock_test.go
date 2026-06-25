package provider

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Compile-time assertion that *Mock satisfies the Provider port (ADR-0009).
var _ Provider = (*Mock)(nil)

func sampleResponse() Response {
	return Response{
		Model:        "mock-1",
		Content:      "hello world",
		FinishReason: "stop",
		Usage:        Usage{PromptTokens: 5, CompletionTokens: 2, TotalTokens: 7},
	}
}

func TestMockProvider_Infer_ReturnsConfiguredResponse(t *testing.T) {
	t.Parallel()
	want := sampleResponse()
	m := New(WithResponse(want))

	got, err := m.Infer(context.Background(), Request{Model: "mock-1"})
	if err != nil {
		t.Fatalf("Infer returned error: %v", err)
	}
	if got.Content != want.Content || got.Model != want.Model {
		t.Errorf("Infer response = %+v, want %+v", got, want)
	}
	if got.Usage != want.Usage {
		t.Errorf("Infer usage = %+v, want %+v (canonical usage must be populated)", got.Usage, want.Usage)
	}
}

func TestMockProvider_Infer_ErrorRate(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("boom")
	tests := []struct {
		name    string
		mock    *Mock
		wantErr error // nil means "no error expected"
	}{
		{
			name:    "always errors with default sentinel",
			mock:    New(WithResponse(sampleResponse()), WithError(nil)),
			wantErr: ErrMockFailure,
		},
		{
			name:    "always errors with custom error",
			mock:    New(WithResponse(sampleResponse()), WithError(sentinel)),
			wantErr: sentinel,
		},
		{
			name:    "error rate >= 1 fails",
			mock:    &Mock{Response: sampleResponse(), ErrorRate: 1},
			wantErr: ErrMockFailure,
		},
		{
			name:    "error rate <= 0 succeeds",
			mock:    &Mock{Response: sampleResponse(), ErrorRate: 0},
			wantErr: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := tt.mock.Infer(context.Background(), Request{})
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("Infer returned unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Infer returned nil error, want %v", tt.wantErr)
			}
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("Infer error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestMockProvider_Infer_ContextCancelled(t *testing.T) {
	t.Parallel()
	m := New(WithResponse(sampleResponse()), WithLatency(2*time.Second))

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := m.Infer(ctx, Request{})
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Infer error = %v, want context.Canceled", err)
	}
	if elapsed >= time.Second {
		t.Errorf("Infer took %v; should have returned promptly on cancel, not after full latency", elapsed)
	}
}

func TestMockProvider_InferStream_EmitsAndClosesChannel(t *testing.T) {
	t.Parallel()
	const chunks = 4
	m := New(WithResponse(sampleResponse()), WithStreamChunks(chunks))

	ch, err := m.InferStream(context.Background(), Request{})
	if err != nil {
		t.Fatalf("InferStream init error: %v", err)
	}

	var content string
	var contentChunks int
	var terminal *Chunk
	for c := range ch {
		if c.Err != nil {
			t.Fatalf("unexpected chunk error: %v", c.Err)
		}
		if c.Done {
			cp := c
			terminal = &cp
			continue
		}
		contentChunks++
		content += c.Content
	}

	if contentChunks != chunks {
		t.Errorf("emitted %d content chunks, want %d", contentChunks, chunks)
	}
	if content != sampleResponse().Content {
		t.Errorf("reassembled content = %q, want %q", content, sampleResponse().Content)
	}
	if terminal == nil {
		t.Fatal("stream ended without a terminal Done chunk")
	}
	if terminal.Usage != sampleResponse().Usage {
		t.Errorf("terminal usage = %+v, want %+v", terminal.Usage, sampleResponse().Usage)
	}
}

func TestMockProvider_InferStream_InitErrorOnFailure(t *testing.T) {
	t.Parallel()
	m := New(WithResponse(sampleResponse()), WithError(nil))

	ch, err := m.InferStream(context.Background(), Request{})
	if !errors.Is(err, ErrMockFailure) {
		t.Fatalf("InferStream error = %v, want ErrMockFailure", err)
	}
	if ch != nil {
		t.Error("InferStream returned a non-nil channel alongside an init error")
	}
}

func TestMockProvider_InferStream_ContextCancelled_StopsAndCloses(t *testing.T) {
	t.Parallel()
	m := New(
		WithResponse(sampleResponse()),
		WithStreamChunks(100),
		WithLatency(50*time.Millisecond),
	)

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := m.InferStream(ctx, Request{})
	if err != nil {
		t.Fatalf("InferStream init error: %v", err)
	}

	// Consume one chunk, then cancel mid-stream.
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first chunk")
	}
	cancel()

	// The channel must close promptly after cancellation. Draining it must
	// terminate (proving the producer goroutine exits — no leak).
	done := make(chan struct{})
	go func() {
		for range ch {
		}
		close(done)
	}()

	select {
	case <-done:
		// Channel closed: producer goroutine returned.
	case <-time.After(time.Second):
		t.Fatal("channel not closed within 1s of cancel; producer goroutine leaked")
	}
}
