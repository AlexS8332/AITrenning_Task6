package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/AlexS8332/AITrenning_Task6/internal/llm"
	"github.com/AlexS8332/AITrenning_Task6/internal/llm/llmtest"
	"github.com/AlexS8332/AITrenning_Task6/internal/tools"
)

// recorder — эмиттер для тестов: копит события и промежуточные результаты.
type recorder struct {
	mu       sync.Mutex
	events   []Event
	partials []Result
}

func (r *recorder) Log(ev Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *recorder) Partial(res Result) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.partials = append(r.partials, res)
}

func (r *recorder) kinds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.events))
	for _, ev := range r.events {
		out = append(out, ev.Kind)
	}
	return out
}

func echoTool() tools.Tool {
	return tools.Func{
		FuncName: "echo", FuncDescription: "повторяет аргументы",
		FuncParameters: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`),
		FuncCall: func(_ context.Context, args json.RawMessage) (string, error) {
			var in struct{ Text string }
			json.Unmarshal(args, &in)
			if in.Text == "boom" {
				return "", errors.New("инструмент сломался")
			}
			return `{"echo":"` + in.Text + `"}`, nil
		},
	}
}

func finishTool(reject bool) Finisher {
	attempts := 0
	return Finisher{
		Name: "finish", Description: "закончить",
		Parameters: json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"]}`),
		Handle: func(args json.RawMessage) (Result, error) {
			attempts++
			if reject && attempts == 1 {
				return Result{}, errors.New("первый раз отклоняю")
			}
			var in struct{ Answer string }
			json.Unmarshal(args, &in)
			return Result{Kind: KindText, Text: in.Answer}, nil
		},
	}
}

func TestRunnerToolLoop(t *testing.T) {
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		switch llmtest.ToolReplies(req) {
		case 0:
			if !llmtest.HasTool(req, "echo") || !llmtest.HasTool(req, "finish") {
				t.Errorf("модели не переданы инструменты: %+v", req.Tools)
			}
			if req.Messages[0].Role != llm.RoleSystem || req.Messages[1].Content != "привет" {
				t.Errorf("история: %+v", req.Messages)
			}
			return llmtest.ToolCall("echo", `{"text":"раз"}`), nil
		case 1:
			if !strings.Contains(llmtest.LastToolReply(req), `"echo":"раз"`) {
				t.Errorf("результат инструмента не дошёл: %q", llmtest.LastToolReply(req))
			}
			// Ответ инструмента должен ссылаться на вызов.
			last := req.Messages[len(req.Messages)-1]
			prev := req.Messages[len(req.Messages)-2]
			if last.Role != llm.RoleTool || last.ToolCallID != prev.ToolCalls[0].ID {
				t.Errorf("связка вызова и ответа: %+v / %+v", prev, last)
			}
			return llmtest.ToolCall("finish", `{"answer":"готово"}`), nil
		}
		return llm.Response{}, errors.New("лишний шаг")
	}}

	rec := &recorder{}
	r := Runner{LLM: fake, Model: "deepseek-v4-flash"}
	res, stats, err := r.Run(context.Background(), Spec{
		Name: "t", System: "s", Tools: []tools.Tool{echoTool()}, Finish: []Finisher{finishTool(false)},
	}, "привет", rec)
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != KindText || res.Text != "готово" {
		t.Errorf("результат: %+v", res)
	}
	if stats.Steps != 2 || stats.ToolCalls != 2 || stats.Usage.Total != 56 {
		t.Errorf("статистика: %+v", stats)
	}
	if !stats.Cost.Known {
		t.Errorf("стоимость известной модели должна считаться")
	}

	want := []string{EventLLMRequest, EventLLMReply, EventToolCall, EventToolResult,
		EventLLMRequest, EventLLMReply, EventToolCall, EventToolResult}
	if got := rec.kinds(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("события: %v", got)
	}
}

func TestRunnerFinisherRejectsThenAccepts(t *testing.T) {
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		if n := llmtest.ToolReplies(req); n == 1 {
			if !strings.Contains(llmtest.LastToolReply(req), "первый раз отклоняю") {
				t.Errorf("модель не получила причину отказа: %q", llmtest.LastToolReply(req))
			}
		}
		return llmtest.ToolCall("finish", `{"answer":"второй раз"}`), nil
	}}
	rec := &recorder{}
	res, stats, err := Runner{LLM: fake}.Run(context.Background(),
		Spec{Name: "t", Finish: []Finisher{finishTool(true)}}, "u", rec)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "второй раз" || stats.Steps != 2 {
		t.Errorf("результат %+v, статистика %+v", res, stats)
	}
	kinds := rec.kinds()
	if kinds[3] != EventToolError {
		t.Errorf("отказ должен попасть в журнал: %v", kinds)
	}
}

