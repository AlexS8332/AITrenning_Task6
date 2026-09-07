// Package agent — контракты агента: задача, результат, события журнала и
// сам интерфейс Agent. Пакет не знает о конкретных агентах и об HTTP:
// его импортируют и агенты, и сервер, и отчёт.
package agent

import (
	"context"
	"time"

	"github.com/AlexS8332/AITrenning_Task6/internal/llm"
)

// Options — что именно искать о животном сверх карточки.
type Options struct {
	Classification bool `json:"classification"`
	Habitat        bool `json:"habitat"`
	Diet           bool `json:"diet"`
}

// Any — включена ли хоть одна опция.
func (o Options) Any() bool {
	return o.Classification || o.Habitat || o.Diet
}

// Task — запрос пользователя вместе с опциями.
type Task struct {
	Query   string  `json:"query"`
	Options Options `json:"options"`
}

// Виды результата.
const (
	KindCard = "card" // карточка животного
	KindNone = "none" // сведений нет, Text объясняет почему
	KindText = "text" // свободный текст (агент без инструментов)
)

// Source — откуда взяты сведения.
type Source struct {
	Title string `json:"title"`
	URL   string `json:"url"`
}

// TaxonNode — узел дерева классификации.
type TaxonNode struct {
	Rank   string `json:"rank"`
	RankRu string `json:"rankRu"`
	Name   string `json:"name"`
	NameRu string `json:"nameRu,omitempty"`
}

// Card — карточка животного. Поля по опциям пусты, если опция не выбрана
// или сведений не нашлось; во втором случае об этом сказано в Notes.
type Card struct {
	Name           string      `json:"name"`
	Latin          string      `json:"latin"`
	Rank           string      `json:"rank,omitempty"`
	TaxonKey       int         `json:"taxonKey,omitempty"`
	Summary        string      `json:"summary"`
	Classification []TaxonNode `json:"classification,omitempty"`
	Habitat        string      `json:"habitat,omitempty"`
	Diet           string      `json:"diet,omitempty"`
	Sources        []Source    `json:"sources,omitempty"`
	Notes          []string    `json:"notes,omitempty"`
}

// AddSource добавляет источник без повторов.
func (c *Card) AddSource(s Source) {
	if s.URL == "" && s.Title == "" {
		return
	}
	for _, have := range c.Sources {
		if have.URL == s.URL && have.Title == s.Title {
			return
		}
	}
	c.Sources = append(c.Sources, s)
}

// Result — итог работы агента.
type Result struct {
	Kind string `json:"kind"`
	Text string `json:"text,omitempty"`
	Card *Card  `json:"card,omitempty"`
}

// Виды событий журнала.
const (
	EventAgentStart = "agent.start"
	EventAgentDone  = "agent.done"
	EventAgentError = "agent.error"
	EventLLMRequest = "llm.request"
	EventLLMReply   = "llm.response"
	EventToolCall   = "tool.call"
	EventToolResult = "tool.result"
	EventToolError  = "tool.error"
	EventNote       = "note"
)

// Event — запись журнала. Title — одна строка для ленты, Detail — текст
// под ней (аргументы, результат инструмента, ответ модели), Usage и Cost
// заполнены только у ответов модели.
type Event struct {
	Seq     int        `json:"seq"`
	Time    time.Time  `json:"time"`
	Agent   string     `json:"agent"`
	Kind    string     `json:"kind"`
	Step    int        `json:"step,omitempty"`
	Title   string     `json:"title"`
	Detail  string     `json:"detail,omitempty"`
	Usage   *llm.Usage `json:"usage,omitempty"`
	Cost    *llm.Cost  `json:"cost,omitempty"`
	Seconds float64    `json:"seconds,omitempty"`
}

// Emitter принимает события и промежуточные результаты. Реализация обязана
// быть безопасной для вызова из нескольких горутин: специалисты в команде
// работают параллельно.
type Emitter interface {
	Log(Event)
	// Partial сообщает промежуточный результат: карточка заполняется по
	// мере того, как отвечают агенты, и интерфейс показывает её сразу.
	Partial(Result)
}

// Agent — то, что запускает приложение. Вся логика запроса к модели,
// вызовов инструментов и разбора ответа живёт внутри Run.
type Agent interface {
	Name() string
	Run(ctx context.Context, task Task, em Emitter) (Result, error)
}

// Nop — эмиттер, который всё отбрасывает. Для тестов и вложенных вызовов,
// где журнал не нужен.
type Nop struct{}

func (Nop) Log(Event)       {}
func (Nop) Partial(Result) {}
