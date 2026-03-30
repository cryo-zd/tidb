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

package testcase

import (
	"context"
	"database/sql"
	"strings"

	"github.com/pingcap/tidb/tests/llmtest/logger"
	"go.uber.org/zap"
)

// StatementResult captures the execution result of a single SQL statement.
type StatementResult struct {
	SQL           string     `json:"sql"`
	StatementType string     `json:"statement_type"`
	Rows          [][]string `json:"rows,omitempty"`
	RowCount      *int       `json:"row_count,omitempty"`
	RowsAffected  *int64     `json:"rows_affected,omitempty"`
	Error         string     `json:"error,omitempty"`
}

const (
	statementTypeQuery = "query"
	statementTypeExec  = "exec"
)

type statementExecutor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func isQueryStatement(query string) bool {
	trimmed := strings.TrimSpace(query)
	if trimmed == "" {
		return false
	}

	lower := strings.ToLower(trimmed)
	switch {
	case strings.HasPrefix(lower, "select "):
		return true
	case strings.HasPrefix(lower, "with "):
		return true
	case strings.HasPrefix(lower, "show "):
		return true
	case strings.HasPrefix(lower, "explain "):
		return true
	case strings.HasPrefix(lower, "describe "):
		return true
	case strings.HasPrefix(lower, "desc "):
		return true
	case strings.HasPrefix(lower, "values "):
		return true
	case strings.HasPrefix(lower, "execute "):
		return true
	case strings.HasPrefix(lower, "call "):
		return true
	default:
		return false
	}
}

func executeSingleQueryInDB(exec statementExecutor, query string, args ...any) ([][]string, error) {
	dbRows, err := exec.QueryContext(context.Background(), query, args...)
	if err != nil {
		return nil, err
	}
	defer dbRows.Close()

	cols, err := dbRows.Columns()
	if err != nil {
		return nil, err
	}
	var queryResults [][]string
	for dbRows.Next() {
		data := make([]any, len(cols))
		for i := range data {
			var s sql.NullString
			data[i] = &s
		}
		err = dbRows.Scan(data...)
		if err != nil {
			return nil, err
		}

		rowStrings := make([]string, len(cols))
		for i, val := range data {
			strVal := val.(*sql.NullString)
			if strVal.Valid {
				rowStrings[i] = strVal.String
			} else {
				rowStrings[i] = "<nil>"
			}
		}
		queryResults = append(queryResults, rowStrings)
	}

	// In some cases (e.g. `SELECT COT(0)`), `dbRows.Next()` will always return false, and the error
	// can be retrieved by calling `dbRows.Err()`. This behavior can be different with TiDB, because
	// for TiDB the client may get error from `dbRows.Scan()`.
	// Not sure whether it's expected, but it looks acceptable for now.
	if dbRows.Err() != nil {
		return nil, dbRows.Err()
	}

	return queryResults, nil
}

// executeStatementInDB is the shared low-level execution entrypoint used by both the structured oracle path and AB test path.
func executeStatementInDB(exec statementExecutor, query string, args ...any) (string, [][]string, sql.Result, error) {
	if isQueryStatement(query) {
		rows, err := executeSingleQueryInDB(exec, query, args...)
		return statementTypeQuery, rows, nil, err
	}

	execResult, err := exec.ExecContext(context.Background(), query, args...)
	return statementTypeExec, nil, execResult, err
}

// buildStatementResult converts the shared execution result into the structured statement payload consumed by the oracle generator.
func buildStatementResult(query string, statementType string, rows [][]string, execResult sql.Result, err error) StatementResult {
	result := StatementResult{
		SQL:           query,
		StatementType: statementType,
	}

	if err != nil {
		result.Error = err.Error()
	}

	switch statementType {
	case statementTypeQuery:
		rowCount := len(rows)
		result.RowCount = &rowCount
		if len(rows) > 0 {
			result.Rows = rows
		}
	case statementTypeExec:
		if execResult != nil {
			rowsAffected, rowsAffectedErr := execResult.RowsAffected()
			if rowsAffectedErr == nil {
				result.RowsAffected = &rowsAffected
			}
		}
	}

	return result
}

