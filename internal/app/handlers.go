package app

import (
	"context"
	"net/http"

	"work2api/internal/core/provider"
	wbruntime "work2api/internal/workbuddy/runtime"
)

var sseWriter = wbruntime.SSEWriter

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	principal, aerr := s.auth(r)
	if aerr != nil {
		writeAPIErr(w, aerr)
		return
	}
	payload, err := readJSON(r)
	if err != nil {
		writeJSON(w, 400, errBody(400, "bad json", "invalid_request_error").Body)
		return
	}
	if _, ok := payload["messages"]; !ok {
		writeJSON(w, 400, errBody(400, "messages is required", "invalid_request_error").Body)
		return
	}
	if aerr := s.prepareModel(principal, payload); aerr != nil {
		writeAPIErr(w, aerr)
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), requestStreamKey{}, boolVal(payload["stream"])))

	var finishSession func()
	r, finishSession, err = s.prepareProviderRequest(r, payload, principal)
	if err != nil {
		st, body := errToHTTP(err)
		writeJSON(w, st, body)
		return
	}
	defer finishSession()
	r, release, admitted := s.admitModel(w, r, principal)
	if !admitted {
		return
	}
	defer release()
	var refreshed bool
	r, refreshed = s.refreshPortalRequest(w, r, principal, payload)
	if !refreshed {
		return
	}
	if !s.reservePortalBudget(w, principal, payload) {
		return
	}
	defer principal.quota.settle()
	if s.dispatchRuntime(w, r, provider.ProtocolChat, payload, principal) {
		return
	}
	writeAPIErr(w, errBody(503, "模型渠道未就绪", "model_unavailable"))
}

// runChatPath is the default (workbuddy) chat path with an injectable writer,
// so the model test endpoint can capture the response with a recorder.
func (s *Server) runChatPath(w http.ResponseWriter, r *http.Request, payload map[string]any, principal *Principal) {
	s.dispatchWorkBuddy(w, r, provider.ProtocolChat, payload, principal)
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	s.handleConverted(w, r, "anthropic")
}

func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	s.handleConverted(w, r, "responses")
}

// converter is the shared interface of the two stream converters.
func (s *Server) handleConverted(w http.ResponseWriter, r *http.Request, protocol string) {
	principal, aerr := s.auth(r)
	if aerr != nil {
		writeAPIErr(w, aerr)
		return
	}
	payload, err := readJSON(r)
	if err != nil {
		writeJSON(w, 400, errBody(400, "bad json", "invalid_request_error").Body)
		return
	}
	if aerr := s.prepareModel(principal, payload); aerr != nil {
		writeAPIErr(w, aerr)
		return
	}
	streamHint := false
	if value, ok := payload["stream"]; ok {
		streamHint = boolVal(value)
	}
	r = r.WithContext(context.WithValue(r.Context(), requestStreamKey{}, streamHint))

	var finishSession func()
	r, finishSession, err = s.prepareProviderRequest(r, payload, principal)
	if err != nil {
		st, body := errToHTTP(err)
		writeJSON(w, st, body)
		return
	}
	defer finishSession()
	r, release, admitted := s.admitModel(w, r, principal)
	if !admitted {
		return
	}
	defer release()
	var refreshed bool
	r, refreshed = s.refreshPortalRequest(w, r, principal, payload)
	if !refreshed {
		return
	}
	if !s.reservePortalBudget(w, principal, payload) {
		return
	}
	defer principal.quota.settle()
	// Route namespaced models to their provider runtime (it does its own
	// protocol conversion from the original anthropic/responses payload).
	proto := provider.ProtocolAnthropic
	if protocol == "responses" {
		proto = provider.ProtocolResponses
	}
	if s.dispatchRuntime(w, r, proto, payload, principal) {
		return
	}
	writeAPIErr(w, errBody(503, "模型渠道未就绪", "model_unavailable"))
}

var errToHTTP = wbruntime.ErrToHTTP
var collectSummary = wbruntime.CollectSummary
var boolVal = wbruntime.BoolVal
var strOr = wbruntime.StrOr
var appendLogText = wbruntime.AppendLogText
var isLocalOverload = wbruntime.IsLocalOverload

func (s *Server) prepareProviderRequest(r *http.Request, payload map[string]any, p *Principal) (*http.Request, func(), error) {
	rt, ok := s.o.runtimes.Resolve(p.EffectiveModel)
	if !ok {
		rt = s.o.wb
	}
	if prepare, ok := rt.(provider.RequestPreparer); ok {
		body := make(map[string]any, len(payload))
		for k, v := range payload {
			body[k] = v
		}
		body["model"] = p.EffectiveModel
		ctx, finish, err := prepare.PrepareRequest(r.Context(), provider.ServeRequest{Caller: p.SessionCaller(), Payload: body, Headers: r.Header})
		return r.WithContext(ctx), finish, err
	}
	return r, func() {}, nil
}
