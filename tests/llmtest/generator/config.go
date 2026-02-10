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
	"os"
	"sync/atomic"
)

var tidbDSN atomic.Value

// SetTiDBDSN sets the TiDB DSN for prompt generators that need to execute SQL.
func SetTiDBDSN(dsn string) {
	tidbDSN.Store(dsn)
}

func getTiDBDSN() string {
	if value := tidbDSN.Load(); value != nil {
		if dsn, ok := value.(string); ok {
			return dsn
		}
	}
	return os.Getenv("TIDB_DSN")
}
