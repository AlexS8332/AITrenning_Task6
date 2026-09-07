package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/AlexS8332/AITrenning_Task6/internal/agent"
	"github.com/AlexS8332/AITrenning_Task6/internal/tools"
)

// Solo — один агент со всеми инструментами. Сам ищет статью, сверяет латынь,
// читает разделы по опциям и собирает карточку одним завершающим вызовом.
type Solo struct {
	deps Deps
}

func NewSolo(deps Deps) agent.Agent {
	return &Solo{deps: deps}
}

func (s *Solo) Name() string { return "solo" }

const soloMaxSteps = 14

const soloSystem = `Ты агент-справочник по животным. Собираешь карточку животного по русскому названию, пользуясь инструментами.

` + groundingRules + `

Порядок работы:
1. search_wikipedia по запросу. Выбери статью строго по правилам проверки названия.
2. read_wikipedia без section: получишь вступление и список разделов. Латинское название обычно стоит во вступлении в скобках после «лат.».
3. match_taxon с латинским названием. Карточку можно собирать, только если found = true. Если found = false, сведений нет.
4. Если нужно классификационное дерево: taxon_tree с usage_key из match_taxon. В карточке к каждому узлу добавь русское название таксона, если уверен в нём (Животные, Хордовые, Млекопитающие, Хищные, Кошачьи…); иначе оставь пустым.
5. Если нужна среда обитания: read_wikipedia с section из списка разделов — подойдут «Распространение», «Ареал», «Среда обитания», «Место обитания», «Образ жизни». Перескажи 3–6 предложениями строго по тексту раздела.
6. Если нужно питание: read_wikipedia с section «Питание», «Пища», «Образ жизни» или похожим. Перескажи 3–6 предложениями строго по тексту.
7. submit_card. Краткое описание (summary) — 2–4 предложения по вступлению статьи. Если для опции раздела в статье нет, оставь поле пустым и объясни это в notes.

Не вызывай инструменты, которые не нужны для выбранных опций.`

// cardArgs — аргументы submit_card. Схема ниже описывает их модели.
type cardArgs struct {
	Name           string `json:"name_ru"`
	Latin          string `json:"latin"`
	Rank           string `json:"rank"`
	Summary        string `json:"summary"`
	Classification []struct {
		Rank   string `json:"rank"`
		Name   string `json:"name"`
		NameRu string `json:"name_ru"`
	} `json:"classification"`
	Habitat string `json:"habitat"`
	Diet    string `json:"diet"`
	Sources []struct {
		Title string `json:"title"`
		URL   string `json:"url"`
	} `json:"sources"`
	Notes []string `json:"notes"`
}

const cardSchema = `{
  "type": "object",
  "properties": {
    "name_ru": {"type": "string", "description": "Общепринятое русское название"},
    "latin": {"type": "string", "description": "Латинское название, ровно то, что подтвердил match_taxon"},
    "rank": {"type": "string", "description": "Ранг таксона из match_taxon: SPECIES, GENUS и т. п."},
    "summary": {"type": "string", "description": "Краткое описание, 2–4 предложения по вступлению статьи"},
    "classification": {
      "type": "array",
      "description": "Дерево классификации от царства до таксона, только если оно запрошено",
      "items": {
        "type": "object",
        "properties": {
          "rank": {"type": "string"},
          "name": {"type": "string", "description": "Латинское название таксона"},
          "name_ru": {"type": "string", "description": "Русское название, если уверен; иначе пусто"}
        },
        "required": ["rank", "name"]
      }
    },
    "habitat": {"type": "string", "description": "Среда обитания и ареал по тексту раздела, если запрошено"},
    "diet": {"type": "string", "description": "Питание по тексту раздела, если запрошено"},
    "sources": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {"title": {"type": "string"}, "url": {"type": "string"}},
        "required": ["title", "url"]
      }
    },
    "notes": {"type": "array", "items": {"type": "string"}, "description": "Оговорки: чего в источнике не нашлось"}
  },
  "required": ["name_ru", "latin", "summary"]
}`

func (s *Solo) Run(ctx context.Context, task agent.Task, em agent.Emitter) (agent.Result, error) {
	if em == nil {
		em = agent.Nop{}
	}
	started := time.Now()
	em.Log(agent.Event{Agent: s.Name(), Kind: agent.EventAgentStart,
		Title: "один агент: " + optionsText(task.Options)})

	tr := newTracker()
	observed := make([]tools.Tool, 0, 5)
	for _, t := range s.deps.Tools.Pick("search_wikipedia", "read_wikipedia", "match_taxon", "taxon_tree", "vernacular_names") {
		observed = append(observed, tr.observe(t))
	}

	spec := agent.Spec{
		Name:     s.Name(),
		System:   soloSystem,
		Tools:    observed,
		Finish:   []agent.Finisher{cardFinisher(tr, task.Options), notFoundFinisher()},
		MaxSteps: soloMaxSteps,
	}

	res, stats, err := s.deps.Runner.Run(ctx, spec, userMessage(task), em)
	if err != nil {
		em.Log(agent.Event{Agent: s.Name(), Kind: agent.EventAgentError, Title: err.Error(),
			Seconds: time.Since(started).Seconds()})
		return agent.Result{}, err
	}
	em.Log(agent.Event{Agent: s.Name(), Kind: agent.EventAgentDone,
		Title:   fmt.Sprintf("готово: шагов %d, вызовов инструментов %d", stats.Steps, stats.ToolCalls),
		Seconds: time.Since(started).Seconds()})
	return res, nil
}

