// Package agent runs agent sessions: the loop that calls the model, the
// tools it sends to the session's toolbox, proposals and approvals
// (adr/0006, adr/0011, adr/0030).
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// ErrModel is returned when the model provider refuses or fails a call.
var ErrModel = errors.New("the model call failed")

// Message is one message of a chat, in the OpenAI form that OpenRouter
// takes.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type Tool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Parameters  any    `json:"parameters"`
	} `json:"function"`
}

type Request struct {
	Model    string
	Messages []Message
	Tools    []Tool
}

type Response struct {
	Message Message
	// Cost is what the provider charged, in US dollars.
	Cost float64
}

// Model is the model client, one of the extension points that may have an
// interface (adr/0006).
type Model interface {
	Complete(ctx context.Context, key string, req Request) (Response, error)
}

// OpenRouter calls OpenRouter's chat completions API.
// https://openrouter.ai/docs/api-reference/chat-completion
type OpenRouter struct {
	URL  string
	HTTP *http.Client
}

func (o OpenRouter) Complete(ctx context.Context, key string, req Request) (Response, error) {
	body, err := json.Marshal(map[string]any{
		"model":    req.Model,
		"messages": req.Messages,
		"tools":    req.Tools,
		// Automatic prompt caching for Claude models.
		// https://openrouter.ai/docs/features/prompt-caching
		"cache_control": map[string]string{"type": "ephemeral"},
	})
	if err != nil {
		return Response{}, err
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, o.URL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Response{}, err
	}
	r.Header.Set("Authorization", "Bearer "+key)
	r.Header.Set("Content-Type", "application/json")
	resp, err := o.HTTP.Do(r)
	if err != nil {
		return Response{}, fmt.Errorf("%w: %w", ErrModel, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return Response{}, fmt.Errorf("%w: %w", ErrModel, err)
	}
	var out struct {
		Choices []struct {
			Message Message `json:"message"`
		} `json:"choices"`
		// cost is in credits, which are US dollars.
		// https://openrouter.ai/docs/use-cases/usage-accounting
		Usage struct {
			Cost float64 `json:"cost"`
		} `json:"usage"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return Response{}, fmt.Errorf("%w: OpenRouter answered %d with no JSON", ErrModel, resp.StatusCode)
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return Response{}, fmt.Errorf("%w: OpenRouter refused the key", ErrModel)
	case resp.StatusCode == http.StatusPaymentRequired:
		return Response{}, fmt.Errorf("%w: the OpenRouter account has no credit left", ErrModel)
	case out.Error != nil:
		return Response{}, fmt.Errorf("%w: %s", ErrModel, out.Error.Message)
	case resp.StatusCode != http.StatusOK || len(out.Choices) == 0:
		return Response{}, fmt.Errorf("%w: OpenRouter answered %d", ErrModel, resp.StatusCode)
	}
	return Response{Message: out.Choices[0].Message, Cost: out.Usage.Cost}, nil
}
