package runs

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/AlexS8332/AITrenning_Task6/internal/agent"
	"github.com/AlexS8332/AITrenning_Task6/internal/agents"
)

// Сколько прогонов держим в памяти: отчёт лежит в файле, а в памяти нужны
// только открытые во вкладках.
const maxRunsInMemory = 20

// Manager хранит прогоны и запускает агентов.
type Manager struct {
	mu      sync.Mutex
	runs    map[string]*Session
	deps    agents.Deps
	store   *ReportStore
	timeout time.Duration
}

func NewManager(deps agents.Deps, store *ReportStore, timeout time.Duration) *Manager {
	return &Manager{
		runs:    make(map[string]*Session),
		deps:    deps,
		store:   store,
		timeout: timeout,
	}
}

// Get — прогон по идентификатору.
func (m *Manager) Get(id string) (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.runs[id]
	return s, ok
}

// Start создаёт прогон и уходит: HTTP-запрос не должен ждать модель.
func (m *Manager) Start(agentKey string, task agent.Task) (*Session, error) {
	entry, ok := agents.Find(agentKey)
	if !ok {
		return nil, fmt.Errorf("неизвестный тип агента: %q", agentKey)
	}

	session := newSession(View{
		ID:         newID(),
		AgentKey:   entry.Key,
		AgentTitle: entry.Title,
		Model:      m.deps.Runner.Model,
		Task:       task,
		Started:    time.Now(),
		ReportPath: m.store.Path(),
	})

	m.mu.Lock()
	m.runs[session.view.ID] = session
	m.evictLocked()
	m.mu.Unlock()

	go m.run(session, entry.Build(m.deps), task)
	return session, nil
}

func (m *Manager) run(session *Session, a agent.Agent, task agent.Task) {
	ctx, cancel := context.WithTimeout(context.Background(), m.timeout)
	defer cancel()

	res, err := a.Run(ctx, task, session)
	session.finish(res, err)

	// Отчёт пишется до закрытия подписчиков: интерфейс должен увидеть и
	// ошибку записи, если она случится.
	session.setSaveError(m.store.Save(session.View(), session.Events()))
	session.closeSubs()
}

// evictLocked выбрасывает самые старые завершённые прогоны.
func (m *Manager) evictLocked() {
	if len(m.runs) <= maxRunsInMemory {
		return
	}
	ids := make([]string, 0, len(m.runs))
	for id := range m.runs {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		return m.runs[ids[i]].View().Started.Before(m.runs[ids[j]].View().Started)
	})
	for _, id := range ids {
		if len(m.runs) <= maxRunsInMemory {
			return
		}
		if m.runs[id].View().Status != StatusRunning {
			delete(m.runs, id)
		}
	}
}
