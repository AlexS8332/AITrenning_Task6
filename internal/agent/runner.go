package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/AlexS8332/AITrenning_Task6/internal/llm"
	"github.com/AlexS8332/AITrenning_Task6/internal/tools"
)

const (
	// Сколько символов результата инструмента и ответа модели показывать в
	// журнале. Полный текст уходит модели, а в ленте нужен обозримый кусок.
	logDetailRunes = 6000

	// Сколько раз напоминать модели, что закончить надо вызовом инструмента,
	// прежде чем сдаться.
	maxReminders = 2
)

// ErrStepLimit — агент не уложился в лимит шагов.
var ErrStepLimit = errors.New("исчерпан лимит шагов агента")

// Finisher — завершающий инструмент: модель вызывает его, когда готова
// отдать результат. Аргументы проверяет Handle; если он возвращает ошибку,
// текст ошибки уходит модели как результат вызова, и цикл продолжается.
// Так контракт результата проверяет программа, а не модель.
type Finisher struct {
	Name        string
	Description string
	Parameters  json.RawMessage
	Handle      func(args json.RawMessage) (Result, error)
}

// Spec — описание агента для Runner: роль, инструменты, лимиты.
type Spec struct {
	Name     string
	System   string
	Tools    []tools.Tool
	Finish   []Finisher
	MaxSteps int
	// AllowText разрешает закончить обычным текстом. Без него текст без
	// вызова инструмента считается ошибкой протокола и модели напоминают
	// о завершающем инструменте.
	AllowText bool
}

// Stats — итог одного прогона цикла.
type Stats struct {
	Steps     int
	ToolCalls int
	Usage     llm.Usage
	Cost      llm.Cost
}

// Runner исполняет цикл агента: запрос к модели, вызовы инструментов,
// ответ. Один Runner обслуживает всех агентов приложения.
type Runner struct {
	LLM         llm.Chatter
	Model       string
	Temperature float64
}

// Run ведёт диалог с моделью до результата, ошибки или лимита шагов.
func (r Runner) Run(ctx context.Context, spec Spec, user string, em Emitter) (Result, Stats, error) {
	if em == nil {
		em = Nop{}
	}
	if spec.MaxSteps <= 0 {
		spec.MaxSteps = 12
	}

	messages := []llm.Message{
		{Role: llm.RoleSystem, Content: spec.System},
		{Role: llm.RoleUser, Content: user},
	}

	byName := make(map[string]tools.Tool, len(spec.Tools))
	defs := tools.Defs(spec.Tools)
	for _, t := range spec.Tools {
		byName[t.Name()] = t
	}
	finishers := make(map[string]Finisher, len(spec.Finish))
	for _, f := range spec.Finish {
		finishers[f.Name] = f
		defs = append(defs, llm.NewToolDef(f.Name, f.Description, f.Parameters))
	}

	var stats Stats
	reminders := 0

	for step := 1; step <= spec.MaxSteps; step++ {
		stats.Steps = step

		em.Log(Event{Agent: spec.Name, Kind: EventLLMRequest, Step: step,
			Title:  fmt.Sprintf("запрос к модели, сообщений в истории: %d", len(messages)),
			Detail: lastMessageDetail(messages)})

		started := time.Now()
		resp, err := r.LLM.Chat(ctx, llm.Request{
			Model:       r.Model,
			Messages:    messages,
			Tools:       defs,
			Temperature: r.Temperature,
		})
		elapsed := time.Since(started)
		if err != nil {
			return Result{}, stats, fmt.Errorf("шаг %d: %w", step, err)
		}

		cost := llm.PriceOf(r.Model, resp.Usage, started)
		stats.Usage = stats.Usage.Add(resp.Usage)
		stats.Cost = stats.Cost.Add(cost)
		usage := resp.Usage
		em.Log(Event{Agent: spec.Name, Kind: EventLLMReply, Step: step,
			Title:   replyTitle(resp),
			Detail:  replyDetail(resp),
			Usage:   &usage,
			Cost:    &cost,
			Seconds: elapsed.Seconds()})

		messages = append(messages, resp.Message)

		if !resp.HasToolCalls() {
			if spec.AllowText {
				return Result{Kind: KindText, Text: resp.Message.Content}, stats, nil
			}
			if reminders >= maxReminders {
				return Result{}, stats, fmt.Errorf("модель отвечает текстом вместо вызова завершающего инструмента")
			}
			reminders++
			reminder := "Ответ текстом не принимается. Заверши работу вызовом одного из инструментов: " +
				strings.Join(finisherNames(spec.Finish), ", ") + "."
			em.Log(Event{Agent: spec.Name, Kind: EventNote, Step: step,
				Title: "модель ответила текстом, напоминаю о завершающем инструменте"})
			messages = append(messages, llm.Message{Role: llm.RoleUser, Content: reminder})
			continue
		}

		for _, call := range resp.Message.ToolCalls {
			stats.ToolCalls++
			args := json.RawMessage(call.Function.Arguments)

			em.Log(Event{Agent: spec.Name, Kind: EventToolCall, Step: step,
				Title:  "вызов " + call.Function.Name,
				Detail: prettyJSON(call.Function.Arguments)})

			if f, ok := finishers[call.Function.Name]; ok {
				res, err := f.Handle(args)
				if err == nil {
					em.Log(Event{Agent: spec.Name, Kind: EventToolResult, Step: step,
						Title: call.Function.Name + ": результат принят"})
					return res, stats, nil
				}
				em.Log(Event{Agent: spec.Name, Kind: EventToolError, Step: step,
					Title:  call.Function.Name + ": результат отклонён",
					Detail: err.Error()})
				messages = append(messages, toolReply(call.ID, errorPayload(err)))
				continue
			}

			t, ok := byName[call.Function.Name]
			if !ok {
				err := fmt.Errorf("инструмента %s нет; доступны: %s", call.Function.Name,
					strings.Join(toolNames(spec), ", "))
				em.Log(Event{Agent: spec.Name, Kind: EventToolError, Step: step,
					Title: "неизвестный инструмент " + call.Function.Name, Detail: err.Error()})
				messages = append(messages, toolReply(call.ID, errorPayload(err)))
				continue
			}

			toolStarted := time.Now()
			out, err := t.Call(ctx, args)
			toolElapsed := time.Since(toolStarted)
			if err != nil {
				em.Log(Event{Agent: spec.Name, Kind: EventToolError, Step: step,
					Title: call.Function.Name + ": ошибка", Detail: err.Error(),
					Seconds: toolElapsed.Seconds()})
				messages = append(messages, toolReply(call.ID, errorPayload(err)))
				continue
			}
			em.Log(Event{Agent: spec.Name, Kind: EventToolResult, Step: step,
				Title:   fmt.Sprintf("%s: %s", call.Function.Name, sizeLabel(out)),
				Detail:  tools.Truncate(prettyJSON(out), logDetailRunes),
				Seconds: toolElapsed.Seconds()})
			messages = append(messages, toolReply(call.ID, out))
		}
	}

	return Result{}, stats, fmt.Errorf("%w (%d)", ErrStepLimit, spec.MaxSteps)
}

