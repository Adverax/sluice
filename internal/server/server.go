// Package server implements COMP-001 (HTTP Handler & Router) and the
// non-streaming side of COMP-002 (Proxy Core). It provides the concrete
// implementation of the OpenAPI-generated api.StrictServerInterface
// (ADR-0011, contract-first): the handlers map the generated DTOs onto the
// canonical provider.Request/Response (the ADR-0009 anti-corruption layer),
// route by model through the proxy.Router (FR-002), and translate the
// readiness verdict from internal/health onto the spec's readiness schema.
//
// The HTTP boundary is fully generated: routes are registered via
// api.HandlerFromMux on a *http.ServeMux (CON-001, no web framework). This
// package owns only the behaviour behind that boundary, plus the seams for
// later cards (ADR-0006): the middleware chain wrap points in New and the
// infer hook where retry/breaker (FR-007) will wrap the provider call.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/legacy"

	"github.com/adverax/sluice/internal/api"
	"github.com/adverax/sluice/internal/health"
	"github.com/adverax/sluice/internal/provider"
	"github.com/adverax/sluice/internal/proxy"
)

// Server implements api.StrictServerInterface. It is the seam between the
// generated HTTP boundary and the gateway core: it holds the model router
// (FR-002) and the readiness handler (FR-009), both injected (ADR-0008).
type Server struct {
	router *proxy.Router
	health *health.Handler
	logger *slog.Logger

	// infer wraps the provider call. By default it is provider.Provider.Infer
	// directly; later cards (FR-007 retry/circuit-breaker) replace it via
	// WithInferFunc without touching the mapping/routing code (ADR-0006).
	infer InferFunc
}

// InferFunc executes a single non-streaming inference against the chosen
// provider. It is the wrap point for resilience decorators (retry, circuit
// breaker) added by later cards (ADR-0006); the default simply calls
// p.Infer(ctx, req).
type InferFunc func(ctx context.Context, p provider.Provider, req provider.Request) (provider.Response, error)

// Option configures a Server via the functional-options pattern (CON-001).
type Option func(*Server)

// WithInferFunc overrides the provider-call hook so later cards can wrap it
// with retry/circuit-breaker logic (FR-007) without changing the handler.
func WithInferFunc(fn InferFunc) Option {
	return func(s *Server) {
		if fn != nil {
			s.infer = fn
		}
	}
}

