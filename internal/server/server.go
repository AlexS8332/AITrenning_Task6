// Package server — HTTP API и раздача фронтенда. Обработчики ничего не
// знают об агентах: они создают прогон, отдают его состояние и журнал.
package server

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"github.com/AlexS8332/AITrenning_Task6/internal/agent"
	"github.com/AlexS8332/AITrenning_Task6/internal/agents"
	"github.com/AlexS8332/AITrenning_Task6/internal/runs"
)

const (
	maxRequestBody = 64 << 10
	// keepAlive держит поток живым: прогон молчит, пока модель думает, а
	// некоторые прокси рвут молчащее соединение.
	keepAliveInterval = 20 * time.Second
)

type Server struct {
	runs  *runs.Manager
	store *runs.ReportStore
	mux   *http.ServeMux
}

// New собирает сервер. static — каталог фронтенда (обычно встроенный).
func New(manager *runs.Manager, store *runs.ReportStore, static fs.FS) *Server {
	s := &Server{runs: manager, store: store, mux: http.NewServeMux()}
	s.mux.Handle("/", http.FileServer(http.FS(static)))
	s.mux.HandleFunc("/api/agents", s.handleAgents)
	s.mux.HandleFunc("/api/runs", s.handleCreateRun)
	s.mux.HandleFunc("/api/runs/", s.handleRun)
	s.mux.HandleFunc("/api/report", s.handleReport)
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// handleAgents отдаёт каталог типов агентов и путь к файлу отчёта.
func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"agents":     agents.Infos(),
		"reportPath": s.store.Path(),
	})
}

type createRunRequest struct {
	Agent   string        `json:"agent"`
	Query   string        `json:"query"`
	Options agent.Options `json:"options"`
}

func (s *Server) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "нужен POST")
		return
	}
	var body createRunRequest
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	query := strings.TrimSpace(body.Query)
	if query == "" {
		writeError(w, http.StatusBadRequest, "запрос не введён")
		return
	}
	if body.Agent == "" {
		body.Agent = agents.Catalog()[0].Key
	}

	session, err := s.runs.Start(body.Agent, agent.Task{Query: query, Options: body.Options})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, session.View())
}

// handleRun разбирает /api/runs/{id} и /api/runs/{id}/events.
func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/runs/")
	id, action, _ := strings.Cut(rest, "/")

	session, ok := s.runs.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "прогон не найден: возможно, сервер перезапускали")
		return
	}

	switch action {
	case "":
		writeJSON(w, http.StatusOK, runs.Snapshot{View: session.View(), Events: session.Events()})
	case "events":
		s.stream(w, r, session)
	default:
		writeError(w, http.StatusNotFound, "неизвестное действие "+action)
	}
}

// stream отдаёт прогон потоком Server-Sent Events: сначала снимок
// (состояние и весь журнал), затем события по мере поступления. Состояние
// и журнал идут разными типами событий, и фронтенд обновляет их в разных
// панелях независимо.
func (s *Server) stream(w http.ResponseWriter, r *http.Request, session *runs.Session) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "поток событий не поддерживается")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	snap, updates, unsubscribe := session.Subscribe()
	defer unsubscribe()

	writeEvent(w, "snapshot", mustJSON(snap))
	flusher.Flush()

	ping := time.NewTicker(keepAliveInterval)
	defer ping.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case msg, ok := <-updates:
			if !ok {
				writeEvent(w, "state", mustJSON(session.View()))
				writeEvent(w, "done", "{}")
				flusher.Flush()
				return
			}
			writeEvent(w, msg.Event, msg.Data)
			flusher.Flush()
		}
	}
}

func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	text, err := s.store.Read()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path":     s.store.Path(),
		"markdown": text,
	})
}

func decodeJSON(r *http.Request, target any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, maxRequestBody))
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("тело запроса не разобралось: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// writeEvent пишет одно SSE-сообщение. json.Marshal переводов строки не
// ставит, поэтому многострочные данные здесь невозможны по построению.
func writeEvent(w io.Writer, event, data string) {
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
}

func mustJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(data)
}