// cardFinisher принимает карточку и проверяет её по журналу инструментов:
// латынь сверена, дерево взято из GBIF, разделы по опциям либо заполнены,
// либо объяснены в notes.
func cardFinisher(tr *tracker, opts agent.Options) agent.Finisher {
	return agent.Finisher{
		Name:        "submit_card",
		Description: "Отдать готовую карточку животного. Вызывается один раз, когда все нужные сведения собраны.",
		Parameters:  json.RawMessage(cardSchema),
		Handle: func(args json.RawMessage) (agent.Result, error) {
			var in cardArgs
			if err := tools.ParseArgs(args, &in); err != nil {
				return agent.Result{}, err
			}

			card, err := buildCard(tr, opts, in)
			if err != nil {
				return agent.Result{}, err
			}
			return agent.Result{Kind: agent.KindCard, Card: card}, nil
		},
	}
}

func buildCard(tr *tracker, opts agent.Options, in cardArgs) (*agent.Card, error) {
	in.Name = strings.TrimSpace(in.Name)
	in.Latin = strings.TrimSpace(in.Latin)
	in.Summary = strings.TrimSpace(in.Summary)

	var problems []string
	if in.Name == "" {
		problems = append(problems, "name_ru пуст")
	}
	if in.Summary == "" {
		problems = append(problems, "summary пуст")
	}
	key, matched := tr.matchedKey(in.Latin)
	if !matched {
		problems = append(problems, fmt.Sprintf("латинское название «%s» не подтверждено: сначала вызови match_taxon и получи found = true", in.Latin))
	}

	rank := strings.ToUpper(strings.TrimSpace(in.Rank))
	if r := tr.rank(key); r != "" {
		rank = r
	}
	card := &agent.Card{
		Name:     in.Name,
		Latin:    in.Latin,
		Rank:     rank,
		TaxonKey: key,
		Summary:  in.Summary,
		Habitat:  strings.TrimSpace(in.Habitat),
		Diet:     strings.TrimSpace(in.Diet),
	}
	for _, n := range in.Notes {
		if n = strings.TrimSpace(n); n != "" {
			card.Notes = append(card.Notes, n)
		}
	}

	if opts.Classification {
		tree, haveTree := tr.tree(key)
		switch {
		case !haveTree:
			problems = append(problems, "дерево классификации не запрошено: вызови taxon_tree с usage_key")
		case len(in.Classification) == 0:
			card.Classification = tree
		default:
			// Латынь и ранги берутся из GBIF, от модели — только русские
			// названия: их она знает, а придумать дерево не сможет.
			ru := make(map[string]string, len(in.Classification))
			for _, n := range in.Classification {
				ru[strings.ToLower(strings.TrimSpace(n.Name))] = strings.TrimSpace(n.NameRu)
			}
			card.Classification = make([]agent.TaxonNode, len(tree))
			for i, n := range tree {
				n.NameRu = ru[strings.ToLower(n.Name)]
				card.Classification[i] = n
			}
		}
	}
	if opts.Habitat && card.Habitat == "" && len(card.Notes) == 0 {
		problems = append(problems, "среда обитания запрошена, но habitat пуст и notes не объясняют почему")
	}
	if opts.Diet && card.Diet == "" && len(card.Notes) == 0 {
		problems = append(problems, "питание запрошено, но diet пуст и notes не объясняют почему")
	}

	if len(problems) > 0 {
		return nil, fmt.Errorf("карточка не принята: %s", strings.Join(problems, "; "))
	}

	for _, src := range in.Sources {
		card.AddSource(agent.Source{Title: strings.TrimSpace(src.Title), URL: strings.TrimSpace(src.URL)})
	}
	// Источник Википедии добавляется программой по журналу чтения: модель
	// нередко забывает про sources, а ссылка на прочитанную статью обязана
	// быть в карточке.
	if title, u, ok := tr.anyArticle(); ok && len(card.Sources) == 0 {
		card.AddSource(agent.Source{Title: "Википедия: " + title, URL: u})
	}
	if key > 0 {
		card.AddSource(agent.Source{Title: "GBIF: " + in.Latin, URL: fmt.Sprintf("https://www.gbif.org/species/%d", key)})
	}
	return card, nil
}
