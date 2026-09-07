package runs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AlexS8332/AITrenning_Task6/internal/agent"
	"github.com/AlexS8332/AITrenning_Task6/internal/agents"
	"github.com/AlexS8332/AITrenning_Task6/internal/llm"
	"github.com/AlexS8332/AITrenning_Task6/internal/llm/llmtest"
	"github.com/AlexS8332/AITrenning_Task6/internal/tools"
)

func TestSessionLogAndSubscribe(t *testing.T) {
	s := newSession(View{ID: "r1", Started: time.Now()})

	snap, updates, unsubscribe := s.Subscribe()
	defer unsubscribe()
	if snap.View.Status != StatusRunning || len(snap.Events) != 0 {
		t.Fatalf("снимок: %+v", snap)
	}

	usage := llm.Usage{Prompt: 10, Completion: 5, Total: 15}
	cost := llm.Cost{USD: 0.001, Tariff: llm.TariffOffPeak, Known: true}
	s.Log(agent.Event{Agent: "a", Kind: agent.EventLLMReply, Title: "ответ", Usage: &usage, Cost: &cost})
	s.Log(agent.Event{Agent: "a", Kind: agent.EventToolCall, Title: "вызов"})

	events := s.Events()
	if len(events) != 2 || events[0].Seq != 1 || events[1].Seq != 2 || events[0].Time.IsZero() {
		t.Errorf("журнал: %+v", events)
	}
	v := s.View()
	if v.Totals.LLMCalls != 1 || v.Totals.ToolCalls != 1 || v.Totals.Usage.Total != 15 || !v.Totals.Cost.Known || v.Events != 2 {
		t.Errorf("счётчики: %+v", v.Totals)
	}

	// На каждое событие подписчик получает журнал и состояние.
	var kinds []string
	for i := range 4 {
		select {
		case m := <-updates:
			kinds = append(kinds, m.Event)
		case <-time.After(time.Second):
			t.Fatalf("сообщений получено %d: %v", i, kinds)
		}
	}
	if strings.Join(kinds, ",") != "log,state,log,state" {
		t.Errorf("порядок сообщений: %v", kinds)
	}

	card := &agent.Card{Name: "Рысь", Notes: []string{"a"}}
	s.Partial(agent.Result{Kind: agent.KindCard, Card: card})
	card.Notes = append(card.Notes, "b") // правка после Partial не должна попасть в снимок
	if got := s.View().Result.Card.Notes; len(got) != 1 {
		t.Errorf("Partial должен копировать карточку: %v", got)
	}

	s.finish(agent.Result{Kind: agent.KindText, Text: "x"}, nil)
	if v := s.View(); v.Status != StatusDone || v.Result.Text != "x" {
		t.Errorf("завершение: %+v", v)
	}
	s.closeSubs()
	for range updates {
	}
	if _, ch, _ := s.Subscribe(); ch == nil {
		t.Errorf("подписка после закрытия должна вернуть закрытый канал")
	} else if _, ok := <-ch; ok {
		t.Errorf("канал должен быть закрыт")
	}
}

func TestSessionFinishWithError(t *testing.T) {
	s := newSession(View{ID: "r2", Started: time.Now()})
	s.finish(agent.Result{}, context.DeadlineExceeded)
	if v := s.View(); v.Status != StatusFailed || v.Error == "" {
		t.Errorf("ошибка: %+v", v)
	}
}

