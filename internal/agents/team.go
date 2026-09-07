package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/AlexS8332/AITrenning_Task6/internal/agent"
	"github.com/AlexS8332/AITrenning_Task6/internal/tools"
)

// Team — координатор и его агенты. Координатор написан кодом, а не
// промптом: опции уже определяют, кого звать, и детерминированный порядок
// проще проверить и объяснить по журналу. Каждый агент внутри — цикл с
// моделью и своими инструментами.
type Team struct {
	deps Deps
}

func NewTeam(deps Deps) agent.Agent {
	return &Team{deps: deps}
}

func (t *Team) Name() string { return "coordinator" }

const (
	identifierMaxSteps     = 10
	classificationMaxSteps = 4
	sectionMaxSteps        = 6
)

// taxon — результат идентификатора: подтверждённый таксон и статья о нём.
type taxon struct {
	Name     string   `json:"name_ru"`
	Latin    string   `json:"latin"`
	Rank     string   `json:"rank"`
	UsageKey int      `json:"usage_key"`
	Title    string   `json:"wiki_title"`
	URL      string   `json:"wiki_url"`
	Summary  string   `json:"summary"`
	Sections []string `json:"-"`
}

func (t *Team) Run(ctx context.Context, task agent.Task, em agent.Emitter) (agent.Result, error) {
	if em == nil {
		em = agent.Nop{}
	}
	started := time.Now()
	em.Log(agent.Event{Agent: t.Name(), Kind: agent.EventAgentStart,
		Title: "команда: сначала идентификатор, затем " + optionsText(task.Options)})

	tx, res, err := t.identify(ctx, task, em)
	if err != nil {
		em.Log(agent.Event{Agent: t.Name(), Kind: agent.EventAgentError, Title: err.Error(),
			Seconds: time.Since(started).Seconds()})
		return agent.Result{}, err
	}
	if res.Kind == agent.KindNone {
		em.Log(agent.Event{Agent: t.Name(), Kind: agent.EventAgentDone,
			Title:   "идентификатор не нашёл животное, специалисты не запускались",
			Seconds: time.Since(started).Seconds()})
		return res, nil
	}

	card := &agent.Card{
		Name:     tx.Name,
		Latin:    tx.Latin,
		Rank:     tx.Rank,
		TaxonKey: tx.UsageKey,
		Summary:  tx.Summary,
	}
	card.AddSource(agent.Source{Title: "Википедия: " + tx.Title, URL: tx.URL})
	card.AddSource(agent.Source{Title: "GBIF: " + tx.Latin, URL: fmt.Sprintf("https://www.gbif.org/species/%d", tx.UsageKey)})

	var mu sync.Mutex
	publish := func() {
		mu.Lock()
		copyCard := *card
		mu.Unlock()
		em.Partial(agent.Result{Kind: agent.KindCard, Card: &copyCard})
	}
	publish()

	// Специалисты независимы друг от друга и работают параллельно: прогон
	// длится столько, сколько самый долгий из них.
	// Специалист возвращает правку карточки; сама правка применяется под
	// замком, а работа с моделью идёт без него.
	var wg sync.WaitGroup
	launch := func(name string, work func() (func(*agent.Card), error)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			apply, err := work()
			mu.Lock()
			if err != nil {
				card.Notes = append(card.Notes, fmt.Sprintf("агент %s не справился: %v", name, err))
			} else {
				apply(card)
			}
			mu.Unlock()
			publish()
		}()
	}

	if task.Options.Classification {
		launch("classification", func() (func(*agent.Card), error) {
			tree, err := t.classify(ctx, tx, em)
			return func(c *agent.Card) { c.Classification = tree }, err
		})
	}
	if task.Options.Habitat {
		launch("habitat", func() (func(*agent.Card), error) {
			text, note, err := t.section(ctx, sectionSpecs["habitat"], tx, em)
			return func(c *agent.Card) { c.Habitat = text; c.Notes = appendNote(c.Notes, note) }, err
		})
	}
	if task.Options.Diet {
		launch("diet", func() (func(*agent.Card), error) {
			text, note, err := t.section(ctx, sectionSpecs["diet"], tx, em)
			return func(c *agent.Card) { c.Diet = text; c.Notes = appendNote(c.Notes, note) }, err
		})
	}
	wg.Wait()

	em.Log(agent.Event{Agent: t.Name(), Kind: agent.EventAgentDone, Title: "карточка собрана",
		Seconds: time.Since(started).Seconds()})
	mu.Lock()
	final := *card
	mu.Unlock()
	return agent.Result{Kind: agent.KindCard, Card: &final}, nil
}

