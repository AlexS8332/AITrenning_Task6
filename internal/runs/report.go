package runs

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/AlexS8332/AITrenning_Task6/internal/agent"
)

// Шапка файла. При перезаписи пишется заново вместе с прогоном, при
// дописывании — один раз, при создании файла.
const reportHeader = `# Агент-справочник по животным: прогоны

Файл ведёт программа. Для каждого прогона записаны запрос, тип агента,
опции, результат и журнал работы: запросы к модели, вызовы инструментов,
расход токенов.
`

func blockStart(id string) string { return "<!-- run:" + id + " -->" }
func blockEnd(id string) string   { return "<!-- /run:" + id + " -->" }

// ReportStore — файл прогонов. По умолчанию файл отдаётся под последний
// прогон целиком; с appendMode он копит прогоны, и блок каждого ищется
// по метке.
type ReportStore struct {
	mu         sync.Mutex
	path       string
	appendMode bool
}

func NewReportStore(path string, appendMode bool) *ReportStore {
	return &ReportStore{path: path, appendMode: appendMode}
}

func (s *ReportStore) Path() string { return s.path }

// Save записывает прогон.
func (s *ReportStore) Save(view View, events []agent.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	block := blockStart(view.ID) + "\n" + RenderReport(view, events) + blockEnd(view.ID) + "\n"

	data, err := os.ReadFile(s.path)
	missing := os.IsNotExist(err)
	if err != nil && !missing {
		return fmt.Errorf("чтение %s: %w", s.path, err)
	}

	updated := reportHeader + block
	if s.appendMode {
		previous := string(data)
		if missing {
			previous = reportHeader
		}
		var replaced bool
		if updated, replaced = replaceBlock(previous, view.ID, block); !replaced {
			updated = previous + block
		}
	}
	if updated == string(data) {
		return nil
	}
	if err := os.WriteFile(s.path, []byte(updated), 0o644); err != nil {
		return fmt.Errorf("запись в %s: %w", s.path, err)
	}
	return nil
}

// Read отдаёт файл целиком.
func (s *ReportStore) Read() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("чтение %s: %w", s.path, err)
	}
	return string(data), nil
}

// replaceBlock подменяет блок прогона по его меткам, не трогая соседей.
func replaceBlock(text, id, block string) (string, bool) {
	start := strings.Index(text, blockStart(id))
	if start < 0 {
		return text, false
	}
	endMark := blockEnd(id)
	end := strings.Index(text[start:], endMark)
	if end < 0 {
		return text, false
	}
	end += start + len(endMark)
	if end < len(text) && text[end] == '\n' {
		end++
	}
	return text[:start] + block + text[end:], true
}

// RenderReport собирает markdown одного прогона: запрос, результат, счётчики
// и журнал. Отдельная функция от записи: формат проверяется тестом без файла.
func RenderReport(view View, events []agent.Event) string {
	var b strings.Builder

	fmt.Fprintf(&b, "\n---\n\n## Прогон %s\n\n", view.Started.Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&b, "- Запрос: «%s»\n", strings.TrimSpace(view.Task.Query))
	fmt.Fprintf(&b, "- Агент: %s (`%s`), модель `%s`\n", view.AgentTitle, view.AgentKey, view.Model)
	fmt.Fprintf(&b, "- Опции: %s\n", optionsLine(view.Task.Options))
	fmt.Fprintf(&b, "- Статус: %s\n\n", statusLine(view))

	b.WriteString("### Результат\n\n")
	b.WriteString(RenderResult(view.Result))

	b.WriteString("### Счётчики\n\n")
	t := view.Totals
	fmt.Fprintf(&b, "- Время: %.1f с\n", t.Seconds)
	fmt.Fprintf(&b, "- Запросов к модели: %d, вызовов инструментов: %d\n", t.LLMCalls, t.ToolCalls)
	fmt.Fprintf(&b, "- Токены: запрос %d (кэш %d), ответ %d, всего %d\n",
		t.Usage.Prompt, t.Usage.CacheHit, t.Usage.Completion, t.Usage.Total)
	if t.Cost.Known {
		fmt.Fprintf(&b, "- Стоимость: %s (%s тариф)\n", FormatUSD(t.Cost.USD), t.Cost.Tariff)
	}
	b.WriteString("\n### Промпты\n\n")
	b.WriteString(renderPrompts(events))
	b.WriteString("### Журнал\n\n")
	b.WriteString(renderLog(view, events))
	return b.String()
}

// renderPrompts — что получала модель на старте каждого агента: системный
// промпт, первое сообщение, инструменты. Промпты свёрнуты: они длинные и
// повторяются от прогона к прогону, а читать файл приходят за результатом.
func renderPrompts(events []agent.Event) string {
	var b strings.Builder
	for _, ev := range events {
		if ev.Kind != agent.EventPrompt {
			continue
		}
		var p agent.Prompt
		if err := json.Unmarshal([]byte(ev.Detail), &p); err != nil {
			fmt.Fprintf(&b, "**%s**\n\n```\n%s\n```\n\n", ev.Agent, strings.TrimSpace(ev.Detail))
			continue
		}
		fmt.Fprintf(&b, "<details>\n<summary>Агент %s</summary>\n\n", ev.Agent)
		fmt.Fprintf(&b, "Сообщение `system`:\n\n```\n%s\n```\n\n", strings.TrimSpace(p.System))
		fmt.Fprintf(&b, "Сообщение `user`:\n\n```\n%s\n```\n\n", strings.TrimSpace(p.User))
		if len(p.Tools) > 0 {
			b.WriteString("Инструменты:\n\n")
			for _, t := range p.Tools {
				mark := ""
				if t.Final {
					mark = " (завершающий)"
				}
				fmt.Fprintf(&b, "- `%s`%s — %s\n", t.Name, mark, t.Description)
			}
			b.WriteString("\n")
		}
		b.WriteString("</details>\n\n")
	}
	if b.Len() == 0 {
		return "Промптов нет.\n\n"
	}
	return b.String()
}