func sampleView() (View, []agent.Event) {
	started := time.Date(2026, 9, 7, 12, 0, 0, 0, time.Local)
	usage := llm.Usage{Prompt: 100, Completion: 20, Total: 120, CacheHit: 50}
	view := View{
		ID: "abc", AgentKey: "team", AgentTitle: "Команда агентов", Model: "deepseek-v4-flash",
		Task:    agent.Task{Query: "рысь", Options: agent.Options{Classification: true, Diet: true}},
		Started: started, Status: StatusDone,
		Result: &agent.Result{Kind: agent.KindCard, Card: &agent.Card{
			Name: "Обыкновенная рысь", Latin: "Lynx lynx", Rank: "SPECIES", Summary: "Вид из рода рысей.",
			Classification: []agent.TaxonNode{{Rank: "KINGDOM", RankRu: "царство", Name: "Animalia", NameRu: "Животные"}, {Rank: "SPECIES", Name: "Lynx lynx"}},
			Diet:           "Зайцы.",
			Notes:          []string{"среда обитания не запрашивалась"},
			Sources:        []agent.Source{{Title: "Википедия: Обыкновенная рысь", URL: "https://ru.wikipedia.org/wiki/x"}},
		}},
		Totals: Totals{LLMCalls: 3, ToolCalls: 4, Usage: usage, Cost: llm.Cost{USD: 0.0012, Tariff: llm.TariffOffPeak, Known: true}, Seconds: 12.5},
	}
	events := []agent.Event{
		{Seq: 1, Time: started, Agent: "identifier", Kind: agent.EventAgentStart, Title: "старт | с чертой"},
		{Seq: 2, Time: started.Add(2 * time.Second), Agent: "identifier", Kind: agent.EventLLMReply, Title: "ответ", Usage: &usage},
	}
	return view, events
}

func TestRenderReport(t *testing.T) {
	view, events := sampleView()
	text := RenderReport(view, events)

	for _, want := range []string{
		"## Прогон 2026-09-07 12:00:00",
		"- Запрос: «рысь»",
		"Команда агентов (`team`)",
		"- Опции: классификация, питание",
		"**Обыкновенная рысь** (*Lynx lynx*, вид)",
		"- царство: *Animalia* (Животные)",
		"- species: *Lynx lynx*",
		"**Питание**\n\nЗайцы.",
		"- среда обитания не запрашивалась",
		"[Википедия: Обыкновенная рысь](https://ru.wikipedia.org/wiki/x)",
		"- Запросов к модели: 3, вызовов инструментов: 4",
		"- Стоимость: $0.001200 (непиковый тариф)",
		"| 1 | +0.0 с | identifier | agent.start | старт \\| с чертой |  |",
		"| 2 | +2.0 с | identifier | llm.response | ответ | 100 / 20 |",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("в отчёте нет %q\n%s", want, text)
		}
	}
	if !strings.Contains(RenderResult(&agent.Result{Kind: agent.KindNone, Text: "выдумка"}), "**Сведений нет.** выдумка") {
		t.Errorf("результат «нет»")
	}
	if !strings.Contains(RenderResult(&agent.Result{Kind: agent.KindText, Text: "# md"}), "# md") {
		t.Errorf("текстовый результат")
	}
	if RenderResult(nil) == "" || !strings.Contains(renderLog(view, nil), "пуст") {
		t.Errorf("пустые случаи")
	}
	failed := view
	failed.Status = StatusFailed
	failed.Error = "лимит"
	if !strings.Contains(RenderReport(failed, nil), "ошибка: лимит") {
		t.Errorf("статус ошибки")
	}
}

