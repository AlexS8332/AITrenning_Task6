package agents

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/AlexS8332/AITrenning_Task6/internal/agent"
	"github.com/AlexS8332/AITrenning_Task6/internal/llm"
	"github.com/AlexS8332/AITrenning_Task6/internal/llm/llmtest"
	"github.com/AlexS8332/AITrenning_Task6/internal/tools"
)

// Подставные источники: одна статья о рыси с разделами и один таксон в GBIF.

const lynxExtract = "Обыкновенная рысь (лат. Lynx lynx) — вид млекопитающих из рода рысей.\n\n== Распространение ==\nЛесная зона Евразии.\n\n== Образ жизни, поведение и питание ==\nОхотится на зайцев и косуль.\n"

func fakeSources(t *testing.T) *tools.Registry {
	t.Helper()
	wiki := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case q.Get("list") == "search":
			if strings.Contains(q.Get("srsearch"), "манул") {
				w.Write([]byte(`{"query":{"search":[{"title":"Московский зоопарк","snippet":"..."}]}}`))
				return
			}
			w.Write([]byte(`{"query":{"search":[{"title":"Обыкновенная рысь","snippet":"вид"}]}}`))
		case q.Get("prop") == "extracts":
			if q.Get("titles") != "Обыкновенная рысь" {
				json.NewEncoder(w).Encode(map[string]any{"query": map[string]any{"pages": []map[string]any{{"title": q.Get("titles"), "missing": true}}}})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"query": map[string]any{"pages": []map[string]any{{"title": "Обыкновенная рысь", "extract": lynxExtract}}}})
		}
	}))
	gbif := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/species/match":
			if r.URL.Query().Get("name") == "Lynx lynx" {
				w.Write([]byte(`{"usageKey":2435240,"canonicalName":"Lynx lynx","rank":"SPECIES","status":"ACCEPTED","confidence":99,"matchType":"EXACT"}`))
				return
			}
			w.Write([]byte(`{"confidence":100,"matchType":"NONE"}`))
		case "/species/2435240/parents":
			w.Write([]byte(`[{"key":1,"rank":"KINGDOM","canonicalName":"Animalia"},{"key":2435239,"rank":"GENUS","canonicalName":"Lynx"}]`))
		case "/species/2435240":
			w.Write([]byte(`{"key":2435240,"rank":"SPECIES","canonicalName":"Lynx lynx"}`))
		case "/species/2435240/vernacularNames":
			w.Write([]byte(`{"results":[{"vernacularName":"Рысь","language":"rus"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(wiki.Close)
	t.Cleanup(gbif.Close)

	f := tools.NewFetcher()
	return tools.NewRegistry(append(tools.NewWikipedia(wiki.URL, f).Tools(), tools.NewGBIF(gbif.URL, f).Tools()...)...)
}

// scripted — сценарий модели для команды и одиночного агента. Роль
// определяется по набору инструментов в запросе, шаг — по числу ответов
// инструментов в истории.
func scripted(t *testing.T) *llmtest.Fake {
	t.Helper()
	return &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		n := llmtest.ToolReplies(req)
		switch {
		case llmtest.HasTool(req, "submit_taxon"): // идентификатор
			switch n {
			case 0:
				return llmtest.ToolCall("search_wikipedia", `{"query":"рысь"}`), nil
			case 1:
				return llmtest.ToolCall("read_wikipedia", `{"title":"Обыкновенная рысь"}`), nil
			case 2:
				return llmtest.ToolCall("match_taxon", `{"scientific_name":"Lynx lynx"}`), nil
			default:
				return llmtest.ToolCall("submit_taxon", `{"name_ru":"Обыкновенная рысь","latin":"Lynx lynx","rank":"SPECIES","usage_key":1,"wiki_title":"Обыкновенная рысь","summary":"Вид млекопитающих из рода рысей."}`), nil
			}
		case llmtest.HasTool(req, "submit_classification"):
			if n == 0 {
				return llmtest.ToolCall("taxon_tree", `{"usage_key":2435240}`), nil
			}
			return llmtest.ToolCall("submit_classification", `{"tree":[{"rank":"KINGDOM","name":"Animalia","name_ru":"Животные"},{"rank":"SPECIES","name":"Lynx lynx","name_ru":"Обыкновенная рысь"}]}`), nil
		case llmtest.HasTool(req, "submit_section"):
			system := req.Messages[0].Content
			if strings.Contains(system, "питание") {
				if n == 0 {
					return llmtest.ToolCall("read_wikipedia", `{"title":"Обыкновенная рысь","section":"Питание"}`), nil
				}
				// Модель называет раздел коротко, инструмент вернул длинный заголовок.
				return llmtest.ToolCall("submit_section", `{"found":true,"section":"Питание","text":"Охотится на зайцев и косуль."}`), nil
			}
			switch n {
			case 0:
				// Первая попытка — пересказ без чтения: должна быть отклонена.
				return llmtest.ToolCall("submit_section", `{"found":true,"section":"Распространение","text":"Из памяти."}`), nil
			case 1:
				if !strings.Contains(llmtest.LastToolReply(req), "не был прочитан") {
					t.Errorf("отказ не объяснён модели: %q", llmtest.LastToolReply(req))
				}
				return llmtest.ToolCall("read_wikipedia", `{"title":"Обыкновенная рысь","section":"Распространение"}`), nil
			default:
				return llmtest.ToolCall("submit_section", `{"found":true,"section":"Распространение","text":"Лесная зона Евразии."}`), nil
			}
		case llmtest.HasTool(req, "submit_card"): // одиночный агент
			switch n {
			case 0:
				return llmtest.ToolCall("search_wikipedia", `{"query":"рысь"}`), nil
			case 1:
				return llmtest.ToolCall("read_wikipedia", `{"title":"Обыкновенная рысь"}`), nil
			case 2:
				return llmtest.ToolCall("match_taxon", `{"scientific_name":"Lynx lynx"}`), nil
			case 3:
				return llmtest.ToolCall("taxon_tree", `{"usage_key":2435240}`), nil
			case 4:
				return llmtest.ToolCall("read_wikipedia", `{"title":"Обыкновенная рысь","section":"Питание"}`), nil
			default:
				return llmtest.ToolCall("submit_card", `{"name_ru":"Обыкновенная рысь","latin":"Lynx lynx","rank":"SPECIES","summary":"Вид из рода рысей.","diet":"Зайцы и косули.","notes":["раздела о среде обитания не читал"]}`), nil
			}
		default:
			return llmtest.Text("Рысь (Lynx lynx) — хищник."), nil
		}
	}}
}

type recorder struct {
	mu       sync.Mutex
	events   []agent.Event
	partials []agent.Result
}

func (r *recorder) Log(ev agent.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *recorder) Partial(res agent.Result) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.partials = append(r.partials, res)
}

func (r *recorder) agents() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]int)
	for _, ev := range r.events {
		out[ev.Agent]++
	}
	return out
}

func deps(t *testing.T, fake *llmtest.Fake) Deps {
	return Deps{Runner: agent.Runner{LLM: fake, Model: "deepseek-v4-flash"}, Tools: fakeSources(t)}
}

func TestTeamBuildsCard(t *testing.T) {
	fake := scripted(t)
	rec := &recorder{}
	team := NewTeam(deps(t, fake))

	res, err := team.Run(context.Background(), agent.Task{Query: "рысь", Options: agent.Options{Classification: true, Habitat: true, Diet: true}}, rec)
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != agent.KindCard || res.Card == nil {
		t.Fatalf("результат: %+v", res)
	}
	c := res.Card
	if c.Name != "Обыкновенная рысь" || c.Latin != "Lynx lynx" || c.TaxonKey != 2435240 {
		t.Errorf("карточка: %+v", c)
	}
	if len(c.Classification) != 3 || c.Classification[0].NameRu != "Животные" || c.Classification[2].Name != "Lynx lynx" || c.Classification[1].NameRu != "" {
		t.Errorf("дерево: %+v", c.Classification)
	}
	if c.Habitat != "Лесная зона Евразии." || c.Diet != "Охотится на зайцев и косуль." {
		t.Errorf("разделы: habitat=%q diet=%q", c.Habitat, c.Diet)
	}
	if len(c.Sources) != 2 || !strings.Contains(c.Sources[0].Title, "Википедия") || !strings.Contains(c.Sources[1].URL, "2435240") {
		t.Errorf("источники: %+v", c.Sources)
	}
	if len(c.Notes) != 0 {
		t.Errorf("оговорок быть не должно: %v", c.Notes)
	}

	seen := rec.agents()
	for _, name := range []string{"coordinator", "identifier", "classification", "habitat", "diet"} {
		if seen[name] == 0 {
			t.Errorf("в журнале нет событий агента %s", name)
		}
	}
	// Промежуточных результатов не меньше четырёх: после идентификатора и
	// после каждого из трёх специалистов.
	if len(rec.partials) < 4 {
		t.Errorf("промежуточных результатов: %d", len(rec.partials))
	}
	if rec.partials[0].Card.Habitat != "" {
		t.Errorf("первый промежуточный результат должен быть без разделов")
	}
}

func TestTeamNotFoundSkipsSpecialists(t *testing.T) {
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		if llmtest.ToolReplies(req) == 0 {
			return llmtest.ToolCall("search_wikipedia", `{"query":"полосатый манул"}`), nil
		}
		return llmtest.ToolCall("report_not_found", `{"reason":"статьи о таком животном нет"}`), nil
	}}
	rec := &recorder{}
	res, err := NewTeam(deps(t, fake)).Run(context.Background(), agent.Task{Query: "полосатый манул", Options: agent.Options{Diet: true}}, rec)
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != agent.KindNone || !strings.Contains(res.Text, "нет") {
		t.Errorf("результат: %+v", res)
	}
	if seen := rec.agents(); seen["diet"] != 0 || fake.Calls() != 2 {
		t.Errorf("специалисты не должны запускаться: %v, вызовов модели %d", seen, fake.Calls())
	}
}

func TestTeamSpecialistFailureBecomesNote(t *testing.T) {
	fake := scripted(t)
	base := fake.Fn
	fake.Fn = func(req llm.Request) (llm.Response, error) {
		if llmtest.HasTool(req, "submit_classification") {
			return llmtest.Text("не буду вызывать инструменты"), nil
		}
		return base(req)
	}
	res, err := NewTeam(deps(t, fake)).Run(context.Background(), agent.Task{Query: "рысь", Options: agent.Options{Classification: true}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Card.Classification) != 0 || len(res.Card.Notes) != 1 || !strings.Contains(res.Card.Notes[0], "classification") {
		t.Errorf("ошибка специалиста должна стать оговоркой: %+v", res.Card)
	}
}

func TestTeamIdentifierRejectsUnverifiedTaxon(t *testing.T) {
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		switch llmtest.ToolReplies(req) {
		case 0:
			// Сразу submit без чтения и сверки — должен быть отклонён.
			return llmtest.ToolCall("submit_taxon", `{"name_ru":"Рысь","latin":"Lynx lynx","usage_key":1,"wiki_title":"Обыкновенная рысь","summary":"x"}`), nil
		case 1:
			reply := llmtest.LastToolReply(req)
			if !strings.Contains(reply, "не подтверждено") || !strings.Contains(reply, "не была прочитана") {
				t.Errorf("отказ должен перечислить обе проблемы: %q", reply)
			}
			return llmtest.ToolCall("read_wikipedia", `{"title":"Обыкновенная рысь"}`), nil
		case 2:
			return llmtest.ToolCall("match_taxon", `{"scientific_name":"Lynx lynx"}`), nil
		default:
			return llmtest.ToolCall("submit_taxon", `{"name_ru":"Рысь","latin":"Lynx lynx","usage_key":1,"wiki_title":"Обыкновенная рысь","summary":"x"}`), nil
		}
	}}
	res, err := NewTeam(deps(t, fake)).Run(context.Background(), agent.Task{Query: "рысь"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Card == nil || res.Card.TaxonKey != 2435240 {
		t.Errorf("usage_key должен быть взят из сверки, а не из ответа модели: %+v", res.Card)
	}
}

func TestSoloBuildsCard(t *testing.T) {
	fake := scripted(t)
	rec := &recorder{}
	res, err := NewSolo(deps(t, fake)).Run(context.Background(), agent.Task{Query: "рысь", Options: agent.Options{Classification: true, Habitat: true, Diet: true}}, rec)
	if err != nil {
		t.Fatal(err)
	}
	c := res.Card
	if c == nil || c.Latin != "Lynx lynx" || c.Diet != "Зайцы и косули." {
		t.Fatalf("карточка: %+v", c)
	}
	// Дерево модель не передала — взято из журнала taxon_tree.
	if len(c.Classification) != 3 || c.Classification[2].Name != "Lynx lynx" {
		t.Errorf("дерево из журнала: %+v", c.Classification)
	}
	if len(c.Notes) != 1 || len(c.Sources) != 2 {
		t.Errorf("оговорки %v, источники %+v", c.Notes, c.Sources)
	}
	if seen := rec.agents(); len(seen) != 1 || seen["solo"] == 0 {
		t.Errorf("у одиночного агента один участник журнала: %v", seen)
	}
}

func TestSoloCardValidation(t *testing.T) {
	tr := newTracker()
	opts := agent.Options{Classification: true, Habitat: true}

	_, err := buildCard(tr, opts, cardArgs{Name: "Рысь", Latin: "Lynx lynx", Summary: "x"})
	if err == nil || !strings.Contains(err.Error(), "match_taxon") {
		t.Fatalf("латынь без сверки должна отклоняться: %v", err)
	}

	tr.record("match_taxon", `{"found":true,"usage_key":7,"canonical_name":"Lynx lynx"}`)
	_, err = buildCard(tr, opts, cardArgs{Name: "Рысь", Latin: "lynx LYNX", Summary: "x"})
	if err == nil || !strings.Contains(err.Error(), "taxon_tree") {
		t.Fatalf("без дерева при включённой опции: %v", err)
	}

	tr.record("taxon_tree", `{"usage_key":7,"tree":[{"rank":"KINGDOM","rank_ru":"царство","name":"Animalia"}]}`)
	_, err = buildCard(tr, opts, cardArgs{Name: "Рысь", Latin: "Lynx lynx", Summary: "x"})
	if err == nil || !strings.Contains(err.Error(), "habitat") {
		t.Fatalf("пустой habitat без оговорки: %v", err)
	}

	card, err := buildCard(tr, opts, cardArgs{Name: "Рысь", Latin: "Lynx lynx", Summary: "x", Notes: []string{"раздела нет"}})
	if err != nil {
		t.Fatal(err)
	}
	if card.TaxonKey != 7 || len(card.Classification) != 1 || len(card.Sources) != 1 {
		t.Errorf("карточка: %+v", card)
	}
}

func TestTrackerReadSection(t *testing.T) {
	tr := newTracker()
	tr.record("read_wikipedia", `{"title":"Обыкновенная рысь","url":"u","intro":"...","sections":["Распространение","  Подвиды"]}`)
	tr.record("read_wikipedia", `{"title":"Обыкновенная рысь","url":"u","section":"Распространение и места обитания","found":true,"text":"..."}`)
	tr.record("read_wikipedia", `{"title":"Обыкновенная рысь","url":"u","section":"Генетика","found":false}`)

	cases := map[string]bool{
		"Распространение":                  true,
		"распространение и места обитания": true,
		"вступление":                       true,
		"":                                 true,
		"Генетика":                         false,
		"Питание":                          false,
	}
	for section, want := range cases {
		if got := tr.readSection("обыкновенная рысь", section); got != want {
			t.Errorf("readSection(%q) = %v, ожидалось %v", section, got, want)
		}
	}
	if tr.readSection("Другая статья", "") {
		t.Errorf("непрочитанная статья")
	}
	if list := tr.sectionList("Обыкновенная рысь"); strings.Join(list, "|") != "Распространение|Подвиды" {
		t.Errorf("оглавление: %q", list)
	}
	if title, u, ok := tr.anyArticle(); !ok || title != "Обыкновенная рысь" || u != "u" {
		t.Errorf("anyArticle: %q %q %v", title, u, ok)
	}
}

func TestChatAgent(t *testing.T) {
	fake := scripted(t)
	res, err := NewChat(deps(t, fake)).Run(context.Background(), agent.Task{Query: "рысь", Options: agent.Options{Diet: true}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != agent.KindText || !strings.Contains(res.Text, "Lynx") {
		t.Errorf("результат: %+v", res)
	}
	user := fake.Requests[0].Messages[1].Content
	if !strings.Contains(user, "рысь") || !strings.Contains(user, "питание") || strings.Contains(user, "дерево") {
		t.Errorf("опции должны попасть в запрос выборочно: %q", user)
	}
}

func TestCatalog(t *testing.T) {
	infos := Infos()
	if len(infos) != 3 || infos[0].Key != "team" {
		t.Errorf("каталог: %+v", infos)
	}
	if _, ok := Find("nope"); ok {
		t.Errorf("неизвестный тип найден")
	}
	e, ok := Find("chat")
	if !ok || e.Build(Deps{}).Name() != "chat" {
		t.Errorf("chat: %+v", e)
	}
	if got := optionsText(agent.Options{}); !strings.Contains(got, "только карточка") {
		t.Errorf("опции без выбора: %q", got)
	}
}
