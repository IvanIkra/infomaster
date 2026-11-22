// Package llm is the one way into the LLM (§4, §7): every call passes the daily limit and lands in the journal.
//
// Processing code depends on Model, the small interface Haiku implements; tests use Fake.
package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/pyjson"
	"github.com/IvanIkra/infomaster/go/internal/settings"
)

// ModelID is the model of every call.
const ModelID = "claude-haiku-5-5"

// Tasks are the kinds of calls. The journal may still hold "summary" rows from before the summaries were removed
// (D55); no new call is one. quick: the web app's «+».
var Tasks = []string{"batch", "pdf", "image", "subjects", "grades", "hseapp_labels", "quick"}

// DefaultMaxTokens is max_tokens of a request that names none.
const DefaultMaxTokens = 8000

// ErrorKind tells the LLM errors apart.
type ErrorKind int

const (
	Failed    ErrorKind = iota // the call failed on the way (LLMError)
	BadAnswer                  // the model answered, but not usably: a refusal, a cut answer, no text; the same input would fail again
	Rejected                   // the API refused the request as invalid (400): the input or the setup, only a caller that knows which treats it as final
)

// Error is a failed call (LLMError, and its BadAnswer and Rejected). It is not a *BudgetExceeded.
type Error struct {
	Kind ErrorKind
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

// NewError is an LLMError of a kind; tests of other packages use it to make the fake fail.
func NewError(kind ErrorKind, msg string) error { return &Error{Kind: kind, Msg: msg} }

// IsBadAnswer reports whether err is a BadAnswer.
func IsBadAnswer(err error) bool { return isKind(err, BadAnswer) }

// IsRejected reports whether err is a Rejected request.
func IsRejected(err error) bool { return isKind(err, Rejected) }

func isKind(err error, kind ErrorKind) bool {
	var e *Error
	return errors.As(err, &e) && e.Kind == kind
}

// IsBudget reports whether err is a *BudgetExceeded (DoesNotFit too).
func IsBudget(err error) bool {
	var e *BudgetExceeded
	return errors.As(err, &e)
}

// Document is a PDF for the model to read, its pages as images and text: a scan has no other way in (§6.3).
type Document struct {
	Data  []byte
	Pages int
}

// Image is a picture for the model to look at (§5.1, D12): JPEG, PNG, GIF or WebP.
type Image struct {
	Data      []byte
	MediaType string
}

// Request is one call whose answer must match Schema; Documents, then Images go before the prompt, in order.
type Request struct {
	Task   string
	System string
	Prompt string
	// Schema is the JSON schema of the answer: a map[string]any, or json.RawMessage (or a string of JSON) to keep the
	// order of the properties, which is the order the model writes them in.
	Schema    any
	MaxTokens int    // 0: DefaultMaxTokens
	ChatID    *int64 // the chat the input came from, if any
	Documents []Document
	Images    []Image
}

// Model is what processing code calls.
type Model interface {
	// Available reports whether a key is set.
	Available() bool
	// JSON makes one call; the answer is the model's JSON text. Errors: *BudgetExceeded when the limit does not let
	// the call happen (nothing is spent or logged then), *Error when the call failed (it is logged with whatever
	// tokens it spent); its Kind BadAnswer when the failure is in the answer itself rather than in the way to it.
	JSON(ctx context.Context, req Request) (json.RawMessage, error)
}

// Haiku is Claude Haiku with JSON-schema structured output, behind the daily limit and the journal (§7).
type Haiku struct {
	Budget  *Budget
	Journal *Journal
	Model   string

	options []option.RequestOption
	once    sync.Once
	client  anthropic.Client
}

// NewHaiku makes the client; options go to the SDK after the service's own (a 120 s timeout, 3 retries).
func NewHaiku(budget *Budget, journal *Journal, options ...option.RequestOption) *Haiku {
	return &Haiku{Budget: budget, Journal: journal, Model: ModelID, options: options}
}

// Available reports whether ANTHROPIC_API_KEY or ANTHROPIC_AUTH_TOKEN is set.
func (h *Haiku) Available() bool {
	return os.Getenv("ANTHROPIC_API_KEY") != "" || os.Getenv("ANTHROPIC_AUTH_TOKEN") != ""
}

// getClient makes the SDK client on first use, when the key is known to be set.
func (h *Haiku) getClient() *anthropic.Client {
	h.once.Do(func() {
		opts := append([]option.RequestOption{option.WithRequestTimeout(120 * time.Second), option.WithMaxRetries(3)},
			h.options...)
		h.client = anthropic.NewClient(opts...)
	})
	return &h.client
}

// schemaMap is the schema as the SDK takes it: its top-level keys, each value as the caller wrote it.
func schemaMap(schema any) (map[string]any, string, error) {
	var raw []byte
	switch s := schema.(type) {
	case json.RawMessage:
		raw = s
	case []byte:
		raw = s
	case string:
		raw = []byte(s)
	default:
		var err error
		if raw, err = json.Marshal(schema); err != nil {
			return nil, "", err
		}
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, "", fmt.Errorf("the schema is not a JSON object: %w", err)
	}
	m := make(map[string]any, len(top))
	for k, v := range top {
		m[k] = v
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, "", err
	}
	ascii, err := pyjson.DumpsASCII(value) // json.dumps(schema): its length goes into the estimate
	return m, ascii, err
}

// JSON makes one call; see Model.
func (h *Haiku) JSON(ctx context.Context, req Request) (json.RawMessage, error) {
	if !slices.Contains(Tasks, req.Task) {
		return nil, fmt.Errorf("unknown LLM task '%s'", req.Task)
	}
	if !h.Available() {
		return nil, &Error{Failed, "ANTHROPIC_API_KEY is not set"}
	}
	price, ok, err := h.Budget.Price(h.Model)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, &Error{Failed, fmt.Sprintf("no price for %s in the llm_prices setting", h.Model)}
	}
	schema, schemaText, err := schemaMap(req.Schema)
	if err != nil {
		return nil, err
	}
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = DefaultMaxTokens
	}
	pages := 0
	for _, d := range req.Documents {
		pages += d.Pages
	}
	inputChars := utf8.RuneCountInString(req.System) + utf8.RuneCountInString(req.Prompt) + len(schemaText) +
		(pages*PageTokens+len(req.Images)*ImageTokens)*CharsPerToken
	ticket, err := h.Budget.Reserve(req.Task, Estimate(price, inputChars, maxTokens))
	if err != nil {
		return nil, err
	}
	started := time.Now()
	var content []anthropic.ContentBlockParamUnion
	for _, d := range req.Documents {
		content = append(content, anthropic.NewDocumentBlock(anthropic.Base64PDFSourceParam{
			Data: base64.StdEncoding.EncodeToString(d.Data)}))
	}
	for _, i := range req.Images {
		content = append(content, anthropic.NewImageBlockBase64(i.MediaType, base64.StdEncoding.EncodeToString(i.Data)))
	}
	content = append(content, anthropic.NewTextBlock(req.Prompt))
	response, text, err := h.ask(ctx, req.System, content, schema, maxTokens)
	if err == nil && !json.Valid([]byte(text)) {
		var probe any
		err = &Error{BadAnswer, "the answer is not JSON: " + json.Unmarshal([]byte(text), &probe).Error()}
	}
	if ctx.Err() != nil && err != nil {
		// Python's `except Exception` lets a cancelled task pass: no journal row, the reserve still goes.
		h.Budget.Release(ticket)
		return nil, err
	}
	message := ""
	if err != nil {
		message = err.Error()
	}
	h.record(req.Task, req.ChatID, price, started, response, err != nil, message)
	if releaseErr := h.Budget.Release(ticket); err == nil && releaseErr != nil {
		return nil, releaseErr
	}
	if err != nil {
		return nil, err
	}
	return json.RawMessage(text), nil
}

