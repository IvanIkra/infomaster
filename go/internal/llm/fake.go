package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
)

// Fake is a Model for tests: it records every request and answers with Respond. It checks the task like Haiku and
// touches neither the budget nor the journal.
type Fake struct {
	// Respond gives the answer of a request: any JSON-ready value (json.RawMessage as it is), or an error such as
	// NewError(BadAnswer, "...") or &BudgetExceeded{...}. Nil answers {}.
	Respond func(Request) (any, error)
	// Off makes Available false.
	Off bool

	mu    sync.Mutex
	calls []Request
}

// Available reports !Off.
func (f *Fake) Available() bool { return !f.Off }

// JSON records the request and answers it with Respond.
func (f *Fake) JSON(ctx context.Context, req Request) (json.RawMessage, error) {
	if !slices.Contains(Tasks, req.Task) {
		return nil, fmt.Errorf("unknown LLM task '%s'", req.Task)
	}
	f.mu.Lock()
	f.calls = append(f.calls, req)
	respond := f.Respond
	f.mu.Unlock()
	if respond == nil {
		return json.RawMessage("{}"), nil
	}
	answer, err := respond(req)
	if err != nil {
		return nil, err
	}
	if raw, ok := answer.(json.RawMessage); ok {
		return raw, nil
	}
	return json.Marshal(answer)
}

// Calls are the requests so far, oldest first.
func (f *Fake) Calls() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

var _ Model = (*Fake)(nil)
var _ Model = (*Haiku)(nil)
