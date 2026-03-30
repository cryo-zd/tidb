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
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/openai/openai-go"
	"github.com/pingcap/tidb/tests/llmtest/logger"
	"go.uber.org/zap"
)

const issuePatternSystemPrompt = `You are a conservative database bug triage and normalization engine for TiDB compatibility testing.

Your task is to convert one upstream database bug report into exactly one normalized bug-pattern JSON object for downstream SQL testcase generation in a constrained harness.

Operating principles:
1. Reason privately. Do not reveal chain-of-thought, hidden analysis, or intermediate notes.
2. Output exactly one JSON object and nothing else.
3. Be evidence-driven and conservative. Do not invent missing SQL details unless they are strongly implied by the report.
4. Prefer false-negative over false-positive: if the report is not clearly suitable for TiDB SQL compatibility testing, mark it as irrelevant.
5. Preserve the required ID exactly as provided by the user prompt.
6. Treat this as an extraction-and-normalization task, not a creative writing task.
7. The downstream harness can only validate bugs that are reproducible in one fresh isolated database using one connection/session and sequential SQL statements whose correctness is judged from direct statement results.`

const issuePatternUserPromptTemplate = `Normalize the following upstream bug report into one bug-pattern JSON object for TiDB compatibility testing.

Input issue (JSON):
{{ISSUE_JSON}}

Required id: {{ID}}

Target execution boundary:
- The downstream harness creates one fresh isolated database.
- It runs setup_sql first and then trigger_sql sequentially in one connection/session.
- It can observe only direct statement results: returned rows, row_count, rows_affected, and direct statement errors.
- It cannot observe a second session, concurrent interleavings, background jobs, replication/CDC effects, server restarts, failover, logs, metrics, exact timing thresholds, or optimizer plan quality.
- If a prerequisite is expressible directly in one session, such as SET sql_mode or SET time_zone, it may still be relevant and should be included explicitly in setup_sql.

Output schema:
{
  "id": "must exactly equal the Required id",
  "source_db": "mysql | mariadb | postgres | cockroach | yugabytedb | ...",
  "title": "short factual summary including the primary bug mechanism",
  "relevant": true,
  "relevance_reason": "empty string if relevant=true; otherwise explain why the report is not suitable for TiDB SQL compatibility testing in the target execution boundary",
  "symptom": "wrong_result | crash | panic | error | timeout | perf | incompat",
  "feature": ["3 to 6 short normalized feature tags"],
  "setup_sql": ["precondition SQL only"],
  "trigger_sql": ["bug-triggering SQL only"],
  "expected": "one approved expectation template",
  "notes": "brief caveats such as inferred expectation, required mode/config/version, or missing evidence"
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

Statement array contract:
- Every setup_sql element must be exactly one executable SQL statement.
- Every trigger_sql element must be exactly one executable SQL statement.
- Do not emit comments, pseudo-steps, or multiple SQL statements in one array element.

Few-shot examples:
Example A: semantic SQL bug inside the boundary -> relevant=true
Input:
{"source":"mysql-bugs","title":"Wrong result for COUNT with NULL under GROUP BY","description":"COUNT on a nullable column under GROUP BY returns an incorrect count for NULL values.","steps":["CREATE TABLE t1 (id INT, v INT);","INSERT INTO t1 VALUES (1, NULL), (1, NULL), (2, 10);","SELECT id, COUNT(v) FROM t1 GROUP BY id ORDER BY id;"],"version":"8.0.0","links":["https://bugs.mysql.com/bug.php?id=10001"]}
Output:
{"id":"mysql-10001","source_db":"mysql","title":"Wrong COUNT NULL semantics under GROUP BY","relevant":true,"relevance_reason":"","symptom":"wrong_result","feature":["GROUP BY","COUNT","NULL"],"setup_sql":["DROP TABLE IF EXISTS t1;","CREATE TABLE t1 (id INT, v INT);","INSERT INTO t1 VALUES (1, NULL), (1, NULL), (2, 10);"],"trigger_sql":["SELECT id, COUNT(v) FROM t1 GROUP BY id ORDER BY id;"],"expected":"Exact rows: [[\"1\",\"0\"],[\"2\",\"1\"]]","notes":""}

Example B: session-variable-dependent semantic error inside the boundary -> relevant=true
Input:
{"source":"mysql-bugs","title":"ONLY_FULL_GROUP_BY is ignored after SET SESSION sql_mode","description":"After enabling ONLY_FULL_GROUP_BY in the session, a grouped query that selects a nonaggregated column still succeeds instead of failing.","steps":["SET SESSION sql_mode = 'ONLY_FULL_GROUP_BY';","CREATE TABLE t1 (a INT, b INT);","INSERT INTO t1 VALUES (1, 10), (1, 20);","SELECT a, b FROM t1 GROUP BY a;"],"version":"8.0.0","links":["https://bugs.mysql.com/bug.php?id=10004"]}
Output:
{"id":"mysql-10004","source_db":"mysql","title":"ONLY_FULL_GROUP_BY enforcement after session sql_mode change","relevant":true,"relevance_reason":"","symptom":"error","feature":["sql_mode","GROUP BY","error-class"],"setup_sql":["SET SESSION sql_mode = 'ONLY_FULL_GROUP_BY';","DROP TABLE IF EXISTS t1;","CREATE TABLE t1 (a INT, b INT);","INSERT INTO t1 VALUES (1, 10), (1, 20);"],"trigger_sql":["SELECT a, b FROM t1 GROUP BY a;"],"expected":"Expected error: ONLY_FULL_GROUP_BY","notes":"The session variable is part of the testcase contract and remains inside the single-session boundary."}

Example C: performance-only -> relevant=false
Input:
{"source":"mysql-bugs","title":"SELECT becomes slow with optimizer hint","description":"The query result is correct, but execution time increases dramatically after adding an optimizer hint.","steps":["SELECT /*+ SOME_HINT */ * FROM t WHERE a > 10;"],"version":"8.0.0","links":["https://bugs.mysql.com/bug.php?id=10002"]}
Output:
{"id":"mysql-10002","source_db":"mysql","title":"Performance regression with optimizer hint","relevant":false,"relevance_reason":"The report is primarily about performance rather than a clear SQL semantic compatibility bug.","symptom":"perf","feature":["optimizer hint","performance"],"setup_sql":[],"trigger_sql":[],"expected":"","notes":"No stable SQL-semantic oracle is available from the report."}

Example D: multi-session locking bug outside the boundary -> relevant=false
Input:
{"source":"mysql-bugs","title":"SELECT FOR UPDATE waits forever when another transaction holds the row lock","description":"Session A updates a row and does not commit. Session B then runs SELECT ... FOR UPDATE and hangs.","steps":["-- session A","BEGIN;","UPDATE t SET v = v + 1 WHERE id = 1;","-- session B","BEGIN;","SELECT * FROM t WHERE id = 1 FOR UPDATE;"],"version":"8.0.0","links":["https://bugs.mysql.com/bug.php?id=10003"]}
Output:
{"id":"mysql-10003","source_db":"mysql","title":"Concurrent row-lock wait with SELECT FOR UPDATE","relevant":false,"relevance_reason":"The report fundamentally depends on a second concurrent session and lock timing, which the target single-session harness cannot reproduce or judge directly.","symptom":"timeout","feature":["transaction","locking","SELECT FOR UPDATE"],"setup_sql":[],"trigger_sql":[],"expected":"","notes":"Do not convert this into a single-session proxy case because that would no longer test the same mechanism."}

Inference pattern:
- If the report implies a semantic rule such as PRIMARY KEY uniqueness but does not explicitly state the expected error, infer the observable behavior conservatively and mention that inference in notes.
- If the report depends on session state that can be expressed by explicit SQL such as SET statements, keep it relevant only when that state is essential and directly controllable in one session.

Decision policy:
1. Set relevant=false if the report is primarily about documentation, tooling, deployment, monitoring, backup/restore tooling, non-SQL admin behavior, runtime warnings, performance only, or an upstream-only feature that TiDB does not aim to support.
2. Set relevant=false if the report fundamentally depends on multi-session concurrency, lock interleavings, background workers, replication/CDC lag, failover/restart behavior, external logs/metrics, or any oracle that is not directly observable from one-session statement results.
3. Set relevant=true only if the report is plausibly testable through one-session SQL behavior on TiDB within the target execution boundary.
4. If the report can only be approximated by removing concurrency or other missing execution context, prefer relevant=false rather than fabricating a proxy testcase.
5. If the report lacks enough evidence to reconstruct a meaningful SQL test, prefer relevant=false rather than fabricating details.
6. Use the Required id exactly as the output "id".
7. "title" should carry the primary mechanism, not just a generic symptom.
8. "setup_sql" must contain only prerequisite statements such as DROP, CREATE, INSERT, or SET needed before the trigger, with exactly one executable SQL statement per array element.
9. "trigger_sql" must contain only the statements that expose the bug, with exactly one executable SQL statement per array element.
10. "expected" must use exactly one approved expectation template selected by the template selection rules above. Do not write vague text like "should behave correctly".
11. If expected behavior is inferred rather than explicitly stated, keep it precise and mention the inference in "notes".
12. Keep feature tags concrete, short, and normalized. Avoid duplicates and avoid generic tags like "SQL", "database", or "query".
13. Output strict JSON only. No markdown. No comments. No extra keys.

Private reasoning checklist:
- Is this actually a SQL-compatibility bug candidate for TiDB inside the target boundary?
- What is the minimal bug mechanism?
- Which statements are setup versus trigger?
- What exact observable should hold if behavior is correct?
- Does the case secretly require another session, background state, or timing oracle?
- Am I inferring too much beyond the report?`

