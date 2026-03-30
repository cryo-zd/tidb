// Copyright 2025 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package generator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/pingcap/tidb/tests/llmtest/logger"
	"go.uber.org/zap"
)

// Generator generates items and writes them to the store to reach a specific count.
type Generator[T any] struct {
	inputCh     chan string
	store       Store[T]
	parallelism int

	wg *sync.WaitGroup

	openAIToken   string
	openAIBaseURL string
	modelName     string

	promptGenerator PromptGenerator[T]
	testCaseCount   int
}

// New creates a new Generator.
func New[T any](
	store Store[T],
	parallelism int,
	openAIToken string,
	openAIBaseURL string,
	modelName string,
	promptGenerator PromptGenerator[T],
	testCaseCount int) *Generator[T] {
	return &Generator[T]{
		inputCh:     make(chan string, len(promptGenerator.Groups())),
		store:       store,
		parallelism: parallelism,

		wg: new(sync.WaitGroup),

		openAIToken:   openAIToken,
		openAIBaseURL: openAIBaseURL,
		modelName:     modelName,

		promptGenerator: promptGenerator,
		testCaseCount:   testCaseCount,
	}
}

// Run starts the generator.
func (g *Generator[T]) Run() {
	for range g.parallelism {
		g.wg.Add(1)
		go g.runWorker()
	}

	for _, group := range g.promptGenerator.Groups() {
		g.inputCh <- group
	}
	close(g.inputCh)
}

// generateItemsForGroup executes one prompt attempt for a group. For one-shot generators, the prompt runs at most once per group
// and `testCaseCount` only controls whether the group has already been generated.
func (g *Generator[T]) generateItemsForGroup(client *openai.Client, group string) ([]T, error) {
	existCases := g.store.Exist(group)
	generateCount := g.testCaseCount
	if g.promptGenerator.OneShotPerGroup() {
		if len(existCases) > 0 {
			return nil, nil
		}
		generateCount = 1
	} else if len(existCases) >= g.testCaseCount {
		return nil, nil
	}

	logger.Global.Info("generating test SQLs for function",
		zap.String("group", group),
		zap.Int("existCases", len(existCases)), zap.Int("generateCount", generateCount))

	prompt := g.promptGenerator.GeneratePrompt(group, generateCount, existCases)
	if prompt == nil {
		return nil, errors.New("failed to generate prompt")
	}

	options := make([]option.RequestOption, 0)
	if strings.Contains(g.modelName, "deepseek") {
		options = append(options, option.WithJSONSet("provider", map[string]any{
			// Together always returns the reasoning part int the response.
			"ignore": []string{"Together"},
		}))
	}
	completion, err := client.Chat.Completions.New(context.Background(), openai.ChatCompletionNewParams{
		Model:    openai.F(g.modelName),
		Messages: openai.F(prompt),
		ResponseFormat: openai.F[openai.ChatCompletionNewParamsResponseFormatUnion](
			openai.ChatCompletionNewParamsResponseFormat{
				// `JSON_SCHEMA` is not implemented by many models, so use `JSON_OBJECT` instead.
				// (Though `JSON_OBJECT` is also not supported by some models.)
				Type: openai.F(openai.ChatCompletionNewParamsResponseFormatTypeJSONObject),
			},
		),
		// Usually the input uses less than 250 tokens, and the output uses less than 5000 tokens.
		MaxTokens: openai.F[int64](6000),
	}, options...)
	if err != nil {
		return nil, err
	}

	if len(completion.Choices) == 0 {
		return nil, fmt.Errorf("no completion choices")
	}

	cases := g.promptGenerator.Unmarshal(completion.Choices[0].Message.Content)
	return cases, nil
}

func (g *Generator[T]) runWorker() {
	defer g.wg.Done()

	client := openai.NewClient(
		option.WithAPIKey(g.openAIToken),
		option.WithBaseURL(g.openAIBaseURL),
		// For deepseek series model, enable reasoning will remove the reasoning part from
		// the content. Ref https://openrouter.ai/docs/use-cases/reasoning-tokens.
		option.WithJSONSet("include_reasoning", true),
	)

	for input := range g.inputCh {
		cases, err := g.generateItemsForGroup(client, input)
		if err != nil {
			logger.Global.Error("failed to generate test SQLs", zap.Error(err))
			continue
		}

		for _, c := range cases {
			g.store.Append(input, c)
		}
	}
}

// Wait waits for all workers to finish.
func (g *Generator[T]) Wait() {
	g.wg.Wait()
}