// executeSingleStatementInDB preserves the AB test contract while reusing the shared statement execution logic above.
func executeSingleStatementInDB(exec statementExecutor, query string, args ...any) ([][]string, error) {
	statementType, rows, _, err := executeStatementInDB(exec, query, args...)
	if statementType == statementTypeQuery {
		return rows, err
	}
	if err != nil {
		return nil, err
	}
	return [][]string{}, nil
}

func executeStatementWithMetadataInDB(exec statementExecutor, query string, args ...any) (StatementResult, error) {
	statementType, rows, execResult, err := executeStatementInDB(exec, query, args...)
	return buildStatementResult(query, statementType, rows, execResult, err), err
}

func executeStatements(exec statementExecutor, sqls []string, args ...any) ([]StatementResult, error) {
	results := make([]StatementResult, 0, len(sqls))

	for _, raw := range sqls {
		query := strings.TrimSpace(raw)
		if query == "" {
			continue
		}

		result, err := executeStatementWithMetadataInDB(exec, query, args...)
		if err != nil {
			results = append(results, result)
			return results, err
		}
		results = append(results, result)
	}

	return results, nil
}

// ExecuteStatements runs a list of SQL statements and returns per-statement results.
// If any statement fails, it returns the error after recording the failure.
func ExecuteStatements(db *sql.DB, sqls []string, args ...any) ([]StatementResult, error) {
	return executeStatements(db, sqls, args...)
}

// ExecuteStatementsOnConn runs a list of SQL statements on a pinned connection.
func ExecuteStatementsOnConn(conn *sql.Conn, sqls []string, args ...any) ([]StatementResult, error) {
	return executeStatements(conn, sqls, args...)
}

func executeSQLs(exec statementExecutor, c *Case) (ret [][][]string, retErr error) {
	allQueries := strings.Split(c.SQL, ";")
	allResults := make([][][]string, 0, len(allQueries))

	for _, query := range allQueries {
		query = strings.TrimSpace(query)
		if len(query) == 0 {
			continue
		}

		queryResults, err := executeSingleStatementInDB(exec, query, c.Args...)
		if err != nil {
			return nil, err
		}
		allResults = append(allResults, queryResults)
	}

	return allResults, nil
}

func executeSQLsInPinnedConn(db *sql.DB, c *Case) (ret [][][]string, retErr error) {
	conn, err := db.Conn(context.Background())
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return executeSQLs(conn, c)
}

// RunABTest runs the A/B test on two databases.
func (m *Manager) RunABTest(db1 *sql.DB, db2 *sql.DB, recheckPassed bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Run A/B test
	for _, cases := range m.cases {
	caseLoop:
		for _, c := range cases {
			if c.Known {
				continue
			}
			if !recheckPassed && c.Pass {
				continue
			}
			logger := logger.Global.With(
				zap.String("sql", c.SQL), zap.Any("args", c.Args),
			)
			result1, err1 := executeSQLsInPinnedConn(db1, c)
			result2, err2 := executeSQLsInPinnedConn(db2, c)

			if err1 != nil || err2 != nil {
				// all of them should fail
				if !(err1 != nil && err2 != nil) {
					logger.Info("One of the result is error", zap.Error(err1), zap.Error(err2))
					c.Pass = false
				} else {
					c.Pass = true
				}

				continue
			}

			if len(result1) != len(result2) {
				logger.Info("Different result set count", zap.Any("result1", result1), zap.Any("result2", result2))
				c.Pass = false
				continue
			}

			for i := range result1 {
				if len(result1[i]) != len(result2[i]) {
					c.Pass = false
					logger.Info("Different row count", zap.Any("result1", result1[i]), zap.Any("result2", result2[i]))
					continue caseLoop
				}
				for j := range result1[i] {
					if len(result1[i][j]) != len(result2[i][j]) {
						c.Pass = false
						logger.Info("Different column length", zap.Strings("result1", result1[i][j]), zap.Strings("result2", result2[i][j]))
						continue caseLoop
					}

					for k := range result1[i][j] {
						if result1[i][j][k] != result2[i][j][k] {
							c.Pass = false
							logger.Info("Different result", zap.Strings("result1", result1[i][j]), zap.Strings("result2", result2[i][j]))
							continue caseLoop
						}
					}
				}
			}

			c.Pass = true
		}
	}
}
