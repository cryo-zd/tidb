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
)

// BugSeed represents a bug pattern used for prompt generation.
type BugSeed struct {
	ID       string   `json:"id"`
	SourceDB string   `json:"source_db"`
	Title    string   `json:"title"`
	Symptom  string   `json:"symptom"`
	Feature  []string `json:"feature"`

	SetupSQL   []string `json:"setup_sql"`
	TriggerSQL []string `json:"trigger_sql"`

	Expected string `json:"expected"`
	Notes    string `json:"notes"`
}

// BugSeedStore manages bug seeds stored on disk.
type BugSeedStore struct {
	mu sync.Mutex

	path string

	seeds map[string][]*BugSeed
}

// OpenBugSeedStore creates a new BugSeedStore on a given path.
func OpenBugSeedStore(path string) (*BugSeedStore, error) {
	store := &BugSeedStore{
		path:  path,
		seeds: make(map[string][]*BugSeed),
	}
	return store, nil
}

// Save saves the bug seeds to the file.
func (s *BugSeedStore) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 'MarshalIdent' to make it more readable
	bytes, err := json.MarshalIndent(s.seeds, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(s.path, bytes, 0644)
}

// Append appends a bug seed to the store.
func (s *BugSeedStore) Append(group string, seed *BugSeed) {
	if seed == nil {
		return
	}
	if seed.ID == "" {
		logger.Global.Warn("failed to append one bug seed that did not have an ID")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.seeds[group] = append(s.seeds[group], seed)
}

// Exist returns the bug seeds in a group.
func (s *BugSeedStore) Exist(group string) []*BugSeed {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.seeds[group]
}