func appendNote(notes []string, note string) []string {
	if note == "" {
		return notes
	}
	return append(notes, note)
}

// --- идентификатор ---

const identifierSystem = `Ты агент-идентификатор. Твоя задача: по русскому названию найти статью в Википедии о запрошенном животном и подтвердить его латинское название по базе GBIF.

` + groundingRules + `

Порядок работы:
1. search_wikipedia по запросу. Выбери статью строго по правилам проверки названия.
2. read_wikipedia без section: вступление и список разделов. Латинское название обычно во вступлении в скобках после «лат.».
3. match_taxon с латинским названием. Подтверждено, только если found = true.
4. При сомнении, относится ли русское название к этому таксону, проверь vernacular_names по usage_key.
5. submit_taxon с русским названием, латинским названием, рангом, usage_key, заголовком статьи и кратким описанием (2–4 предложения по вступлению). Ничего сверх этого не ищи: разделы читают другие агенты.`

const taxonSchema = `{
  "type": "object",
  "properties": {
    "name_ru": {"type": "string", "description": "Общепринятое русское название"},
    "latin": {"type": "string", "description": "Латинское название, ровно то, что подтвердил match_taxon"},
    "rank": {"type": "string", "description": "Ранг из match_taxon"},
    "usage_key": {"type": "integer", "description": "usage_key из match_taxon"},
    "wiki_title": {"type": "string", "description": "Заголовок прочитанной статьи Википедии"},
    "summary": {"type": "string", "description": "Краткое описание, 2–4 предложения по вступлению статьи"}
  },
  "required": ["name_ru", "latin", "usage_key", "wiki_title", "summary"]
}`

func (t *Team) identify(ctx context.Context, task agent.Task, em agent.Emitter) (taxon, agent.Result, error) {
	const name = "identifier"
	started := time.Now()
	em.Log(agent.Event{Agent: name, Kind: agent.EventAgentStart, Title: "ищу статью и сверяю латынь"})

	tr := newTracker()
	observed := make([]tools.Tool, 0, 4)
	for _, tool := range t.deps.Tools.Pick("search_wikipedia", "read_wikipedia", "match_taxon", "vernacular_names") {
		observed = append(observed, tr.observe(tool))
	}

	var found taxon
	finish := agent.Finisher{
		Name:        "submit_taxon",
		Description: "Отдать подтверждённый таксон: русское и латинское название, ранг, usage_key, заголовок статьи, краткое описание.",
		Parameters:  json.RawMessage(taxonSchema),
		Handle: func(args json.RawMessage) (agent.Result, error) {
			var in taxon
			if err := tools.ParseArgs(args, &in); err != nil {
				return agent.Result{}, err
			}
			in.Name = strings.TrimSpace(in.Name)
			in.Latin = strings.TrimSpace(in.Latin)
			in.Title = strings.TrimSpace(in.Title)
			in.Summary = strings.TrimSpace(in.Summary)
			in.Rank = strings.ToUpper(strings.TrimSpace(in.Rank))

			var problems []string
			key, ok := tr.matchedKey(in.Latin)
			switch {
			case !ok:
				problems = append(problems, fmt.Sprintf("латинское название «%s» не подтверждено match_taxon", in.Latin))
			default:
				in.UsageKey = key
				if r := tr.rank(key); r != "" {
					in.Rank = r
				}
			}
			u, read := tr.articleURL(in.Title)
			if !read {
				problems = append(problems, fmt.Sprintf("статья «%s» не была прочитана через read_wikipedia", in.Title))
			}
			if in.Name == "" || in.Summary == "" {
				problems = append(problems, "name_ru и summary обязательны")
			}
			if len(problems) > 0 {
				return agent.Result{}, fmt.Errorf("таксон не принят: %s", strings.Join(problems, "; "))
			}
			in.URL = u
			in.Sections = tr.sectionList(in.Title)
			found = in
			return agent.Result{Kind: agent.KindCard}, nil
		},
	}

	spec := agent.Spec{
		Name:     name,
		System:   identifierSystem,
		Tools:    observed,
		Finish:   []agent.Finisher{finish, notFoundFinisher()},
		MaxSteps: identifierMaxSteps,
	}
	res, stats, err := t.deps.Runner.Run(ctx, spec, userMessage(agent.Task{Query: task.Query}), em)
	if err != nil {
		em.Log(agent.Event{Agent: name, Kind: agent.EventAgentError, Title: err.Error(),
			Seconds: time.Since(started).Seconds()})
		return taxon{}, agent.Result{}, fmt.Errorf("идентификатор: %w", err)
	}
	title := "сведений нет"
	if res.Kind == agent.KindCard {
		title = fmt.Sprintf("подтверждено: %s (%s)", found.Name, found.Latin)
	}
	em.Log(agent.Event{Agent: name, Kind: agent.EventAgentDone,
		Title:   fmt.Sprintf("%s; шагов %d, вызовов %d", title, stats.Steps, stats.ToolCalls),
		Seconds: time.Since(started).Seconds()})
	return found, res, nil
}

