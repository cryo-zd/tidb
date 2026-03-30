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
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/go-sql-driver/mysql"
	"github.com/openai/openai-go"
	"github.com/pingcap/tidb/tests/llmtest/logger"
	"github.com/pingcap/tidb/tests/llmtest/testcase"
	"go.uber.org/zap"
)

const oracleSystemPrompt = `You are a conservative TiDB bug judge.

You receive:
- a bug-derived testcase
- TiDB execution results with statement-level metadata
- a suggested expected behavior

Your task is to decide whether the evidence supports verdict = "bug", "ok", or "uncertain".

Operating principles:
1. Reason privately. Do not reveal chain-of-thought, hidden analysis, or intermediate notes.
2. Output exactly one JSON object and nothing else.
3. Be conservative:
   - choose "bug" only when the observed TiDB behavior clearly contradicts SQL semantics or the testcase's observable expectation
   - choose "ok" only when the observed behavior clearly matches
   - otherwise choose "uncertain"
4. Do not treat expected_behavior as ground truth. Treat it as a hypothesis to be checked against SQL semantics and actual results.
5. Use the execution metadata literally:
   - statement_type = "query" means a result set statement
   - statement_type = "exec" means a non-query statement that executed successfully unless an error is present
   - row_count distinguishes zero-row query results from missing data
   - rows_affected applies only to exec statements
6. Do not invent hidden rows, hidden outputs, or hidden execution states.
7. If evidence is incomplete or ambiguous, prefer "uncertain".
8. Target observability is limited to one-session direct execution results. Never assume a missing second session, hidden lock holder, background task, timing threshold, process crash log, or cluster-side event.`

