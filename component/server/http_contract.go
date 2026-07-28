package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

type (
	StrictServerInterface interface {
		GetHealthz(ctx context.Context, request GetHealthzRequestObject) (GetHealthzResponseObject, error)
		PostAutomationTrigger(ctx context.Context, request PostAutomationTriggerRequestObject) (PostAutomationTriggerResponseObject, error)
		PostAutomationResume(ctx context.Context, request PostAutomationResumeRequestObject) (PostAutomationResumeResponseObject, error)
	}

	StrictHandlerFunc    func(ctx context.Context, w http.ResponseWriter, r *http.Request, request interface{}) (interface{}, error)
	StrictMiddlewareFunc func(StrictHandlerFunc, string) StrictHandlerFunc
	MiddlewareFunc       func(http.Handler) http.Handler

	ServeMux interface {
		http.Handler
		HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request))
	}

	ServerInterface = *strictHandler
)

type strictHandler struct {
	server               StrictServerInterface
	middlewares          []StrictMiddlewareFunc
	requestErrorHandler  func(http.ResponseWriter, *http.Request, error)
	responseErrorHandler func(http.ResponseWriter, *http.Request, error)
}

type StrictHTTPServerOptions struct {
	RequestErrorHandlerFunc  func(http.ResponseWriter, *http.Request, error)
	ResponseErrorHandlerFunc func(http.ResponseWriter, *http.Request, error)
}

type StdHTTPServerOptions struct {
	BaseURL          string
	BaseRouter       ServeMux
	Middlewares      []MiddlewareFunc
	ErrorHandlerFunc func(http.ResponseWriter, *http.Request, error)
}

type (
	GetHealthzRequestObject  struct{}
	GetHealthzResponseObject interface {
		VisitGetHealthzResponse(http.ResponseWriter) error
	}
	GetHealthz200JSONResponse struct {
		Status string `json:"status"`
		NodeId string `json:"nodeId,omitempty"`
		// ActiveStreams: open MemqlService.Stream sessions on this pod.
		// Always serialized (including 0) so the blue/green cutover (#616)
		// can poll it to know when an OLD-color pod has drained.
		ActiveStreams int64                    `json:"activeStreams"`
		NodeType      string                   `json:"nodeType,omitempty"`
		Flavor        string                   `json:"flavor,omitempty"`
		GRPCAddress   string                   `json:"grpcAddress,omitempty"`
		Dependencies  []dependencyHealthStatus `json:"dependencies"`
	}
	GetHealthz503JSONResponse struct {
		Status string `json:"status"`
		NodeId string `json:"nodeId,omitempty"`
		// ActiveStreams: see GetHealthz200JSONResponse. A draining OLD-color
		// pod reports 503 here while activeStreams winds down toward 0 (#616).
		ActiveStreams int64                    `json:"activeStreams"`
		NodeType      string                   `json:"nodeType,omitempty"`
		Flavor        string                   `json:"flavor,omitempty"`
		GRPCAddress   string                   `json:"grpcAddress,omitempty"`
		Dependencies  []dependencyHealthStatus `json:"dependencies"`
	}
	// Automation trigger types
	PostAutomationTriggerRequestObject struct {
		AutomationName string
	}
	PostAutomationTriggerResponseObject interface {
		VisitPostAutomationTriggerResponse(http.ResponseWriter) error
	}
	PostAutomationTrigger202JSONResponse struct {
		ExecutionId      string `json:"executionId"`
		Status           string `json:"status"`
		Message          string `json:"message"`
		InitialChainHead string `json:"initialChainHead,omitempty"`
		ChainHead        string `json:"chainHead,omitempty"`
	}
	// PostAutomationTrigger403JSONResponse is the authorization refusal
	// (memql#2937). Triggering runs an automation's action chain
	// server-side; it is an operator action, gated like resume.
	PostAutomationTrigger403JSONResponse struct {
		Error string `json:"error"`
	}
	PostAutomationTrigger404JSONResponse struct {
		Error string `json:"error"`
	}
	PostAutomationTrigger409JSONResponse struct {
		Error string `json:"error"`
	}

	// Automation resume types
	PostAutomationResumeJSONRequestBody struct {
		ExecutionId string `json:"executionId"`
		FromStep    string `json:"fromStep,omitempty"`
	}
	PostAutomationResumeRequestObject struct {
		Body *PostAutomationResumeJSONRequestBody
	}
	PostAutomationResumeResponseObject interface {
		VisitPostAutomationResumeResponse(http.ResponseWriter) error
	}
	PostAutomationResume202JSONResponse struct {
		ExecutionId  string `json:"executionId"`
		ResumedFrom  string `json:"resumedFrom"`
		StepsSkipped int    `json:"stepsSkipped"`
		Status       string `json:"status"`
		ChainHead    string `json:"chainHead,omitempty"`
	}
	PostAutomationResume400JSONResponse struct {
		Error string `json:"error"`
	}
	// PostAutomationResume403JSONResponse is the authorization refusal
	// (memql#2908). Resuming re-executes a checkpoint's remaining steps
	// through the PRODUCTION step registry under the automation's system
	// actor, so it is an operator action, not a caller action.
	PostAutomationResume403JSONResponse struct {
		Error string `json:"error"`
	}
	PostAutomationResume404JSONResponse struct {
		Error string `json:"error"`
	}
	PostAutomationResume409JSONResponse struct {
		Error string `json:"error"`
	}
	PostAutomationResume410JSONResponse struct {
		Error string `json:"error"`
	}
)

