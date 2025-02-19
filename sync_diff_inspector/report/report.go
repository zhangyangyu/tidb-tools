// Copyright 2021 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package report

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/olekukonko/tablewriter"
	"github.com/pingcap/errors"
	"github.com/pingcap/log"
	"github.com/pingcap/tidb-tools/pkg/dbutil"
	"github.com/pingcap/tidb-tools/sync_diff_inspector/chunk"
	"github.com/pingcap/tidb-tools/sync_diff_inspector/config"
	"github.com/pingcap/tidb-tools/sync_diff_inspector/source/common"
	"github.com/pingcap/tidb-tools/sync_diff_inspector/utils"
	"go.uber.org/zap"
)

const (
	// Pass means all data and struct of tables are equal
	Pass = "pass"
	// Fail means not all data or struct of tables are equal
	Fail  = "fail"
	Error = "error"
)

// ReportConfig stores the config information for the user
type ReportConfig struct {
	Host     string `toml:"host"`
	Port     int    `toml:"port"`
	User     string `toml:"user"`
	Snapshot string `toml:"snapshot,omitempty"`
	SqlMode  string `toml:"sql-mode,omitempty"`
}

// TableResult saves the check result for every table.
type TableResult struct {
	Schema      string                  `json:"schema"`
	Table       string                  `json:"table"`
	StructEqual bool                    `json:"struct-equal"`
	DataSkip    bool                    `json:"data-skip"`
	DataEqual   bool                    `json:"data-equal"`
	MeetError   error                   `json:"-"`
	ChunkMap    map[string]*ChunkResult `json:"chunk-result"` // `ChunkMap` stores the `ChunkResult` of each chunk of the table
}

// ChunkResult save the necessarily information to provide summary information
type ChunkResult struct {
	RowsAdd    int `json:"rows-add"`    // `RowAdd` is the number of rows needed to add
	RowsDelete int `json:"rows-delete"` // `RowDelete` is the number of rows needed to delete
}

// Report saves the check results.
type Report struct {
	sync.RWMutex
	Result       string                             `json:"-"`             // Result is pass or fail
	PassNum      int32                              `json:"-"`             // The pass number of tables
	FailedNum    int32                              `json:"-"`             // The failed number of tables
	TableResults map[string]map[string]*TableResult `json:"table-results"` // TableResult saved the map of  `schema` => `table` => `tableResult`
	StartTime    time.Time                          `json:"start-time"`
	Duration     time.Duration                      `json:"time-duration"`
	TotalSize    int64                              `json:"-"` // Total size of the checked tables
	SourceConfig [][]byte                           `json:"-"`
	TargetConfig []byte                             `json:"-"`

	task *config.TaskConfig `json:"-"`
}

// LoadReport loads the report from the checkpoint
func (r *Report) LoadReport(reportInfo *Report) {
	r.StartTime = time.Now()
	r.Duration = reportInfo.Duration
	r.TotalSize = reportInfo.TotalSize
	for schema, tableMap := range reportInfo.TableResults {
		if _, ok := r.TableResults[schema]; !ok {
			r.TableResults[schema] = make(map[string]*TableResult)
		}
		for table, result := range tableMap {
			r.TableResults[schema][table] = result
		}
	}
}

func (r *Report) getSortedTables() []string {
	equalTables := make([]string, 0)
	for schema, tableMap := range r.TableResults {
		for table, result := range tableMap {
			if result.StructEqual && result.DataEqual {
				equalTables = append(equalTables, dbutil.TableName(schema, table))
			}
		}
	}
	sort.Slice(equalTables, func(i, j int) bool { return equalTables[i] < equalTables[j] })
	return equalTables
}

func (r *Report) getDiffRows() [][]string {
	diffRows := make([][]string, 0)
	for schema, tableMap := range r.TableResults {
		for table, result := range tableMap {
			if result.StructEqual && result.DataEqual {
				continue
			}
			diffRow := make([]string, 0)
			diffRow = append(diffRow, dbutil.TableName(schema, table))
			if !result.StructEqual {
				diffRow = append(diffRow, "false")
			} else {
				diffRow = append(diffRow, "true")
			}
			rowAdd, rowDelete := 0, 0
			for _, chunkResult := range result.ChunkMap {
				rowAdd += chunkResult.RowsAdd
				rowDelete += chunkResult.RowsDelete
			}
			diffRow = append(diffRow, fmt.Sprintf("+%d/-%d", rowAdd, rowDelete))
			diffRows = append(diffRows, diffRow)
		}
	}
	return diffRows
}