func toolReply(callID, content string) llm.Message {
	return llm.Message{Role: llm.RoleTool, ToolCallID: callID, Content: content}
}

// errorPayload — ошибка в виде JSON: модели проще отличить её от результата.
func errorPayload(err error) string {
	data, _ := json.Marshal(map[string]string{"error": err.Error()})
	return string(data)
}

func finisherNames(fs []Finisher) []string {
	names := make([]string, 0, len(fs))
	for _, f := range fs {
		names = append(names, f.Name)
	}
	return names
}

func toolNames(spec Spec) []string {
	names := make([]string, 0, len(spec.Tools)+len(spec.Finish))
	for _, t := range spec.Tools {
		names = append(names, t.Name())
	}
	return append(names, finisherNames(spec.Finish)...)
}

func replyTitle(resp llm.Response) string {
	if !resp.HasToolCalls() {
		return "ответ текстом"
	}
	names := make([]string, 0, len(resp.Message.ToolCalls))
	for _, c := range resp.Message.ToolCalls {
		names = append(names, c.Function.Name)
	}
	return "модель просит вызвать: " + strings.Join(names, ", ")
}

func replyDetail(resp llm.Response) string {
	if resp.Message.Content == "" {
		return ""
	}
	return tools.Truncate(resp.Message.Content, logDetailRunes)
}

// lastMessageDetail показывает, что именно ушло модели последним: на первом
// шаге это запрос пользователя, дальше — результаты инструментов.
func lastMessageDetail(messages []llm.Message) string {
	if len(messages) == 0 {
		return ""
	}
	last := messages[len(messages)-1]
	if last.Role == llm.RoleUser {
		return tools.Truncate(last.Content, logDetailRunes)
	}
	return ""
}

func sizeLabel(s string) string {
	n := len([]rune(s))
	switch {
	case n < 1000:
		return fmt.Sprintf("%d символов", n)
	default:
		return fmt.Sprintf("%.1f тыс. символов", float64(n)/1000)
	}
}

// prettyJSON форматирует JSON для журнала; не JSON возвращается как есть.
func prettyJSON(s string) string {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return s
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return s
	}
	return string(data)
}
