// Package httpapi exposes the stable v1 HTTP and SSE contract.
package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	v1 "github.com/amirulashraf/parrot-coder/internal/api/v1"
	"github.com/amirulashraf/parrot-coder/internal/diagnostics"
)

const defaultMaxBodyBytes int64 = 1 << 20

type LogRecord struct {
	RequestID string
	Method    string
	Path      string
	Status    int
	Duration  time.Duration
	ErrorRef  string
}

type Logger interface {
	Log(context.Context, LogRecord)
}

type LoggerFunc func(context.Context, LogRecord)

func (f LoggerFunc) Log(ctx context.Context, record LogRecord) { f(ctx, record) }

type Config struct {
	MaxBodyBytes      int64
	HeartbeatInterval time.Duration
	Logger            Logger
}

type Server struct {
	backend Backend
	config  Config
	mux     *http.ServeMux
	openapi []byte
}

type Route struct {
	Method      string
	Path        string
	OperationID string
}

var routes = []Route{
	{"GET", "/api/v1/health", "getHealth"},
	{"GET", "/api/v1/runtime", "getRuntime"},
	{"GET", "/api/v1/sessions", "listSessions"},
	{"POST", "/api/v1/sessions", "createSession"},
	{"GET", "/api/v1/sessions/{id}", "getSession"},
	{"DELETE", "/api/v1/sessions/{id}", "deleteSession"},
	{"PUT", "/api/v1/sessions/{id}/selection", "updateSessionSelection"},
	{"GET", "/api/v1/sessions/{id}/messages", "listMessages"},
	{"GET", "/api/v1/sessions/{id}/todos", "listTodos"},
	{"POST", "/api/v1/sessions/{id}/prompts", "createPrompt"},
	{"POST", "/api/v1/sessions/{id}/compact", "compactSession"},
	{"POST", "/api/v1/sessions/{id}/interrupt", "interruptSession"},
	{"GET", "/api/v1/sessions/{id}/events", "streamSessionEvents"},
	{"GET", "/api/v1/sessions/{id}/permissions", "listPermissions"},
	{"POST", "/api/v1/sessions/{id}/permissions/{request}/reply", "replyPermission"},
	{"GET", "/api/v1/sessions/{id}/questions", "listQuestions"},
	{"POST", "/api/v1/sessions/{id}/questions/{request}/reply", "replyQuestion"},
	{"POST", "/api/v1/sessions/{id}/undo", "undo"},
	{"POST", "/api/v1/sessions/{id}/redo", "redo"},
	{"GET", "/api/v1/models", "listModels"},
	{"GET", "/api/v1/usage", "getSubscriptionUsage"},
	{"GET", "/api/v1/agents", "listAgents"},
	{"GET", "/api/v1/modes", "listModes"},
	{"GET", "/openapi.json", "getOpenAPI"},
}

func Routes() []Route { return append([]Route(nil), routes...) }

