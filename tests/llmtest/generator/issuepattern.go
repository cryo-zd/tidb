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
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/openai/openai-go"
	"github.com/pingcap/tidb/tests/llmtest/logger"
	"go.uber.org/zap"
)

const issuePatternSystemPrompt = `You are an expert and honest Database Bug Triage Specialist.
Your task is to read a MySQL bug report record (JSON) and produce a normalized bug pattern JSON.`

const issuePatternUserPromptTemplate = `You are an expert and honest Database Bug Triage Specialist suitable for TiDB compatibility testing. Given the following MySQL bug report, analyze it and extract a structured "Bug Pattern" in JSON format. Think step-by-step as below examples.

Example:
Input:
{
  "source": "mysql-bugs",
  "title": "Server crash with CTE and UNION ALL",
  "description": "The server crashes when a recursive CTE involves a UNION ALL operation. This seems to happen only when the column type is VARCHAR.",
  "steps": [
    "CREATE TABLE tree (id INT, path VARCHAR(100));",
	"INSERT INTO tree VALUES (1, 'root');",
	"WITH RECURSIVE cte (id, path) AS (SELECT id, path FROM tree UNION ALL SELECT id+1, path FROM cte WHERE id < 5) SELECT * FROM cte;"
  ],
  "version": "8.0.23",
  "links": ["https://bugs.mysql.com/bug.php?id=12345"]
}
Output:
{
  "id": "mysql-12345",
  "source_db": "mysql",
  "title": "Server crash with CTE and UNION ALL",
  "relevant": true,
  "relevance_reason": "",
  "symptom": "crash",
  "feature": ["CTE", "Recursive", "UNION ALL", "VARCHAR"],
  "setup_sql": [
    "DROP TABLE IF EXISTS tree;",
    "CREATE TABLE tree (id INT, path VARCHAR(100));",
    "INSERT INTO tree VALUES (1, 'root');"
  ],
  "trigger_sql": [
    "WITH RECURSIVE cte (id, path) AS (SELECT id, path FROM tree UNION ALL SELECT id+1, path FROM cte WHERE id < 5) SELECT * FROM cte;"
  ],
  "expected": "The query should return 5 rows.",
  "notes": "..."
}

Now, process the following input bug pattern and return only JSON output:
Input issue (JSON):
{{ISSUE_JSON}}

Required id: {{ID}}

Output schema (strict JSON):
{
  "id": "local-unique-id",
  "source_db": "mysql | cockroach | postgres | mariadb | yugabytedb | ...",
  "title": "short summary",
  "relevant": true,
  "relevance_reason": "if relevant=false, explain why it is not applicable to TiDB",
  "symptom": "wrong_result | crash | panic | timeout | perf | incompat",
  "feature": ["keyword1", "keyword2"],
  "setup_sql": ["..."],
  "trigger_sql": ["..."],
  "expected": "expected behavior or example result",
  "notes": "optional constraints (version, config, isolation, hints)"
}

Rules:
1) If the bug is about documentation, tooling, runtime warnings, or a MySQL-only feature, set relevant=false and explain in relevance_reason.
2) Prefer minimal reproduction SQL; split into setup_sql and trigger_sql.
3) The description of expected should be as clear and specific as possible. If expected/actual are unclear, infer from description and say so in notes.
4) Use the Required id exactly as the "id" in the output JSON.
5) Output strict JSON only. No extra text.
`

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
		logger.Global.Info("Skip one irrelevant bug issue pattern")
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
	issueSeedFile := "/Users/cryo/project/Crawler/server_dml_closed_90-119.json"
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
