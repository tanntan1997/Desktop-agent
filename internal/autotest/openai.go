package autotest

import (
	"context"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

// NewOpenAILLM returns an LLM backed by the OpenAI Chat Completions API. The
// SDK reads OPENAI_API_KEY, OPENAI_BASE_URL, OPENAI_ORG_ID and
// OPENAI_PROJECT_ID from the environment; a base URL points it at Azure
// OpenAI or another OpenAI-compatible server.
func NewOpenAILLM(opts ...option.RequestOption) LLM {
	return &openAILLM{client: openai.NewClient(opts...)}
}

type openAILLM struct{ client openai.Client }

func (l *openAILLM) New(ctx context.Context, params openai.ChatCompletionNewParams) (*openai.ChatCompletion, error) {
	return l.client.Chat.Completions.New(ctx, params)
}
