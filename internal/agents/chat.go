package agents

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/AlexS8332/AITrenning_Task6/internal/agent"
)

// Chat — агент без инструментов: один вызов модели, ответ из её памяти.
// Минимум по условию курса и точка отсчёта: по нему видно, что дают
// инструменты по сравнению с «просто спросить модель».
type Chat struct {
	deps Deps
}

func NewChat(deps Deps) agent.Agent {
	return &Chat{deps: deps}
}

func (c *Chat) Name() string { return "chat" }

const chatSystem = `Ты справочник по животным. Отвечай по-русски, по существу, в markdown.
Начни с общепринятого русского названия и латинского названия в скобках, затем краткое описание в один абзац.
Если запрос не про реальное животное или название тебе неизвестно, так и скажи одной фразой, не придумывай карточку и не подменяй запрос похожим животным.`

func (c *Chat) Run(ctx context.Context, task agent.Task, em agent.Emitter) (agent.Result, error) {
	if em == nil {
		em = agent.Nop{}
	}
	started := time.Now()
	em.Log(agent.Event{Agent: c.Name(), Kind: agent.EventAgentStart, Title: "чат без инструментов: один запрос к модели"})

	var extra []string
	if task.Options.Classification {
		extra = append(extra, "Приведи классификационное дерево от царства до вида списком: ранг, латинское и русское название.")
	}
	if task.Options.Habitat {
		extra = append(extra, "Опиши среду обитания и ареал отдельным разделом.")
	}
	if task.Options.Diet {
		extra = append(extra, "Опиши питание отдельным разделом.")
	}

	user := fmt.Sprintf("Расскажи о животном: «%s».", strings.TrimSpace(task.Query))
	if len(extra) > 0 {
		user += "\n" + strings.Join(extra, "\n")
	}

	res, _, err := c.deps.Runner.Run(ctx, agent.Spec{
		Name:      c.Name(),
		System:    chatSystem,
		MaxSteps:  1,
		AllowText: true,
	}, user, em)
	if err != nil {
		em.Log(agent.Event{Agent: c.Name(), Kind: agent.EventAgentError, Title: err.Error(), Seconds: time.Since(started).Seconds()})
		return agent.Result{}, err
	}
	em.Log(agent.Event{Agent: c.Name(), Kind: agent.EventAgentDone, Title: "ответ получен", Seconds: time.Since(started).Seconds()})
	return res, nil
}