const oracleUserPromptTemplate = `Evaluate each case below and judge TiDB behavior.

Input JSON:
{{ORACLE_INPUT}}

Judging boundary:
- All cases in the same bug group run in one shared isolated database dedicated to that group.
- Within each case, setup_sql and trigger_sql were executed sequentially in one pinned connection/session.
- You may use only the provided SQL text, expected_behavior hypothesis, statement metadata, direct errors, returned rows, row_count, and rows_affected.
- You may not assume hidden sessions, hidden retries, logs, metrics, optimizer plan baselines, replication state, server restarts, or any external system state not present in the input.
- If the intended bug mechanism depends on those missing observables, default toward verdict="uncertain".

Expected_behavior templates you should recognize:
- Exact rows: [[...], [...]]
- Exact row count: N
- Expected error: <stable error class or stable substring>
- Expected NULL semantics: <precise rule>
- Expected ordering: <precise rule>

Output schema:
{
  "cases": [
    {
      "case_id": "exactly the same as input",
      "reason": "2-4 sentences: expected, observed, comparison, and why the verdict follows",
      "verdict": "bug | ok | uncertain",
      "report_draft": "short bug report draft only when verdict=bug and evidence is strong; otherwise empty string"
    }
  ],
  "summary": {
    "root_cause_groups": [
      {
        "group_id": "g1",
        "case_ids": ["caseA", "caseB"],
        "relationship": "same_root_cause | variant_failed | different"
      }
    ],
    "canonical_case_id": "the clearest minimal bug case, or empty string if none",
    "minimization_hint": "brief concrete advice for shrinking the best failing case, or empty string if none",
    "notes": "brief cross-case synthesis"
  }
}

Few-shot examples:
Example A: clear ok
Input:
{"bug_id":"mysql-30001","cases":[{"case_id":"mysql-30001:repro","title":"Direct reproduction preserving GREATEST scalar semantics","setup_sql":[],"trigger_sql":["SELECT GREATEST(3, 5);"],"expected_behavior":"Exact rows: [[\"5\"]]","tidb_result":{"trigger":[{"sql":"SELECT GREATEST(3, 5);","statement_type":"query","rows":[["5"]],"row_count":1}]}}]}
Output:
{"cases":[{"case_id":"mysql-30001:repro","reason":"Expected exact rows [[\"5\"]]. TiDB returned one query row containing 5. The observed result matches the expected scalar semantics, so the verdict is ok.","verdict":"ok","report_draft":""}],"summary":{"root_cause_groups":[],"canonical_case_id":"","minimization_hint":"","notes":"No bug evidence."}}

Example B: clear bug
Input:
{"bug_id":"mysql-30002","cases":[{"case_id":"mysql-30002:repro","title":"Direct reproduction preserving COUNT NULL semantics","setup_sql":[],"trigger_sql":["SELECT COUNT(NULL);"],"expected_behavior":"Exact rows: [[\"0\"]]","tidb_result":{"trigger":[{"sql":"SELECT COUNT(NULL);","statement_type":"query","rows":[["1"]],"row_count":1}]}}]}
Output:
{"cases":[{"case_id":"mysql-30002:repro","reason":"Expected exact rows [[\"0\"]] because COUNT over NULL should count no non-NULL values. TiDB returned one row containing 1 instead. This is a direct semantic mismatch, so the verdict is bug.","verdict":"bug","report_draft":"TiDB returns 1 for SELECT COUNT(NULL), but the correct result should be 0."}],"summary":{"root_cause_groups":[{"group_id":"g1","case_ids":["mysql-30002:repro"],"relationship":"same_root_cause"}],"canonical_case_id":"mysql-30002:repro","minimization_hint":"Reduce the case to SELECT COUNT(NULL).","notes":"Single clear semantic failure."}}

Example C: clear bug for expected error oracle
Input:
{"bug_id":"mysql-30005","cases":[{"case_id":"mysql-30005:repro","title":"Direct reproduction preserving ONLY_FULL_GROUP_BY error enforcement","setup_sql":["SET SESSION sql_mode = 'ONLY_FULL_GROUP_BY';","DROP TABLE IF EXISTS t1;","CREATE TABLE t1 (a INT, b INT);","INSERT INTO t1 VALUES (1, 10), (1, 20);"],"trigger_sql":["SELECT a, b FROM t1 GROUP BY a;"],"expected_behavior":"Expected error: ONLY_FULL_GROUP_BY","tidb_result":{"trigger":[{"sql":"SELECT a, b FROM t1 GROUP BY a;","statement_type":"query","rows":[["1","10"]],"row_count":1}]}}]}
Output:
{"cases":[{"case_id":"mysql-30005:repro","reason":"Expected an error containing ONLY_FULL_GROUP_BY after the session sql_mode enabled strict GROUP BY enforcement. TiDB returned query rows instead of raising an error. That violates the expected error oracle, so the verdict is bug.","verdict":"bug","report_draft":"With SET SESSION sql_mode = 'ONLY_FULL_GROUP_BY', TiDB still returns rows for SELECT a, b FROM t1 GROUP BY a instead of raising the expected ONLY_FULL_GROUP_BY error."}],"summary":{"root_cause_groups":[{"group_id":"g1","case_ids":["mysql-30005:repro"],"relationship":"same_root_cause"}],"canonical_case_id":"mysql-30005:repro","minimization_hint":"Keep only the session sql_mode change, a two-row table, and the noncompliant GROUP BY query.","notes":"Single clear expected-error failure under a session-scoped prerequisite."}}

Example D: clear uncertain because a required second session is missing
Input:
{"bug_id":"mysql-30004","cases":[{"case_id":"mysql-30004:repro","title":"Concurrent row-lock wait requiring another session","setup_sql":["DROP TABLE IF EXISTS t1;","CREATE TABLE t1 (id INT PRIMARY KEY, v INT);","INSERT INTO t1 VALUES (1, 10);"],"trigger_sql":["BEGIN;","SELECT * FROM t1 WHERE id = 1 FOR UPDATE;"],"expected_behavior":"Expected error: lock wait or blocking behavior caused by another concurrent session.","tidb_result":{"trigger":[{"sql":"BEGIN;","statement_type":"exec","rows_affected":0},{"sql":"SELECT * FROM t1 WHERE id = 1 FOR UPDATE;","statement_type":"query","rows":[["1","10"]],"row_count":1}]}}]}
Output:
{"cases":[{"case_id":"mysql-30004:repro","reason":"The claimed blocking behavior depends on another concurrent session holding the row lock, but no such session is represented in the input. The observed single-session success therefore does not prove either correctness or a bug. Because the testcase requires missing execution context outside the judging boundary, the verdict is uncertain.","verdict":"uncertain","report_draft":""}],"summary":{"root_cause_groups":[],"canonical_case_id":"","minimization_hint":"","notes":"The testcase depends on missing multi-session context."}}

Verdict policy:
1. Return "bug" only if the observed TiDB behavior clearly violates the expected semantic outcome.
2. Return "ok" only if the observed behavior clearly matches the expected semantic outcome.
3. Return "uncertain" if:
   - the setup failed
   - the trigger failed but the failure is not clearly itself the bug
   - the expectation is ambiguous
   - the SQL semantics are unclear from the provided information
   - the case fundamentally depends on a second session, concurrency timing, background work, replication state, restart/failover, logs, metrics, or optimizer plan shape
   - the evidence is insufficient to distinguish bug from unsupported or unclear behavior
4. Do not mark a case as "bug" merely because TiDB returned an error. Decide whether that error itself is clearly incorrect for the testcase.
5. For query statements:
   - use row_count first
   - use rows if present
   - row_count=0 with missing rows means an empty result set, not missing evidence
6. For exec statements:
   - use statement_type="exec"
   - use rows_affected if present
   - do not expect query rows from exec statements
7. The reason field must explicitly cover:
   - what should happen
   - what TiDB actually did
   - why that supports the verdict
   - whether any missing execution context limits confidence
8. When expected_behavior uses an approved template, interpret it literally rather than paraphrasing it loosely.
9. report_draft should be non-empty only for strong "bug" cases.
10. In summary:
   - use same_root_cause only when multiple bug cases clearly indicate the same failure mechanism
   - use variant_failed when only a subset of variants fail while others do not
   - use different when failures do not clearly share one mechanism
11. Choose canonical_case_id only if there is at least one clear bug case.
12. Output strict JSON only. No markdown. No comments. No extra keys.

Private reasoning checklist:
- What is the correct semantic expectation?
- What exactly was observed in TiDB?
- Does the case depend on hidden sessions, timing, or background context that I cannot observe here?
- Is the mismatch explicit, or am I guessing?
- If I am guessing, downgrade to uncertain.
- Which failing case is the clearest minimal representative?`