func TestRunnerRemindsAboutFinisher(t *testing.T) {
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		return llmtest.Text("просто текст"), nil
	}}
	_, stats, err := Runner{LLM: fake}.Run(context.Background(),
		Spec{Name: "t", Finish: []Finisher{finishTool(false)}}, "u", nil)
	if err == nil || !strings.Contains(err.Error(), "текстом") {
		t.Fatalf("ожидалась ошибка протокола, получено: %v", err)
	}
	if stats.Steps != maxReminders+1 {
		t.Errorf("напоминаний должно быть %d, шагов %d", maxReminders, stats.Steps)
	}
	last := fake.Requests[len(fake.Requests)-1].Messages
	if last[len(last)-1].Role != llm.RoleUser || !strings.Contains(last[len(last)-1].Content, "finish") {
		t.Errorf("напоминание не ушло модели: %+v", last[len(last)-1])
	}
}

func TestRunnerAllowText(t *testing.T) {
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		if len(req.Tools) != 0 {
			t.Errorf("у чата не должно быть инструментов")
		}
		return llmtest.Text("ответ"), nil
	}}
	res, _, err := Runner{LLM: fake}.Run(context.Background(), Spec{Name: "chat", AllowText: true}, "u", nil)
	if err != nil || res.Kind != KindText || res.Text != "ответ" {
		t.Fatalf("результат %+v, ошибка %v", res, err)
	}
}

func TestRunnerStepLimitAndToolErrors(t *testing.T) {
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		switch llmtest.ToolReplies(req) {
		case 0:
			return llmtest.ToolCall("echo", `{"text":"boom"}`), nil
		case 1:
			if !strings.Contains(llmtest.LastToolReply(req), "инструмент сломался") {
				t.Errorf("ошибка инструмента не дошла до модели: %q", llmtest.LastToolReply(req))
			}
			return llmtest.ToolCall("nope", `{}`), nil
		default:
			if !strings.Contains(llmtest.LastToolReply(req), "инструмента nope нет") {
				t.Errorf("сообщение о неизвестном инструменте: %q", llmtest.LastToolReply(req))
			}
			return llmtest.ToolCall("echo", `{"text":"ещё"}`), nil
		}
	}}
	rec := &recorder{}
	_, stats, err := Runner{LLM: fake}.Run(context.Background(),
		Spec{Name: "t", Tools: []tools.Tool{echoTool()}, Finish: []Finisher{finishTool(false)}, MaxSteps: 3}, "u", rec)
	if !errors.Is(err, ErrStepLimit) {
		t.Fatalf("ожидался лимит шагов, получено: %v", err)
	}
	if stats.Steps != 3 {
		t.Errorf("шагов: %d", stats.Steps)
	}
	errorsSeen := 0
	for _, k := range rec.kinds() {
		if k == EventToolError {
			errorsSeen++
		}
	}
	if errorsSeen != 2 {
		t.Errorf("ошибок инструментов в журнале: %d", errorsSeen)
	}
}

func TestRunnerLLMError(t *testing.T) {
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		return llm.Response{}, fmt.Errorf("сеть упала")
	}}
	_, _, err := Runner{LLM: fake}.Run(context.Background(), Spec{Name: "t", AllowText: true}, "u", nil)
	if err == nil || !strings.Contains(err.Error(), "сеть упала") || !strings.Contains(err.Error(), "шаг 1") {
		t.Fatalf("ошибка: %v", err)
	}
}

func TestParallelToolCallsInOneReply(t *testing.T) {
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		if llmtest.ToolReplies(req) == 0 {
			return llmtest.ToolCalls(llmtest.Call{Name: "echo", Args: `{"text":"a"}`}, llmtest.Call{Name: "echo", Args: `{"text":"b"}`}), nil
		}
		if llmtest.ToolReplies(req) != 2 {
			t.Errorf("оба ответа инструментов должны быть в истории: %d", llmtest.ToolReplies(req))
		}
		return llmtest.ToolCall("finish", `{"answer":"ok"}`), nil
	}}
	_, stats, err := Runner{LLM: fake}.Run(context.Background(),
		Spec{Name: "t", Tools: []tools.Tool{echoTool()}, Finish: []Finisher{finishTool(false)}}, "u", nil)
	if err != nil || stats.ToolCalls != 3 {
		t.Fatalf("ошибка %v, статистика %+v", err, stats)
	}
}

func TestPrettyJSONAndHelpers(t *testing.T) {
	if got := prettyJSON(`{"a":1}`); !strings.Contains(got, "\n") {
		t.Errorf("JSON не отформатирован: %q", got)
	}
	if got := prettyJSON("не json"); got != "не json" {
		t.Errorf("не JSON должен вернуться как есть: %q", got)
	}
	if got := sizeLabel(strings.Repeat("x", 2500)); got != "2.5 тыс. символов" {
		t.Errorf("размер: %q", got)
	}
	card := &Card{}
	card.AddSource(Source{Title: "a", URL: "u"})
	card.AddSource(Source{Title: "a", URL: "u"})
	card.AddSource(Source{})
	if len(card.Sources) != 1 {
		t.Errorf("источники должны быть без повторов: %+v", card.Sources)
	}
	if !(Options{Diet: true}).Any() || (Options{}).Any() {
		t.Errorf("Options.Any")
	}
}
