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
	"encoding/json"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/openai/openai-go"
	"github.com/pingcap/tidb/tests/llmtest/logger"
	"go.uber.org/zap"
)

const bugSeedSystemPrompt = `You are a helpful and honest Database Fuzzing Engineer expert specializing in Cross-Database Bug Migration.
Your goal is to leverage known bugs from MySQL to detect potential vulnerabilities in TiDB.

You do NOT just replay the bug. You MUST analyze the root cause and generate "Mutated Variants" to attack similar logic paths in TiDB.

Response Rules:
1. Analyze First: Always perform a deep root cause analysis before writing SQL.
2. Mutate Carefully: Generate 1-4 cases. Always include a direct reproduction and 3 optional light variants.
3. Format Strictly: Output valid JSON only. No markdown, no extra keys.
`

const bugSeedUserPromptTemplate = `You are a helpful and honest Database Fuzzing Engineer expert in TiDB. Given the following MySQL bug pattern, please generate 1-3 distinct SQL test cases for TiDB. Think step-by-step as below examples.
Example:
Input Bug Pattern:
{
  "id": "mysql_bug_12345",
  "source_db": "mysql",
  "title": "Wrong result with GROUP BY and NULL values",
  "symptom": "wrong_result",
  "feature": ["GROUP BY", "NULL", "Optimizer"],
  "setup_sql": ["CREATE TABLE t1 (id INT, val INT);", "INSERT INTO t1 VALUES (1, NULL), (1, NULL), (2, 10);"],
  "trigger_sql": ["SELECT id, COUNT(val) FROM t1 GROUP BY id;"],
  "expected": "Should return (1, 0), (2, 1). MySQL returns (1, 2) erroneously.",
  "notes": "MySQL incorrectly counts NULLs in COUNT aggregation when GROUP BY is used."
}
Model Output:
{
  "cases": [
    {
      "id": "repro_int_null",
      "title": "Direct Reproduction with INT",
      "setup_sql": [
	  		"CREATE TABLE t_repro (id INT, val INT);", 
			"INSERT INTO t_repro VALUES (1, NULL), (1, NULL);"
		],
      "trigger_sql": [
	  		"SELECT id, COUNT(val) FROM t_repro GROUP BY id ORDER BY id;"
		],
      "expected_behavior": "Count should be 0 for NULL values.",
      "mutation_reason": "Baseline check to see if TiDB shares the exact MySQL bug."
    },
    {
      "id": "mutate_varchar_type",
      "title": "Type Mutation: VARCHAR NULLs",
      "setup_sql": [
	  		"CREATE TABLE t_var (id INT, val VARCHAR(10));", 
			"INSERT INTO t_var VALUES (1, NULL), (1, '');"
		],
      "trigger_sql": [
	  		"SELECT id, COUNT(val) FROM t_var GROUP BY id ORDER BY id;"
		],
      "expected_behavior": "NULL count 0, Empty String count 1.",
      "mutation_reason": "Checking if string collation/encoding handling of NULLs triggers the same aggregation logic."
    }
  ]
}

Now, process the following input bug pattern and return only JSON output:
{{BUG_SEED_INPUT}}

Constraints & Requirements:
1. Determinism: Each setup/trigger SQL statement should be output in one line. Avoid RAND(), NOW(), SYSDATE(), or any non-deterministic functions.
2. Minimalism: For any table to be created in the setup_sql, use "DROP IF EXISTS" to drop the table if it exists. Use the smallest possible schema to reproduce the logic.
3. The generated SQL statements must satisfy the following requirements (CRITICAL):
	3.1 They must be syntactically and semantically correct.
	3.2 They must be executable in TiDB.
	3.3 Before output, correct any syntax or semantic errors in the generated SQL statements.
4. Mutation Strategy (CRITICAL): You must always inclde the direct reproduction case. 
	As for the one optional light variant case, you can apply strategies including but not limited to:Type Variation, Light Syntax Variation, Edge Case Variation. 
	You are also encouranged to create a case that differs greatly from the input in the syntax structures and used keywords, as long as you believe they are correct corresponding to the bug.
5. Refer to the "expected" field in the input for understanding the original bug behavior in MySQL and carefully infer the "expected_bahavior" for each case step by step.
`