type oraclePromptCaseInput struct {
	CaseID         string       `json:"case_id"`
	Title          string       `json:"title"`
	SetupSQL       []string     `json:"setup_sql"`
	TriggerSQL     []string     `json:"trigger_sql"`
	Expected       string       `json:"expected_behavior"`
	TiDBResult     OracleResult `json:"tidb_result"`
	ExecutionError string       `json:"execution_error,omitempty"`
}

type oraclePromptInput struct {
	BugID string                  `json:"bug_id"`
	Cases []oraclePromptCaseInput `json:"cases"`
}

type oraclePromptResponse struct {
	Cases   []oraclePromptCaseVerdict `json:"cases"`
	Summary *OracleSummary            `json:"summary"`
}

type oraclePromptCaseVerdict struct {
	CaseID      string `json:"case_id"`
	Verdict     string `json:"verdict"`
	Reason      string `json:"reason"`
	ReportDraft string `json:"report_draft"`
}

type oraclePromptGenerator struct {
	seedByID map[string][]BugSeed
}

// Name implements PromptGenerator.Name.
func (g *oraclePromptGenerator) Name() string {
	return "oracle"
}

// Kind implements PromptGenerator.Kind.
func (g *oraclePromptGenerator) Kind() GeneratorKind {
	return GeneratorKindBugSeed
}

