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
package o2m

import (
	"context"
	"fmt"
	"github.com/wentaojin/transferdb/common"
	"github.com/wentaojin/transferdb/config"
	"github.com/wentaojin/transferdb/database/meta"
	"github.com/wentaojin/transferdb/database/mysql"
	"github.com/wentaojin/transferdb/database/oracle"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
	"strconv"
	"strings"
	"time"
)

type Migrate struct {
	Ctx    context.Context
	Cfg    *config.Config
	Oracle *oracle.Oracle
	Mysql  *mysql.MySQL
	MetaDB *meta.Meta
}

func NewO2MFuller(ctx context.Context, cfg *config.Config, oracle *oracle.Oracle, mysql *mysql.MySQL, metaDB *meta.Meta) *Migrate {
	return &Migrate{
		Ctx:    ctx,
		Cfg:    cfg,
		Oracle: oracle,
		Mysql:  mysql,
		MetaDB: metaDB,
	}
}

func (r *Migrate) NewFuller() error {
	startTime := time.Now()
	zap.L().Info("source schema full table data sync start",
		zap.String("schema", r.Cfg.OracleConfig.SchemaName))

	// 判断上游 Oracle 数据库版本
	// 需要 oracle 11g 及以上
	oracleDBVersion, err := r.Oracle.GetOracleDBVersion()
	if err != nil {
		return err
	}
	if common.VersionOrdinal(oracleDBVersion) < common.VersionOrdinal(common.RequireOracleDBVersion) {
		return fmt.Errorf("oracle db version [%v] is less than 11g, can't be using transferdb tools", oracleDBVersion)
	}
	oracleCollation := false
	if common.VersionOrdinal(oracleDBVersion) >= common.VersionOrdinal(common.OracleTableColumnCollationDBVersion) {
		oracleCollation = true
	}

	// 获取配置文件待同步表列表
	exporters, err := filterCFGTable(r.Cfg, r.Oracle)
	if err != nil {
		return err
	}

	// 判断 error_log_detail 是否存在错误记录，是否可进行 CSV
	errTotals, err := meta.NewErrorLogDetailModel(r.MetaDB).CountsErrorLogBySchema(r.Ctx, &meta.ErrorLogDetail{
		DBTypeS:     common.TaskDBOracle,
		DBTypeT:     common.TaskDBMySQL,
		SchemaNameS: common.StringUPPER(r.Cfg.OracleConfig.SchemaName),
		RunMode:     common.FullO2MMode,
	})
	if errTotals > 0 || err != nil {
		return fmt.Errorf("full schema [%s] table mode [%s] task failed: %v, table [error_log_detail] exist failed error, please clear and rerunning", strings.ToUpper(r.Cfg.OracleConfig.SchemaName), common.CompareO2MMode, err)
	}

	// 判断并记录待同步表列表
	for _, tableName := range exporters {
		waitSyncMetas, err := meta.NewWaitSyncMetaModel(r.MetaDB).DetailWaitSyncMeta(r.Ctx, &meta.WaitSyncMeta{
			DBTypeS:     common.TaskDBOracle,
			DBTypeT:     common.TaskDBMySQL,
			SchemaNameS: common.StringUPPER(r.Cfg.OracleConfig.SchemaName),
			TableNameS:  tableName,
			Mode:        common.FullO2MMode,
		})
		if err != nil {
			return err
		}
		// 初始同步表全量任务为 -1 表示未进行全量初始化，初始化完成会变更
		// 全量同步完成，增量阶段，值预期都是 0
		if len(waitSyncMetas) == 0 {
			// 初始同步表全量任务为 -1 表示未进行全量初始化，初始化完成会变更
			// 全量同步完成，增量阶段，值预期都是 0
			err = meta.NewWaitSyncMetaModel(r.MetaDB).CreateWaitSyncMeta(r.Ctx, &meta.WaitSyncMeta{
				DBTypeS:        common.TaskDBOracle,
				DBTypeT:        common.TaskDBMySQL,
				SchemaNameS:    common.StringUPPER(r.Cfg.OracleConfig.SchemaName),
				TableNameS:     common.StringUPPER(tableName),
				Mode:           common.FullO2MMode,
				FullGlobalSCN:  0,
				FullSplitTimes: -1,
			})
			if err != nil {
				return err
			}
		}
	}

	// 关于全量断点恢复
	//  - 若想断点恢复，设置 enable-checkpoint true,首次一旦运行则 batch 数不能调整，
	//  - 若不想断点恢复或者重新调整 batch 数，设置 enable-checkpoint false,清理元数据表 [wait_sync_meta],重新运行全量任务
	if !r.Cfg.FullConfig.EnableCheckpoint {
		err = meta.NewFullSyncMetaModel(r.MetaDB).DeleteFullSyncMetaBySchemaSyncMode(
			r.Ctx, &meta.FullSyncMeta{
				DBTypeS:     common.TaskDBOracle,
				DBTypeT:     common.TaskDBMySQL,
				SchemaNameS: r.Cfg.OracleConfig.SchemaName,
				Mode:        common.FullO2MMode,
			})
		if err != nil {
			return err
		}
		for _, tableName := range exporters {
			err = meta.NewWaitSyncMetaModel(r.MetaDB).DeleteWaitSyncMeta(r.Ctx, &meta.WaitSyncMeta{
				DBTypeS:     common.TaskDBOracle,
				DBTypeT:     common.TaskDBMySQL,
				SchemaNameS: r.Cfg.OracleConfig.SchemaName,
				TableNameS:  tableName,
				Mode:        common.FullO2MMode,
			})
			if err != nil {
				return err
			}
			// 清理已有表数据
			if err := r.Mysql.TruncateMySQLTable(r.Cfg.MySQLConfig.SchemaName, tableName); err != nil {
				return err
			}
			// 判断并记录待同步表列表
			waitSyncMetas, err := meta.NewWaitSyncMetaModel(r.MetaDB).DetailWaitSyncMeta(r.Ctx, &meta.WaitSyncMeta{
				DBTypeS:     common.TaskDBOracle,
				DBTypeT:     common.TaskDBMySQL,
				SchemaNameS: common.StringUPPER(r.Cfg.OracleConfig.SchemaName),
				TableNameS:  tableName,
				Mode:        common.FullO2MMode,
			})
			if err != nil {
				return err
			}
			if len(waitSyncMetas) == 0 {
				// 初始同步表全量任务为 -1 表示未进行全量初始化，初始化完成会变更
				// 全量同步完成，增量阶段，值预期都是 0
				err = meta.NewWaitSyncMetaModel(r.MetaDB).CreateWaitSyncMeta(r.Ctx, &meta.WaitSyncMeta{
					DBTypeS:        common.TaskDBOracle,
					DBTypeT:        common.TaskDBMySQL,
					SchemaNameS:    common.StringUPPER(r.Cfg.OracleConfig.SchemaName),
					TableNameS:     common.StringUPPER(tableName),
					Mode:           common.FullO2MMode,
					FullGlobalSCN:  0,
					FullSplitTimes: -1,
				})
				if err != nil {
					return err
				}
			}
		}
	}

	// 获取等待同步以及未同步完成的表列表
	var (
		waitSyncTableMetas []meta.WaitSyncMeta
		waitSyncTables     []string
	)

	waitSyncDetails, err := meta.NewWaitSyncMetaModel(r.MetaDB).DetailWaitSyncMeta(r.Ctx, &meta.WaitSyncMeta{
		DBTypeS:        common.TaskDBOracle,
		DBTypeT:        common.TaskDBMySQL,
		SchemaNameS:    common.StringUPPER(r.Cfg.OracleConfig.SchemaName),
		Mode:           common.FullO2MMode,
		FullGlobalSCN:  0,
		FullSplitTimes: -1,
	})
	if err != nil {
		return err
	}
	waitSyncTableMetas = waitSyncDetails
	if len(waitSyncTableMetas) > 0 {
		for _, table := range waitSyncTableMetas {
			waitSyncTables = append(waitSyncTables, common.StringUPPER(table.TableNameS))
		}
	}

	var (
		partSyncTableMetas []meta.WaitSyncMeta
		partSyncTables     []string
	)
	partSyncDetails, err := meta.NewWaitSyncMetaModel(r.MetaDB).BatchQueryWaitSyncMeta(r.Ctx, &meta.WaitSyncMeta{
		DBTypeS:     common.TaskDBOracle,
		DBTypeT:     common.TaskDBMySQL,
		SchemaNameS: common.StringUPPER(r.Cfg.OracleConfig.SchemaName),
		Mode:        common.FullO2MMode,
	})
	if err != nil {
		return err
	}
	partSyncTableMetas = partSyncDetails
	if len(partSyncTableMetas) > 0 {
		for _, table := range partSyncTableMetas {
			partSyncTables = append(partSyncTables, common.StringUPPER(table.TableNameS))
		}
	}

	if len(waitSyncTableMetas) == 0 && len(partSyncTableMetas) == 0 {
		endTime := time.Now()
		zap.L().Info("source schema full table data finished",
			zap.String("schema", r.Cfg.OracleConfig.SchemaName),
			zap.String("cost", endTime.Sub(startTime).String()))
		return nil
	}

	// 判断未同步完成的表列表能否断点续传
	var panicTblFullSlice []string

	for _, partSyncMeta := range partSyncTableMetas {
		tableNameArray, err2 := meta.NewFullSyncMetaModel(r.MetaDB).DistinctFullSyncMetaByTableNameS(r.Ctx, &meta.FullSyncMeta{
			DBTypeS:     common.TaskDBOracle,
			DBTypeT:     common.TaskDBMySQL,
			SchemaNameS: r.Cfg.OracleConfig.SchemaName})
		if err2 != nil {
			return err2
		}

		if !common.IsContainString(tableNameArray, partSyncMeta.TableNameS) {
			panicTblFullSlice = append(panicTblFullSlice, partSyncMeta.TableNameS)
		}
	}

	if len(panicTblFullSlice) != 0 {
		endTime := time.Now()
		zap.L().Error("source schema full table data sync error",
			zap.String("schema", r.Cfg.OracleConfig.SchemaName),
			zap.String("cost", endTime.Sub(startTime).String()),
			zap.Strings("panic tables", panicTblFullSlice))
		return fmt.Errorf("checkpoint isn't consistent, please reruning [enable-checkpoint = fase]")
	}

	// 数据迁移
	// 优先存在断点的表
	// partSyncTables -> waitSyncTables
	if len(partSyncTables) > 0 {
		err = r.fullPartSyncTable(partSyncTables)
		if err != nil {
			return err
		}
	}
	if len(waitSyncTables) > 0 {
		err = r.fullWaitSyncTable(waitSyncTables, oracleCollation)
		if err != nil {
			return err
		}
	}

	endTime := time.Now()
	zap.L().Info("all full table data sync finished",
		zap.String("schema", r.Cfg.OracleConfig.SchemaName),
		zap.String("cost", endTime.Sub(startTime).String()))
	return nil
}

