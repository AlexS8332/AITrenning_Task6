// Package agents — конкретные агенты приложения и их каталог. Все агенты
// собираются из одних и тех же частей: Runner из пакета agent, инструменты
// из реестра, промпты и завершающие инструменты отсюда.
package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/AlexS8332/AITrenning_Task6/internal/agent"
	"github.com/AlexS8332/AITrenning_Task6/internal/tools"
)

// Deps — то, что нужно любому агенту: цикл с моделью и инструменты.
type Deps struct {
	Runner agent.Runner
	Tools  *tools.Registry
}

// Info — описание типа агента для интерфейса.
type Info struct {
	Key         string `json:"key"`
	Title       string `json:"title"`
	Description string `json:"description"`
	// Agents — кто работает внутри: одно имя у одиночных агентов, список
	// у команды. Интерфейс по нему раскрашивает журнал.
	Agents []string `json:"agents"`
}

// Entry — тип агента в каталоге: описание и сборка.
type Entry struct {
	Info
	Build func(Deps) agent.Agent
}

// Catalog — все типы агентов в порядке показа. Ключ типа приходит из
// интерфейса в запросе на прогон.
func Catalog() []Entry {
	return []Entry{
		{
			Info: Info{
				Key:   "team",
				Title: "Команда агентов",
				Description: "Координатор запускает идентификатора, затем параллельно специалистов " +
					"по выбранным опциям: классификация, среда обитания, питание. Каждый агент " +
					"работает по своим инструментам и отвечает за свою часть карточки.",
				Agents: []string{"coordinator", "identifier", "classification", "habitat", "diet"},
			},
			Build: NewTeam,
		},
		{
			Info: Info{
				Key:   "solo",
				Title: "Один агент с инструментами",
				Description: "Один агент со всеми инструментами: сам ищет статью, сверяет латынь с GBIF, " +
					"читает нужные разделы и собирает карточку целиком.",
				Agents: []string{"solo"},
			},
			Build: NewSolo,
		},
		{
			Info: Info{
				Key:   "chat",
				Title: "Чат без инструментов",
				Description: "Один вызов модели без проверок: ответ из памяти модели. " +
					"Точка отсчёта, с которой сравниваются агенты с инструментами.",
				Agents: []string{"chat"},
			},
			Build: NewChat,
		},
	}
}

// Find ищет тип агента по ключу.
func Find(key string) (Entry, bool) {
	for _, e := range Catalog() {
		if e.Key == key {
			return e, true
		}
	}
	return Entry{}, false
}

// Infos — описания всех типов для интерфейса.
func Infos() []Info {
	entries := Catalog()
	infos := make([]Info, 0, len(entries))
	for _, e := range entries {
		infos = append(infos, e.Info)
	}
	return infos
}

// optionsText описывает выбранные опции словами — для сообщения модели.
func optionsText(o agent.Options) string {
	var parts []string
	if o.Classification {
		parts = append(parts, "классификационное дерево")
	}
	if o.Habitat {
		parts = append(parts, "среда обитания")
	}
	if o.Diet {
		parts = append(parts, "питание")
	}
	if len(parts) == 0 {
		return "только карточка: название, латинское название, краткое описание"
	}
	return strings.Join(parts, ", ")
}

// userMessage — первое сообщение пользователя для агентов с инструментами.
func userMessage(task agent.Task) string {
	return fmt.Sprintf("Запрос: «%s».\nНужно: %s.", strings.TrimSpace(task.Query), optionsText(task.Options))
}

// tracker запоминает, что агент реально проверил через инструменты. По нему
// завершающий инструмент отклоняет карточку с латынью, которую никто не
// сверял, и раздел, который никто не читал: ответ по памяти модели не
// проходит, даже если выглядит правдоподобно.
type tracker struct {
	mu       sync.Mutex
	matched  map[string]int             // латынь в нижнем регистре → usage_key
	ranks    map[int]string             // usage_key → ранг из GBIF
	trees    map[int][]agent.TaxonNode  // usage_key → дерево из GBIF
	articles map[string]string          // заголовок статьи в нижнем регистре → URL
	titles   map[string]string          // заголовок в нижнем регистре → как он написан в статье
	lists    map[string][]string        // заголовок статьи → оглавление
	sections map[string]map[string]bool // заголовок статьи → прочитанные разделы
}

func newTracker() *tracker {
	return &tracker{
		matched:  make(map[string]int),
		ranks:    make(map[int]string),
		trees:    make(map[int][]agent.TaxonNode),
		articles: make(map[string]string),
		titles:   make(map[string]string),
		lists:    make(map[string][]string),
		sections: make(map[string]map[string]bool),
	}
}

// observe оборачивает инструмент: результат уходит модели как есть, а
// копия разбирается и запоминается.
func (t *tracker) observe(tool tools.Tool) tools.Tool {
	return tools.Func{
		FuncName:        tool.Name(),
		FuncDescription: tool.Description(),
		FuncParameters:  tool.Parameters(),
		FuncCall: func(ctx context.Context, args json.RawMessage) (string, error) {
			out, err := tool.Call(ctx, args)
			if err == nil {
				t.record(tool.Name(), out)
			}
			return out, err
		},
	}
}

