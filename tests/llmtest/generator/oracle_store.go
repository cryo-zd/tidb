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
	"os"
	"sync"

	"github.com/pingcap/tidb/tests/llmtest/logger"
	"github.com/pingcap/tidb/tests/llmtest/testcase"
)

// OracleResult represents execution results from TiDB.
type OracleResult struct {
	Setup   []testcase.StatementResult `json:"setup,omitempty"`
	Trigger []testcase.StatementResult `json:"trigger,omitempty"`
	Error   string                     `json:"error,omitempty"`
}

// OracleRootCauseGroup describes a grouping for related cases.
type OracleRootCauseGroup struct {
	GroupID      string   `json:"group_id"`
	CaseIDs      []string `json:"case_ids"`
	Relationship string   `json:"relationship"`
}

// OracleSummary summarizes the case-level verdicts for one bug id.
type OracleSummary struct {
	RootCauseGroups  []OracleRootCauseGroup `json:"root_cause_groups,omitempty"`
	CanonicalCaseID  string                 `json:"canonical_case_id,omitempty"`
	MinimizationHint string                 `json:"minimization_hint,omitempty"`
	Notes            string                 `json:"notes,omitempty"`
}

// OracleCase represents one LLM oracle verdict for a test case.
type OracleCase struct {
	CaseID      string `json:"case_id"`
	Verdict     string `json:"verdict"`
	Reason      string `json:"reason"`
	ReportDraft string `json:"report_draft,omitempty"`

	Summary *OracleSummary `json:"-"`
}

// OracleGroup groups all cases and summary for one bug id.
type OracleGroup struct {
	Summary *OracleSummary `json:"summary,omitempty"`
	Cases   []*OracleCase  `json:"cases,omitempty"`
}

// OracleStore manages oracle results stored on disk.
type OracleStore struct {
	mu sync.Mutex

	path string

	cases map[string]*OracleGroup
}

// OpenOracleStore creates a new OracleStore on a given path.
func OpenOracleStore(path string) (*OracleStore, error) {
	store := &OracleStore{
		path:  path,
		cases: make(map[string]*OracleGroup),
	}
	return store, nil
}

// Save saves oracle results to disk.
func (s *OracleStore) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	bytes, err := json.MarshalIndent(s.cases, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(s.path, bytes, 0644)
}

// Append appends an oracle case to the store.
func (s *OracleStore) Append(group string, c *OracleCase) {
	if c == nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.cases[group]
	if !ok {
		entry = &OracleGroup{}
		s.cases[group] = entry
	}

	if c.Summary != nil {
		entry.Summary = c.Summary
		return
	}

	if c.CaseID == "" {
		logger.Global.Warn("failed to append oracle case without case id")
		return
	}
	entry.Cases = append(entry.Cases, c)
}

// Exist returns oracle cases in a group.
func (s *OracleStore) Exist(group string) []*OracleCase {
	s.mu.Lock()
	defer s.mu.Unlock()

	if entry := s.cases[group]; entry != nil {
		return entry.Cases
	}
	return nil
}