func New(backend Backend, config Config) *Server {
	if config.MaxBodyBytes <= 0 {
		config.MaxBodyBytes = defaultMaxBodyBytes
	}
	if config.HeartbeatInterval <= 0 {
		config.HeartbeatInterval = 15 * time.Second
	}
	s := &Server{backend: backend, config: config, mux: http.NewServeMux()}
	s.openapi = buildOpenAPI()
	s.mux.HandleFunc("/api/v1/health", s.health)
	s.mux.HandleFunc("/api/v1/runtime", s.runtime)
	s.mux.HandleFunc("/api/v1/sessions", s.sessions)
	s.mux.HandleFunc("/api/v1/sessions/{id}", s.session)
	s.mux.HandleFunc("/api/v1/sessions/{id}/selection", s.selection)
	s.mux.HandleFunc("/api/v1/sessions/{id}/messages", s.messages)
	s.mux.HandleFunc("/api/v1/sessions/{id}/todos", s.todos)
	s.mux.HandleFunc("/api/v1/sessions/{id}/prompts", s.prompts)
	s.mux.HandleFunc("/api/v1/sessions/{id}/compact", s.compact)
	s.mux.HandleFunc("/api/v1/sessions/{id}/interrupt", s.interrupt)
	s.mux.HandleFunc("/api/v1/sessions/{id}/events", s.events)
	s.mux.HandleFunc("/api/v1/sessions/{id}/permissions", s.permissions)
	s.mux.HandleFunc("/api/v1/sessions/{id}/permissions/{request}/reply", s.permissionReply)
	s.mux.HandleFunc("/api/v1/sessions/{id}/questions", s.questions)
	s.mux.HandleFunc("/api/v1/sessions/{id}/questions/{request}/reply", s.questionReply)
	s.mux.HandleFunc("/api/v1/sessions/{id}/undo", s.undo)
	s.mux.HandleFunc("/api/v1/sessions/{id}/redo", s.redo)
	s.mux.HandleFunc("/api/v1/models", s.models)
	s.mux.HandleFunc("/api/v1/usage", s.subscriptionUsage)
	s.mux.HandleFunc("/api/v1/agents", s.agents)
	s.mux.HandleFunc("/api/v1/modes", s.modes)
	s.mux.HandleFunc("/openapi.json", s.openAPIDocument)
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestID := newOpaqueID("req")
	w.Header().Set("X-Request-ID", requestID)
	r = r.WithContext(context.WithValue(r.Context(), requestIDKey{}, requestID))
	tracker := &responseTracker{ResponseWriter: w}
	started := time.Now()
	defer func() {
		if recovered := recover(); recovered != nil {
			diagnostics.Panic("httpapi", recovered)
			if !tracker.wroteHeader {
				s.writeProblem(tracker, r, internalProblem(requestID))
			}
		}
		if s.config.Logger != nil {
			s.config.Logger.Log(r.Context(), LogRecord{RequestID: requestID, Method: r.Method, Path: r.URL.Path, Status: tracker.statusCode(), Duration: time.Since(started), ErrorRef: tracker.errorRef})
		}
	}()
	_, pattern := s.mux.Handler(r)
	if pattern == "" {
		s.writeProblem(tracker, r, problem(requestID, http.StatusNotFound, "route_not_found", "Route not found", "The requested route does not exist."))
		return
	}
	s.mux.ServeHTTP(tracker, r)
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if !s.requireMethod(w, r, http.MethodGet) {
		return
	}
	s.writeJSON(w, http.StatusOK, v1.Health{Status: "ok"})
}

func (s *Server) runtime(w http.ResponseWriter, r *http.Request) {
	if !s.requireMethod(w, r, http.MethodGet) {
		return
	}
	item, err := s.backend.Runtime(r.Context())
	s.respond(w, r, http.StatusOK, item, err)
}

func (s *Server) sessions(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		item, err := s.backend.ListSessions(r.Context())
		if err == nil {
			item, err = paginateSessions(r, item)
		}
		s.respond(w, r, http.StatusOK, item, err)
	case http.MethodPost:
		var request v1.CreateSessionRequest
		if !s.decode(w, r, &request) {
			return
		}
		item, err := s.backend.CreateSession(r.Context(), request)
		if err == nil {
			w.Header().Set("Location", "/api/v1/sessions/"+item.ID)
		}
		s.respond(w, r, http.StatusCreated, item, err)
	default:
		s.methodProblem(w, r, http.MethodGet+", "+http.MethodPost)
	}
}

func (s *Server) session(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		item, err := s.backend.GetSession(r.Context(), r.PathValue("id"))
		s.respond(w, r, http.StatusOK, item, err)
	case http.MethodDelete:
		err := s.backend.DeleteSession(r.Context(), r.PathValue("id"))
		s.respondEmpty(w, r, err)
	default:
		s.methodProblem(w, r, http.MethodGet+", "+http.MethodDelete)
	}
}

func (s *Server) selection(w http.ResponseWriter, r *http.Request) {
	if !s.requireMethod(w, r, http.MethodPut) {
		return
	}
	var request v1.UpdateSessionSelectionRequest
	if !s.decode(w, r, &request) {
		return
	}
	if request.Agent == "" && request.Model == "" && request.Variant == nil {
		s.writeProblem(w, r, invalidProblem(requestID(r), "agent, model, or variant is required."))
		return
	}
	item, err := s.backend.UpdateSessionSelection(r.Context(), r.PathValue("id"), request)
	s.respond(w, r, http.StatusOK, item, err)
}

