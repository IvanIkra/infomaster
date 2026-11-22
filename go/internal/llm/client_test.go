package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/option"
)

const schemaText = `{"type": "object", "properties": {"x": {"type": "integer"}, "a": {"type": "string"}}, "required": ["x"]}`

// server answers every request with status and body, and keeps the request bodies.
type server struct {
	mu     sync.Mutex
	bodies []map[string]any
	raw    []string
}

func answer(text, stopReason string, outputTokens int) string {
	content, _ := json.Marshal(text)
	return fmt.Sprintf(`{"id": "msg_1", "type": "message", "role": "assistant", "model": "claude-haiku-5-5",
		"content": [{"type": "text", "text": %s}], "stop_reason": "%s", "stop_details": null,
		"usage": {"input_tokens": 1000, "output_tokens": %d, "cache_creation_input_tokens": 0,
		"cache_read_input_tokens": 0}}`, content, stopReason, outputTokens)
}

func (f *fixture) haiku(t *testing.T, status int, body string) (*Haiku, *server) {
	t.Helper()
	t.Setenv("ANTHROPIC_API_KEY", "test")
	s := &server{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		var parsed map[string]any
		json.Unmarshal(data, &parsed)
		s.mu.Lock()
		s.bodies, s.raw = append(s.bodies, parsed), append(s.raw, string(data))
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(ts.Close)
	return NewHaiku(f.budget, f.journal, option.WithBaseURL(ts.URL), option.WithAPIKey("test"),
		option.WithMaxRetries(0)), s
}

func request(task string) Request {
	return Request{Task: task, System: "sys", Prompt: "data", Schema: json.RawMessage(schemaText)}
}

func TestOKCallIsLoggedAndReleased(t *testing.T) {
	f := setup(t)
	llm, srv := f.haiku(t, 200, answer(`{"x": 1}`, "end_turn", 100))
	req := request("batch")
	req.ChatID = id(-100)
	got := must(llm.JSON(context.Background(), req))
	if string(got) != `{"x": 1}` {
		t.Fatal(string(got))
	}
	call := srv.bodies[0]
	if call["model"] != "claude-haiku-5-5" || call["max_tokens"] != 8000.0 {
		t.Fatal(call)
	}
	var schema any
	json.Unmarshal([]byte(schemaText), &schema)
	want := map[string]any{"effort": "low", "format": map[string]any{"type": "json_schema", "schema": schema}}
	if !reflect.DeepEqual(call["output_config"], want) {
		t.Fatal(call["output_config"])
	}
	if !strings.Contains(srv.raw[0], `"properties":{"x":{"type":"integer"},"a":{"type":"string"}}`) {
		t.Fatal("the schema's properties lost their order: " + srv.raw[0])
	}
	messages := call["messages"].([]any)[0].(map[string]any)
	if !reflect.DeepEqual(messages["content"], []any{map[string]any{"type": "text", "text": "data"}}) {
		t.Fatal(messages)
	}
	run := must(f.journal.Runs(100, ""))[0]
	if !run.OK || run.Task != "batch" || *run.ChatID != -100 || run.Error != nil {
		t.Fatalf("%+v", run)
	}
	if !approx(float64(run.CostUSD), (1000*0.10+100*0.50)/1e6) {
		t.Fatal(run.CostUSD)
	}
	if spent := must(f.budget.Spent()); !approx(spent, float64(run.CostUSD)) { // the reserve is gone
		t.Fatal(spent)
	}
}

func TestDocumentsAndImagesGoFirst(t *testing.T) {
	f := setup(t)
	llm, srv := f.haiku(t, 200, answer(`{"x": 1}`, "end_turn", 10))
	req := request("pdf")
	req.Documents = []Document{{Data: []byte("%PDF"), Pages: 2}}
	req.Images = []Image{{Data: []byte("png"), MediaType: "image/png"}}
	req.MaxTokens = 400
	must(llm.JSON(context.Background(), req))
	content := srv.bodies[0]["messages"].([]any)[0].(map[string]any)["content"].([]any)
	want := []any{
		map[string]any{"type": "document", "source": map[string]any{"type": "base64", "media_type": "application/pdf",
			"data": "JVBERg=="}},
		map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png",
			"data": "cG5n"}},
		map[string]any{"type": "text", "text": "data"},
	}
	if !reflect.DeepEqual(content, want) || srv.bodies[0]["max_tokens"] != 400.0 {
		t.Fatal(content)
	}
}

