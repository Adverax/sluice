package api_test

import (
	"context"
	"net/http"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/adverax/sluice/internal/api"
)

// specPath resolves api/openapi.yaml relative to this test file, independent of
// the working directory `go test` runs in.
func specPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// internal/api/api_test.go -> repo root -> api/openapi.yaml
	root := filepath.Join(filepath.Dir(thisFile), "..", "..")
	return filepath.Join(root, "api", "openapi.yaml")
}

// TestOpenAPISpec_IsValid proves AC-G1: api/openapi.yaml loads and validates as
// a well-formed OpenAPI 3.0 document (the contract is the single source of
// truth, ADR-0011).
func TestOpenAPISpec_IsValid(t *testing.T) {
	loader := &openapi3.Loader{Context: context.Background()}
	doc, err := loader.LoadFromFile(specPath(t))
	if err != nil {
		t.Fatalf("LoadFromFile: %v", err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatalf("doc.Validate: %v", err)
	}
}

// stubServer is a no-op implementation of the generated StrictServerInterface.
// Its sole purpose is the compile-time assertion below: it cannot compile if the
// generated chat-completions contract (or any other endpoint) is missing or has
// a different shape.
type stubServer struct{}

func (stubServer) GetHealthz(context.Context, api.GetHealthzRequestObject) (api.GetHealthzResponseObject, error) {
	return nil, nil
}

func (stubServer) GetMetrics(context.Context, api.GetMetricsRequestObject) (api.GetMetricsResponseObject, error) {
	return nil, nil
}

func (stubServer) GetReadyz(context.Context, api.GetReadyzRequestObject) (api.GetReadyzResponseObject, error) {
	return nil, nil
}

func (stubServer) CreateChatCompletion(context.Context, api.CreateChatCompletionRequestObject) (api.CreateChatCompletionResponseObject, error) {
	return nil, nil
}

// Compile-time proof that stubServer satisfies the generated interface.
var _ api.StrictServerInterface = stubServer{}

// TestGeneratedAPI_HasChatCompletionsContract proves AC-G2: the generated
// request/response/error types and the StrictServerInterface for
// POST /v1/chat/completions exist and are exported. The test references them
// directly so it fails to compile if the contract is missing.
func TestGeneratedAPI_HasChatCompletionsContract(t *testing.T) {
	// Request: model + messages (with role enum) + optional stream/max_tokens/temperature.
	stream := false
	maxTokens := 16
	temperature := float32(0.7)
	req := api.ChatCompletionRequest{
		Model: "gpt-4o-mini",
		Messages: []api.Message{
			{Role: api.System, Content: "you are a test"},
			{Role: api.User, Content: "hello"},
		},
		Stream:      &stream,
		MaxTokens:   &maxTokens,
		Temperature: &temperature,
	}
	if req.Model == "" || len(req.Messages) != 2 {
		t.Fatalf("unexpected zero-value request: %+v", req)
	}
	if req.Messages[1].Role != api.User {
		t.Fatalf("role enum mismatch: %v", req.Messages[1].Role)
	}

	// Response: model + content + finish_reason + usage{prompt,completion,total}.
	resp := api.ChatCompletionResponse{
		Model:        "gpt-4o-mini",
		Content:      "hi",
		FinishReason: "stop",
		Usage: api.Usage{
			PromptTokens:     1,
			CompletionTokens: 1,
			TotalTokens:      2,
		},
	}
	if resp.Usage.TotalTokens != 2 {
		t.Fatalf("usage mismatch: %+v", resp.Usage)
	}

	// Error envelope: {error, message}.
	apiErr := api.Error{Error: "bad_request", Message: "missing model"}
	if apiErr.Error == "" || apiErr.Message == "" {
		t.Fatalf("unexpected error envelope: %+v", apiErr)
	}

	// The strict interface and the route registrar must exist; wire the stub
	// through NewStrictHandler (strict-server) and HandlerFromMux
	// (std-http-server, CON-001) to prove the full generated boundary exists.
	var _ api.StrictServerInterface = stubServer{}
	si := api.NewStrictHandler(stubServer{}, nil)
	handler := api.HandlerFromMux(si, http.NewServeMux())
	if handler == nil {
		t.Fatal("HandlerFromMux returned nil handler")
	}
}