// CalculateTotalSize calculate the total size of all the checked tables
// Notice, user should run the analyze table first, when some of tables' size are zero.
func (r *Report) CalculateTotalSize(ctx context.Context, db *sql.DB) {
	for schema, tableMap := range r.TableResults {
		for table := range tableMap {
			size, err := utils.GetTableSize(ctx, db, schema, table)
			if err != nil {
				r.SetTableMeetError(schema, table, err)
			}
			if size == 0 {
				log.Warn("fail to get the correct size of table, if you want to get the correct size, please analyze the corresponding tables", zap.String("table", dbutil.TableName(schema, table)))
			} else {
				r.TotalSize += size
			}
		}
	}
}

// CommitSummary commit summary info
func (r *Report) CommitSummary() error {
	passNum, failedNum := int32(0), int32(0)
	for _, tableMap := range r.TableResults {
		for _, result := range tableMap {
			if result.StructEqual && result.DataEqual {
				passNum++
			} else {
				failedNum++
			}
		}
	}
	r.PassNum = passNum
	r.FailedNum = failedNum
	summaryPath := filepath.Join(r.task.OutputDir, "summary.txt")
	summaryFile, err := os.Create(summaryPath)
	if err != nil {
		return errors.Trace(err)
	}
	defer summaryFile.Close()
	summaryFile.WriteString("Summary\n\n\n\n")
	summaryFile.WriteString("Source Database\n\n\n\n")
	for i := 0; i < len(r.SourceConfig); i++ {
		summaryFile.Write(r.SourceConfig[i])
		summaryFile.WriteString("\n")
	}
	summaryFile.WriteString("Target Databases\n\n\n\n")
	summaryFile.Write(r.TargetConfig)
	summaryFile.WriteString("\n")

	summaryFile.WriteString("Comparison Result\n\n\n\n")
	summaryFile.WriteString("The table structure and data in following tables are equivalent\n\n")
	equalTables := r.getSortedTables()
	for _, table := range equalTables {
		summaryFile.WriteString(table + "\n")
	}
	if r.Result == Fail {
		summaryFile.WriteString("\nThe following tables contains inconsistent data\n\n")
		tableString := &strings.Builder{}
		table := tablewriter.NewWriter(tableString)
		table.SetHeader([]string{"Table", "Structure equality", "Data diff rows"})
		diffRows := r.getDiffRows()
		for _, v := range diffRows {
			table.Append(v)
		}
		table.Render()
		summaryFile.WriteString(tableString.String())
	}
	duration := r.Duration + time.Since(r.StartTime)
	summaryFile.WriteString(fmt.Sprintf("Time Cost: %s\n", duration))
	summaryFile.WriteString(fmt.Sprintf("Average Speed: %fMB/s\n", float64(r.TotalSize)/(1024.0*1024.0*duration.Seconds())))
	return nil
}

func (r *Report) Print(w io.Writer) error {
	var summary strings.Builder
	if r.Result == Pass {
		summary.WriteString(fmt.Sprintf("A total of %d table have been compared and all are equal.\n", r.FailedNum+r.PassNum))
		summary.WriteString(fmt.Sprintf("You can view the comparision details through '%s/%s'\n", r.task.OutputDir, config.LogFileName))
	} else if r.Result == Fail {
		for schema, tableMap := range r.TableResults {
			for table, result := range tableMap {
				if !result.StructEqual {
					if result.DataSkip {
						summary.WriteString(fmt.Sprintf("The structure of %s is not equal, and data-check is skipped\n", dbutil.TableName(schema, table)))
					} else {
						summary.WriteString(fmt.Sprintf("The structure of %s is not equal\n", dbutil.TableName(schema, table)))
					}
				}
				if !result.DataEqual {
					summary.WriteString(fmt.Sprintf("The data of %s is not equal\n", dbutil.TableName(schema, table)))
				}
			}
		}
		summary.WriteString("\n")
		summary.WriteString("The rest of tables are all equal.\n")
		summary.WriteString(fmt.Sprintf("The patch file has been generated in \n\t'%s/'\n", r.task.FixDir))
		summary.WriteString(fmt.Sprintf("You can view the comparision details through '%s/%s'\n", r.task.OutputDir, config.LogFileName))
	} else {
		summary.WriteString("Error in comparison process:\n")
		for schema, tableMap := range r.TableResults {
			for table, result := range tableMap {
				summary.WriteString(fmt.Sprintf("%s error occured in %s\n", result.MeetError.Error(), dbutil.TableName(schema, table)))
			}
		}
		summary.WriteString(fmt.Sprintf("You can view the comparision details through '%s/%s'\n", r.task.OutputDir, config.LogFileName))
	}
	fmt.Fprint(w, summary.String())
	return nil
}