func (resp GetHealthz200JSONResponse) VisitGetHealthzResponse(w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	return json.NewEncoder(w).Encode(resp)
}

func (resp GetHealthz503JSONResponse) VisitGetHealthzResponse(w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	return json.NewEncoder(w).Encode(resp)
}

func (resp PostAutomationTrigger202JSONResponse) VisitPostAutomationTriggerResponse(w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	return json.NewEncoder(w).Encode(resp)
}

func (resp PostAutomationTrigger403JSONResponse) VisitPostAutomationTriggerResponse(w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	return json.NewEncoder(w).Encode(resp)
}

func (resp PostAutomationTrigger404JSONResponse) VisitPostAutomationTriggerResponse(w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	return json.NewEncoder(w).Encode(resp)
}

func (resp PostAutomationTrigger409JSONResponse) VisitPostAutomationTriggerResponse(w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict)
	return json.NewEncoder(w).Encode(resp)
}

func (resp PostAutomationResume202JSONResponse) VisitPostAutomationResumeResponse(w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	return json.NewEncoder(w).Encode(resp)
}

func (resp PostAutomationResume400JSONResponse) VisitPostAutomationResumeResponse(w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	return json.NewEncoder(w).Encode(resp)
}

func (resp PostAutomationResume403JSONResponse) VisitPostAutomationResumeResponse(w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	return json.NewEncoder(w).Encode(resp)
}

func (resp PostAutomationResume404JSONResponse) VisitPostAutomationResumeResponse(w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	return json.NewEncoder(w).Encode(resp)
}

func (resp PostAutomationResume409JSONResponse) VisitPostAutomationResumeResponse(w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict)
	return json.NewEncoder(w).Encode(resp)
}

func (resp PostAutomationResume410JSONResponse) VisitPostAutomationResumeResponse(w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusGone)
	return json.NewEncoder(w).Encode(resp)
}

func NewStrictHandler(si StrictServerInterface, middlewares []StrictMiddlewareFunc) ServerInterface {
	return NewStrictHandlerWithOptions(si, middlewares, StrictHTTPServerOptions{})
}

func NewStrictHandlerWithOptions(si StrictServerInterface, middlewares []StrictMiddlewareFunc, opts StrictHTTPServerOptions) ServerInterface {
	handler := &strictHandler{
		server:               si,
		middlewares:          append([]StrictMiddlewareFunc(nil), middlewares...),
		requestErrorHandler:  defaultStrictRequestErrorHandler,
		responseErrorHandler: defaultStrictResponseErrorHandler,
	}
	if opts.RequestErrorHandlerFunc != nil {
		handler.requestErrorHandler = opts.RequestErrorHandlerFunc
	}
	if opts.ResponseErrorHandlerFunc != nil {
		handler.responseErrorHandler = opts.ResponseErrorHandlerFunc
	}
	return handler
}

func HandlerWithOptions(handler ServerInterface, options StdHTTPServerOptions) http.Handler {
	if handler == nil || handler.server == nil {
		return http.NewServeMux()
	}

	baseRouter := options.BaseRouter
	if baseRouter == nil {
		baseRouter = http.NewServeMux()
	}

	basePath := strings.TrimSuffix(strings.TrimSpace(options.BaseURL), "/")

	registerRoute(baseRouter, http.MethodGet, joinPath(basePath, "/healthz"), handler.getHealthz())
	// /readyz asserts critical schema presence (#657); plain handler, outside
	// the OpenAPI strict surface, public + unauthenticated (see PublicPaths).
	registerRoute(baseRouter, http.MethodGet, joinPath(basePath, "/readyz"), ReadyzHandler())
	// /livez is a pure process-liveness probe (#1117): 200 while the process
	// can serve HTTP, independent of dependency health AND draining, so a
	// transient mesh/dep blip never liveness-kills an otherwise-alive pod.
	registerRoute(baseRouter, http.MethodGet, joinPath(basePath, "/livez"), LivezHandler())
	registerAutomationTriggerRoute(baseRouter, basePath, handler.postAutomationTrigger(basePath))
	registerRoute(baseRouter, http.MethodPost, joinPath(basePath, "/automations/resume"), handler.postAutomationResume())

	var final http.Handler = baseRouter
	for i := len(options.Middlewares) - 1; i >= 0; i-- {
		if mw := options.Middlewares[i]; mw != nil {
			final = mw(final)
		}
	}
	return final
}

func (h *strictHandler) applyMiddlewares(final StrictHandlerFunc, operationId string) StrictHandlerFunc {
	wrapped := final
	for i := len(h.middlewares) - 1; i >= 0; i-- {
		wrapped = h.middlewares[i](wrapped, operationId)
	}
	return wrapped
}