func (r *Migrate) fullPartSyncTable(fullPartTables []string) error {
	taskTime := time.Now()

	g := &errgroup.Group{}
	g.SetLimit(r.Cfg.FullConfig.TableThreads)

	for _, table := range fullPartTables {
		t := table
		g.Go(func() error {
			startTime := time.Now()
			fullMetas, err := meta.NewFullSyncMetaModel(r.MetaDB).DetailFullSyncMeta(r.Ctx, &meta.FullSyncMeta{
				DBTypeS:     common.TaskDBOracle,
				DBTypeT:     common.TaskDBMySQL,
				SchemaNameS: common.StringUPPER(r.Cfg.OracleConfig.SchemaName),
				TableNameS:  common.StringUPPER(t),
				Mode:        common.FullO2MMode,
			})
			if err != nil {
				return err
			}

			g1 := &errgroup.Group{}
			g1.SetLimit(r.Cfg.FullConfig.SQLThreads)
			for _, fullMeta := range fullMetas {
				m := fullMeta
				g1.Go(func() error {
					// 数据写入
					columnFields, batchResults, err := IExtractor(
						NewTable(r.Ctx, m, r.Oracle, r.Cfg.AppConfig.InsertBatchSize))
					if err != nil {
						return err
					}
					err = ITranslator(NewChunk(r.Ctx, m, r.Oracle, r.Mysql, r.MetaDB, columnFields, batchResults, r.Cfg.FullConfig.ApplyThreads, r.Cfg.AppConfig.InsertBatchSize, true))
					if err != nil {
						return err
					}
					err = IApplier(NewChunk(r.Ctx, m, r.Oracle, r.Mysql, r.MetaDB, columnFields, batchResults, r.Cfg.FullConfig.ApplyThreads, r.Cfg.AppConfig.InsertBatchSize, true))
					if err != nil {
						return err
					}
					return nil
				})
			}

			if err = g1.Wait(); err != nil {
				return err
			}

			// 更新表级别记录
			err = meta.NewWaitSyncMetaModel(r.MetaDB).ModifyWaitSyncMetaColumnFullSplitTimesZero(r.Ctx, &meta.WaitSyncMeta{
				DBTypeS:        common.TaskDBOracle,
				DBTypeT:        common.TaskDBMySQL,
				SchemaNameS:    r.Cfg.OracleConfig.SchemaName,
				TableNameS:     t,
				Mode:           common.FullO2MMode,
				FullSplitTimes: 0,
			})
			if err != nil {
				return err
			}

			zap.L().Info("source schema table data finished",
				zap.String("schema", r.Cfg.OracleConfig.SchemaName),
				zap.String("table", t),
				zap.String("cost", time.Now().Sub(startTime).String()))
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return err
	}

	zap.L().Info("source schema all table data loader finished",
		zap.String("schema", r.Cfg.OracleConfig.SchemaName),
		zap.Int("table totals", len(fullPartTables)),
		zap.String("cost", time.Now().Sub(taskTime).String()))
	return nil
}

func (r *Migrate) fullWaitSyncTable(fullWaitTables []string, oracleCollation bool) error {
	err := r.initWaitSyncTableRowID(fullWaitTables, oracleCollation)
	if err != nil {
		return err
	}
	err = r.fullPartSyncTable(fullWaitTables)
	if err != nil {
		return err
	}

	return nil
}

func (r *Migrate) initWaitSyncTableRowID(csvWaitTables []string, oracleCollation bool) error {
	startTask := time.Now()
	// 获取自定义库表名规则
	tableNameRule, err := r.getTableNameRule()
	if err != nil {
		return err
	}

	// 全量同步前，获取 SCN 以及初始化元数据表
	globalSCN, err := r.Oracle.GetOracleCurrentSnapshotSCN()
	if err != nil {
		return err
	}
	partitionTables, err := r.Oracle.GetOracleSchemaPartitionTable(r.Cfg.OracleConfig.SchemaName)
	if err != nil {
		return err
	}

	g := &errgroup.Group{}
	g.SetLimit(r.Cfg.FullConfig.TaskThreads)

	for idx, table := range csvWaitTables {
		t := table
		workerID := idx
		g.Go(func() error {
			startTime := time.Now()
			// 库名、表名规则
			var targetTableName string
			if val, ok := tableNameRule[common.StringUPPER(t)]; ok {
				targetTableName = val
			} else {
				targetTableName = common.StringUPPER(t)
			}

			sourceColumnInfo, err := r.adjustTableSelectColumn(t, oracleCollation)
			if err != nil {
				return err
			}

			var (
				isPartition string
			)
			if common.IsContainString(partitionTables, common.StringUPPER(t)) {
				isPartition = "YES"
			} else {
				isPartition = "NO"
			}

			tableRowsByStatistics, err := r.Oracle.GetOracleTableRowsByStatistics(r.Cfg.OracleConfig.SchemaName, t)
			if err != nil {
				return err
			}
			// 统计信息数据行数 0，直接全表扫
			if tableRowsByStatistics == 0 {
				zap.L().Warn("get oracle table rows",
					zap.String("schema", r.Cfg.OracleConfig.SchemaName),
					zap.String("table", t),
					zap.String("column", sourceColumnInfo),
					zap.String("where", "1 = 1"),
					zap.Int("statistics rows", tableRowsByStatistics))

				err = meta.NewCommonModel(r.MetaDB).CreateFullSyncMetaAndUpdateWaitSyncMeta(r.Ctx, &meta.FullSyncMeta{
					DBTypeS:     common.TaskDBOracle,
					DBTypeT:     common.TaskDBMySQL,
					SchemaNameS: common.StringUPPER(r.Cfg.OracleConfig.SchemaName),
					TableNameS:  common.StringUPPER(t),
					SchemaNameT: common.StringUPPER(r.Cfg.MySQLConfig.SchemaName),
					TableNameT:  common.StringUPPER(targetTableName),
					GlobalScnS:  globalSCN,
					ColumnInfoS: sourceColumnInfo,
					RowidInfoS:  "1 = 1",
					Mode:        common.FullO2MMode,
					IsPartition: isPartition,
				}, &meta.WaitSyncMeta{
					DBTypeS:        common.TaskDBOracle,
					DBTypeT:        common.TaskDBMySQL,
					SchemaNameS:    common.StringUPPER(r.Cfg.OracleConfig.SchemaName),
					TableNameS:     common.StringUPPER(t),
					Mode:           common.FullO2MMode,
					FullGlobalSCN:  globalSCN,
					FullSplitTimes: 1,
					IsPartition:    isPartition,
				})
				if err != nil {
					return err
				}
				return nil
			}

			zap.L().Info("get oracle table statistics rows",
				zap.String("schema", common.StringUPPER(r.Cfg.OracleConfig.SchemaName)),
				zap.String("table", common.StringUPPER(t)),
				zap.Int("rows", tableRowsByStatistics))

			taskName := common.StringsBuilder(common.StringUPPER(r.Cfg.OracleConfig.SchemaName), `_`, common.StringUPPER(t), `_`, `TASK`, strconv.Itoa(workerID))

			if err = r.Oracle.StartOracleChunkCreateTask(taskName); err != nil {
				return err
			}

			if err = r.Oracle.StartOracleCreateChunkByRowID(taskName, common.StringUPPER(r.Cfg.OracleConfig.SchemaName), common.StringUPPER(t), strconv.Itoa(r.Cfg.CSVConfig.Rows)); err != nil {
				return err
			}

			chunkRes, err := r.Oracle.GetOracleTableChunksByRowID(taskName)
			if err != nil {
				return err
			}

			// 判断数据是否存在
			if len(chunkRes) == 0 {
				zap.L().Warn("get oracle table rowids rows",
					zap.String("schema", common.StringUPPER(r.Cfg.OracleConfig.SchemaName)),
					zap.String("table", common.StringUPPER(t)),
					zap.String("column", sourceColumnInfo),
					zap.String("where", "1 = 1"),
					zap.Int("rowids rows", len(chunkRes)))

				err = meta.NewCommonModel(r.MetaDB).CreateFullSyncMetaAndUpdateWaitSyncMeta(r.Ctx, &meta.FullSyncMeta{
					DBTypeS:     common.TaskDBOracle,
					DBTypeT:     common.TaskDBMySQL,
					SchemaNameS: common.StringUPPER(r.Cfg.OracleConfig.SchemaName),
					TableNameS:  common.StringUPPER(t),
					SchemaNameT: common.StringUPPER(r.Cfg.MySQLConfig.SchemaName),
					TableNameT:  common.StringUPPER(targetTableName),
					GlobalScnS:  globalSCN,
					ColumnInfoS: sourceColumnInfo,
					RowidInfoS:  "1 = 1",
					Mode:        common.FullO2MMode,
					IsPartition: isPartition,
				}, &meta.WaitSyncMeta{
					DBTypeS:        common.TaskDBOracle,
					DBTypeT:        common.TaskDBMySQL,
					SchemaNameS:    common.StringUPPER(r.Cfg.OracleConfig.SchemaName),
					TableNameS:     common.StringUPPER(t),
					Mode:           common.FullO2MMode,
					FullGlobalSCN:  globalSCN,
					FullSplitTimes: 1,
					IsPartition:    isPartition,
				})
				if err != nil {
					return err
				}

				return nil
			}

			var fullMetas []meta.FullSyncMeta
			for _, res := range chunkRes {
				fullMetas = append(fullMetas, meta.FullSyncMeta{
					DBTypeS:     common.TaskDBOracle,
					DBTypeT:     common.TaskDBMySQL,
					SchemaNameS: common.StringUPPER(r.Cfg.OracleConfig.SchemaName),
					TableNameS:  common.StringUPPER(t),
					SchemaNameT: common.StringUPPER(r.Cfg.MySQLConfig.SchemaName),
					TableNameT:  common.StringUPPER(targetTableName),
					GlobalScnS:  globalSCN,
					ColumnInfoS: sourceColumnInfo,
					RowidInfoS:  res["CMD"],
					Mode:        common.FullO2MMode,
					IsPartition: isPartition,
				})
			}

			// 元数据库信息 batch 写入
			err = meta.NewFullSyncMetaModel(r.MetaDB).BatchCreateFullSyncMeta(r.Ctx, fullMetas, r.Cfg.AppConfig.InsertBatchSize)
			if err != nil {
				return err
			}

			// 更新 wait_sync_meta
			err = meta.NewWaitSyncMetaModel(r.MetaDB).UpdateWaitSyncMeta(r.Ctx, &meta.WaitSyncMeta{
				DBTypeS:        common.TaskDBOracle,
				DBTypeT:        common.TaskDBMySQL,
				SchemaNameS:    common.StringUPPER(r.Cfg.OracleConfig.SchemaName),
				TableNameS:     common.StringUPPER(t),
				Mode:           common.FullO2MMode,
				FullGlobalSCN:  globalSCN,
				FullSplitTimes: len(chunkRes),
				IsPartition:    isPartition,
			})
			if err != nil {
				return err
			}

			if err = r.Oracle.CloseOracleChunkTask(taskName); err != nil {
				return err
			}

			endTime := time.Now()
			zap.L().Info("source table init wait_sync_meta and full_sync_meta finished",
				zap.String("schema", r.Cfg.OracleConfig.SchemaName),
				zap.String("table", t),
				zap.String("cost", endTime.Sub(startTime).String()))
			return nil
		})
	}

	if err = g.Wait(); err != nil {
		return err
	}

	zap.L().Info("source schema init wait_sync_meta and full_sync_meta finished",
		zap.String("schema", r.Cfg.OracleConfig.SchemaName),
		zap.String("cost", time.Now().Sub(startTask).String()))
	return nil
}

func (r *Migrate) getTableNameRule() (map[string]string, error) {
	// 获取表名自定义规则
	tableNameRules, err := meta.NewTableNameRuleModel(r.MetaDB).DetailTableNameRule(r.Ctx, &meta.TableNameRule{
		DBTypeS:     common.TaskDBOracle,
		DBTypeT:     common.TaskDBMySQL,
		SchemaNameS: r.Cfg.OracleConfig.SchemaName,
		SchemaNameT: r.Cfg.MySQLConfig.SchemaName,
	})
	if err != nil {
		return nil, err
	}
	tableNameRuleMap := make(map[string]string)

	if len(tableNameRules) > 0 {
		for _, tr := range tableNameRules {
			tableNameRuleMap[common.StringUPPER(tr.TableNameS)] = common.StringUPPER(tr.TableNameT)
		}
	}
	return tableNameRuleMap, nil
}

func (r *Migrate) adjustTableSelectColumn(sourceTable string, oracleCollation bool) (string, error) {
	// Date/Timestamp 字段类型格式化
	// Interval Year/Day 数据字符 TO_CHAR 格式化
	columnsINFO, err := r.Oracle.GetOracleSchemaTableColumn(r.Cfg.OracleConfig.SchemaName, sourceTable, oracleCollation)
	if err != nil {
		return "", err
	}

	var columnNames []string

	for _, rowCol := range columnsINFO {
		switch strings.ToUpper(rowCol["DATA_TYPE"]) {
		// 数字
		case "NUMBER":
			columnNames = append(columnNames, rowCol["COLUMN_NAME"])
		case "DECIMAL", "DEC", "DOUBLE PRECISION", "FLOAT", "INTEGER", "INT", "REAL", "NUMERIC", "BINARY_FLOAT", "BINARY_DOUBLE", "SMALLINT":
			columnNames = append(columnNames, rowCol["COLUMN_NAME"])
		// 字符
		case "BFILE", "CHARACTER", "LONG", "NCHAR VARYING", "ROWID", "UROWID", "VARCHAR", "CHAR", "NCHAR", "NVARCHAR2", "NCLOB", "CLOB":
			columnNames = append(columnNames, rowCol["COLUMN_NAME"])
		// XMLTYPE
		case "XMLTYPE":
			columnNames = append(columnNames, fmt.Sprintf(" XMLSERIALIZE(CONTENT %s AS CLOB) AS %s", rowCol["COLUMN_NAME"], rowCol["COLUMN_NAME"]))
		// 二进制
		case "BLOB", "LONG RAW", "RAW":
			columnNames = append(columnNames, rowCol["COLUMN_NAME"])
		// 时间
		case "DATE":
			columnNames = append(columnNames, common.StringsBuilder("TO_CHAR(", rowCol["COLUMN_NAME"], ",'yyyy-MM-dd HH24:mi:ss') AS ", rowCol["COLUMN_NAME"]))
		// 默认其他类型
		default:
			if strings.Contains(rowCol["DATA_TYPE"], "INTERVAL") {
				columnNames = append(columnNames, common.StringsBuilder("TO_CHAR(", rowCol["COLUMN_NAME"], ") AS ", rowCol["COLUMN_NAME"]))
			} else if strings.Contains(rowCol["DATA_TYPE"], "TIMESTAMP") {
				dataScale, err := strconv.Atoi(rowCol["DATA_SCALE"])
				if err != nil {
					return "", fmt.Errorf("aujust oracle timestamp datatype scale [%s] strconv.Atoi failed: %v", rowCol["DATA_SCALE"], err)
				}
				if dataScale == 0 {
					columnNames = append(columnNames, common.StringsBuilder("TO_CHAR(", rowCol["COLUMN_NAME"], ",'yyyy-mm-dd hh24:mi:ss') AS ", rowCol["COLUMN_NAME"]))
				} else if dataScale < 0 && dataScale <= 6 {
					columnNames = append(columnNames, common.StringsBuilder("TO_CHAR(", rowCol["COLUMN_NAME"],
						",'yyyy-mm-dd hh24:mi:ss.ff", rowCol["DATA_SCALE"], "') AS ", rowCol["COLUMN_NAME"]))
				} else {
					columnNames = append(columnNames, common.StringsBuilder("TO_CHAR(", rowCol["COLUMN_NAME"], ",'yyyy-mm-dd hh24:mi:ss.ff6') AS ", rowCol["COLUMN_NAME"]))
				}

			} else {
				columnNames = append(columnNames, rowCol["COLUMN_NAME"])
			}
		}

	}

	return strings.Join(columnNames, ","), nil
}
