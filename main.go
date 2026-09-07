// Агент-справочник по животным: локальный сервер с веб-интерфейсом, в
// котором запускаются агенты с инструментами (Википедия, GBIF) поверх
// DeepSeek. Результат и журнал работы агента показываются раздельно.
package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/AlexS8332/AITrenning_Task6/internal/agent"
	"github.com/AlexS8332/AITrenning_Task6/internal/agents"
	"github.com/AlexS8332/AITrenning_Task6/internal/llm"
	"github.com/AlexS8332/AITrenning_Task6/internal/runs"
	"github.com/AlexS8332/AITrenning_Task6/internal/server"
	"github.com/AlexS8332/AITrenning_Task6/internal/tools"
)

// Фронтенд лежит в бинарнике: после `go build` приложение запускается одним
// файлом, без каталога с ассетами рядом.
//
//go:embed web
var webFiles embed.FS

const (
	// Общий срок одного прогона: команда агентов делает до двух десятков
	// запросов к модели и внешним источникам.
	runTimeout = 10 * time.Minute

	// Слушаем только петлевой интерфейс: приложение локальное.
	defaultAddr = "127.0.0.1:8768"
)

func main() {
	addr := flag.String("addr", defaultAddr, "адрес, на котором слушать")
	out := flag.String("out", "results.md", "файл, куда пишется прогон")
	appendMode := flag.Bool("append", false, "дописывать прогоны в конец файла, а не перезаписывать его текущим")
	open := flag.Bool("open", true, "открыть браузер при старте")
	flag.Parse()

	enableUTF8Console()
	loadEnvFiles()

	apiKey := strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY"))
	if apiKey == "" {
		fail(fmt.Errorf("не задан DEEPSEEK_API_KEY — задай переменную окружения или впиши ключ в .env (см. .env.example)"))
	}
	model := strings.TrimSpace(os.Getenv("DEEPSEEK_MODEL"))
	if model == "" {
		model = llm.DefaultModel
	}

	fetcher := tools.NewFetcher()
	wiki := tools.NewWikipedia(os.Getenv("WIKIPEDIA_BASE_URL"), fetcher)
	gbif := tools.NewGBIF(os.Getenv("GBIF_BASE_URL"), fetcher)
	registry := tools.NewRegistry(append(wiki.Tools(), gbif.Tools()...)...)

	deps := agents.Deps{
		Runner: agent.Runner{
			LLM:         llm.NewClient(apiKey, os.Getenv("DEEPSEEK_BASE_URL")),
			Model:       model,
			Temperature: 0,
		},
		Tools: registry,
	}

	static, err := fs.Sub(webFiles, "web")
	if err != nil {
		fail(fmt.Errorf("встроенный фронтенд не читается: %w", err))
	}

	store := runs.NewReportStore(*out, *appendMode)
	manager := runs.NewManager(deps, store, runTimeout)
	handler := server.New(manager, store, static)

	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		fail(fmt.Errorf("не удалось занять %s: %w", *addr, err))
	}

	url := "http://" + listener.Addr().String()
	httpServer := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}

	mode := "перезапись"
	if *appendMode {
		mode = "дописывание"
	}
	fmt.Println("Агент-справочник по животным")
	fmt.Println("  интерфейс:   " + url)
	fmt.Println("  модель:      " + model)
	fmt.Println("  инструменты: " + strings.Join(registry.Names(), ", "))
	fmt.Println("  результаты:  " + store.Path() + " (" + mode + ")")
	fmt.Println("  остановить:  Ctrl+C")

	errs := make(chan error, 1)
	go func() {
		if err := httpServer.Serve(listener); err != nil && err != http.ErrServerClosed {
			errs <- err
		}
	}()

	if *open {
		openBrowser(url)
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt)

	select {
	case err := <-errs:
		fail(err)
	case <-signals:
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	httpServer.Shutdown(ctx)
	fmt.Println("Остановлено.")
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "Ошибка: "+err.Error())
	os.Exit(1)
}