func (h *strictHandler) getHealthz() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			methodNotAllowed(w, r, http.MethodGet)
			return
		}
		handler := h.applyMiddlewares(func(ctx context.Context, _ http.ResponseWriter, _ *http.Request, _ interface{}) (interface{}, error) {
			return h.server.GetHealthz(ctx, GetHealthzRequestObject{})
		}, "GetHealthz")
		resp, err := handler(r.Context(), w, r, GetHealthzRequestObject{})
		h.writeResponse(w, r, resp, err, func(w http.ResponseWriter, resp interface{}) error {
			return resp.(GetHealthzResponseObject).VisitGetHealthzResponse(w)
		})
	}
}

func (h *strictHandler) postAutomationTrigger(basePath string) http.HandlerFunc {
	// Build the prefix that comes before {name} and the suffix after it
	prefix := joinPath(basePath, "/automations/")
	suffix := "/trigger"

	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			methodNotAllowed(w, r, http.MethodPost)
			return
		}

		// Extract automation name from path: {basePath}/automations/{name}/trigger
		name := extractAutomationNameFromPath(r.URL.Path, prefix, suffix)
		if name == "" {
			http.Error(w, "automation name required in path", http.StatusBadRequest)
			return
		}

		req := PostAutomationTriggerRequestObject{AutomationName: name}
		handler := h.applyMiddlewares(func(ctx context.Context, _ http.ResponseWriter, _ *http.Request, request interface{}) (interface{}, error) {
			return h.server.PostAutomationTrigger(ctx, request.(PostAutomationTriggerRequestObject))
		}, "PostAutomationTrigger")
		resp, err := handler(r.Context(), w, r, req)
		h.writeResponse(w, r, resp, err, func(w http.ResponseWriter, resp interface{}) error {
			return resp.(PostAutomationTriggerResponseObject).VisitPostAutomationTriggerResponse(w)
		})
	}
}

func (h *strictHandler) postAutomationResume() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			methodNotAllowed(w, r, http.MethodPost)
			return
		}

		// Parse JSON body
		var body PostAutomationResumeJSONRequestBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteSafeError(w, r, nil, http.StatusBadRequest, "bad_request", "invalid request body", err)
			return
		}

		req := PostAutomationResumeRequestObject{Body: &body}
		handler := h.applyMiddlewares(func(ctx context.Context, _ http.ResponseWriter, _ *http.Request, request interface{}) (interface{}, error) {
			return h.server.PostAutomationResume(ctx, request.(PostAutomationResumeRequestObject))
		}, "PostAutomationResume")
		resp, err := handler(r.Context(), w, r, req)
		h.writeResponse(w, r, resp, err, func(w http.ResponseWriter, resp interface{}) error {
			return resp.(PostAutomationResumeResponseObject).VisitPostAutomationResumeResponse(w)
		})
	}
}

func (h *strictHandler) writeResponse(w http.ResponseWriter, r *http.Request, resp interface{}, err error, visit func(http.ResponseWriter, interface{}) error) {
	if err != nil {
		h.responseErrorHandler(w, r, err)
		return
	}
	if resp == nil {
		return
	}
	if err := visit(w, resp); err != nil {
		h.responseErrorHandler(w, r, err)
	}
}

func registerRoute(m ServeMux, method, path string, handler http.HandlerFunc) {
	if path == "" {
		path = "/"
	}
	m.HandleFunc(method+" "+path, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			methodNotAllowed(w, r, method)
			return
		}
		handler(w, r)
	})
}

// registerAutomationTriggerRoute registers the automation trigger endpoint.
// Uses a prefix pattern to match /automations/{name}/trigger paths.
func registerAutomationTriggerRoute(m ServeMux, basePath string, handler http.HandlerFunc) {
	// Register with trailing slash to match any /automations/* path
	prefix := joinPath(basePath, "/automations/")
	m.HandleFunc("POST "+prefix, handler)
	// Register plain prefix to support muxes that don't include method in the pattern.
	m.HandleFunc(prefix, handler)
}

// extractAutomationNameFromPath extracts the automation name from a path like
// {basePath}/automations/{name}/trigger. Returns empty string if the path doesn't match.
func extractAutomationNameFromPath(path, prefix, suffix string) string {
	// Path must start with prefix and end with suffix
	if !strings.HasPrefix(path, prefix) {
		return ""
	}
	if !strings.HasSuffix(path, suffix) {
		return ""
	}

	// Extract the name between prefix and suffix
	name := path[len(prefix) : len(path)-len(suffix)]

	// Validate: name should not be empty or contain path separators
	name = strings.TrimSpace(name)
	if name == "" || strings.Contains(name, "/") {
		return ""
	}

	return name
}

func joinPath(base, suffix string) string {
	if base == "" || base == "/" {
		return suffix
	}
	return strings.TrimRight(base, "/") + suffix
}

func methodNotAllowed(w http.ResponseWriter, _ *http.Request, allowed string) {
	w.Header().Set("Allow", allowed)
	http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
}