// OneShotPerGroup implements PromptGenerator.OneShotPerGroup.
func (g *oraclePromptGenerator) OneShotPerGroup() bool {
	return true
}

// Validate ensures the external dependency needed by the oracle generator is
// available before worker goroutines start.
func (g *oraclePromptGenerator) Validate() error {
	if getTiDBDSN() == "" {
		return fmt.Errorf("oracle generator requires TiDB DSN; set --tidb_dsn or TIDB_DSN")
	}
	return nil
}

// Groups implements PromptGenerator.Groups.
func (g *oraclePromptGenerator) Groups() []string {
	if len(g.seedByID) == 0 {
		return nil
	}

	return slices.Collect(maps.Keys(g.seedByID))
}

// GeneratePrompt implements PromptGenerator.GeneratePrompt.
func (g *oraclePromptGenerator) GeneratePrompt(group string, _ int, _ []*OracleCase) []openai.ChatCompletionMessageParamUnion {
	tidbDSN := getTiDBDSN()

	seeds, ok := g.seedByID[group]
	if !ok || len(seeds) == 0 {
		logger.Global.Error("oracle bugseed group not found", zap.String("group", group))
		return nil
	}

	dbName := buildIsolatedDBName(group)
	adminDB, err := openDBWithName(tidbDSN, "")
	if err != nil {
		logger.Global.Error("failed to open TiDB admin connection", zap.Error(err))
		return nil
	}
	defer adminDB.Close()

	_, err = adminDB.ExecContext(context.Background(), "CREATE DATABASE IF NOT EXISTS "+dbName)
	if err != nil {
		logger.Global.Error("failed to create isolated database", zap.Error(err), zap.String("db", dbName))
		return nil
	}
	defer func() {
		_, dropErr := adminDB.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+dbName)
		if dropErr != nil {
			logger.Global.Warn("failed to drop isolated database", zap.Error(dropErr), zap.String("db", dbName))
		}
	}()

	caseDB, err := openDBWithName(tidbDSN, dbName)
	if err != nil {
		logger.Global.Error("failed to open isolated database", zap.Error(err), zap.String("db", dbName))
		return nil
	}
	defer caseDB.Close()

	promptInput := oraclePromptInput{
		BugID: group,
		Cases: make([]oraclePromptCaseInput, 0, len(seeds)),
	}

	for idx, seed := range seeds {
		caseKey := buildCaseKey(group, seed.ID, idx)
		caseConn, err := caseDB.Conn(context.Background())
		if err != nil {
			logger.Global.Error("failed to acquire pinned connection for oracle case", zap.Error(err), zap.String("group", group), zap.String("case", caseKey))
			return nil
		}

		execResult := OracleResult{}
		setupResults, setupErr := testcase.ExecuteStatementsOnConn(caseConn, seed.SetupSQL)
		execResult.Setup = setupResults

		var triggerErr error
		var triggerResults []testcase.StatementResult
		if setupErr == nil {
			triggerResults, triggerErr = testcase.ExecuteStatementsOnConn(caseConn, seed.TriggerSQL)
			execResult.Trigger = triggerResults
		}
		if closeErr := caseConn.Close(); closeErr != nil {
			logger.Global.Warn("failed to close pinned connection for oracle case", zap.Error(closeErr), zap.String("group", group), zap.String("case", caseKey))
		}

		if setupErr != nil {
			execResult.Error = setupErr.Error()
		} else if triggerErr != nil {
			execResult.Error = triggerErr.Error()
		}

		caseInput := oraclePromptCaseInput{
			CaseID:         caseKey,
			Title:          seed.Title,
			SetupSQL:       seed.SetupSQL,
			TriggerSQL:     seed.TriggerSQL,
			Expected:       seed.Expected,
			TiDBResult:     execResult,
			ExecutionError: execResult.Error,
		}
		promptInput.Cases = append(promptInput.Cases, caseInput)
	}

	payload, err := json.MarshalIndent(promptInput, "", "  ")
	if err != nil {
		logger.Global.Error("failed to marshal oracle prompt input", zap.Error(err))
		return nil
	}

	content := strings.ReplaceAll(oracleUserPromptTemplate, "{{ORACLE_INPUT}}", string(payload))
	messages := make([]openai.ChatCompletionMessageParamUnion, 0, 2)
	messages = append(messages, openai.SystemMessage(oracleSystemPrompt))
	messages = append(messages, openai.UserMessage(content))
	return messages
}

