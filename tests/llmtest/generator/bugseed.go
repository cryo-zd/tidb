// Copyright 2026 PingCAP, Inc.
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

const bugSeedSystemPrompt = `You are a senior database fuzzing engineer specializing in cross-database bug migration from MySQL-family systems to TiDB.

Your task is to generate high-value TiDB SQL testcases from one normalized bug pattern for a constrained single-session harness.

Operating principles:
1. Reason privately. Do not reveal chain-of-thought, hidden analysis, or intermediate notes.
2. Output exactly one JSON object and nothing else.
3. First infer the likely fault model of the source bug.
4. Then generate controlled testcases that preserve that fault model.
5. Do not generate diversity for its own sake. Every variant must still target the same hypothesized bug mechanism.
6. Prefer fewer, cleaner, more executable cases over many flashy ones.
7. Every testcase must be deterministic, self-contained, executable on TiDB, and easy for a downstream oracle to judge.
8. The harness executes setup_sql and then trigger_sql in one fresh isolated database using one connection/session only and observes only direct statement results.
9. Do not smuggle in requirements that need another session, background work, timing, process observation, or external components.`

const bugSeedUserPromptTemplate = `Generate TiDB SQL testcases from the following normalized bug pattern.

Input bug pattern (JSON):
{{BUG_SEED_INPUT}}

Target harness boundary:
- One fresh isolated database per bug id.
- One connection/session only; setup_sql runs first and trigger_sql runs after it, sequentially.
- The downstream oracle can observe only direct statement results: returned rows, row_count, rows_affected, and direct statement errors.
- The harness cannot provide a second client, manual concurrency, sleep-based orchestration, lock-holding side sessions, background jobs, replication lag, restart/failover events, logs, metrics, or a plan-quality oracle.
- If a required prerequisite is expressible directly in one session, such as SET sql_mode or SET time_zone, include it explicitly in setup_sql.

Output schema:
{
  "cases": [
    {
      "id": "short_case_id",
      "title": "short factual title including case role and preserved mechanism",
      "setup_sql": ["setup statement 1", "setup statement 2"],
      "trigger_sql": ["trigger statement 1", "trigger statement 2"],
      "expected_behavior": "one approved expectation template",
      "mutation_reason": "what changed relative to the direct reproduction, and why this still targets the same bug mechanism"
    }
  ]
}

Approved expectation templates:
- Exact rows: [[...], [...]]
- Exact row count: N
- Expected error: <stable error class or stable substring>
- Expected NULL semantics: <precise rule>
- Expected ordering: <precise rule>

Template selection rules:
- Use Exact rows only when every result value and its textual representation are stable and essential to the bug oracle.
- Use Exact row count when multiplicity matters but exact row values or formatting are incidental or unstable.
- Use Expected NULL semantics when the core property is NULL propagation, NULL counting, or NULL comparison behavior rather than the full textual result set.
- Use Expected error when correctness is whether an error occurs or which stable error class/substr appears.
- Use Expected ordering only when ordering itself is the property under test.
- Do not use Exact rows for floating-point approximation, time or time-zone formatting, charset/collation presentation differences, unstable row order, or any case where textual output may vary while semantics stay the same.

Few-shot examples:
Input:
{"id":"mysql-20001","source_db":"mysql","title":"Wrong result for COUNT with NULL under GROUP BY","symptom":"wrong_result","feature":["GROUP BY","COUNT","NULL"],"setup_sql":["DROP TABLE IF EXISTS t1;","CREATE TABLE t1 (id INT, v INT);","INSERT INTO t1 VALUES (1, NULL), (1, NULL), (2, 10);"],"trigger_sql":["SELECT id, COUNT(v) FROM t1 GROUP BY id ORDER BY id;"],"expected":"The query should return two rows: (1, 0) and (2, 1).","notes":""}
Output:
{"cases":[{"id":"repro_count_null_group_by","title":"Direct reproduction preserving COUNT NULL semantics under GROUP BY","setup_sql":["DROP TABLE IF EXISTS t1;","CREATE TABLE t1 (id INT, v INT);","INSERT INTO t1 VALUES (1, NULL), (1, NULL), (2, 10);"],"trigger_sql":["SELECT id, COUNT(v) FROM t1 GROUP BY id ORDER BY id;"],"expected_behavior":"Exact rows: [[\"1\",\"0\"],[\"2\",\"1\"]]","mutation_reason":"Direct reproduction of the same NULL-counting aggregate bug mechanism."},{"id":"mutate_count_null_group_by_type","title":"Type mutation preserving COUNT NULL semantics under GROUP BY","setup_sql":["DROP TABLE IF EXISTS t1;","CREATE TABLE t1 (id INT, v VARCHAR(10));","INSERT INTO t1 VALUES (1, NULL), (1, NULL), (2, 'x');"],"trigger_sql":["SELECT id, COUNT(v) FROM t1 GROUP BY id ORDER BY id;"],"expected_behavior":"Exact rows: [[\"1\",\"0\"],[\"2\",\"1\"]]","mutation_reason":"Changes the value type while preserving the same COUNT/NULL/GROUP BY mechanism inside the same single-session harness."},{"id":"mutate_count_null_group_by_expr","title":"Expression-form mutation preserving COUNT NULL semantics under GROUP BY","setup_sql":["DROP TABLE IF EXISTS t1;","CREATE TABLE t1 (id INT, a INT, b INT);","INSERT INTO t1 VALUES (1, NULL, 5), (1, NULL, 6), (2, 10, 7);"],"trigger_sql":["SELECT id, COUNT(COALESCE(a, NULL)) FROM t1 GROUP BY id ORDER BY id;"],"expected_behavior":"Exact rows: [[\"1\",\"0\"],[\"2\",\"1\"]]","mutation_reason":"Changes the aggregate argument form while preserving the same NULL-counting aggregate mechanism inside the same single-session harness."}]}

Input:
{"id":"mysql-20002","source_db":"mysql","title":"ONLY_FULL_GROUP_BY enforcement after session sql_mode change","symptom":"error","feature":["sql_mode","GROUP BY","error-class"],"setup_sql":["SET SESSION sql_mode = 'ONLY_FULL_GROUP_BY';","DROP TABLE IF EXISTS t1;","CREATE TABLE t1 (a INT, b INT);","INSERT INTO t1 VALUES (1, 10), (1, 20);"],"trigger_sql":["SELECT a, b FROM t1 GROUP BY a;"],"expected":"Expected error: ONLY_FULL_GROUP_BY","notes":"The session variable is part of the testcase contract and remains inside the single-session boundary."}
Output:
{"cases":[{"id":"repro_only_full_group_by_error","title":"Direct reproduction preserving ONLY_FULL_GROUP_BY error enforcement","setup_sql":["SET SESSION sql_mode = 'ONLY_FULL_GROUP_BY';","DROP TABLE IF EXISTS t1;","CREATE TABLE t1 (a INT, b INT);","INSERT INTO t1 VALUES (1, 10), (1, 20);"],"trigger_sql":["SELECT a, b FROM t1 GROUP BY a;"],"expected_behavior":"Expected error: ONLY_FULL_GROUP_BY","mutation_reason":"Direct reproduction of the same session-scoped sql_mode enforcement mechanism."},{"id":"mutate_only_full_group_by_expr_error","title":"Expression-form mutation preserving ONLY_FULL_GROUP_BY error enforcement","setup_sql":["SET SESSION sql_mode = 'ONLY_FULL_GROUP_BY';","DROP TABLE IF EXISTS t1;","CREATE TABLE t1 (a INT, b INT, c INT);","INSERT INTO t1 VALUES (1, 10, 100), (1, 20, 200);"],"trigger_sql":["SELECT a, b + c FROM t1 GROUP BY a;"],"expected_behavior":"Expected error: ONLY_FULL_GROUP_BY","mutation_reason":"Changes the selected nonaggregated expression while preserving the same session-scoped GROUP BY error mechanism."}]}

Anti-examples:
- Bad mutation: switch to an unrelated JOIN, subquery, or locking bug. That increases diversity but no longer tests the same root mechanism.
- Bad mutation: try to mimic a two-session lock, replication, or timing bug with SLEEP(), comments about another client, or speculative sequencing in one session. That leaves the harness boundary and is not judgeable.

Hard requirements:
1. Generate 1 to 3 cases.
   - Always include one direct reproduction.
   - Add up to two controlled mutations only when they stay inside the target harness and preserve the same fault model.
2. If faithful mutation would leave the harness boundary or become speculative, return fewer cases rather than inventing a proxy testcase.
3. The direct reproduction must stay as close as possible to the original bug mechanism.
4. Each mutation, if present, should preserve the same hypothesized fault model and is encouraged to maximize the diversity across the returned cases.
5. Prefer mutations that explore different SQL formulations or semantic surfaces without drifting into a different root cause.
6. "title" must carry both the case role and the preserved mechanism so the downstream oracle can still understand what this testcase is targeting.
7. Every setup_sql element and every trigger_sql element must be exactly one executable SQL statement. Do not emit comments, pseudo-steps, or multiple SQL statements in one array element.
8. Each SQL statement must be one line.
9. Avoid nondeterministic behavior: do not use RAND(), NOW(), CURRENT_TIMESTAMP(), UUID(), SYSDATE(), sleep-based timing, or unstable ordering.
10. If a table is created, include an appropriate DROP TABLE IF EXISTS in setup_sql before CREATE TABLE.
11. Keep setup_sql minimal. Put only prerequisites there, including required SET statements when they are part of the testcase contract.
12. Put only bug-triggering or verification statements in trigger_sql.
13. All SQL must be syntactically correct, semantically plausible, and executable on TiDB.
14. Avoid upstream-only syntax or features unless the input bug pattern clearly depends on them and they are still relevant to TiDB compatibility.
15. expected_behavior must be directly observable from one-session execution results and must use exactly one approved expectation template selected by the template selection rules above:
    - exact row values
    - exact row count
    - exact NULL semantics
    - exact ordering requirement
    - exact error category if the bug is about an error
    Do not use vague phrases like "should behave correctly".
16. For multi-row query outputs, include stable ORDER BY unless result ordering itself is part of the bug.
17. mutation_reason must explicitly say:
    - what axis changed
    - why the same root mechanism may still be exercised
18. Keep the cases minimal and oracle-friendly. Avoid unnecessary tables, columns, joins, or extra statements.
19. Output strict JSON only. No markdown. No comments. No extra keys.

Private reasoning checklist:
- What is the most likely fault model?
- Does the repro preserve it directly?
- Does each mutation stay inside the target harness?
- Does each mutation change only one major axis?
- Is expected_behavior observable and specific?
- Will a downstream oracle be able to judge this case from actual execution results without hidden sessions or background context?`

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
	return GeneratorKindBugSeed
}

// OneShotPerGroup implements PromptGenerator.OneShotPerGroup.
func (g *bugseedPromptGenerator) OneShotPerGroup() bool {
	return true
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
	registerGenerator("bugseed", GeneratorKindBugSeed, newBugseedPromptGenerator, bugSeedStore)
}