func (h *Haiku) record(task string, chatID *int64, price settings.Price, started time.Time,
	response *anthropic.Message, failed bool, message string) {
	var in, out, write, read int64
	if response != nil {
		u := response.Usage
		in, out, write, read = u.InputTokens, u.OutputTokens, u.CacheCreationInputTokens, u.CacheReadInputTokens
	}
	run := Run{
		At:           clock.UTCISO(clock.NowUTC()),
		Task:         task,
		ChatID:       chatID,
		Model:        h.Model,
		OK:           !failed,
		InputTokens:  in,
		OutputTokens: out,
		CacheWrite:   write,
		CacheRead:    read,
		CostUSD:      pyjson.Float(CostUSD(price, in, out, write, read)),
		DurationS:    pyjson.Float(settings.Round(time.Since(started).Seconds(), 2)),
	}
	if failed {
		run.Error = &message
	}
	if err := h.Journal.Add(run); err != nil {
		slog.Error("cannot record an LLM run", "err", err)
	}
}

// apiMessage is the message of an API error's body, or the body itself.
func apiMessage(e *anthropic.Error) string {
	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	raw := e.RawJSON()
	if json.Unmarshal([]byte(raw), &body) == nil && body.Error.Message != "" {
		return body.Error.Message
	}
	if raw != "" {
		return raw
	}
	return http.StatusText(e.StatusCode)
}

// ask makes the request; the response comes back with a BadAnswer too, since its tokens were spent.
func (h *Haiku) ask(ctx context.Context, system string, content []anthropic.ContentBlockParamUnion,
	schema map[string]any, maxTokens int) (*anthropic.Message, string, error) {
	response, err := h.getClient().Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(h.Model),
		MaxTokens: int64(maxTokens),
		System:    []anthropic.TextBlockParam{{Text: system}},
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(content...)},
		OutputConfig: anthropic.OutputConfigParam{
			Effort: anthropic.OutputConfigEffortLow,
			Format: anthropic.JSONOutputFormatParam{Schema: schema},
		},
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
		var apiErr *anthropic.Error
		if errors.As(err, &apiErr) {
			switch apiErr.StatusCode {
			case http.StatusUnauthorized:
				return nil, "", &Error{Failed, "Anthropic API key was rejected"}
			case http.StatusTooManyRequests:
				return nil, "", &Error{Failed, "Anthropic rate limit"}
			case http.StatusBadRequest:
				return nil, "", &Error{Rejected, fmt.Sprintf("Anthropic API error %d: %s", apiErr.StatusCode,
					apiMessage(apiErr))}
			}
			return nil, "", &Error{Failed, fmt.Sprintf("Anthropic API error %d: %s", apiErr.StatusCode,
				apiMessage(apiErr))}
		}
		return nil, "", &Error{Failed, fmt.Sprintf("cannot reach the Anthropic API: %v", err)}
	}
	slog.Info(fmt.Sprintf("haiku: %d in / %d out tokens", response.Usage.InputTokens, response.Usage.OutputTokens))
	problem := ""
	switch response.StopReason {
	case anthropic.StopReasonRefusal:
		category := string(response.StopDetails.Category)
		if category == "" {
			category = "None"
		}
		problem = fmt.Sprintf("request declined (%s)", category)
	case anthropic.StopReasonMaxTokens:
		problem = "answer was cut at max_tokens"
	}
	text, found := "", false
	for _, block := range response.Content {
		if block.Type == "text" {
			text, found = block.Text, true
			break
		}
	}
	if problem == "" && !found {
		problem = "no text in the answer"
	}
	if problem != "" {
		return response, "", &Error{BadAnswer, problem}
	}
	return response, text, nil
}
