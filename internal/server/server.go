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
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/legacy"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/adverax/sluice/internal/api"
	"github.com/adverax/sluice/internal/health"
	"github.com/adverax/sluice/internal/logging"
	"github.com/adverax/sluice/internal/metering"
	"github.com/adverax/sluice/internal/provider"
	"github.com/adverax/sluice/internal/proxy"
)

// ErrServiceUnavailable is the sentinel the resilience layer (retry/breaker,
// FR-007) wraps when it fast-fails: the per-provider circuit breaker is open
// (AC-022) or the client deadline elapsed during a retry (AC-020). The handler
// maps any error matching it (errors.Is) to HTTP 503. Defining it here, in the
// package that owns the InferFunc seam, avoids an import cycle: the composition
// root (internal/proxy/resilience) imports server, not the reverse.
var ErrServiceUnavailable = errors.New("server: service unavailable")

// retryAfterer is optionally satisfied by an InferFunc error to supply the
// Retry-After hint surfaced on a 503 fast-fail. The resilience Unavailable error
// implements it.
type retryAfterer interface {
	RetryAfter() time.Duration
}

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

	// metricsRegistry is the injected Prometheus registry exposed at GET /metrics
	// (COMP-013, ADR-0008). When nil, GetMetrics serves an empty body so the
	// generated contract is still satisfied without metrics wiring.
	metricsRegistry *prometheus.Registry

	// meter is the async usage-metering sink (COMP-016, FR-014). After a
	// completed inference the handler records a UsageEvent here with a
	// NON-BLOCKING enqueue — the buffer drops on full so the hot path never
	// blocks on metering (INV-003 / CON-006). Defaults to a no-op sink so the
	// server runs without metering wiring.
	meter metering.Sink
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

// WithMetricsRegistry injects the Prometheus registry served at GET /metrics
// (COMP-013, ADR-0008). The registry is created by the composition root and
// passed in; the server never touches the global default registerer.
func WithMetricsRegistry(reg *prometheus.Registry) Option {
	return func(s *Server) {
		if reg != nil {
			s.metricsRegistry = reg
		}
	}
}