type issueRecord struct {
	Source      string   `json:"source"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Steps       []string `json:"steps"`
	Version     string   `json:"version"`
	Links       []string `json:"links"`
}

type issuePatternCase struct {
	ID              string   `json:"id"`
	SourceDB        string   `json:"source_db"`
	Title           string   `json:"title"`
	Relevant        bool     `json:"relevant"`
	RelevanceReason string   `json:"relevance_reason"`
	Symptom         string   `json:"symptom"`
	Feature         []string `json:"feature"`
	SetupSQL        []string `json:"setup_sql"`
	TriggerSQL      []string `json:"trigger_sql"`
	Expected        string   `json:"expected"`
	Notes           string   `json:"notes"`
}

type issuepatternPromptGenerator struct {
	issueByID map[string]issueRecord
}

// Name implements PromptGenerator.Name.
func (g *issuepatternPromptGenerator) Name() string {
	return "issuepattern"
}

// Kind implements PromptGenerator.Kind.
func (g *issuepatternPromptGenerator) Kind() GeneratorKind {
	return GeneratorKindBugSeed
}

// OneShotPerGroup implements PromptGenerator.OneShotPerGroup.
func (g *issuepatternPromptGenerator) OneShotPerGroup() bool {
	return true
}

// Validate ensures the issuepattern generator has an explicit input file.
func (g *issuepatternPromptGenerator) Validate() error {
	if getIssueSeedFile() == "" {
		return fmt.Errorf("issuepattern generator requires --issue_seed_file")
	}
	return nil
}

// Groups implements PromptGenerator.Groups.
func (g *issuepatternPromptGenerator) Groups() []string {
	if len(g.issueByID) == 0 {
		return nil
	}

	return slices.Collect(maps.Keys(g.issueByID))
}

// GeneratePrompt implements PromptGenerator.GeneratePrompt.
func (g *issuepatternPromptGenerator) GeneratePrompt(group string, _ int, _ []*BugSeed) []openai.ChatCompletionMessageParamUnion {
	issue, ok := g.issueByID[group]
	if !ok {
		logger.Global.Error("issue not found", zap.String("group", group))
		return nil
	}

	issueJSON, err := json.MarshalIndent(issue, "", "  ")
	if err != nil {
		logger.Global.Error("failed to marshal issue", zap.Error(err), zap.String("group", group))
		return nil
	}

	content := strings.ReplaceAll(issuePatternUserPromptTemplate, "{{ISSUE_JSON}}", string(issueJSON))
	content = strings.ReplaceAll(content, "{{ID}}", group)

	messages := make([]openai.ChatCompletionMessageParamUnion, 0, 2)
	messages = append(messages, openai.SystemMessage(issuePatternSystemPrompt))
	messages = append(messages, openai.UserMessage(content))
	return messages
}

// Unmarshal implements PromptGenerator.Unmarshal.
func (g *issuepatternPromptGenerator) Unmarshal(response string) []*BugSeed {
	var resp issuePatternCase
	if len(response) == 0 {
		logger.Global.Info("failed to unmarshal EMPTY issuepattern response")
		return nil
	}
	if err := json.Unmarshal([]byte(response), &resp); err != nil {
		logger.Global.Error("failed to unmarshal issuepattern response", zap.Error(err), zap.String("response", response))
		return nil
	}

	if !resp.Relevant {
		logger.Global.Info("Skip one irrelevant bug issue pattern: " + resp.RelevanceReason)
		return nil
	}

	if strings.TrimSpace(resp.ID) == "" {
		logger.Global.Error("issuepattern response missing id", zap.String("response", response))
		return nil
	}

	seedCase := &BugSeed{
		ID:         resp.ID,
		SourceDB:   resp.SourceDB,
		Title:      resp.Title,
		Symptom:    resp.Symptom,
		Feature:    resp.Feature,
		SetupSQL:   resp.SetupSQL,
		TriggerSQL: resp.TriggerSQL,
		Expected:   resp.Expected,
		Notes:      resp.Notes,
	}

	return []*BugSeed{seedCase}
}

func newIssuepatternPromptGenerator() (PromptGenerator[*BugSeed], error) {
	issueSeedFile := getIssueSeedFile()
	data, err := os.ReadFile(issueSeedFile)
	if err != nil || len(data) == 0 {
		logger.Global.Error("failed to load issues", zap.Error(err), zap.String("path", issueSeedFile))
		return &issuepatternPromptGenerator{}, nil
	}

	var issues []issueRecord
	if err := json.Unmarshal(data, &issues); err != nil {
		logger.Global.Error("failed to unmarshall issues", zap.Error(err), zap.String("path", issueSeedFile))
		return &issuepatternPromptGenerator{}, nil
	}
	issueByID := make(map[string]issueRecord, len(issues))
	for idx, issue := range issues {
		id := deriveIssueID(issue, idx)
		if id == "" {
			continue
		}
		issueByID[id] = issue
	}

	return &issuepatternPromptGenerator{
		issueByID: issueByID,
	}, nil
}

func init() {
	registerGenerator("issuepattern", GeneratorKindBugSeed, newIssuepatternPromptGenerator, bugSeedStore)
}

func deriveIssueID(issue issueRecord, idx int) string {
	for _, link := range issue.Links {
		if id := extractMySQLBugID(link); id != "" {
			return "mysql-" + id
		}
	}
	return "issue-" + strconv.Itoa(idx)
}

func extractMySQLBugID(link string) string {
	re := regexp.MustCompile(`id=([0-9]+)`)
	matches := re.FindStringSubmatch(link)
	if len(matches) == 2 {
		return matches[1]
	}
	return ""
}
