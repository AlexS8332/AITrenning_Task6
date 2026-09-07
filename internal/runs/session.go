// Package runs — прогон агента: состояние для интерфейса, журнал событий,
// подписчики и запись отчёта. Здесь результат и журнал разведены по разным
// потокам: интерфейс показывает их в разных панелях и обновляет независимо.
package runs

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/AlexS8332/AITrenning_Task6/internal/agent"
	"github.com/AlexS8332/AITrenning_Task6/internal/llm"
)

// Статусы прогона.
const (
	StatusRunning = "running"
	StatusDone    = "done"
	StatusFailed  = "failed"
)

// Сколько сообщений может копиться у медленного подписчика, прежде чем
// они начнут теряться. Журнал одного прогона — десятки событий, не тысячи.
const subscriberBuffer = 512

// Totals — счётчики прогона: считаются по событиям журнала.
type Totals struct {
	LLMCalls  int       `json:"llmCalls"`
	ToolCalls int       `json:"toolCalls"`
	Usage     llm.Usage `json:"usage"`
	Cost      llm.Cost  `json:"cost"`
	Seconds   float64   `json:"seconds"`
}

// View — состояние прогона для интерфейса и отчёта. Журнал в него не входит:
// он уходит отдельными сообщениями.
type View struct {
	ID         string        `json:"id"`
	AgentKey   string        `json:"agentKey"`
	AgentTitle string        `json:"agentTitle"`
	Model      string        `json:"model"`
	Task       agent.Task    `json:"task"`
	Started    time.Time     `json:"started"`
	Status     string        `json:"status"`
	Result     *agent.Result `json:"result,omitempty"`
	Error      string        `json:"error,omitempty"`
	Totals     Totals        `json:"totals"`
	Events     int           `json:"events"`
	ReportPath string        `json:"reportPath"`
	SaveError  string        `json:"saveError,omitempty"`
}

// Message — одно сообщение подписчику: вид события SSE и его данные.
type Message struct {
	Event string
	Data  string
}

// Snapshot — что получает новый подписчик: состояние и весь журнал.
type Snapshot struct {
	View   View          `json:"view"`
	Events []agent.Event `json:"events"`
}

// Session — прогон. Реализует agent.Emitter: агент пишет в него журнал и
// промежуточные результаты, а сессия рассылает их подписчикам.
type Session struct {
	mu     sync.Mutex
	view   View
	events []agent.Event
	subs   map[chan Message]struct{}
	closed bool
}

func newSession(view View) *Session {
	view.Status = StatusRunning
	return &Session{
		view: view,
		subs: make(map[chan Message]struct{}),
	}
}

// View — копия состояния.
func (s *Session) View() View {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.viewLocked()
}

func (s *Session) viewLocked() View {
	v := s.view
	if v.Result != nil {
		r := cloneResult(*v.Result)
		v.Result = &r
	}
	return v
}

// cloneResult копирует результат глубоко: карточку дописывают горутины
// специалистов, а сериализовать её надо в устойчивом состоянии.
func cloneResult(r agent.Result) agent.Result {
	if r.Card == nil {
		return r
	}
	c := *r.Card
	c.Classification = append([]agent.TaxonNode(nil), c.Classification...)
	c.Sources = append([]agent.Source(nil), c.Sources...)
	c.Notes = append([]string(nil), c.Notes...)
	r.Card = &c
	return r
}

// Events — копия журнала.
func (s *Session) Events() []agent.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]agent.Event(nil), s.events...)
}

// Log — событие от агента. Здесь ему присваиваются номер и время, по нему
// обновляются счётчики, и оно уходит подписчикам.
func (s *Session) Log(ev agent.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ev.Seq = len(s.events) + 1
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	s.events = append(s.events, ev)
	s.view.Events = len(s.events)

	switch {
	case ev.Usage != nil:
		s.view.Totals.LLMCalls++
		s.view.Totals.Usage = s.view.Totals.Usage.Add(*ev.Usage)
		if ev.Cost != nil {
			s.view.Totals.Cost = s.view.Totals.Cost.Add(*ev.Cost)
		}
	case ev.Kind == agent.EventToolCall:
		s.view.Totals.ToolCalls++
	}
	s.view.Totals.Seconds = time.Since(s.view.Started).Seconds()

	s.broadcastLocked(Message{Event: "log", Data: mustJSON(ev)})
	s.broadcastLocked(Message{Event: "state", Data: mustJSON(s.viewLocked())})
}

// Partial — промежуточный результат от агента.
func (s *Session) Partial(res agent.Result) {
	s.mu.Lock()
	defer s.mu.Unlock()

	r := cloneResult(res)
	s.view.Result = &r
	s.broadcastLocked(Message{Event: "state", Data: mustJSON(s.viewLocked())})
}

// finish закрывает прогон результатом или ошибкой.
func (s *Session) finish(res agent.Result, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.view.Totals.Seconds = time.Since(s.view.Started).Seconds()
	if err != nil {
		s.view.Status = StatusFailed
		s.view.Error = err.Error()
	} else {
		s.view.Status = StatusDone
		r := cloneResult(res)
		s.view.Result = &r
	}
	s.broadcastLocked(Message{Event: "state", Data: mustJSON(s.viewLocked())})
}

func (s *Session) setSaveError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.view.SaveError = err.Error()
	} else {
		s.view.SaveError = ""
	}
	s.broadcastLocked(Message{Event: "state", Data: mustJSON(s.viewLocked())})
}

// Subscribe возвращает снимок на момент подписки, канал последующих
// сообщений и функцию отписки. Канал закрывается, когда прогон завершён и
// отчёт записан: для SSE это сигнал закончить поток.
func (s *Session) Subscribe() (Snapshot, <-chan Message, func()) {
	ch := make(chan Message, subscriberBuffer)

	s.mu.Lock()
	defer s.mu.Unlock()

	snap := Snapshot{View: s.viewLocked(), Events: append([]agent.Event(nil), s.events...)}
	if s.closed {
		close(ch)
		return snap, ch, func() {}
	}
	s.subs[ch] = struct{}{}

	return snap, ch, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, ok := s.subs[ch]; ok {
			delete(s.subs, ch)
			close(ch)
		}
	}
}

// broadcastLocked рассылает сообщение без ожидания: медленный подписчик
// теряет сообщения, но не задерживает агента.
func (s *Session) broadcastLocked(msg Message) {
	for ch := range s.subs {
		select {
		case ch <- msg:
		default:
		}
	}
}

func (s *Session) closeSubs() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.closed = true
	for ch := range s.subs {
		delete(s.subs, ch)
		close(ch)
	}
}

func mustJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(data)
}

func newID() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf[:])
}
