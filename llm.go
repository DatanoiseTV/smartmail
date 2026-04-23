package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// LLM is an OpenAI chat-completions client. Works against OpenAI proper and
// any endpoint that implements the same contract — notably LMStudio, Ollama
// (with --openai), vLLM, llama.cpp server, etc.
type LLM struct {
	baseURL string
	apiKey  string
	model   string
	temp    float64
	maxTok  int
	http    *http.Client
}

func NewLLM(cfg LLMConfig) *LLM {
	base := strings.TrimRight(cfg.BaseURL, "/")
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	return &LLM{
		baseURL: base,
		apiKey:  cfg.APIKey,
		model:   cfg.Model,
		temp:    cfg.Temperature,
		maxTok:  cfg.MaxTokens,
		http:    &http.Client{Timeout: time.Duration(cfg.TimeoutSec) * time.Second},
	}
}

// Chat request/response types — intentionally close to the OpenAI wire format
// so we're maximally compatible with every OpenAI-compatible server.

type chatMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
}

type toolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function toolCallFunc `json:"function"`
}

type toolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type tool struct {
	Type     string       `json:"type"`
	Function functionSpec `json:"function"`
}

type functionSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Tools       []tool        `json:"tools,omitempty"`
	ToolChoice  any           `json:"tool_choice,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message      chatMessage `json:"message"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error,omitempty"`
}

// Call sends a chat completion request and returns the assistant message.
// If tools are provided the model is strongly encouraged to call one.
func (l *LLM) Call(ctx context.Context, messages []chatMessage, tools []tool) (*chatMessage, error) {
	req := chatRequest{
		Model:       l.model,
		Messages:    messages,
		Temperature: l.temp,
		MaxTokens:   l.maxTok,
		Tools:       tools,
	}
	if len(tools) > 0 {
		// "required" forces the model to pick one of our tools rather than
		// free-form replying, which is exactly what we want for structured
		// classification. OpenAI supports this; many compatible servers also
		// understand "auto" — we fall back if the server rejects "required".
		req.ToolChoice = "required"
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	call := func(payload []byte) (*chatResponse, error) {
		r, err := http.NewRequestWithContext(ctx, "POST", l.baseURL+"/chat/completions", bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		r.Header.Set("Content-Type", "application/json")
		if l.apiKey != "" {
			r.Header.Set("Authorization", "Bearer "+l.apiKey)
		}
		resp, err := l.http.Do(r)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= 400 {
			return nil, fmt.Errorf("llm http %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
		}
		var out chatResponse
		if err := json.Unmarshal(b, &out); err != nil {
			return nil, fmt.Errorf("parse llm response: %w; body=%s", err, string(b))
		}
		if out.Error != nil {
			return nil, fmt.Errorf("llm error: %s", out.Error.Message)
		}
		if len(out.Choices) == 0 {
			return nil, fmt.Errorf("llm returned no choices")
		}
		return &out, nil
	}

	out, err := call(body)
	if err != nil && strings.Contains(err.Error(), "tool_choice") {
		// Retry once with "auto" for servers that don't accept "required".
		req.ToolChoice = "auto"
		body, _ = json.Marshal(req)
		out, err = call(body)
	}
	if err != nil {
		return nil, err
	}
	msg := out.Choices[0].Message
	return &msg, nil
}
