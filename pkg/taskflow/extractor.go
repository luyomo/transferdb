/*
Copyright © 2020 Marvin

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package taskflow

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/wentaojin/transferdb/utils"

	"github.com/wentaojin/transferdb/service"

	"go.uber.org/zap"

	"github.com/xxjwxc/gowp/workpool"
)

// 捕获全量数据
func extractorTableFullRecord(engine *service.Engine, sourceSchemaName, sourceTableName, oracleQuery string, insertBatchSize int) ([]string, []string, error) {
	startTime := time.Now()
	cols, rowsResult, err := engine.GetOracleTableRowsData(oracleQuery, insertBatchSize)
	if err != nil {
		return cols, rowsResult, fmt.Errorf("get oracle schema [%s] table [%s] record by sql [%v] falied: %v", sourceSchemaName, sourceTableName, oracleQuery, err)
	}

	endTime := time.Now()
	zap.L().Info("single full table rowid data extractor finished",
		zap.String("schema", sourceSchemaName),
		zap.String("table", sourceTableName),
		zap.String("sql", oracleQuery),
		zap.String("cost", endTime.Sub(startTime).String()))

	return cols, rowsResult, nil
}

// 捕获增量数据
func extractorTableIncrementRecord(engine *service.Engine,
	sourceSchemaName string,
	sourceTableNameList []string,
	logFileName string,
	logFileStartSCN int, lastCheckpoint, logminerQueryTimeout int) ([]service.LogminerContent, error) {
	rowsResult, err := engine.GetOracleLogminerContentToMySQL(
		sourceSchemaName,
		utils.StringArrayToCapitalChar(sourceTableNameList),
		strconv.Itoa(lastCheckpoint),
		logminerQueryTimeout)
	if err != nil {
		return []service.LogminerContent{}, err
	}
	zap.L().Info("increment table log extractor", zap.String("logfile", logFileName),
		zap.Int("logfile start scn", logFileStartSCN),
		zap.Int("source table last scn", lastCheckpoint),
		zap.Int("row counts", len(rowsResult)))

	return rowsResult, nil
}

// 按表级别筛选区别数据
func filterOracleRedoGreaterOrEqualRecordByTable(
	rowsResult []service.LogminerContent,
	transferTableList []string,
	transferTableMetaMap map[string]int,
	workerThreads, currentResetFlag int) (map[string][]service.LogminerContent, error) {
	var (
		lcMap map[string][]service.LogminerContent
		lc    []service.LogminerContent
	)
	lcMap = make(map[string][]service.LogminerContent)

	for _, table := range transferTableList {
		lcMap[strings.ToUpper(table)] = lc
	}

	startTime := time.Now()
	zap.L().Info("oracle table redo filter start",
		zap.Time("start time", startTime))

	c := make(chan struct{})
	// 开始准备从 channel 接收数据了
	s := NewScheduleJob(workerThreads, lcMap, func() { c <- struct{}{} })

	wp := workpool.New(workerThreads)
	for _, rs := range rowsResult {
		tfMap := transferTableMetaMap
		rows := rs
		isFirstR := currentResetFlag
		wp.DoWait(func() error {
			// 筛选过滤 Oracle Redo SQL
			// 1、数据同步只同步 INSERT/DELETE/UPDATE DML以及只同步 truncate table/ drop table 限定 DDL
			// 2、根据元数据表 table_increment_meta 对应表已经同步写入得 SCN SQL 记录,过滤 Oracle 提交记录 SCN 号，过滤,防止重复写入
			if isFirstR == 0 {
				if rows.SCN >= tfMap[strings.ToUpper(rows.TableName)] {
					if rows.Operation == utils.DDLOperation {
						splitDDL := strings.Split(rows.SQLRedo, ` `)
						ddl := utils.StringsBuilder(splitDDL[0], ` `, splitDDL[1])
						if strings.ToUpper(ddl) == utils.DropTableOperation {
							// 处理 drop table marvin8 AS "BIN$vVWfliIh6WfgU0EEEKzOvg==$0"
							rows.SQLRedo = strings.Split(strings.ToUpper(rows.SQLRedo), "AS")[0]
							s.AddData(rows)
						}
						if strings.ToUpper(ddl) == utils.TruncateTableOperation {
							// 处理 truncate table marvin8
							s.AddData(rows)
						}
					} else {
						s.AddData(rows)
					}
				}
				return nil

			} else if isFirstR == 1 {
				if rows.SCN > tfMap[strings.ToUpper(rows.TableName)] {
					if rows.Operation == utils.DDLOperation {
						splitDDL := strings.Split(rows.SQLRedo, ` `)
						ddl := utils.StringsBuilder(splitDDL[0], ` `, splitDDL[1])
						if strings.ToUpper(ddl) == utils.DropTableOperation {
							// 处理 drop table marvin8 AS "BIN$vVWfliIh6WfgU0EEEKzOvg==$0"
							rows.SQLRedo = strings.Split(strings.ToUpper(rows.SQLRedo), "AS")[0]
							s.AddData(rows)
						}
						if strings.ToUpper(ddl) == utils.TruncateTableOperation {
							// 处理 truncate table marvin8
							s.AddData(rows)
						}
					} else {
						s.AddData(rows)
					}
				}
				return nil
			} else {
				return fmt.Errorf("filterOracleRedoGreaterOrEqualRecordByTable meet error, isFirstRun value error")
			}
		})
	}
	if err := wp.Wait(); err != nil {
		return lcMap, err
	}
	if !wp.IsDone() {
		return lcMap, fmt.Errorf("filter oracle redo record by table error")
	}

	s.Close()
	<-c

	endTime := time.Now()
	zap.L().Info("oracle table filter finished",
		zap.String("status", "success"),
		zap.Time("start time", startTime),
		zap.Time("end time", endTime),
		zap.String("cost time", time.Since(startTime).String()))

	return lcMap, nil
}

// 1、根据当前表的 SCN 初始化元数据据表
// 2、根据元数据表记录全量导出导入
func initOracleTableConsumeRowID(cfg *service.CfgFile, engine *service.Engine,
	waitSyncTableInfo []string, syncMode string, oraCollation bool) error {
	// 全量同步前，获取 SCN 以及初始化元数据表
	globalSCN, err := engine.GetOracleCurrentSnapshotSCN()
	if err != nil {
		return err
	}

	wp := workpool.New(cfg.FullConfig.TaskThreads)

	for idx, tbl := range waitSyncTableInfo {
		table := tbl
		workerID := idx
		wp.Do(func() error {
			startTime := time.Now()
			// Date/Timestamp 字段类型格式化
			// Interval Year/Day 数据字符 TO_CHAR 格式化
			sourceColumnInfo, err := engine.AdjustTableSelectColumn(cfg.SourceConfig.SchemaName, table, oraCollation)
			if err != nil {
				return err
			}

			if err = engine.InitWaitAndFullSyncMetaRecord(strings.ToUpper(cfg.SourceConfig.SchemaName),
				table, sourceColumnInfo, strings.ToUpper(cfg.TargetConfig.SchemaName), table, workerID, globalSCN,
				cfg.FullConfig.ChunkSize, cfg.AppConfig.InsertBatchSize, "", syncMode); err != nil {
				return err
			}

			endTime := time.Now()
			zap.L().Info("single table init wait_sync_meta and full_sync_meta finished",
				zap.String("schema", cfg.SourceConfig.SchemaName),
				zap.String("table", table),
				zap.Int("global scn", globalSCN),
				zap.String("cost", endTime.Sub(startTime).String()))

			return nil
		})
	}

	if err := wp.Wait(); err != nil {
		return err
	}
	if !wp.IsDone() {
		return fmt.Errorf("init oracle table rowid by scn failed, please rerunning")
	}

	return nil
}

// 根据元数据表记录全量导出导入
func startOracleTableConsumeByCheckpoint(cfg *service.CfgFile, engine *service.Engine, syncTableInfo []string, syncMode string) error {
	wp := workpool.New(cfg.FullConfig.TableThreads)
	for _, tbl := range syncTableInfo {
		table := tbl
		wp.Do(func() error {
			if err := syncOracleRowsByRowID(cfg, engine, table, syncMode); err != nil {
				return fmt.Errorf("sync oracle table rows by rowid failed, error: %v", err)
			}
			return nil
		})
	}
	if err := wp.Wait(); err != nil {
		return fmt.Errorf("sync oracle table rows by checkpoint failed: %v", err)
	}
	if !wp.IsDone() {
		return fmt.Errorf("sync oracle table rows by checkpoint failed, please rerunning")
	}
	return nil
}

func syncOracleRowsByRowID(cfg *service.CfgFile, engine *service.Engine, sourceTableName, syncMode string) error {
	startTime := time.Now()

	fullSyncMetas, err := engine.GetFullSyncMetaRowIDRecord(cfg.SourceConfig.SchemaName, sourceTableName)
	if err != nil {
		return err
	}

	wp := workpool.New(cfg.FullConfig.SQLThreads)
	for _, m := range fullSyncMetas {
		meta := m
		wp.Do(func() error {
			querySQL := utils.StringsBuilder(meta.SourceSQL, meta.RowidSQL)

			// 抽取 Oracle 数据
			var (
				columnFields []string
				rowsResult   []string
			)

			columnFields, rowsResult, err = extractorTableFullRecord(engine, cfg.SourceConfig.SchemaName, sourceTableName, querySQL, cfg.AppConfig.InsertBatchSize)
			if err != nil {
				return err
			}

			if len(rowsResult) == 0 {
				zap.L().Warn("oracle schema table rowid data return null rows, skip",
					zap.String("schema", cfg.SourceConfig.SchemaName),
					zap.String("table", sourceTableName),
					zap.String("sql", querySQL))

				// 清理 full_sync_meta 记录以及更新 wait_sync_meta 记录
				if err = engine.ModifyWaitAndFullSyncTableMetaRecord(
					cfg.TargetConfig.MetaSchema,
					cfg.SourceConfig.SchemaName,
					sourceTableName,
					meta.RowidSQL,
					syncMode); err != nil {
					return err
				}
				return nil
			}

			// 转换/应用 Oracle 数据 -> MySQL
			//prepareSQL1, batchArgs1, prepareSQL2, batchArgs2 := translatorTableFullRecord(
			//	cfg.TargetConfig.SchemaName, sourceTableName,
			//	meta.RowidSQL, columnFields, rowsResult, cfg.AppConfig.InsertBatchSize, safeMode)

			if err = applierTableFullRecord(
				engine,
				cfg.TargetConfig.SchemaName,
				meta.SourceTableName,
				querySQL,
				cfg.FullConfig.ApplyThreads,
				columnFields, rowsResult); err != nil {
				return err
			}

			// 清理 full_sync_meta 记录
			if err = engine.ClearFullSyncTableMetaRecord(
				cfg.TargetConfig.MetaSchema,
				cfg.SourceConfig.SchemaName, meta.SourceTableName, meta.RowidSQL); err != nil {
				return err
			}
			return nil
		})
	}
	if err = wp.Wait(); err != nil {
		return err
	}

	endTime := time.Now()
	if !wp.IsDone() {
		zap.L().Fatal("single full table data loader failed",
			zap.String("schema", cfg.SourceConfig.SchemaName),
			zap.String("table", sourceTableName),
			zap.String("cost", endTime.Sub(startTime).String()))
		return fmt.Errorf("oracle schema [%s] single full table [%v] data loader failed",
			cfg.SourceConfig.SchemaName, sourceTableName)
	}

	// 更新 wait_sync_meta 记录
	if err = engine.ModifyWaitSyncTableMetaRecord(
		cfg.TargetConfig.MetaSchema,
		cfg.SourceConfig.SchemaName, sourceTableName, syncMode); err != nil {
		return err
	}

	zap.L().Info("single full table data loader finished",
		zap.String("schema", cfg.SourceConfig.SchemaName),
		zap.String("table", sourceTableName),
		zap.String("cost", endTime.Sub(startTime).String()))

	return nil
}

// 根据配置文件以及起始 SCN 生成同步表元数据 [increment_sync_meta]
func generateTableIncrementTaskCheckpointMeta(sourceSchemaName, metaSchemaName string, engine *service.Engine, syncMode string) error {
	// 获取所有已完成全量数据的表记录
	tableMeta, _, err := engine.GetFinishFullSyncMetaRecord(sourceSchemaName, syncMode)
	if err != nil {
		return err
	}

	// 记录表起始 SCN 记录
	for _, tm := range tableMeta {
		if err = engine.InitIncrementSyncMetaRecord(tm.SourceSchemaName, tm.SourceTableName, tm.IsPartition, tm.FullGlobalSCN); err != nil {
			return err
		}
	}
	return nil
}