// Unmarshal implements PromptGenerator.Unmarshal.
func (g *oraclePromptGenerator) Unmarshal(response string) []*OracleCase {
	var resp oraclePromptResponse
	if len(response) == 0 {
		logger.Global.Info("failed to unmarshal EMPTY oracle response")
		return nil
	}
	if err := json.Unmarshal([]byte(response), &resp); err != nil {
		logger.Global.Error("failed to unmarshal oracle response", zap.Error(err), zap.String("response", response))
		return nil
	}

	results := make([]*OracleCase, 0, len(resp.Cases)+1)
	for _, verdict := range resp.Cases {
		if verdict.CaseID == "" {
			logger.Global.Warn("oracle verdict case missing case_id", zap.String("verdict", fmt.Sprintf("%+v", verdict)))
			continue
		}

		results = append(results, &OracleCase{
			CaseID:      verdict.CaseID,
			Verdict:     verdict.Verdict,
			Reason:      verdict.Reason,
			ReportDraft: verdict.ReportDraft,
		})
	}

	if resp.Summary != nil {
		results = append(results, &OracleCase{
			Summary: resp.Summary,
		})
	}

	return results
}

func buildCaseKey(group string, caseID string, idx int) string {
	if strings.TrimSpace(caseID) == "" {
		return fmt.Sprintf("%s:case-%d", group, idx+1)
	}
	return fmt.Sprintf("%s:%s", group, caseID)
}

func buildIsolatedDBName(group string) string {
	base := strings.ToLower(group)
	re := regexp.MustCompile(`[^a-z0-9_]+`)
	base = re.ReplaceAllString(base, "_")
	if base == "" {
		base = "llmtest"
	}
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(group))
	suffix := fmt.Sprintf("%08x", hasher.Sum32())
	name := fmt.Sprintf("llmtest_%s_%s", base, suffix)
	if len(name) > 64 {
		name = name[:64]
	}
	return name
}

func openDBWithName(dsn string, dbName string) (*sql.DB, error) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return nil, err
	}
	cfg.Collation = "utf8mb4_bin"
	cfg.DBName = dbName
	cfg.MultiStatements = true
	return sql.Open("mysql", cfg.FormatDSN())
}

func newOraclePromptGenerator() (PromptGenerator[*OracleCase], error) {
	seedPath := "testdata/bugseed.json"
	data, err := os.ReadFile(seedPath)
	if err != nil || len(data) == 0 {
		logger.Global.Error("failed to load bug seeds for oracle", zap.Error(err), zap.String("path", seedPath))
		return &oraclePromptGenerator{}, nil
	}

	seedByID := make(map[string][]BugSeed)
	if err := json.Unmarshal(data, &seedByID); err != nil {
		logger.Global.Error("failed to unmarshal bug seeds for oracle", zap.Error(err), zap.String("path", seedPath))
		return &oraclePromptGenerator{}, nil
	}

	return &oraclePromptGenerator{
		seedByID: seedByID,
	}, nil
}

func init() {
	registerGenerator("oracle", GeneratorKindBugSeed, newOraclePromptGenerator, oracleStore)
}