// RenderResult — результат в markdown.
func RenderResult(res *agent.Result) string {
	if res == nil {
		return "Результата нет.\n\n"
	}
	switch res.Kind {
	case agent.KindNone:
		return "**Сведений нет.** " + strings.TrimSpace(res.Text) + "\n\n"
	case agent.KindText:
		return strings.TrimSpace(res.Text) + "\n\n"
	case agent.KindCard:
		if res.Card == nil {
			return "Карточка пуста.\n\n"
		}
		return renderCard(res.Card)
	default:
		return "Неизвестный вид результата.\n\n"
	}
}

func renderCard(c *agent.Card) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**%s** (*%s*", c.Name, c.Latin)
	if c.Rank != "" {
		fmt.Fprintf(&b, ", %s", rankRu(c.Rank))
	}
	b.WriteString(")\n\n")
	b.WriteString(strings.TrimSpace(c.Summary) + "\n\n")

	if len(c.Classification) > 0 {
		b.WriteString("**Классификация**\n\n")
		for _, n := range c.Classification {
			rank := n.RankRu
			if rank == "" {
				rank = strings.ToLower(n.Rank)
			}
			if n.NameRu != "" {
				fmt.Fprintf(&b, "- %s: *%s* (%s)\n", rank, n.Name, n.NameRu)
			} else {
				fmt.Fprintf(&b, "- %s: *%s*\n", rank, n.Name)
			}
		}
		b.WriteString("\n")
	}
	if c.Habitat != "" {
		b.WriteString("**Среда обитания**\n\n" + strings.TrimSpace(c.Habitat) + "\n\n")
	}
	if c.Diet != "" {
		b.WriteString("**Питание**\n\n" + strings.TrimSpace(c.Diet) + "\n\n")
	}
	if len(c.Notes) > 0 {
		b.WriteString("**Оговорки**\n\n")
		for _, n := range c.Notes {
			fmt.Fprintf(&b, "- %s\n", n)
		}
		b.WriteString("\n")
	}
	if len(c.Sources) > 0 {
		b.WriteString("**Источники**\n\n")
		for _, s := range c.Sources {
			if s.URL != "" {
				fmt.Fprintf(&b, "- [%s](%s)\n", s.Title, s.URL)
			} else {
				fmt.Fprintf(&b, "- %s\n", s.Title)
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

func renderLog(view View, events []agent.Event) string {
	var b strings.Builder
	b.WriteString("| # | Время | Агент | Событие | Что произошло | Токены |\n")
	b.WriteString("|---|---|---|---|---|---|\n")
	rows := 0
	for _, ev := range events {
		if ev.Kind == agent.EventPrompt {
			continue // промпты в своём разделе
		}
		tokens := ""
		if ev.Usage != nil {
			tokens = fmt.Sprintf("%d / %d", ev.Usage.Prompt, ev.Usage.Completion)
		}
		fmt.Fprintf(&b, "| %d | +%.1f с | %s | %s | %s | %s |\n",
			ev.Seq, ev.Time.Sub(view.Started).Seconds(), ev.Agent, ev.Kind, cell(ev.Title), tokens)
		rows++
	}
	if rows == 0 {
		return "Журнал пуст.\n"
	}
	return b.String()
}

// cell экранирует текст для ячейки таблицы.
func cell(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	return strings.ReplaceAll(s, "\n", " ")
}

func optionsLine(o agent.Options) string {
	var parts []string
	if o.Classification {
		parts = append(parts, "классификация")
	}
	if o.Habitat {
		parts = append(parts, "среда обитания")
	}
	if o.Diet {
		parts = append(parts, "питание")
	}
	if len(parts) == 0 {
		return "только карточка"
	}
	return strings.Join(parts, ", ")
}

func statusLine(view View) string {
	switch view.Status {
	case StatusDone:
		return "завершён"
	case StatusFailed:
		return "ошибка: " + view.Error
	default:
		return "выполняется"
	}
}

var ranksRu = map[string]string{
	"SPECIES": "вид", "GENUS": "род", "FAMILY": "семейство", "ORDER": "отряд",
	"CLASS": "класс", "PHYLUM": "тип", "KINGDOM": "царство", "SUBSPECIES": "подвид",
}

func rankRu(rank string) string {
	if ru, ok := ranksRu[strings.ToUpper(rank)]; ok {
		return ru
	}
	return strings.ToLower(rank)
}

// FormatUSD печатает доллары с точностью, достаточной для долей цента.
func FormatUSD(usd float64) string {
	switch {
	case usd >= 0.01:
		return fmt.Sprintf("$%.4f", usd)
	default:
		return fmt.Sprintf("$%.6f", usd)
	}
}
