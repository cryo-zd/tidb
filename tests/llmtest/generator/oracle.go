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

const oracleSystemPrompt = `You are a precise and honest database bug judge.
You will receive TiDB execution results and the expected behavior. Decide whether the observed result indicates a TiDB bug.

Rules:
1) Output strict JSON only (no markdown). Case IDs must be preserved exactly as input.
2) Understand the Scenario: Read the setup SQL, trigger SQL, and the original bug description.
3) Mental Sandbox Execution:
   - Carefully analyze the provided SQLs to understand the scenario and mentally execute the them step by step.
   - Think step by step, analyze and infer the execution result of TiDB based on the SQL semantices as precisely as possible. 
	- Pay attention that the "expected_behavior" field in input may be misleading, you should not directly depend on it without thinking and analysis to figure our the truly correct result!
   	- If your inferred result is contrary to the provided expected result, rethink it carefully but note that the provided one is possible to be misleading. 
   - Compare your expected correct result with TiDB's Actual Result:
   	- If insufficient info, return verdict="uncertain". 
	- If the TiDB's actual result provided only confirms successful SQL execution but does not specify the actual query result set, treat the excution result as “empty set (0 rows)”.
4) you should immediately mark the verdict as "uncertain" if the test case is primarily about:
   - Performance/Timing: Execution time, latency, speed, "query is slow", or specific optimizer execution plans (EXPLAIN output).
   - Internal Metrics: Memory usage specifics, disk I/O, or internal status variables unique to MySQL engine.
   - TiDB result contains an error: i.e., syntac error`

const oracleUserPromptTemplate = `Evaluate each case below and judge whether TiDB behavior is correct step by step.

Input JSON:
{{ORACLE_INPUT}}

Output schema (strict JSON only):
{
  "cases": [
    {
      "case_id": "exactly the same as input",
      "reason": "short justification based on expected vs actual",
	  "verdict": "bug | ok | uncertain",
      "report_draft": "optional: short bug report text"
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
    "canonical_case_id": "caseA",
    "minimization_hint": "how to shrink the repro SQL",
    "notes": "optional"
  }
}
`

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
	if tidbDSN == "" {
		logger.Global.Error("oracle generator missing TiDB DSN; set --tidb_dsn or TIDB_DSN")
		registerGenerator("oracle", GeneratorKindBugSeed, func() (PromptGenerator[*OracleCase], error) {
			return &oraclePromptGenerator{}, nil
		}, oracleStore)
		return nil
	}

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

		execResult := OracleResult{}
		setupResults, setupErr := testcase.ExecuteStatements(caseDB, seed.SetupSQL)
		execResult.Setup = setupResults

		var triggerErr error
		var triggerResults []testcase.StatementResult
		if setupErr == nil {
			triggerResults, triggerErr = testcase.ExecuteStatements(caseDB, seed.TriggerSQL)
			execResult.Trigger = triggerResults
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