func TestCutAnswerIsLoggedAsFailure(t *testing.T) {
	f := setup(t)
	llm, _ := f.haiku(t, 200, answer(`{"x": `, "max_tokens", 8000))
	_, err := llm.JSON(context.Background(), request("batch"))
	if !IsBadAnswer(err) || !strings.Contains(err.Error(), "max_tokens") {
		t.Fatal(err)
	}
	var e *Error
	if !errors.As(err, &e) || IsBudget(err) { // a BadAnswer is an LLMError, not a budget refusal
		t.Fatal(err)
	}
	run := must(f.journal.Runs(100, ""))[0]
	if run.OK || run.OutputTokens != 8000 || *run.Error != "answer was cut at max_tokens" {
		t.Fatalf("%+v", run)
	}
	if spent := must(f.budget.Spent()); !approx(spent, float64(run.CostUSD)) {
		t.Fatal(spent)
	}
}

func TestNotJSONIsABadAnswer(t *testing.T) {
	f := setup(t)
	llm, _ := f.haiku(t, 200, answer(`nope`, "end_turn", 10))
	_, err := llm.JSON(context.Background(), request("batch"))
	if !IsBadAnswer(err) || !strings.HasPrefix(err.Error(), "the answer is not JSON: ") {
		t.Fatal(err)
	}
	if run := must(f.journal.Runs(1, ""))[0]; run.InputTokens != 1000 {
		t.Fatalf("%+v", run)
	}
}

func TestAPIErrors(t *testing.T) {
	for _, c := range []struct {
		status   int
		message  string
		rejected bool
	}{
		{400, "Anthropic API error 400: bad image", true},
		{401, "Anthropic API key was rejected", false},
		{429, "Anthropic rate limit", false},
		{500, "Anthropic API error 500: bad image", false},
	} {
		f := setup(t)
		llm, _ := f.haiku(t, c.status, `{"type": "error", "error": {"type": "x", "message": "bad image"}}`)
		_, err := llm.JSON(context.Background(), request("image"))
		var e *Error
		if !errors.As(err, &e) || e.Msg != c.message || IsRejected(err) != c.rejected {
			t.Fatalf("%d: %v", c.status, err)
		}
		run := must(f.journal.Runs(1, ""))[0]
		if run.OK || *run.Error != c.message || run.InputTokens != 0 {
			t.Fatalf("%+v", run)
		}
	}
}

func TestBudgetRefusalMakesNoCall(t *testing.T) {
	f := setup(t)
	must(f.budget.Settings.Set("llm_daily_limit_usd", 0.001))
	llm, srv := f.haiku(t, 200, answer(`{"x": 1}`, "end_turn", 100))
	_, err := llm.JSON(context.Background(), request("batch"))
	if !IsBudget(err) {
		t.Fatal(err)
	}
	var e *Error
	if errors.As(err, &e) { // a budget refusal is not an LLMError
		t.Fatal(err)
	}
	if len(srv.bodies) != 0 || len(must(f.journal.Runs(100, ""))) != 0 {
		t.Fatal("a call was made")
	}
}

func TestCancelledCallIsNotLogged(t *testing.T) {
	f := setup(t)
	llm, _ := f.haiku(t, 200, answer(`{"x": 1}`, "end_turn", 100))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := llm.JSON(ctx, request("batch")); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if len(must(f.journal.Runs(100, ""))) != 0 || must(f.budget.Spent()) != 0 {
		t.Fatal("logged or still reserved")
	}
}

func TestNoKey(t *testing.T) {
	f := setup(t)
	llm, _ := f.haiku(t, 200, answer("{}", "end_turn", 1))
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	_, err := llm.JSON(context.Background(), request("batch"))
	if err == nil || !strings.Contains(err.Error(), "not set") {
		t.Fatal(err)
	}
}

func TestUnknownTask(t *testing.T) {
	f := setup(t)
	llm, _ := f.haiku(t, 200, answer("{}", "end_turn", 1))
	if _, err := llm.JSON(context.Background(), request("news")); err == nil ||
		err.Error() != "unknown LLM task 'news'" {
		t.Fatal(err)
	}
	if _, err := (&Fake{}).JSON(context.Background(), request("news")); err == nil {
		t.Fatal("the fake took an unknown task")
	}
}

func TestFake(t *testing.T) {
	fake := &Fake{Respond: func(r Request) (any, error) {
		if r.Prompt == "fail" {
			return nil, NewError(BadAnswer, "no")
		}
		return map[string]any{"items": []any{r.Prompt}}, nil
	}}
	var model Model = fake
	got := must(model.JSON(context.Background(), Request{Task: "subjects", Prompt: "p"}))
	if string(got) != `{"items":["p"]}` {
		t.Fatal(string(got))
	}
	if _, err := model.JSON(context.Background(), Request{Task: "subjects", Prompt: "fail"}); !IsBadAnswer(err) {
		t.Fatal(err)
	}
	if len(fake.Calls()) != 2 || !model.Available() {
		t.Fatal(fake.Calls())
	}
}