// --- классификация ---

const classificationSystem = `Ты агент-систематик. Получаешь подтверждённый таксон с usage_key и строишь дерево классификации.

Порядок работы:
1. taxon_tree с usage_key.
2. submit_classification: к каждому узлу дерева добавь русское название таксона, если уверен в нём (Животные, Хордовые, Млекопитающие, Хищные, Кошачьи, Рыси…). Если не уверен, оставь name_ru пустым. Латинские названия и ранги передавай ровно как в ответе taxon_tree.

Отвечай по-русски. Заверши работу вызовом submit_classification, а не текстом.`

const classificationSchema = `{
  "type": "object",
  "properties": {
    "tree": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "rank": {"type": "string"},
          "name": {"type": "string", "description": "Латинское название, как в taxon_tree"},
          "name_ru": {"type": "string", "description": "Русское название таксона, если уверен"}
        },
        "required": ["rank", "name"]
      }
    }
  },
  "required": ["tree"]
}`

func (t *Team) classify(ctx context.Context, tx taxon, em agent.Emitter) ([]agent.TaxonNode, error) {
	const name = "classification"
	started := time.Now()
	em.Log(agent.Event{Agent: name, Kind: agent.EventAgentStart, Title: "строю дерево классификации"})

	tr := newTracker()
	observed := []tools.Tool{tr.observe(t.deps.Tools.Pick("taxon_tree")[0])}

	var tree []agent.TaxonNode
	finish := agent.Finisher{
		Name:        "submit_classification",
		Description: "Отдать дерево классификации с русскими названиями таксонов.",
		Parameters:  json.RawMessage(classificationSchema),
		Handle: func(args json.RawMessage) (agent.Result, error) {
			var in struct {
				Tree []struct {
					Rank   string `json:"rank"`
					Name   string `json:"name"`
					NameRu string `json:"name_ru"`
				} `json:"tree"`
			}
			if err := tools.ParseArgs(args, &in); err != nil {
				return agent.Result{}, err
			}
			base, ok := tr.tree(tx.UsageKey)
			if !ok {
				return agent.Result{}, fmt.Errorf("дерево не запрошено: вызови taxon_tree с usage_key %d", tx.UsageKey)
			}
			ru := make(map[string]string, len(in.Tree))
			for _, n := range in.Tree {
				ru[strings.ToLower(strings.TrimSpace(n.Name))] = strings.TrimSpace(n.NameRu)
			}
			tree = make([]agent.TaxonNode, len(base))
			for i, n := range base {
				n.NameRu = ru[strings.ToLower(n.Name)]
				tree[i] = n
			}
			return agent.Result{Kind: agent.KindCard}, nil
		},
	}

	spec := agent.Spec{
		Name:     name,
		System:   classificationSystem,
		Tools:    observed,
		Finish:   []agent.Finisher{finish},
		MaxSteps: classificationMaxSteps,
	}
	user := fmt.Sprintf("Таксон: %s (%s), ранг %s, usage_key %d.", tx.Name, tx.Latin, tx.Rank, tx.UsageKey)
	_, stats, err := t.deps.Runner.Run(ctx, spec, user, em)
	if err != nil {
		em.Log(agent.Event{Agent: name, Kind: agent.EventAgentError, Title: err.Error(),
			Seconds: time.Since(started).Seconds()})
		return nil, err
	}
	em.Log(agent.Event{Agent: name, Kind: agent.EventAgentDone,
		Title:   fmt.Sprintf("дерево из %d узлов; шагов %d", len(tree), stats.Steps),
		Seconds: time.Since(started).Seconds()})
	return tree, nil
}

// --- специалисты по разделам статьи ---

// sectionSpec — чем отличаются специалисты: тема, кандидаты на название
// раздела и поле карточки.
type sectionSpec struct {
	Agent      string
	Topic      string
	Candidates []string
	Field      string
}

var sectionSpecs = map[string]sectionSpec{
	"habitat": {
		Agent:      "habitat",
		Topic:      "среда обитания и ареал",
		Candidates: []string{"Распространение", "Ареал", "Среда обитания", "Место обитания", "Распространение и среда обитания", "Образ жизни"},
		Field:      "habitat",
	},
	"diet": {
		Agent:      "diet",
		Topic:      "питание",
		Candidates: []string{"Питание", "Пища", "Рацион", "Образ жизни и питание", "Образ жизни"},
		Field:      "diet",
	},
}