func (s *Server) messages(w http.ResponseWriter, r *http.Request) {
	if !s.requireMethod(w, r, http.MethodGet) {
		return
	}
	item, err := s.backend.ListMessages(r.Context(), r.PathValue("id"))
	if err == nil {
		item, err = paginateMessages(r, item)
	}
	s.respond(w, r, http.StatusOK, item, err)
}

func (s *Server) todos(w http.ResponseWriter, r *http.Request) {
	if !s.requireMethod(w, r, http.MethodGet) {
		return
	}
	item, err := s.backend.ListTodos(r.Context(), r.PathValue("id"))
	s.respond(w, r, http.StatusOK, item, err)
}

func (s *Server) prompts(w http.ResponseWriter, r *http.Request) {
	if !s.requireMethod(w, r, http.MethodPost) {
		return
	}
	var request v1.PromptRequest
	if !s.decode(w, r, &request) {
		return
	}
	if request.MessageID == "" || request.Content == "" || request.Delivery != "steer" && request.Delivery != "queue" {
		s.writeProblem(w, r, invalidProblem(requestID(r), "message_id, content, and a delivery of steer or queue are required."))
		return
	}
	item, err := s.backend.AdmitPrompt(r.Context(), r.PathValue("id"), request)
	if err != nil {
		s.writeBackendError(w, r, err)
		return
	}
	s.backend.Wake(r.PathValue("id"))
	s.writeJSON(w, http.StatusAccepted, item)
}

type compactionBackend interface {
	CompactSession(context.Context, string) (v1.Compaction, error)
}

func (s *Server) compact(w http.ResponseWriter, r *http.Request) {
	if !s.requireMethod(w, r, http.MethodPost) || !s.requireEmptyBody(w, r) {
		return
	}
	backend, ok := s.backend.(compactionBackend)
	if !ok {
		s.writeBackendError(w, r, errors.New("httpapi: compaction is unavailable"))
		return
	}
	item, err := backend.CompactSession(r.Context(), r.PathValue("id"))
	s.respond(w, r, http.StatusAccepted, item, err)
}

func (s *Server) interrupt(w http.ResponseWriter, r *http.Request) {
	if !s.requireMethod(w, r, http.MethodPost) || !s.requireEmptyBody(w, r) {
		return
	}
	s.respondEmpty(w, r, s.backend.Interrupt(r.Context(), r.PathValue("id")))
}

func (s *Server) permissions(w http.ResponseWriter, r *http.Request) {
	if !s.requireMethod(w, r, http.MethodGet) {
		return
	}
	item, err := s.backend.ListPermissions(r.Context(), r.PathValue("id"))
	s.respond(w, r, http.StatusOK, item, err)
}

func (s *Server) permissionReply(w http.ResponseWriter, r *http.Request) {
	if !s.requireMethod(w, r, http.MethodPost) {
		return
	}
	var request v1.PermissionReply
	if !s.decode(w, r, &request) {
		return
	}
	validScope := request.Scope == "" || request.Scope == "process" || request.Scope == "session" || request.Scope == "workspace" || request.Scope == "yolo"
	if request.Decision != "allow" && request.Decision != "deny" || request.Decision == "deny" && request.Scope != "" || !validScope {
		s.writeProblem(w, r, invalidProblem(requestID(r), "decision must be allow or deny, and denied replies cannot have a scope."))
		return
	}
	s.respondEmpty(w, r, s.backend.ReplyPermission(r.Context(), r.PathValue("id"), r.PathValue("request"), request))
}

func (s *Server) questions(w http.ResponseWriter, r *http.Request) {
	if !s.requireMethod(w, r, http.MethodGet) {
		return
	}
	item, err := s.backend.ListQuestions(r.Context(), r.PathValue("id"))
	s.respond(w, r, http.StatusOK, item, err)
}

