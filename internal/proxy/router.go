// Package proxy implements COMP-002, the non-streaming proxy core, and the
// model→provider Router (FR-002). The Router is a ports-and-adapters seam: it
// maps a request's `model` field onto a registered provider.Provider (the
// ADR-0009 anti-corruption port) and never imports a concrete provider package.
//
// The proxy core itself — mapping the canonical provider.Request/Response onto
// the OpenAPI DTOs and driving Provider.Infer — lives in the server package,
// which implements the generated api.StrictServerInterface. This package keeps
// the routing/registry concern isolated and unit-testable in isolation.
package proxy

import (
	"errors"
	"sync"

	"github.com/adverax/sluice/internal/provider"
)

// ErrModelNotRegistered is returned by Router.Provider when no provider is
// registered for the requested model. Callers (the HTTP boundary) map this to
// HTTP 404 (AC-007). Match it with errors.Is.
var ErrModelNotRegistered = errors.New("proxy: model not registered")

// Router maps a model name onto the provider.Provider that serves it (FR-002).
// It is the registry the HTTP boundary consults before dispatching an inference
// call. Safe for concurrent use: Register is expected at startup, Provider on
// every request.
type Router struct {
	mu        sync.RWMutex
	providers map[string]provider.Provider
}

// NewRouter constructs an empty Router. Register providers with Register before
// serving traffic.
func NewRouter() *Router {
	return &Router{providers: make(map[string]provider.Provider)}
}

// Register associates model with p. A later Register for the same model
// overwrites the earlier mapping, so configuration is last-write-wins. A nil
// provider or empty model is ignored.
func (r *Router) Register(model string, p provider.Provider) {
	if model == "" || p == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers[model] = p
}

// Provider returns the provider registered for model, or ErrModelNotRegistered
// if none is. An empty model never matches and yields ErrModelNotRegistered;
// the HTTP boundary handles the "absent model" case as 400 before calling here.
func (r *Router) Provider(model string) (provider.Provider, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[model]
	if !ok {
		return nil, ErrModelNotRegistered
	}
	return p, nil
}