func (t *tracker) record(name, out string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	switch name {
	case "match_taxon":
		var m struct {
			Found     bool   `json:"found"`
			UsageKey  int    `json:"usage_key"`
			Canonical string `json:"canonical_name"`
			Rank      string `json:"rank"`
		}
		if json.Unmarshal([]byte(out), &m) == nil && m.Found && m.Canonical != "" {
			t.matched[strings.ToLower(m.Canonical)] = m.UsageKey
			t.ranks[m.UsageKey] = m.Rank
		}
	case "taxon_tree":
		var r struct {
			UsageKey int `json:"usage_key"`
			Tree     []struct {
				Rank   string `json:"rank"`
				RankRu string `json:"rank_ru"`
				Name   string `json:"name"`
			} `json:"tree"`
		}
		if json.Unmarshal([]byte(out), &r) == nil && len(r.Tree) > 0 {
			nodes := make([]agent.TaxonNode, 0, len(r.Tree))
			for _, n := range r.Tree {
				nodes = append(nodes, agent.TaxonNode{Rank: n.Rank, RankRu: n.RankRu, Name: n.Name})
			}
			t.trees[r.UsageKey] = nodes
		}
	case "read_wikipedia":
		var r struct {
			Title    string   `json:"title"`
			URL      string   `json:"url"`
			Section  string   `json:"section"`
			Found    *bool    `json:"found"`
			Sections []string `json:"sections"`
		}
		if json.Unmarshal([]byte(out), &r) == nil && r.Title != "" {
			key := strings.ToLower(r.Title)
			t.articles[key] = r.URL
			t.titles[key] = r.Title
			if len(r.Sections) > 0 {
				t.lists[key] = r.Sections
			}
			if r.Section != "" && (r.Found == nil || *r.Found) {
				if t.sections[key] == nil {
					t.sections[key] = make(map[string]bool)
				}
				t.sections[key][strings.ToLower(r.Section)] = true
			}
		}
	}
}

// matchedKey — ключ таксона, если латынь была сверена и найдена.
func (t *tracker) matchedKey(latin string) (int, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	key, ok := t.matched[strings.ToLower(strings.TrimSpace(latin))]
	return key, ok
}

// rank — ранг таксона по данным сверки; ответ модели здесь не нужен.
func (t *tracker) rank(key int) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.ranks[key]
}

func (t *tracker) tree(key int) ([]agent.TaxonNode, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	nodes, ok := t.trees[key]
	return nodes, ok
}

func (t *tracker) articleURL(title string) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	u, ok := t.articles[strings.ToLower(strings.TrimSpace(title))]
	return u, ok
}

// readSection — был ли прочитан раздел статьи. Названия сравниваются по
// вхождению в обе стороны: инструмент ищет раздел по подстроке, и модель
// может назвать «Распространение», прочитав «Распространение и места
// обитания». Пустое название или «вступление» означают саму статью:
// она считается прочитанной, если её открывали без раздела.
func (t *tracker) readSection(title, section string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	key := strings.ToLower(strings.TrimSpace(title))
	want := strings.ToLower(strings.TrimSpace(section))
	if want == "" || strings.Contains(want, "вступлени") || want == "intro" || want == key {
		_, read := t.articles[key]
		return read
	}
	for have := range t.sections[key] {
		if strings.Contains(have, want) || strings.Contains(want, have) {
			return true
		}
	}
	return false
}

// sectionList — оглавление прочитанной статьи; подразделы без отступа.
func (t *tracker) sectionList(title string) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	list := t.lists[strings.ToLower(strings.TrimSpace(title))]
	out := make([]string, 0, len(list))
	for _, s := range list {
		out = append(out, strings.TrimSpace(s))
	}
	return out
}

// anyArticle — хоть одна прочитанная статья: заголовок и URL.
func (t *tracker) anyArticle() (string, string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for key, u := range t.articles {
		return t.titles[key], u, true
	}
	return "", "", false
}

// notFoundFinisher — общий завершающий инструмент «сведений нет».
func notFoundFinisher() agent.Finisher {
	return agent.Finisher{
		Name: "report_not_found",
		Description: "Сообщить, что сведений о запрошенном животном нет: это не животное, " +
			"название вымышлено, искажено, или статьи именно о нём не нашлось.",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "reason": {"type": "string", "description": "Короткое объяснение, почему сведений нет"}
  },
  "required": ["reason"]
}`),
		Handle: func(args json.RawMessage) (agent.Result, error) {
			var in struct {
				Reason string `json:"reason"`
			}
			if err := tools.ParseArgs(args, &in); err != nil {
				return agent.Result{}, err
			}
			in.Reason = strings.TrimSpace(in.Reason)
			if in.Reason == "" {
				in.Reason = "сведений о таком животном не найдено"
			}
			return agent.Result{Kind: agent.KindNone, Text: in.Reason}, nil
		},
	}
}

// Общие правила для всех агентов с инструментами. Главное здесь — запрет
// отвечать по памяти и запрет подменять незнакомое похожим: обе ошибки
// модель делает уверенно и незаметно.
const groundingRules = `Работай только по данным инструментов этого диалога. Не используй собственную память о животных: всё, что попадёт в результат, должно быть взято из результатов инструментов.

Проверка названия:
- Наличие результатов поиска не означает, что статья о запрошенном животном есть. Принимай статью, только если её заголовок обозначает то же животное, что и запрос (с точностью до формы слова и общепринятого синонима), или перенаправление ведёт с запрошенного названия.
- Похожее не значит то же самое: «полосатый манул» не равен «манулу», «малая выхухоль» не равна «выхухоли». Если статьи именно о запрошенном животном нет, сведений нет.
- Если запрос не про животное, это вымышленное или мифическое существо, искажённое или неизвестное название, сообщи, что сведений нет.
- Запрос без уточнения («рысь», «ёж», «волк») обычно означает типовой вид. Если поиск или перенаправление ведут на статью о роде, а среди результатов есть статья о виде с уточнением «обыкновенный», «европейский» и т. п., бери статью о виде: в ней есть разделы о распространении и питании, а в статье о роде их нет. Если запрошен именно род или группа во множественном числе («рыси», «ежи»), работай со статьёй о роде.

Отвечай по-русски. Заверши работу вызовом завершающего инструмента, а не текстом.`