func (s *Server) questionReply(w http.ResponseWriter, r *http.Request) {
	if !s.requireMethod(w, r, http.MethodPost) {
		return
	}
	var request v1.QuestionReply
	if !s.decode(w, r, &request) {
		return
	}
	if request.Reject == (len(request.Answers) > 0) {
		s.writeProblem(w, r, invalidProblem(requestID(r), "provide answers or reject the question request."))
		return
	}
	s.respondEmpty(w, r, s.backend.ReplyQuestion(r.Context(), r.PathValue("id"), r.PathValue("request"), request))
}

func (s *Server) undo(w http.ResponseWriter, r *http.Request) {
	if !s.requireMethod(w, r, http.MethodPost) || !s.requireEmptyBody(w, r) {
		return
	}
	item, err := s.backend.Undo(r.Context(), r.PathValue("id"))
	s.respond(w, r, http.StatusOK, item, err)
}

func (s *Server) redo(w http.ResponseWriter, r *http.Request) {
	if !s.requireMethod(w, r, http.MethodPost) || !s.requireEmptyBody(w, r) {
		return
	}
	item, err := s.backend.Redo(r.Context(), r.PathValue("id"))
	s.respond(w, r, http.StatusOK, item, err)
}

func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	if !s.requireMethod(w, r, http.MethodGet) {
		return
	}
	item, err := s.backend.ListModels(r.Context())
	s.respond(w, r, http.StatusOK, item, err)
}

func (s *Server) subscriptionUsage(w http.ResponseWriter, r *http.Request) {
	if !s.requireMethod(w, r, http.MethodGet) {
		return
	}
	item, err := s.backend.SubscriptionUsage(r.Context())
	s.respond(w, r, http.StatusOK, item, err)
}

func (s *Server) agents(w http.ResponseWriter, r *http.Request) {
	if !s.requireMethod(w, r, http.MethodGet) {
		return
	}
	item, err := s.backend.ListAgents(r.Context())
	s.respond(w, r, http.StatusOK, item, err)
}

func (s *Server) modes(w http.ResponseWriter, r *http.Request) {
	if !s.requireMethod(w, r, http.MethodGet) {
		return
	}
	lister, ok := s.backend.(interface {
		ListModes(context.Context) (v1.ModeList, error)
	})
	if !ok {
		s.respond(w, r, http.StatusOK, v1.ModeList{Items: []v1.Mode{}}, nil)
		return
	}
	item, err := lister.ListModes(r.Context())
	s.respond(w, r, http.StatusOK, item, err)
}