// WithMeteringSink injects the async usage-metering sink (COMP-016, FR-014).
// The handler records a UsageEvent after each completed inference via a
// non-blocking Enqueue; on a full buffer the event is dropped so the hot path
// never blocks (INV-003 / CON-006). When nil, a no-op sink is used.
func WithMeteringSink(sink metering.Sink) Option {
	return func(s *Server) {
		if sink != nil {
			s.meter = sink
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
		meter:  metering.NopSink{},
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

// GetMetrics is the Prometheus exposition endpoint (COMP-013, FR-010/NFR-007).
// It serves the injected registry via promhttp.HandlerFor in Prometheus text
// format. When no registry is injected it falls back to an empty 200 body so the
// generated contract stays satisfied without metrics wiring.
func (s *Server) GetMetrics(_ context.Context, _ api.GetMetricsRequestObject) (api.GetMetricsResponseObject, error) {
	if s.metricsRegistry == nil {
		return api.GetMetrics200TextResponse(""), nil
	}
	return metricsResponse{reg: s.metricsRegistry}, nil
}

// metricsResponse adapts the promhttp exposition handler onto the generated
// GetMetricsResponseObject seam, mirroring serviceUnavailableResponse: the
// generated 200-text response cannot stream the Prometheus exposition, so this
// thin wrapper delegates straight to promhttp.HandlerFor, which writes the
// status, the Content-Type, and the text-format body itself.
type metricsResponse struct {
	reg *prometheus.Registry
}

// VisitGetMetricsResponse implements the generated response contract by serving
// the registry through the standard promhttp handler.
func (r metricsResponse) VisitGetMetricsResponse(w http.ResponseWriter) error {
	h := promhttp.HandlerFor(r.reg, promhttp.HandlerOpts{})
	h.ServeHTTP(w, &http.Request{Method: http.MethodGet, Header: make(http.Header)})
	return nil
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

	// Streaming path (FR-001 AC-002, FR-003): when the client requests a stream
	// we cannot return a buffered response object — we must take over the raw
	// http.ResponseWriter. The strict server exposes that seam through the
	// response visitor (CreateChatCompletionResponseObject), so we return a
	// custom streamResponse whose VisitCreateChatCompletionResponse drives the
	// SSE forwarding loop. The provider is resolved here (same as unary); the
	// per-request context is threaded into InferStream inside Visit so client
	// disconnect/deadline aborts the upstream (AC-008, AC-009, INV-002).
	//
	// Resilience-for-streaming (open-breaker fast-fail on initiation, pool slot)
	// is a documented follow-up: retry cannot apply to a partially-sent
	// response, and the breaker/pool wrap the unary infer hook only. The unary
	// path retains full resilience.
	if req.Stream {
		return streamResponse{
			ctx:      ctx,
			provider: prov,
			req:      req,
			logger:   s.logger,
			model:    body.Model,
			meter:    s.meter,
			start:    time.Now(),
		}, nil
	}

	start := time.Now()
	resp, err := s.infer(ctx, prov, req)
	if err != nil {
		// Fast-fail (open breaker / deadline during retry) → 503 + Retry-After
		// (AC-022, AC-020, INV-005). Checked before the generic 502 mapping so
		// resilience signals are not masked as upstream provider errors.
		if errors.Is(err, ErrServiceUnavailable) {
			s.logger.LogAttrs(ctx, slog.LevelWarn, "request fast-failed (resilience)",
				slog.String("model", body.Model),
				slog.String("error", err.Error()),
			)
			return s.serviceUnavailable(err), nil
		}
		// Provider failure (e.g. upstream 5xx, retries exhausted) → 502 (AC-003,
		// AC-019). A non-retryable 4xx StatusError also lands here (AC-021).
		s.logger.LogAttrs(ctx, slog.LevelError, "provider inference failed",
			slog.String("model", body.Model),
			slog.String("error", err.Error()),
		)
		return badGateway("provider_error", "upstream provider request failed"), nil
	}

	// Async usage metering (FR-014): record the completed inference. Enqueue is
	// non-blocking — on a full buffer the event is dropped (AC-036) so this never
	// delays the response (INV-003 / CON-006). The routing key is the model alias
	// (FR-002); the model is the resolved response model; tokens come from the
	// canonical Usage (ADR-0009); status is 200 (success path).
	s.recordUsage(ctx, body.Model, resp.Model, resp.Usage, time.Since(start), http.StatusOK)

	return api.CreateChatCompletion200JSONResponse(toAPIResponse(resp)), nil
}

// recordUsage builds a UsageEvent from a completed inference and enqueues it on
// the metering sink. The enqueue is non-blocking by contract (Sink.Enqueue), so
// the request hot path is never blocked on metering (INV-003 / CON-006). routeKey
// is the model alias used for routing (FR-002); model is the resolved model that
// produced the completion; usage carries the canonical token accounting
// (ADR-0009).
func (s *Server) recordUsage(ctx context.Context, routeKey, model string, usage provider.Usage, latency time.Duration, status int) {
	if model == "" {
		model = routeKey
	}
	s.meter.Enqueue(metering.UsageEvent{
		Provider:         routeKey,
		Model:            model,
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		TotalTokens:      usage.TotalTokens,
		Latency:          latency,
		Status:           status,
		RequestID:        logging.RequestIDFromContext(ctx),
		Timestamp:        time.Now(),
	})
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

// serviceUnavailable builds the 503 response for a resilience fast-fail. If the
// error supplies a Retry-After hint (via the retryAfterer interface) it is
// surfaced as the Retry-After header (AC-022). The default Retry-After applies
// when no positive hint is present.
func (s *Server) serviceUnavailable(err error) api.CreateChatCompletionResponseObject {
	retryAfter := time.Duration(0)
	var ra retryAfterer
	if errors.As(err, &ra) {
		retryAfter = ra.RetryAfter()
	}
	return serviceUnavailableResponse{
		body: api.CreateChatCompletion503JSONResponse{
			ServiceUnavailableJSONResponse: api.ServiceUnavailableJSONResponse{
				Error:   "service_unavailable",
				Message: "upstream temporarily unavailable; retry later",
			},
		},
		retryAfter: retryAfter,
	}
}

// serviceUnavailableResponse wraps the generated 503 response and adds a
// Retry-After header (INV-005, AC-022). The generated response type does not
// set that header, so the handler emits this thin wrapper instead.
type serviceUnavailableResponse struct {
	body       api.CreateChatCompletion503JSONResponse
	retryAfter time.Duration
}

// VisitCreateChatCompletionResponse implements the generated response contract:
// it sets Retry-After (seconds) when a positive hint is present, then delegates
// the JSON body + 503 status to the generated response.
func (r serviceUnavailableResponse) VisitCreateChatCompletionResponse(w http.ResponseWriter) error {
	if r.retryAfter > 0 {
		secs := int(r.retryAfter.Round(time.Second) / time.Second)
		if secs < 1 {
			secs = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(secs))
	}
	return r.body.VisitCreateChatCompletionResponse(w)
}

// streamResponse is the custom strict-server response for the streaming path
// (FR-001 AC-002). It implements the generated CreateChatCompletionResponseObject
// seam: VisitCreateChatCompletionResponse takes over the raw http.ResponseWriter
// to forward Server-Sent Events, instead of returning a buffered JSON body.
//
// ctx is the per-request context (r.Context()) captured in the handler. It is
// threaded straight into Provider.InferStream so any cancellation — client
// disconnect or deadline — propagates to the upstream call and aborts it
// promptly (FR-003, AC-008, AC-009, INV-002). The Mock honours ctx at every
// emit step and closes its channel, so the consumer never leaks a goroutine.
type streamResponse struct {
	ctx      context.Context
	provider provider.Provider
	req      provider.Request
	logger   *slog.Logger
	model    string
	// meter records a best-effort UsageEvent after the stream completes, using
	// the terminal chunk's usage (FR-014). Enqueue is non-blocking (INV-003).
	meter metering.Sink
	// start is when the handler dispatched the stream, used for latency.
	start time.Time
}

// sseDone is the conventional terminal marker of the chat-completions SSE
// protocol; clients stop reading when they see it.
const sseDone = "data: [DONE]\n\n"

// VisitCreateChatCompletionResponse drives the SSE forwarding loop. It writes
// the streaming headers + 200 status, resolves the provider stream via
// InferStream(ctx, ...), and forwards each canonical Chunk as an SSE `data:`
// event, flushing after every write so clients see deltas as they arrive.
//
// Flush goes through http.NewResponseController(w).Flush(): the metrics/tracing
// middleware wrap the ResponseWriter and implement Unwrap() http.ResponseWriter,
// so the controller traverses the unwrap chain to reach the real flusher — a
// raw w.(http.Flusher) assertion would fail through those wrappers.
//
// The select on ctx.Done() vs the chunk channel guarantees prompt return on
// client disconnect: when ctx is cancelled we stop forwarding and return, which
// (because ctx was passed to InferStream) cancels the upstream and lets the
// provider goroutine drain and close the channel — no goroutine leak.
func (r streamResponse) VisitCreateChatCompletionResponse(w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	rc := http.NewResponseController(w)
	_ = rc.Flush() // flush headers so the client sees the stream open immediately.

	ch, err := r.provider.InferStream(r.ctx, r.req)
	if err != nil {
		// Failure to initialise the stream: headers/200 are already sent, so we
		// cannot change the status. Emit a terminal marker and log (AC-003 cannot
		// apply once committed to 200). This mirrors the streaming SSE convention.
		r.logger.LogAttrs(r.ctx, slog.LevelError, "InferStream initialisation failed",
			slog.String("model", r.model),
			slog.String("error", err.Error()),
		)
		_, _ = w.Write([]byte(sseDone))
		_ = rc.Flush()
		return nil
	}

	for {
		select {
		case <-r.ctx.Done():
			// Client disconnect / deadline: stop forwarding and return. Returning
			// keeps ctx cancelled, which aborts the upstream; the provider drains
			// and closes ch. We do not range the channel further (no leak).
			r.logger.LogAttrs(r.ctx, slog.LevelDebug, "streaming context cancelled; stopping forward",
				slog.String("model", r.model),
				slog.String("error", r.ctx.Err().Error()),
			)
			return nil
		case chunk, ok := <-ch:
			if !ok {
				// Channel closed: stream finished normally.
				_, _ = w.Write([]byte(sseDone))
				_ = rc.Flush()
				return nil
			}
			if chunk.Err != nil {
				// Mid-stream transport error: log and end the stream (cannot retry a
				// partially-sent response).
				r.logger.LogAttrs(r.ctx, slog.LevelError, "stream chunk error",
					slog.String("model", r.model),
					slog.String("error", chunk.Err.Error()),
				)
				_, _ = w.Write([]byte(sseDone))
				_ = rc.Flush()
				return nil
			}
			if chunk.Done {
				// Terminal success chunk: emit any usage-bearing event then [DONE].
				payload, mErr := json.Marshal(toAPIChunk(chunk))
				if mErr == nil {
					_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
					_ = rc.Flush()
				}
				_, _ = w.Write([]byte(sseDone))
				_ = rc.Flush()
				// Async usage metering for the streaming path (FR-014, best-effort):
				// record the completed stream using the terminal chunk's usage. The
				// enqueue is non-blocking so it never delays stream teardown (INV-003).
				r.recordUsage(chunk.Usage)
				return nil
			}
			// Content delta: forward as an SSE data event and flush so the client
			// receives it as it arrives (AC-002).
			payload, mErr := json.Marshal(toAPIChunk(chunk))
			if mErr != nil {
				r.logger.LogAttrs(r.ctx, slog.LevelError, "marshal stream chunk",
					slog.String("model", r.model),
					slog.String("error", mErr.Error()),
				)
				continue
			}
			if _, wErr := fmt.Fprintf(w, "data: %s\n\n", payload); wErr != nil {
				// Write failure usually means the client went away; stop forwarding.
				return nil
			}
			_ = rc.Flush()
		}
	}
}

// recordUsage enqueues a best-effort UsageEvent for the completed stream. The
// sink defaults to a no-op so an un-metered server is safe; Enqueue is
// non-blocking (drop-on-full) so stream teardown is never delayed (INV-003).
func (r streamResponse) recordUsage(usage provider.Usage) {
	if r.meter == nil {
		return
	}
	r.meter.Enqueue(metering.UsageEvent{
		Provider:         r.model,
		Model:            r.model,
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		TotalTokens:      usage.TotalTokens,
		Latency:          time.Since(r.start),
		Status:           http.StatusOK,
		RequestID:        logging.RequestIDFromContext(r.ctx),
		Timestamp:        time.Now(),
	})
}

// streamChunk is the wire shape of one SSE data event on the streaming path. It
// keeps the canonical Chunk fields (content + usage) crossing the boundary as
// provider-agnostic JSON (ADR-0009); no provider-specific type is exposed.
type streamChunk struct {
	Content string    `json:"content,omitempty"`
	Done    bool      `json:"done,omitempty"`
	Usage   api.Usage `json:"usage,omitempty"`
}

// toAPIChunk maps a canonical provider.Chunk onto the SSE wire shape.
func toAPIChunk(c provider.Chunk) streamChunk {
	return streamChunk{
		Content: c.Content,
		Done:    c.Done,
		Usage: api.Usage{
			PromptTokens:     c.Usage.PromptTokens,
			CompletionTokens: c.Usage.CompletionTokens,
			TotalTokens:      c.Usage.TotalTokens,
		},
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
	// GetSpec is the non-deprecated accessor (GetSwagger predates kin-openapi's
	// openapi3.Swagger→openapi3.T rename and is deprecated, SA1019).
	swagger, err := api.GetSpec()
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