const sectionSystemTemplate = `Ты агент-специалист по теме «%s». Получаешь статью Википедии о животном и список её разделов. Твоя задача: найти в статье сведения по своей теме и пересказать их.

Правила:
- Работай только по тексту, который вернул read_wikipedia. Ничего не добавляй из памяти.
- Начни с раздела, название которого ближе всего к теме (например: %s). Если в списке нет подходящего названия, проверь разделы, где тема могла быть описана внутри («Образ жизни», «Биология», «Описание»).
- Если подходящих разделов нет, но сведения по теме есть во вступлении статьи (read_wikipedia без section), перескажи их и укажи section = «вступление».
- Пересказ: 3–6 предложений по-русски, по существу, без вступлений. В поле section передавай название раздела так, как его вернул read_wikipedia.
- Если сведений по теме в статье нет, вызови submit_section с found = false и коротким объяснением.

Заверши работу вызовом submit_section, а не текстом.`

const sectionSchema = `{
  "type": "object",
  "properties": {
    "found": {"type": "boolean", "description": "Нашлись ли сведения по теме"},
    "section": {"type": "string", "description": "Название раздела, из которого взят пересказ"},
    "text": {"type": "string", "description": "Пересказ 3–6 предложениями; при found = false — объяснение"}
  },
  "required": ["found", "text"]
}`

// section запускает специалиста и возвращает текст, примечание и ошибку.
func (t *Team) section(ctx context.Context, sp sectionSpec, tx taxon, em agent.Emitter) (string, string, error) {
	started := time.Now()
	em.Log(agent.Event{Agent: sp.Agent, Kind: agent.EventAgentStart,
		Title: "читаю статью «" + tx.Title + "»: " + sp.Topic})

	tr := newTracker()
	observed := []tools.Tool{tr.observe(t.deps.Tools.Pick("read_wikipedia")[0])}

	var (
		text, note string
	)
	finish := agent.Finisher{
		Name:        "submit_section",
		Description: "Отдать пересказ по теме или сообщить, что сведений в статье нет.",
		Parameters:  json.RawMessage(sectionSchema),
		Handle: func(args json.RawMessage) (agent.Result, error) {
			var in struct {
				Found   bool   `json:"found"`
				Section string `json:"section"`
				Text    string `json:"text"`
			}
			if err := tools.ParseArgs(args, &in); err != nil {
				return agent.Result{}, err
			}
			in.Text = strings.TrimSpace(in.Text)
			if in.Text == "" {
				return agent.Result{}, fmt.Errorf("text пуст")
			}
			if !in.Found {
				note = fmt.Sprintf("%s: в статье «%s» сведений не нашлось (%s)", sp.Topic, tx.Title, in.Text)
				return agent.Result{Kind: agent.KindCard}, nil
			}
			if !tr.readSection(tx.Title, in.Section) {
				return agent.Result{}, fmt.Errorf("раздел «%s» не был прочитан через read_wikipedia: пересказ принимается только по прочитанному тексту", in.Section)
			}
			text = in.Text
			return agent.Result{Kind: agent.KindCard}, nil
		},
	}

	spec := agent.Spec{
		Name:     sp.Agent,
		System:   fmt.Sprintf(sectionSystemTemplate, sp.Topic, strings.Join(sp.Candidates, ", ")),
		Tools:    observed,
		Finish:   []agent.Finisher{finish},
		MaxSteps: sectionMaxSteps,
	}
	user := fmt.Sprintf("Животное: %s (%s).\nСтатья: «%s».\nРазделы статьи:\n- %s",
		tx.Name, tx.Latin, tx.Title, strings.Join(tx.Sections, "\n- "))
	_, stats, err := t.deps.Runner.Run(ctx, spec, user, em)
	if err != nil {
		em.Log(agent.Event{Agent: sp.Agent, Kind: agent.EventAgentError, Title: err.Error(),
			Seconds: time.Since(started).Seconds()})
		return "", "", err
	}
	title := "сведения найдены"
	if text == "" {
		title = "сведений в статье нет"
	}
	em.Log(agent.Event{Agent: sp.Agent, Kind: agent.EventAgentDone,
		Title:   fmt.Sprintf("%s; шагов %d, вызовов %d", title, stats.Steps, stats.ToolCalls),
		Seconds: time.Since(started).Seconds()})
	return text, note, nil
}