func (s *Server) openAPIDocument(w http.ResponseWriter, r *http.Request) {
	if !s.requireMethod(w, r, http.MethodGet) {
		return
	}
	w.Header().Set("Content-Type", v1.MediaTypeJSON)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(s.openapi)
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	if !s.requireMethod(w, r, http.MethodGet) {
		return
	}
	after := int64(-1)
	if raw := r.URL.Query().Get("after"); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value < -1 {
			s.writeProblem(w, r, invalidProblem(requestID(r), "after must be an event sequence."))
			return
		}
		after = value
	}
	stream, err := s.backend.OpenEvents(r.Context(), r.PathValue("id"), after)
	if err != nil {
		s.writeBackendError(w, r, err)
		return
	}
	defer stream.Close()
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.writeProblem(w, r, internalProblem(requestID(r)))
		return
	}
	w.Header().Set("Content-Type", v1.MediaTypeSSE)
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	connected := v1.Event{ID: newOpaqueID("evt"), Type: v1.EventServerConnected, SessionID: r.PathValue("id"), Data: json.RawMessage(`{}`)}
	if writeSSE(w, flusher, connected) != nil {
		return
	}
	for _, item := range stream.Replay {
		if writeSSE(w, flusher, item) != nil {
			return
		}
	}
	activeAssistant, lifecycleKnown := replayAssistantState(stream.Replay)
	var deferredLive []v1.Event
	writeLive := func(item v1.Event) error {
		return writeSSE(w, flusher, item)
	}
	deferOrWriteLive := func(item v1.Event) error {
		messageID := liveEventMessageID(item)
		if messageID == "" || messageID == activeAssistant {
			return writeLive(item)
		}
		if activeAssistant == "" && !lifecycleKnown {
			// A client may resume after session.assistant.started. Adopt the first
			// scoped live event until a durable lifecycle event catches up.
			activeAssistant = messageID
			lifecycleKnown = true
			return writeLive(item)
		}
		deferredLive = append(deferredLive, item)
		return nil
	}
	flushDeferred := func() error {
		for i := 0; i < len(deferredLive); {
			if liveEventMessageID(deferredLive[i]) != activeAssistant {
				i++
				continue
			}
			item := deferredLive[i]
			deferredLive = append(deferredLive[:i], deferredLive[i+1:]...)
			if err := writeLive(item); err != nil {
				return err
			}
		}
		return nil
	}
	drainLiveFor := func(messageID string) error {
		for {
			select {
			case item, ok := <-stream.Live:
				if !ok {
					return io.EOF
				}
				itemMessageID := liveEventMessageID(item)
				if itemMessageID == "" || itemMessageID == messageID {
					if err := writeLive(item); err != nil {
						return err
					}
				} else {
					deferredLive = append(deferredLive, item)
				}
			default:
				return nil
			}
		}
	}
	heartbeat := time.NewTicker(s.config.HeartbeatInterval)
	defer heartbeat.Stop()
	for {
		select {
		case item, ok := <-stream.Durable:
			if !ok {
				return
			}
			// Live deltas are published before the durable completion they lead
			// to. Drain only that assistant's ready events: blindly draining the
			// shared queue can pull the next tool-turn assistant ahead of this
			// completion and make the terminal switch streams prematurely.
			if settledID := settledAssistantID(item); settledID != "" {
				if err := drainLiveFor(settledID); err != nil {
					return
				}
			}
			if writeSSE(w, flusher, item) != nil {
				return
			}
			if messageID := startedAssistantID(item); messageID != "" {
				activeAssistant = messageID
				lifecycleKnown = true
				if err := flushDeferred(); err != nil {
					return
				}
			} else if messageID := settledAssistantID(item); messageID != "" {
				if activeAssistant == messageID {
					activeAssistant = ""
				}
				lifecycleKnown = true
			}
		case item, ok := <-stream.Live:
			if !ok {
				return
			}
			if deferOrWriteLive(item) != nil {
				return
			}
		case <-heartbeat.C:
			if _, err := io.WriteString(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func replayAssistantState(events []v1.Event) (string, bool) {
	active := ""
	known := false
	for _, item := range events {
		if messageID := startedAssistantID(item); messageID != "" {
			active, known = messageID, true
		} else if messageID := settledAssistantID(item); messageID != "" {
			if active == messageID {
				active = ""
			}
			known = true
		}
	}
	return active, known
}

func startedAssistantID(item v1.Event) string {
	if item.Type != "session.assistant.started" {
		return ""
	}
	return eventMessageID(item)
}

func settledAssistantID(item v1.Event) string {
	switch item.Type {
	case "session.assistant.complete", "session.assistant.error", "session.assistant.interrupted":
		return eventMessageID(item)
	default:
		return ""
	}
}

func liveEventMessageID(item v1.Event) string {
	switch item.Type {
	case v1.EventMessagePartDelta, v1.EventSessionStatus:
		return eventMessageID(item)
	default:
		return ""
	}
}

func eventMessageID(item v1.Event) string {
	var payload struct {
		MessageID string `json:"message_id"`
	}
	if json.Unmarshal(item.Data, &payload) != nil {
		return ""
	}
	return payload.MessageID
}

func writeSSE(w io.Writer, flusher http.Flusher, event v1.Event) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if _, err = fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", event.ID, event.Type, data); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

func (s *Server) decode(w http.ResponseWriter, r *http.Request, target any) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != v1.MediaTypeJSON {
		s.writeProblem(w, r, problem(requestID(r), http.StatusUnsupportedMediaType, "unsupported_media_type", "Unsupported media type", "Content-Type must be application/json."))
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.config.MaxBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		if isBodyTooLarge(err) {
			s.writeProblem(w, r, problem(requestID(r), http.StatusRequestEntityTooLarge, "body_too_large", "Request body too large", "The request body exceeds the configured limit."))
			return false
		}
		s.writeProblem(w, r, invalidProblem(requestID(r), "The JSON request body is invalid."))
		return false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if isBodyTooLarge(err) {
			s.writeProblem(w, r, problem(requestID(r), http.StatusRequestEntityTooLarge, "body_too_large", "Request body too large", "The request body exceeds the configured limit."))
			return false
		}
		s.writeProblem(w, r, invalidProblem(requestID(r), "The request body contains trailing data."))
		return false
	}
	return true
}

func isBodyTooLarge(err error) bool {
	var tooLarge *http.MaxBytesError
	return errors.As(err, &tooLarge)
}

func pageRequest(r *http.Request) (string, int, error) {
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 100 {
			return "", 0, ErrInvalid
		}
		limit = value
	}
	return r.URL.Query().Get("cursor"), limit, nil
}

func paginateSessions(r *http.Request, list v1.SessionList) (v1.SessionList, error) {
	cursor, limit, err := pageRequest(r)
	if err != nil {
		return v1.SessionList{}, err
	}
	start, err := cursorStart(cursor, "session", len(list.Items), func(i int) string { return list.Items[i].ID })
	if err != nil {
		return v1.SessionList{}, err
	}
	end := min(start+limit, len(list.Items))
	out := v1.SessionList{Items: append([]v1.Session(nil), list.Items[start:end]...)}
	if end < len(list.Items) {
		out.NextCursor = makeCursor("session", list.Items[end-1].ID)
	}
	return out, nil
}

func paginateMessages(r *http.Request, list v1.MessageList) (v1.MessageList, error) {
	cursor, limit, err := pageRequest(r)
	if err != nil {
		return v1.MessageList{}, err
	}
	start, err := cursorStart(cursor, "message", len(list.Items), func(i int) string { return list.Items[i].ID })
	if err != nil {
		return v1.MessageList{}, err
	}
	end := min(start+limit, len(list.Items))
	out := v1.MessageList{Items: append([]v1.Message(nil), list.Items[start:end]...)}
	if end < len(list.Items) {
		out.NextCursor = makeCursor("message", list.Items[end-1].ID)
	}
	return out, nil
}

func cursorStart(cursor, kind string, length int, idAt func(int) string) (int, error) {
	if cursor == "" {
		return 0, nil
	}
	data, err := base64.RawURLEncoding.DecodeString(cursor)
	prefix := kind + "\x00"
	if err != nil || !strings.HasPrefix(string(data), prefix) {
		return 0, ErrInvalid
	}
	id := strings.TrimPrefix(string(data), prefix)
	for i := 0; i < length; i++ {
		if idAt(i) == id {
			return i + 1, nil
		}
	}
	return 0, ErrInvalid
}

func makeCursor(kind, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(kind + "\x00" + id))
}