// New constructs a Server. The router maps models to providers (FR-002) and the
// health handler aggregates the readiness checkers (FR-009). The logger is
// injected (ADR-0008). It is a compile-time error if Server stops satisfying
// the generated interface (see the assertion below).
func New(router *proxy.Router, healthHandler *health.Handler, logger *slog.Logger, opts ...Option) *Server {
	s := &Server{
		router: router,
		health: healthHandler,
		logger: logger,
		infer: func(ctx context.Context, p provider.Provider, req provider.Request) (provider.Response, error) {
			return p.Infer(ctx, req)
		},
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Compile-time proof that Server satisfies the generated contract (ADR-0011).
var _ api.StrictServerInterface = (*Server)(nil)

// GetHealthz implements the liveness probe (FR-008 / AC-025): always 200 with
// {"status":"ok"}.
func (s *Server) GetHealthz(_ context.Context, _ api.GetHealthzRequestObject) (api.GetHealthzResponseObject, error) {
	return api.GetHealthz200JSONResponse{Status: "ok"}, nil
}

// GetReadyz implements the readiness probe (FR-009): 200 when every registered
// dependency checker is healthy (AC-026), 503 with the per-dependency status
// map otherwise (AC-027 redis:down, AC-028 postgres:down). The verdict comes
// from health.Handler.Evaluate so the body is consistent regardless of which
// wiring serves it.
func (s *Server) GetReadyz(ctx context.Context, _ api.GetReadyzRequestObject) (api.GetReadyzResponseObject, error) {
	res := s.health.Evaluate(ctx)

	if res.Healthy {
		return api.GetReadyz200JSONResponse{
			Status:       "ok",
			Dependencies: res.Dependencies,
		}, nil
	}
	return api.GetReadyz503JSONResponse{
		Status:       "unavailable",
		Dependencies: res.Dependencies,
	}, nil
}

// GetMetrics is the Prometheus exposition endpoint. Real metrics land in a
// later card (COMP-013); for now it answers 200 with an empty body so the
// generated contract is fully satisfied and the route is mountable.
func (s *Server) GetMetrics(_ context.Context, _ api.GetMetricsRequestObject) (api.GetMetricsResponseObject, error) {
	return api.GetMetrics200TextResponse(""), nil
}

// CreateChatCompletion implements the non-streaming proxy (FR-001, COMP-002).
// The generated strict server has already decoded the JSON body (malformed body
// → 400, AC-004). This handler validates the model is present (AC-006 → 400),
// routes by model (AC-007 unknown → 404), maps the generated request onto the
// canonical provider.Request (ADR-0009 ACL), calls the provider (via the infer
// hook), and maps the canonical response back (AC-001 → 200) or the provider
// error to 502 (AC-003).
func (s *Server) CreateChatCompletion(ctx context.Context, request api.CreateChatCompletionRequestObject) (api.CreateChatCompletionResponseObject, error) {
	if request.Body == nil {
		// Defensive: the strict server populates Body on a successful decode and
		// surfaces a malformed/absent body as 400 before reaching here. If it is
		// somehow nil, treat it as a bad request rather than panicking.
		return badRequest("missing_body", "request body is required"), nil
	}
	body := *request.Body

	if body.Model == "" {
		// Absent model: do not contact any provider (AC-006).
		return badRequest("missing_model", "the 'model' field is required"), nil
	}

	if len(body.Messages) == 0 {
		// Empty messages: the provider has nothing to infer from; reject early
		// rather than forwarding an empty conversation (defensive AC).
		return badRequest("empty_messages", "the 'messages' field must contain at least one message"), nil
	}

	prov, err := s.router.Provider(body.Model)
	if err != nil {
		if errors.Is(err, proxy.ErrModelNotRegistered) {
			// Unregistered model: do not contact any provider (AC-007).
			return notFound("unknown_model", "no provider is registered for model "+body.Model), nil
		}
		// Unexpected routing failure.
		s.logger.LogAttrs(ctx, slog.LevelError, "router lookup failed",
			slog.String("model", body.Model),
			slog.String("error", err.Error()),
		)
		return badGateway("routing_error", "failed to route request"), nil
	}

	req := toCanonicalRequest(body)

	resp, err := s.infer(ctx, prov, req)
	if err != nil {
		// Provider failure (e.g. upstream 500, retries exhausted) → 502 (AC-003).
		s.logger.LogAttrs(ctx, slog.LevelError, "provider inference failed",
			slog.String("model", body.Model),
			slog.String("error", err.Error()),
		)
		return badGateway("provider_error", "upstream provider request failed"), nil
	}

	return api.CreateChatCompletion200JSONResponse(toAPIResponse(resp)), nil
}

// toCanonicalRequest maps the generated request DTO onto the canonical
// provider.Request (ADR-0009). Public temperature is float32; the canonical
// field is *float64, so a present temperature is widened and pointed to.
func toCanonicalRequest(body api.ChatCompletionRequest) provider.Request {
	req := provider.Request{
		Model:    body.Model,
		Messages: make([]provider.Message, 0, len(body.Messages)),
	}
	if body.Stream != nil {
		req.Stream = *body.Stream
	}
	if body.MaxTokens != nil {
		req.MaxTokens = *body.MaxTokens
	}
	if body.Temperature != nil {
		t := float64(*body.Temperature)
		req.Temperature = &t
	}
	for _, m := range body.Messages {
		req.Messages = append(req.Messages, provider.Message{
			Role:    provider.Role(m.Role),
			Content: m.Content,
		})
	}
	return req
}

// toAPIResponse maps the canonical provider.Response back onto the generated
// response DTO (ADR-0009).
func toAPIResponse(resp provider.Response) api.ChatCompletionResponse {
	return api.ChatCompletionResponse{
		Model:        resp.Model,
		Content:      resp.Content,
		FinishReason: resp.FinishReason,
		Usage: api.Usage{
			PromptTokens:     resp.Usage.PromptTokens,
			CompletionTokens: resp.Usage.CompletionTokens,
			TotalTokens:      resp.Usage.TotalTokens,
		},
	}
}

func badRequest(code, msg string) api.CreateChatCompletion400JSONResponse {
	return api.CreateChatCompletion400JSONResponse{
		BadRequestJSONResponse: api.BadRequestJSONResponse{Error: code, Message: msg},
	}
}

func notFound(code, msg string) api.CreateChatCompletion404JSONResponse {
	return api.CreateChatCompletion404JSONResponse{
		NotFoundJSONResponse: api.NotFoundJSONResponse{Error: code, Message: msg},
	}
}

func badGateway(code, msg string) api.CreateChatCompletion502JSONResponse {
	return api.CreateChatCompletion502JSONResponse{
		BadGatewayJSONResponse: api.BadGatewayJSONResponse{Error: code, Message: msg},
	}
}

// Handler builds the http.Handler for the gateway: it wraps this Server in the
// generated strict handler and registers all routes (incl. /v1/chat/completions,
// /healthz, /readyz, /metrics) on mux via api.HandlerFromMux (CON-001). The
// caller supplies the mux so application routes can be counted as in-flight
// (FR-012) while probes are mounted uncounted at the top level (ADR-0006
// composition order). Returns the populated mux wrapped with an OpenAPI request
// validator (ADR-0011) so invalid bodies (unknown enum values, missing required
// fields) are rejected 400 BEFORE reaching CreateChatCompletion.
func (s *Server) Handler(mux *http.ServeMux) http.Handler {
	si := api.NewStrictHandler(s, nil)
	inner := api.HandlerFromMux(si, mux)

	// Build the kin-openapi request-validation middleware from the embedded spec.
	// swagger.Servers is cleared to avoid host/scheme matching rejections in tests
	// and in environments where the Host header doesn't match a declared server.
	swagger, err := api.GetSwagger()
	if err != nil {
		// This is a programming error (bad embedded spec); panic early so it is
		// caught by tests rather than silently skipping validation at runtime.
		panic("server: failed to load embedded OpenAPI spec: " + err.Error())
	}
	swagger.Servers = nil

	router, err := legacy.NewRouter(swagger)
	if err != nil {
		panic("server: failed to build OpenAPI router: " + err.Error())
	}

	validator := openapi3filter.NewValidator(
		router,
		openapi3filter.OnErr(func(ctx context.Context, w http.ResponseWriter, status int, code openapi3filter.ErrCode, err error) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(api.Error{
				Error:   "validation_error",
				Message: err.Error(),
			})
		}),
		// Only validate requests; leave response validation off (not needed and
		// adds per-request buffering overhead — strict mode can be enabled later).
		openapi3filter.ValidationOptions(openapi3filter.Options{
			ExcludeResponseBody: true,
		}),
	)

	return validator.Middleware(inner)
}
