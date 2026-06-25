package proxy_test

import (
	"context"
	"errors"
	"testing"

	"github.com/adverax/sluice/internal/provider"
	"github.com/adverax/sluice/internal/proxy"
)

// spyProvider records whether Infer was called, so negative-path tests can
// assert the provider is NOT contacted (AC-006, AC-007).
type spyProvider struct {
	resp   provider.Response
	called bool
}

func (s *spyProvider) Infer(_ context.Context, _ provider.Request) (provider.Response, error) {
	s.called = true
	return s.resp, nil
}

func (s *spyProvider) InferStream(context.Context, provider.Request) (<-chan provider.Chunk, error) {
	return nil, errors.New("not implemented in test")
}

// TestRouter_RoutesToCorrectProvider covers AC-005: two providers registered
// for two models; a lookup returns the provider registered for that model.
func TestRouter_RoutesToCorrectProvider(t *testing.T) {
	gpt4 := &spyProvider{resp: provider.Response{Model: "gpt-4"}}
	claude := &spyProvider{resp: provider.Response{Model: "claude"}}

	r := proxy.NewRouter()
	r.Register("gpt-4", gpt4)
	r.Register("claude-3", claude)

	tests := []struct {
		name  string
		model string
		want  provider.Provider
	}{
		{name: "routes gpt-4", model: "gpt-4", want: gpt4},
		{name: "routes claude-3", model: "claude-3", want: claude},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := r.Provider(tt.model)
			if err != nil {
				t.Fatalf("Provider(%q) error = %v", tt.model, err)
			}
			if got != tt.want {
				t.Errorf("Provider(%q) = %p, want %p", tt.model, got, tt.want)
			}
		})
	}
}

// TestRouter_UnknownModel covers the routing-layer side of AC-007: an
// unregistered model yields ErrModelNotRegistered (mapped to 404 at the HTTP
// boundary).
func TestRouter_UnknownModel(t *testing.T) {
	r := proxy.NewRouter()
	r.Register("gpt-4", &spyProvider{})

	_, err := r.Provider("does-not-exist")
	if !errors.Is(err, proxy.ErrModelNotRegistered) {
		t.Fatalf("Provider(unknown) error = %v, want ErrModelNotRegistered", err)
	}
}

// TestRouter_EmptyModel covers the routing-layer side of AC-006: an empty model
// never matches.
func TestRouter_EmptyModel(t *testing.T) {
	r := proxy.NewRouter()
	r.Register("gpt-4", &spyProvider{})

	if _, err := r.Provider(""); !errors.Is(err, proxy.ErrModelNotRegistered) {
		t.Fatalf("Provider(\"\") error = %v, want ErrModelNotRegistered", err)
	}
}