func (s *Server) requireEmptyBody(w http.ResponseWriter, r *http.Request) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1)
	data, err := io.ReadAll(r.Body)
	if err != nil || len(strings.TrimSpace(string(data))) != 0 {
		s.writeProblem(w, r, invalidProblem(requestID(r), "This operation does not accept a request body."))
		return false
	}
	return true
}

func (s *Server) requireMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == method {
		return true
	}
	s.methodProblem(w, r, method)
	return false
}

func (s *Server) methodProblem(w http.ResponseWriter, r *http.Request, allow string) {
	w.Header().Set("Allow", allow)
	s.writeProblem(w, r, problem(requestID(r), http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed", "The request method is not supported for this route."))
}

func (s *Server) respond(w http.ResponseWriter, r *http.Request, status int, value any, err error) {
	if err != nil {
		s.writeBackendError(w, r, err)
		return
	}
	s.writeJSON(w, status, value)
}

func (s *Server) respondEmpty(w http.ResponseWriter, r *http.Request, err error) {
	if err != nil {
		s.writeBackendError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) writeBackendError(w http.ResponseWriter, r *http.Request, err error) {
	id := requestID(r)
	switch {
	case errors.Is(err, ErrNotFound):
		s.writeProblem(w, r, problem(id, http.StatusNotFound, "session_not_found", "Session not found", "The requested resource does not exist."))
	case errors.Is(err, ErrInvalid):
		s.writeProblem(w, r, invalidProblem(id, "The request is not valid for this operation."))
	case errors.Is(err, ErrConflict):
		s.writeProblem(w, r, problem(id, http.StatusConflict, "conflict", "Conflict", "The request conflicts with current state."))
	case errors.Is(err, ErrModelRequired):
		s.writeProblem(w, r, problem(id, http.StatusConflict, "model_required", "Model required", "Select an agent and model before creating or prompting a coding session."))
	case errors.Is(err, ErrSessionActive):
		s.writeProblem(w, r, problem(id, http.StatusConflict, "session_active", "Session active", "Selection cannot change while the session is running."))
	case errors.Is(err, ErrInvalidSelection):
		s.writeProblem(w, r, problem(id, http.StatusBadRequest, "invalid_selection", "Invalid selection", "The requested agent or model is not available."))
	case errors.Is(err, ErrIdempotencyConflict):
		s.writeProblem(w, r, problem(id, http.StatusConflict, "idempotency_conflict", "Idempotency conflict", "The message ID was already used for a different prompt."))
	case errors.Is(err, ErrPermissionNotFound):
		s.writeProblem(w, r, problem(id, http.StatusNotFound, "permission_not_found", "Permission request not found", "The permission request does not exist or is already settled."))
	case errors.Is(err, ErrQuestionNotFound):
		s.writeProblem(w, r, problem(id, http.StatusNotFound, "question_not_found", "Question request not found", "The question request does not exist or is already settled."))
	case errors.Is(err, ErrNoUndo):
		s.writeProblem(w, r, problem(id, http.StatusConflict, "nothing_to_undo", "Nothing to undo", "There is no transaction to undo."))
	case errors.Is(err, ErrNoRedo):
		s.writeProblem(w, r, problem(id, http.StatusConflict, "nothing_to_redo", "Nothing to redo", "There is no transaction to redo."))
	default:
		s.writeProblem(w, r, internalProblem(id))
	}
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", v1.MediaTypeJSON)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (s *Server) writeProblem(w http.ResponseWriter, r *http.Request, item v1.Problem) {
	w.Header().Set("Content-Type", v1.MediaTypeProblem)
	w.Header().Set("Cache-Control", "no-store")
	if tracker, ok := w.(*responseTracker); ok {
		tracker.errorRef = item.ErrorRef
	}
	w.WriteHeader(item.Status)
	_ = json.NewEncoder(w).Encode(item)
}

func problem(requestID string, status int, code, title, detail string) v1.Problem {
	return v1.Problem{Type: "https://parrot.invalid/problems/" + strings.ReplaceAll(code, "_", "-"), Title: title, Status: status, Detail: detail, Code: code, RequestID: requestID}
}

func invalidProblem(requestID, detail string) v1.Problem {
	return problem(requestID, http.StatusBadRequest, "invalid_request", "Invalid request", detail)
}

func internalProblem(requestID string) v1.Problem {
	item := problem(requestID, http.StatusInternalServerError, "internal_error", "Internal error", "The request could not be completed.")
	item.ErrorRef = newOpaqueID("err")
	return item
}

type requestIDKey struct{}

func requestID(r *http.Request) string {
	value, _ := r.Context().Value(requestIDKey{}).(string)
	return value
}

func newOpaqueID(prefix string) string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return prefix + "_unavailable"
	}
	return prefix + "_" + hex.EncodeToString(b)
}

type responseTracker struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	errorRef    string
}

func (w *responseTracker) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.status, w.wroteHeader = status, true
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseTracker) Write(data []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(data)
}

func (w *responseTracker) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *responseTracker) statusCode() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}