// NewReport returns a new Report.
func NewReport(task *config.TaskConfig) *Report {
	return &Report{
		TableResults: make(map[string]map[string]*TableResult),
		Result:       Pass,
		task:         task,
	}
}

func (r *Report) Init(tableDiffs []*common.TableDiff, sourceConfig [][]byte, targetConfig []byte) {
	r.StartTime = time.Now()
	r.SourceConfig = sourceConfig
	r.TargetConfig = targetConfig
	for _, tableDiff := range tableDiffs {
		schema, table := tableDiff.Schema, tableDiff.Table
		if _, ok := r.TableResults[schema]; !ok {
			r.TableResults[schema] = make(map[string]*TableResult)
		}
		r.TableResults[schema][table] = &TableResult{
			Schema:      schema,
			Table:       table,
			StructEqual: true,
			DataEqual:   true,
			MeetError:   nil,
			ChunkMap:    make(map[string]*ChunkResult),
		}
	}
}

// SetTableStructCheckResult sets the struct check result for table.
func (r *Report) SetTableStructCheckResult(schema, table string, equal bool, skip bool) {
	r.Lock()
	defer r.Unlock()
	tableResult := r.TableResults[schema][table]
	tableResult.StructEqual = equal
	tableResult.DataSkip = skip
	if !equal && r.Result != Error {
		r.Result = Fail
	}
}

// SetTableDataCheckResult sets the data check result for table.
func (r *Report) SetTableDataCheckResult(schema, table string, equal bool, rowsAdd, rowsDelete int, id *chunk.ChunkID) {
	r.Lock()
	defer r.Unlock()
	if !equal {
		result := r.TableResults[schema][table]
		result.DataEqual = equal
		if _, ok := result.ChunkMap[id.ToString()]; !ok {
			result.ChunkMap[id.ToString()] = &ChunkResult{
				RowsAdd:    0,
				RowsDelete: 0,
			}
		}
		result.ChunkMap[id.ToString()].RowsAdd += rowsAdd
		result.ChunkMap[id.ToString()].RowsDelete += rowsDelete
		if r.Result != Error {
			r.Result = Fail
		}
	}
	if !equal && r.Result != Error {
		r.Result = Fail
	}
}

// SetTableMeetError sets meet error when check the table.
func (r *Report) SetTableMeetError(schema, table string, err error) {
	r.Lock()
	defer r.Unlock()
	if _, ok := r.TableResults[schema]; !ok {
		r.TableResults[schema] = make(map[string]*TableResult)
		r.TableResults[schema][table] = &TableResult{
			MeetError: err,
		}
		return
	}

	r.TableResults[schema][table].MeetError = err
	r.Result = Error
}

// GetSnapshot get the snapshot of the current state of the report, then we can restart the
// sync-diff and get the correct report state.
func (r *Report) GetSnapshot(chunkID *chunk.ChunkID, schema, table string) (*Report, error) {
	r.RLock()
	defer r.RUnlock()
	targetID := utils.UniqueID(schema, table)
	reserveMap := make(map[string]map[string]*TableResult)
	for schema, tableMap := range r.TableResults {
		reserveMap[schema] = make(map[string]*TableResult)
		for table, result := range tableMap {
			reportID := utils.UniqueID(schema, table)
			if reportID >= targetID {
				chunkRes := make(map[string]*ChunkResult)
				reserveMap[schema][table] = &TableResult{
					Schema:      result.Schema,
					Table:       result.Table,
					StructEqual: result.StructEqual,
					DataEqual:   result.DataEqual,
					MeetError:   result.MeetError,
				}
				for id, chunkResult := range result.ChunkMap {
					sid := new(chunk.ChunkID)
					err := sid.FromString(id)
					if err != nil {
						return nil, errors.Trace(err)
					}
					if sid.Compare(chunkID) <= 0 {
						chunkRes[id] = chunkResult
					}
				}
				reserveMap[schema][table].ChunkMap = chunkRes
			}
		}
	}

	result := r.Result
	totalSize := r.TotalSize
	duration := time.Since(r.StartTime)
	task := r.task
	return &Report{
		PassNum:      0,
		FailedNum:    0,
		Result:       result,
		TableResults: reserveMap,
		StartTime:    r.StartTime,
		Duration:     duration,
		TotalSize:    totalSize,

		task: task,
	}, nil
}