type bugseedPromptGenerator struct {
	seedByID map[string][]BugSeed
}

type bugseedPromptResponse struct {
	Cases []bugseedCase `json:"cases"`
}

type bugseedCase struct {
	ID               string   `json:"id"`
	Title            string   `json:"title"`
	SetupSQL         []string `json:"setup_sql"`
	TriggerSQL       []string `json:"trigger_sql"`
	ExpectedBehavior string   `json:"expected_behavior"`
	MutationReason   string   `json:"mutation_reason"`
}

// Name implements PromptGenerator.Name.
func (g *bugseedPromptGenerator) Name() string {
	return "bugseed"
}

// Kind implements PromptGenerator.Kind.
func (g *bugseedPromptGenerator) Kind() GeneratorKind {
	return GeneratorKindTestcase
}

// Groups implements PromptGenerator.Groups.
func (g *bugseedPromptGenerator) Groups() []string {
	if len(g.seedByID) == 0 {
		return nil
	}

	return slices.Collect(maps.Keys(g.seedByID))
}

// GeneratePrompt implements PromptGenerator.GeneratePrompt.
func (g *bugseedPromptGenerator) GeneratePrompt(group string, _ int, _ []*BugSeed) []openai.ChatCompletionMessageParamUnion {
	seed, ok := g.seedByID[group]
	if !ok {
		logger.Global.Error("bugseed not found", zap.String("group", group))
		return nil
	}
	if len(seed) != 1 {
		logger.Global.Error("bugseed group must contain exactly one seed", zap.String("group", group), zap.Int("count", len(seed)))
		return nil
	}

	seedJSON, err := json.MarshalIndent(seed[0], "", "  ")
	if err != nil {
		logger.Global.Error("failed to marshal bug seed", zap.Error(err), zap.String("group", group))
		return nil
	}

	messages := make([]openai.ChatCompletionMessageParamUnion, 0, 2)
	messages = append(messages, openai.SystemMessage(bugSeedSystemPrompt))
	messages = append(messages, openai.UserMessage(
		strings.ReplaceAll(bugSeedUserPromptTemplate, "{{BUG_SEED_INPUT}}", string(seedJSON)),
	))

	return messages
}

// Unmarshal implements PromptGenerator.Unmarshal.
func (g *bugseedPromptGenerator) Unmarshal(response string) []*BugSeed {
	var resp bugseedPromptResponse
	if len(response) == 0 {
		logger.Global.Info("failed to unmarshal EMPTY bugseed prompt response")
		return nil
	}
	if err := json.Unmarshal([]byte(response), &resp); err != nil {
		logger.Global.Error("failed to unmarshal bugseed prompt response", zap.Error(err), zap.String("response", response))
		return nil
	}

	cases := make([]*BugSeed, 0, len(resp.Cases))
	for _, c := range resp.Cases {
		cases = append(cases, &BugSeed{
			ID:         c.ID,
			Title:      c.Title,
			SetupSQL:   c.SetupSQL,
			TriggerSQL: c.TriggerSQL,
			Expected:   c.ExpectedBehavior,
			Notes:      c.MutationReason,
		})
	}

	return cases
}

func newBugseedPromptGenerator() (PromptGenerator[*BugSeed], error) {
	issuepatternPath := "testdata/issuepattern.json"
	data, err := os.ReadFile(issuepatternPath)
	if err != nil || len(data) == 0 {
		logger.Global.Error("failed to load bug seeds", zap.Error(err), zap.String("path", issuepatternPath))
		return &bugseedPromptGenerator{}, nil
	}

	seedByID := make(map[string][]BugSeed)
	err = json.Unmarshal(data, &seedByID)
	if err != nil {
		logger.Global.Error("failed to unmarshal bug seeds", zap.Error(err), zap.String("path", issuepatternPath))
		return &bugseedPromptGenerator{}, nil
	}

	return &bugseedPromptGenerator{
		seedByID: seedByID,
	}, nil
}

func init() {
	registerGenerator("bugseed", GeneratorKindTestcase, newBugseedPromptGenerator, bugSeedStore)
}