func TestReportStoreOverwriteAndAppend(t *testing.T) {
	dir := t.TempDir()
	view, events := sampleView()

	overwrite := NewReportStore(filepath.Join(dir, "o.md"), false)
	if err := overwrite.Save(view, events); err != nil {
		t.Fatal(err)
	}
	second := view
	second.ID = "def"
	second.Task.Query = "ёж"
	if err := overwrite.Save(second, events); err != nil {
		t.Fatal(err)
	}
	text, _ := overwrite.Read()
	if strings.Contains(text, "«рысь»") || !strings.Contains(text, "«ёж»") || strings.Count(text, reportHeader) != 1 {
		t.Errorf("перезапись должна оставить один прогон:\n%s", text)
	}

	appendStore := NewReportStore(filepath.Join(dir, "a.md"), true)
	appendStore.Save(view, events)
	appendStore.Save(second, events)
	view.Task.Query = "рысь обновлённая"
	appendStore.Save(view, events) // повторная запись того же прогона заменяет его блок
	text, _ = appendStore.Read()
	if strings.Count(text, "<!-- run:") != 2 || !strings.Contains(text, "«рысь обновлённая»") || !strings.Contains(text, "«ёж»") {
		t.Errorf("дописывание:\n%s", text)
	}
	if strings.Index(text, "рысь обновлённая") > strings.Index(text, "«ёж»") {
		t.Errorf("блок должен заменяться на месте, а не дописываться в конец")
	}

	if got, _ := NewReportStore(filepath.Join(dir, "missing.md"), false).Read(); got != "" {
		t.Errorf("отсутствующий файл читается как пустой")
	}
	if err := NewReportStore(filepath.Join(dir, "nodir", "x.md"), false).Save(view, events); err == nil {
		t.Errorf("запись в несуществующий каталог должна быть ошибкой")
	}
	if err := os.WriteFile(filepath.Join(dir, "same.md"), []byte(reportHeader+blockStart("abc")+"\n"+RenderReport(view, events)+blockEnd("abc")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReplaceBlock(t *testing.T) {
	text := "head\n<!-- run:a -->\nold\n<!-- /run:a -->\n<!-- run:b -->\nb\n<!-- /run:b -->\n"
	got, ok := replaceBlock(text, "a", "<!-- run:a -->\nnew\n<!-- /run:a -->\n")
	if !ok || got != "head\n<!-- run:a -->\nnew\n<!-- /run:a -->\n<!-- run:b -->\nb\n<!-- /run:b -->\n" {
		t.Errorf("замена: %q", got)
	}
	if _, ok := replaceBlock(text, "c", "x"); ok {
		t.Errorf("отсутствующий блок")
	}
	if _, ok := replaceBlock("<!-- run:a -->\nno end", "a", "x"); ok {
		t.Errorf("без метки конца блок не режется")
	}
}

func TestManagerRunsChatAgentAndSavesReport(t *testing.T) {
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		return llmtest.Text("Рысь — хищник."), nil
	}}
	store := NewReportStore(filepath.Join(t.TempDir(), "r.md"), false)
	m := NewManager(agents.Deps{Runner: agent.Runner{LLM: fake, Model: "deepseek-v4-flash"}, Tools: tools.NewRegistry()}, store, time.Minute)

	if _, err := m.Start("nope", agent.Task{Query: "x"}); err == nil {
		t.Errorf("неизвестный агент должен быть ошибкой")
	}

	s, err := m.Start("chat", agent.Task{Query: "рысь"})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := m.Get(s.View().ID); !ok || got != s {
		t.Errorf("прогон не найден по идентификатору")
	}

	_, updates, unsubscribe := s.Subscribe()
	defer unsubscribe()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-updates:
			if !ok {
				goto done
			}
		case <-deadline:
			t.Fatal("прогон не завершился")
		}
	}
done:
	v := s.View()
	if v.Status != StatusDone || v.Result == nil || v.Result.Text != "Рысь — хищник." || v.AgentTitle == "" || v.Model != "deepseek-v4-flash" {
		t.Errorf("состояние: %+v", v)
	}
	if v.SaveError != "" {
		t.Errorf("ошибка записи: %s", v.SaveError)
	}
	text, _ := store.Read()
	if !strings.Contains(text, "Рысь — хищник.") || !strings.Contains(text, "chat") {
		t.Errorf("отчёт:\n%s", text)
	}
}

func TestFormatUSD(t *testing.T) {
	if got := FormatUSD(0.0123); got != "$0.0123" {
		t.Errorf("%q", got)
	}
	if got := FormatUSD(0.000123); got != "$0.000123" {
		t.Errorf("%q", got)
	}
}
