package app

import (
	"context"
	"net/http"

	coreprotocol "work2api/internal/core/protocol"
	"work2api/internal/core/provider"
	"work2api/internal/streamwatch"
	wbruntime "work2api/internal/workbuddy/runtime"
)

var sseWriter = streamwatch.SSEWriter

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
	payload = coreprotocol.NormalizeInput(coreprotocol.Chat, payload)
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
	payload = coreprotocol.NormalizeInput(coreprotocol.Protocol(protocol), payload)
	if aerr := s.prepareModel(principal, payload); aerr != nil {
		writeAPIErr(w, aerr)
		return
	}
	if protocol == "systemone" {
		rt, ok := s.o.runtimes.Resolve(principal.EffectiveModel)
		supported := false
		if ok && principal.RouteModel == "" {
			for _, m := range rt.Models(r.Context()) {
				if s.o.resolveModel(m.ID) == principal.EffectiveModel && m.Extra["native_protocol"] == "systemone" {
					supported = true
					break
				}
			}
		}
		if !supported {
			writeAPIErr(w, errBody(400, "请选择原生 SystemOne 决策模型，不支持聊天模型或合并路由", "invalid_request_error"))
			return
		}
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
	} else if protocol == "systemone" {
		proto = provider.ProtocolSystemOne
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
	if p.RouteModel != "" {
		return withRequestSessionIdentity(r, payload), func() {}, nil
	}
	rt, ok := s.o.runtimes.Resolve(p.EffectiveModel)
	if !ok {
		return r, func() {}, nil
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
