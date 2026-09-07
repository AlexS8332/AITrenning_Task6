package server

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/AlexS8332/AITrenning_Task6/internal/agent"
	"github.com/AlexS8332/AITrenning_Task6/internal/agents"
	"github.com/AlexS8332/AITrenning_Task6/internal/llm"
	"github.com/AlexS8332/AITrenning_Task6/internal/llm/llmtest"
	"github.com/AlexS8332/AITrenning_Task6/internal/runs"
	"github.com/AlexS8332/AITrenning_Task6/internal/tools"
)

func newTestServer(t *testing.T, fake *llmtest.Fake) (*httptest.Server, *runs.ReportStore) {
	t.Helper()
	store := runs.NewReportStore(filepath.Join(t.TempDir(), "results.md"), false)
	manager := runs.NewManager(agents.Deps{
		Runner: agent.Runner{LLM: fake, Model: "deepseek-v4-flash"},
		Tools:  tools.NewRegistry(),
	}, store, time.Minute)
	static := fstest.MapFS{"index.html": {Data: []byte("<!DOCTYPE html><title>t</title>")}}
	srv := httptest.NewServer(New(manager, store, static))
	t.Cleanup(srv.Close)
	return srv, store
}

func TestAgentsAndStatic(t *testing.T) {
	srv, store := newTestServer(t, &llmtest.Fake{})

	resp, err := http.Get(srv.URL + "/api/agents")
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Agents     []agents.Info `json:"agents"`
		ReportPath string        `json:"reportPath"`
	}
	json.NewDecoder(resp.Body).Decode(&body)
	if len(body.Agents) != 3 || body.ReportPath != store.Path() {
		t.Errorf("каталог: %+v", body)
	}

	resp, _ = http.Get(srv.URL + "/")
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
		t.Errorf("страница: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}

func TestCreateRunValidation(t *testing.T) {
	srv, _ := newTestServer(t, &llmtest.Fake{})

	resp, _ := http.Get(srv.URL + "/api/runs")
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /api/runs: %d", resp.StatusCode)
	}
	resp, _ = http.Post(srv.URL+"/api/runs", "application/json", strings.NewReader(`{"query":"  "}`))
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("пустой запрос: %d", resp.StatusCode)
	}
	resp, _ = http.Post(srv.URL+"/api/runs", "application/json", strings.NewReader(`{"query":"рысь","agent":"nope"}`))
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("неизвестный агент: %d", resp.StatusCode)
	}
	resp, _ = http.Post(srv.URL+"/api/runs", "application/json", strings.NewReader(`{bad`))
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("битый JSON: %d", resp.StatusCode)
	}
	resp, _ = http.Get(srv.URL + "/api/runs/missing")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("отсутствующий прогон: %d", resp.StatusCode)
	}
}

func TestRunStreamsStateAndLogSeparately(t *testing.T) {
	release := make(chan struct{})
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		<-release
		return llmtest.Text("Рысь — хищник."), nil
	}}
	srv, store := newTestServer(t, fake)

	resp, err := http.Post(srv.URL+"/api/runs", "application/json", strings.NewReader(`{"agent":"chat","query":"рысь","options":{"diet":true}}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("создание прогона: %d", resp.StatusCode)
	}
	var view runs.View
	json.NewDecoder(resp.Body).Decode(&view)
	if view.ID == "" || view.Status != runs.StatusRunning || !view.Task.Options.Diet {
		t.Fatalf("состояние: %+v", view)
	}

	// Поток открывается, пока модель ещё «думает»: первым приходит снимок.
	stream, err := http.Get(srv.URL + "/api/runs/" + view.ID + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	if ct := stream.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type: %s", ct)
	}

	reader := bufio.NewReader(stream.Body)
	readEvent := func() (string, string) {
		var name, data string
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatalf("чтение потока: %v", err)
			}
			line = strings.TrimRight(line, "\r\n")
			switch {
			case strings.HasPrefix(line, "event: "):
				name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			case line == "" && name != "":
				return name, data
			}
		}
	}

	name, data := readEvent()
	if name != "snapshot" || !strings.Contains(data, `"view"`) {
		t.Fatalf("первым должен быть снимок: %s %s", name, data)
	}
	close(release)

	var seen []string
	var lastState runs.View
	for {
		name, data := readEvent()
		seen = append(seen, name)
		if name == "state" {
			json.Unmarshal([]byte(data), &lastState)
		}
		if name == "log" {
			var ev agent.Event
			json.Unmarshal([]byte(data), &ev)
			if ev.Agent != "chat" || ev.Seq == 0 {
				t.Errorf("событие журнала: %+v", ev)
			}
		}
		if name == "done" {
			break
		}
		if len(seen) > 50 {
			t.Fatal("поток не заканчивается")
		}
	}
	if !slices.Contains(seen, "log") || !slices.Contains(seen, "state") {
		t.Errorf("в потоке должны быть и журнал, и состояние: %v", seen)
	}
	if lastState.Status != runs.StatusDone || lastState.Result == nil || lastState.Result.Text != "Рысь — хищник." {
		t.Errorf("финальное состояние: %+v", lastState)
	}
	if lastState.Totals.LLMCalls != 1 {
		t.Errorf("счётчики: %+v", lastState.Totals)
	}

	// Снимок по обычному GET и отчёт.
	resp, _ = http.Get(srv.URL + "/api/runs/" + view.ID)
	var snap runs.Snapshot
	json.NewDecoder(resp.Body).Decode(&snap)
	if snap.View.Status != runs.StatusDone || len(snap.Events) < 3 {
		t.Errorf("снимок: %+v", snap.View)
	}
	resp, _ = http.Get(srv.URL + "/api/report")
	var report struct {
		Path     string `json:"path"`
		Markdown string `json:"markdown"`
	}
	json.NewDecoder(resp.Body).Decode(&report)
	if report.Path != store.Path() || !strings.Contains(report.Markdown, "Рысь — хищник.") {
		t.Errorf("отчёт: %+v", report)
	}

	resp, _ = http.Get(srv.URL + "/api/runs/" + view.ID + "/nope")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("неизвестное действие: %d", resp.StatusCode)
	}
}
