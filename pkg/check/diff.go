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
package check

import (
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jedib0t/go-pretty/v6/table"

	"github.com/wentaojin/transferdb/utils"

	"github.com/wentaojin/transferdb/service"

	"github.com/wentaojin/transferdb/pkg/reverser"

	"go.uber.org/zap"
)

type DiffWriter struct {
	SourceSchemaName string
	TargetSchemaName string
	TableName        string
	Engine           *service.Engine
	*FileMW
}

type FileMW struct {
	Mutex  sync.Mutex
	Writer io.Writer
}

func NewDiffWriter(sourceSchemaName, targetSchemaName, tableName string, engine *service.Engine, fileMW *FileMW) *DiffWriter {
	return &DiffWriter{
		SourceSchemaName: sourceSchemaName,
		TargetSchemaName: targetSchemaName,
		TableName:        tableName,
		Engine:           engine,
		FileMW:           fileMW,
	}
}

func (d *FileMW) Write(b []byte) (n int, err error) {
	d.Mutex.Lock()
	defer d.Mutex.Unlock()
	return d.Writer.Write(b)
}

// 表结构对比
// 以上游 oracle 表结构信息为基准，对比下游 MySQL 表结构
// 1、若上游存在，下游不存在，则输出记录，若上游不存在，下游存在，则默认不输出
// 2、忽略上下游不同索引名、约束名对比，只对比下游是否存在同等约束下同等字段是否存在
// 3、分区只对比分区类型、分区键、分区表达式等，不对比具体每个分区下的情况
func (d *DiffWriter) DiffOracleAndMySQLTable() error {
	startTime := time.Now()
	service.Logger.Info("check table start",
		zap.String("oracle table", fmt.Sprintf("%s.%s", d.SourceSchemaName, d.TableName)),
		zap.String("mysql table", fmt.Sprintf("%s.%s", d.TargetSchemaName, d.TableName)))

	service.Logger.Info("check table",
		zap.String("get oracle table", fmt.Sprintf("%s.%s", d.SourceSchemaName, d.TableName)),
		zap.String("get mysql table", fmt.Sprintf("%s.%s", d.TargetSchemaName, d.TableName)))
	oracleTable, err := NewOracleTableINFO(d.SourceSchemaName, d.TableName, d.Engine)
	if err != nil {
		return err
	}

	mysqlTable, mysqlVersion, err := NewMySQLTableINFO(d.TargetSchemaName, d.TableName, d.Engine)
	if err != nil {
		return err
	}

	isTiDB := false
	if strings.Contains(mysqlVersion, "TiDB") {
		isTiDB = true
	}

	// 输出格式：表结构一致不输出，只输出上下游不一致信息且输出以下游可执行 SQL 输出
	var builder strings.Builder

	// 表类型检查
	service.Logger.Info("check table",
		zap.String("table partition type check", fmt.Sprintf("%s.%s", d.SourceSchemaName, d.TableName)))

	if oracleTable.IsPartition != mysqlTable.IsPartition {
		builder.WriteString("/*\n")
		builder.WriteString(fmt.Sprintf(" oracle table type is different from mysql table type\n"))
		builder.WriteString(fmt.Sprintf(" oracle table [%s.%s] is partition type [%t]\n", d.SourceSchemaName, d.TableName, oracleTable.IsPartition))
		builder.WriteString(fmt.Sprintf(" mysql table [%s.%s] is partition type [%t]\n", d.TargetSchemaName, d.TableName, mysqlTable.IsPartition))
		builder.WriteString("*/\n")
		builder.WriteString(fmt.Sprintf("-- the above info comes from oracle table [%s.%s]\n", d.SourceSchemaName, d.TableName))
		builder.WriteString(fmt.Sprintf("-- the above info comes from mysql table [%s.%s]\n", d.TargetSchemaName, d.TableName))
		if _, err := fmt.Fprintln(d.FileMW, builder.String()); err != nil {
			return err
		}

		service.Logger.Warn("table type different",
			zap.String("oracle table", fmt.Sprintf("%s.%s", d.SourceSchemaName, d.TableName)),
			zap.String("mysql table", fmt.Sprintf("%s.%s", d.TargetSchemaName, d.TableName)),
			zap.String("msg", builder.String()))
		return nil
	}

	// 表注释检查
	service.Logger.Info("check table",
		zap.String("table comment check", fmt.Sprintf("%s.%s", d.SourceSchemaName, d.TableName)))

	if oracleTable.TableComment != mysqlTable.TableComment {
		builder.WriteString("/*\n")
		builder.WriteString(fmt.Sprintf(" oracle table comment [%s]\n", oracleTable.TableComment))
		builder.WriteString(fmt.Sprintf(" mysql table comment [%s]\n", mysqlTable.TableComment))
		builder.WriteString("*/\n")
		builder.WriteString(fmt.Sprintf("ALTER TABLE %s.%s COMMENT '%s';\n", d.TargetSchemaName, d.TableName, oracleTable.TableComment))
	}

	// 表级别字符集以及排序规则检查
	service.Logger.Info("check table",
		zap.String("table character set and collation check", fmt.Sprintf("%s.%s", d.SourceSchemaName, d.TableName)))

	if !strings.Contains(mysqlTable.TableCharacterSet, OracleUTF8CharacterSet) || !strings.Contains(mysqlTable.TableCollation, OracleCollationBin) {
		builder.WriteString("/*\n")
		builder.WriteString(fmt.Sprintf(" oracle table character set [%s], collation [%s]\n", oracleTable.TableCharacterSet, oracleTable.TableCollation))
		builder.WriteString(fmt.Sprintf(" mysql table character set [%s], collation [%s]\n", mysqlTable.TableCharacterSet, mysqlTable.TableCollation))
		builder.WriteString("*/\n")
		builder.WriteString(fmt.Sprintf("ALTER TABLE %s.%s CHARACTER SET = %s, COLLATE = %s;\n", d.TargetSchemaName, d.TableName, MySQLCharacterSet, MySQLCollation))
	}

	// 表字段级别字符集以及排序规则校验 -> 基于原表字段类型以及字符集、排序规则
	service.Logger.Info("check table",
		zap.String("table column character set and collation check", fmt.Sprintf("%s.%s", d.TargetSchemaName, d.TableName)))

	for mysqlColName, mysqlColInfo := range mysqlTable.Columns {
		if mysqlColInfo.CharacterSet != "UNKNOWN" || mysqlColInfo.Collation != "UNKNOWN" {
			if mysqlColInfo.CharacterSet != strings.ToUpper(MySQLCharacterSet) || mysqlColInfo.Collation != strings.ToUpper(MySQLCollation) {
				builder.WriteString("/*\n")
				builder.WriteString(fmt.Sprintf(" mysql column character set and collation check, generate created sql\n"))
				builder.WriteString("*/\n")
				builder.WriteString(fmt.Sprintf("ALTER TABLE %s.%s MODIFY %s %s(%s) CHARACTER SET %s COLLATE %s;\n",
					d.TargetSchemaName, d.TableName, mysqlColName, mysqlColInfo.DataType, mysqlColInfo.DataLength, MySQLCharacterSet, MySQLCollation))
			}
		}
	}

	// 表约束、索引以及分区检查
	service.Logger.Info("check table",
		zap.String("table pk and uk constraint check", fmt.Sprintf("%s.%s", d.SourceSchemaName, d.TableName)),
		zap.String("oracle struct", oracleTable.String(PUConstraintJSON)),
		zap.String("mysql struct", mysqlTable.String(PUConstraintJSON)))

	// 函数 utils.DiffStructArray 都忽略 structA 空，但 structB 存在情况
	addDiffPU, _, isOK := utils.DiffStructArray(oracleTable.PUConstraints, mysqlTable.PUConstraints)
	if len(addDiffPU) != 0 && !isOK {
		builder.WriteString("/*\n")
		builder.WriteString(" oracle table primary key and unique key\n")
		builder.WriteString(" mysql table primary key and unique key\n")
		builder.WriteString("*/\n")
		for _, pu := range addDiffPU {
			value, ok := pu.(ConstraintPUKey)
			if ok {
				switch value.ConstraintType {
				case "PK":
					builder.WriteString(fmt.Sprintf("ALTER TABLE %s.%s ADD PRIMARY KEY(%s);\n", d.TargetSchemaName, d.TableName, value.ConstraintColumn))
					continue
				case "UK":
					builder.WriteString(fmt.Sprintf("ALTER TABLE %s.%s ADD UNIQUE(%s);\n", d.TargetSchemaName, d.TableName, value.ConstraintColumn))
					continue
				default:
					return fmt.Errorf("table constraint primary and unique key diff failed: not support type [%s]", value.ConstraintType)
				}
			}
			return fmt.Errorf("table constraint primary and unique key assert ConstraintPUKey failed")
		}
	}

	// TiDB 版本排除外键以及检查约束
	if !isTiDB {
		service.Logger.Info("check table",
			zap.String("table fk constraint check", fmt.Sprintf("%s.%s", d.SourceSchemaName, d.TableName)),
			zap.String("oracle struct", oracleTable.String(FKConstraintJSON)),
			zap.String("mysql struct", mysqlTable.String(FKConstraintJSON)))

		addDiffFK, _, isOK := utils.DiffStructArray(oracleTable.ForeignConstraints, mysqlTable.ForeignConstraints)
		if len(addDiffFK) != 0 && !isOK {
			builder.WriteString("/*\n")
			builder.WriteString(" oracle table foreign key\n")
			builder.WriteString(" mysql table foreign key\n")
			builder.WriteString("*/\n")

			for _, fk := range addDiffFK {
				value, ok := fk.(ConstraintForeign)
				if ok {
					builder.WriteString(fmt.Sprintf("ALTER TABLE %s.%s ADD FOREIGN KEY(%s) REFERENCES %s.%s(%s）ON DELETE %s;\n", d.TargetSchemaName, d.TableName, value.ColumnName, d.TargetSchemaName, value.ReferencedTableName, value.ReferencedColumnName, value.DeleteRule))
					continue
				}
				return fmt.Errorf("table constraint foreign key assert ConstraintForeign failed")
			}
		}

		var dbVersion string
		if strings.Contains(mysqlVersion, MySQLVersionDelimiter) {
			dbVersion = strings.Split(mysqlVersion, MySQLVersionDelimiter)[0]
		} else {
			dbVersion = mysqlVersion
		}
		if utils.VersionOrdinal(dbVersion) > utils.VersionOrdinal(MySQLCheckConsVersion) {
			service.Logger.Info("check table",
				zap.String("table ck constraint check", fmt.Sprintf("%s.%s", d.SourceSchemaName, d.TableName)),
				zap.String("oracle struct", oracleTable.String(CKConstraintJSON)),
				zap.String("mysql struct", mysqlTable.String(CKConstraintJSON)))

			addDiffCK, _, isOK := utils.DiffStructArray(oracleTable.CheckConstraints, mysqlTable.CheckConstraints)
			if len(addDiffCK) != 0 && !isOK {
				builder.WriteString("/*\n")
				builder.WriteString(" oracle table check key\n")
				builder.WriteString(" mysql table check key\n")
				builder.WriteString("*/\n")
				for _, ck := range addDiffCK {
					value, ok := ck.(ConstraintCheck)
					if ok {
						builder.WriteString(fmt.Sprintf("ALTER TABLE %s.%s ADD CONSTRAINT %s CHECK(%s);\n",
							d.TargetSchemaName, d.TableName, fmt.Sprintf("%s_check_key", d.TableName), value.ConstraintExpression))
						continue
					}
					return fmt.Errorf("table constraint check key assert ConstraintCheck failed")
				}
			}
		}
	}

	service.Logger.Info("check table",
		zap.String("table indexes check", fmt.Sprintf("%s.%s", d.SourceSchemaName, d.TableName)),
		zap.String("oracle struct", oracleTable.String(IndexJSON)),
		zap.String("mysql struct", mysqlTable.String(IndexJSON)))

	var createIndexSQL []string

	addDiffIndex, _, isOK := utils.DiffStructArray(oracleTable.Indexes, mysqlTable.Indexes)
	if len(addDiffIndex) != 0 && !isOK {
		for _, idx := range addDiffIndex {
			value, ok := idx.(Index)
			if ok {
				if value.Uniqueness == "NONUNIQUE" && value.IndexType == "NORMAL" {
					// 考虑 MySQL 索引类型 BTREE，额外判断处理
					var equalArray []interface{}
					for _, mysqlIndexInfo := range mysqlTable.Indexes {
						if reflect.DeepEqual(value.IndexInfo, mysqlIndexInfo.IndexInfo) {
							equalArray = append(equalArray, value.IndexInfo)
						}
					}
					if len(equalArray) == 0 {
						createIndexSQL = append(createIndexSQL, fmt.Sprintf("CREATE INDEX %s ON %s.%s(%s);\n",
							fmt.Sprintf("idx_%s", strings.ReplaceAll(value.IndexColumn, ",", "_")), d.TargetSchemaName, d.TableName, value.IndexColumn))
					}
					continue
				}
				if value.Uniqueness == "UNIQUE" && value.IndexType == "NORMAL" {
					// 考虑 MySQL 索引类型 BTREE，额外判断处理
					var equalArray []interface{}
					for _, mysqlIndexInfo := range mysqlTable.Indexes {
						if reflect.DeepEqual(value.IndexInfo, mysqlIndexInfo.IndexInfo) {
							equalArray = append(equalArray, value.IndexInfo)
						}
					}
					if len(equalArray) == 0 {
						createIndexSQL = append(createIndexSQL, fmt.Sprintf("CREATE UNIQUE INDEX %s ON %s.%s(%s);\n",
							fmt.Sprintf("idx_%s_unique", strings.ReplaceAll(value.IndexColumn, ",", "_")), d.TargetSchemaName, d.TableName, value.IndexColumn))
					}
					continue
				}
				if value.Uniqueness == "NONUNIQUE" && value.IndexType == "BITMAP" {
					createIndexSQL = append(createIndexSQL, fmt.Sprintf("CREATE BITMAP INDEX %s ON %s.%s(%s);\n",
						fmt.Sprintf("idx_%s", strings.ReplaceAll(value.IndexColumn, ",", "_")), d.TargetSchemaName, d.TableName, value.IndexColumn))
					continue
				}
				if value.Uniqueness == "NONUNIQUE" && value.IndexType == "FUNCTION-BASED NORMAL" {
					createIndexSQL = append(createIndexSQL, fmt.Sprintf("CREATE INDEX %s ON %s.%s(%s);\n",
						fmt.Sprintf("idx_%s", strings.ReplaceAll(value.IndexColumn, ",", "_")), d.TargetSchemaName, d.TableName, value.ColumnExpress))
					continue
				}
			}
			return fmt.Errorf("table index assert Index failed")
		}
	}

	if len(createIndexSQL) != 0 {
		builder.WriteString("/*\n")
		builder.WriteString(" oracle table indexes\n")
		builder.WriteString(" mysql table indexes\n")
		builder.WriteString("*/\n")
		for _, indexSQL := range createIndexSQL {
			builder.WriteString(indexSQL)
		}
	}

	if mysqlTable.IsPartition && oracleTable.IsPartition {
		service.Logger.Info("check table",
			zap.String("table partition check", fmt.Sprintf("%s.%s", d.SourceSchemaName, d.TableName)),
			zap.String("oracle struct", oracleTable.String(PartitionJSON)),
			zap.String("mysql struct", mysqlTable.String(PartitionJSON)))

		addDiffParts, _, isOK := utils.DiffStructArray(oracleTable.Partitions, mysqlTable.Partitions)
		if len(addDiffParts) != 0 && !isOK {
			builder.WriteString("/*\n")
			builder.WriteString(" oracle table partitions\n")
			builder.WriteString(" mysql table partitions\n")
			builder.WriteString("*/\n")
			builder.WriteString("-- oracle partition info exist, mysql partition isn't exist, please manual modify\n")

			for _, part := range addDiffParts {
				value, ok := part.(Partition)
				if ok {
					partJSON, err := json.Marshal(value)
					if err != nil {
						return err
					}
					builder.WriteString(fmt.Sprintf("# oracle partition info: %s, ", partJSON))
					continue
				}
				return fmt.Errorf("table paritions assert Partition failed")
			}
		}
	}

	// 表字段检查
	// 注释格式化
	var (
		diffColumnMsgs    []string
		createColumnMetas []string
		tableRowArr       []table.Row
	)

	service.Logger.Info("check table",
		zap.String("table column info check", fmt.Sprintf("%s.%s", d.SourceSchemaName, d.TableName)))

	for oracleColName, oracleColInfo := range oracleTable.Columns {
		mysqlColInfo, ok := mysqlTable.Columns[oracleColName]
		if ok {
			diffColumnMsg, tableRows, err := OracleTableMapRuleCheck(
				d.SourceSchemaName,
				d.TargetSchemaName,
				d.TableName,
				oracleColName,
				oracleColInfo,
				mysqlColInfo)
			if err != nil {
				return err
			}
			if diffColumnMsg != "" && len(tableRows) != 0 {
				diffColumnMsgs = append(diffColumnMsgs, diffColumnMsg)
				tableRowArr = append(tableRowArr, tableRows...)
			}
			continue
		}
		columnMeta, err := reverser.ReverseOracleTableColumnMapRule(
			d.SourceSchemaName,
			d.TableName,
			oracleColName,
			oracleColInfo.DataType,
			oracleColInfo.NULLABLE,
			oracleColInfo.Comment,
			oracleColInfo.DataDefault,
			oracleColInfo.DataScale,
			oracleColInfo.DataPrecision,
			oracleColInfo.DataLength,
			[]reverser.ColumnType{})
		if err != nil {
			return err
		}
		if columnMeta != "" {
			createColumnMetas = append(createColumnMetas, columnMeta)
		}
	}

	if len(tableRowArr) != 0 && len(diffColumnMsgs) != 0 {
		service.Logger.Info("check table",
			zap.String("table column info check, generate fixed sql", fmt.Sprintf("%s.%s", d.SourceSchemaName, d.TableName)),
			zap.String("oracle struct", oracleTable.String(ColumnsJSON)),
			zap.String("mysql struct", mysqlTable.String(ColumnsJSON)))

		textTable := table.NewWriter()
		textTable.SetStyle(table.StyleLight)
		textTable.AppendHeader(table.Row{"Column", "ORACLE", "MySQL", "Suggest"})

		builder.WriteString("/*\n")
		builder.WriteString(fmt.Sprintf(" oracle table columns info is different from mysql\n"))
		builder.WriteString(fmt.Sprintf("%s\n", textTable.Render()))
		builder.WriteString("*/\n")
		builder.WriteString(fmt.Sprintf("-- oracle table columns info is different from mysql, generate fixed sql\n"))
		for _, diffColMsg := range diffColumnMsgs {
			builder.WriteString(diffColMsg)
		}
	}

	if len(createColumnMetas) != 0 {
		service.Logger.Info("check table",
			zap.String("table column info check, generate created sql", fmt.Sprintf("%s.%s", d.SourceSchemaName, d.TableName)),
			zap.String("oracle struct", oracleTable.String(ColumnsJSON)),
			zap.String("mysql struct", mysqlTable.String(ColumnsJSON)))

		builder.WriteString("/*\n")
		builder.WriteString(fmt.Sprintf(" oracle table columns info isn't exist in mysql, generate created sql\n"))
		builder.WriteString("*/\n")
		for _, columnMeta := range createColumnMetas {
			builder.WriteString(fmt.Sprintf("ALTER TABLE %s.%s ADD COLUMN %s;\n",
				d.TargetSchemaName, d.TableName, columnMeta))
		}
	}

	// diff 记录不为空
	if builder.String() != "" {
		builder.WriteString(fmt.Sprintf("-- the above info comes from oracle table [%s.%s]\n", d.SourceSchemaName, d.TableName))
		builder.WriteString(fmt.Sprintf("-- the above info comes from mysql table [%s.%s]\n", d.TargetSchemaName, d.TableName))
		if _, err := fmt.Fprintln(d.FileMW, builder.String()); err != nil {
			return err
		}
	}

	endTime := time.Now()
	service.Logger.Info("check table finished",
		zap.String("oracle table", fmt.Sprintf("%s.%s", d.SourceSchemaName, d.TableName)),
		zap.String("mysql table", fmt.Sprintf("%s.%s", d.TargetSchemaName, d.TableName)),
		zap.String("cost", endTime.Sub(startTime).String()))
	return nil
}

func OracleTableMapRuleCheck(
	sourceSchema, targetSchema, tableName, columnName string,
	oracleColInfo, mysqlColInfo Column) (string, []table.Row, error) {
	var tableRows []table.Row

	// 字段精度类型转换
	oracleDataLength, err := strconv.Atoi(oracleColInfo.DataLength)
	if err != nil {
		return "", nil, fmt.Errorf("oracle schema [%s] table [%s] column data_length string to int failed: %v", sourceSchema, tableName, err)
	}
	oracleDataPrecision, err := strconv.Atoi(oracleColInfo.DataPrecision)
	if err != nil {
		return "", nil, fmt.Errorf("oracle schema [%s] table [%s] column data_precision string to int failed: %v", sourceSchema, tableName, err)
	}
	oracleDataScale, err := strconv.Atoi(oracleColInfo.DataScale)
	if err != nil {
		return "", nil, fmt.Errorf("oracle schema [%s] table [%s] column data_scale string to int failed: %v", sourceSchema, tableName, err)
	}

	mysqlDataLength, err := strconv.Atoi(mysqlColInfo.DataLength)
	if err != nil {
		return "", nil, fmt.Errorf("mysql schema table [%s.%s] column data_length string to int failed: %v",
			targetSchema, tableName, err)
	}
	mysqlDataPrecision, err := strconv.Atoi(mysqlColInfo.DataPrecision)
	if err != nil {
		return "", nil, fmt.Errorf("mysql schema table [%s.%s] reverser column data_precision string to int failed: %v", targetSchema, tableName, err)
	}
	mysqlDataScale, err := strconv.Atoi(mysqlColInfo.DataScale)
	if err != nil {
		return "", nil, fmt.Errorf("mysql schema table [%s.%s] reverser column data_scale string to int failed: %v", targetSchema, tableName, err)
	}
	mysqlDatetimePrecision, err := strconv.Atoi(mysqlColInfo.DatetimePrecision)
	if err != nil {
		return "", nil, fmt.Errorf("mysql schema table [%s.%s] reverser column datetime_precision string to int failed: %v", targetSchema, tableName, err)
	}
	// 字段默认值、注释判断
	mysqlDataType := strings.ToUpper(mysqlColInfo.DataType)
	oracleDataType := strings.ToUpper(oracleColInfo.DataType)
	var (
		fixedMsg             string
		oracleColumnCharUsed string
	)

	if oracleColInfo.CharUsed == "C" {
		oracleColumnCharUsed = "char"
	} else {
		oracleColumnCharUsed = "bytes"
	}

	oracleColMeta := generateColumnNullCommentDefaultMeta(oracleColInfo.NULLABLE, oracleColInfo.Comment, oracleColInfo.DataDefault)
	mysqlColMeta := generateColumnNullCommentDefaultMeta(mysqlColInfo.NULLABLE, mysqlColInfo.Comment, mysqlColInfo.DataDefault)

	// 字段类型判断
	// CHARACTER SET %s COLLATE %s（OnLy 作用字符类型）
	switch oracleDataType {
	// 数字
	case "NUMBER":
		switch {
		case oracleDataScale > 0:
			if mysqlDataType == "DECIMAL" && oracleDataPrecision == mysqlDataPrecision && oracleDataScale == mysqlDataScale && oracleColMeta == mysqlColMeta {
				return "", nil, nil
			}

			tableRows = append(tableRows, table.Row{columnName,
				fmt.Sprintf("NUMBER(%d,%d) %s", oracleDataPrecision, oracleDataScale, oracleColMeta),
				fmt.Sprintf("%s(%d,%d) %s", mysqlDataType, mysqlDataPrecision, mysqlDataScale, mysqlColMeta),
				fmt.Sprintf("DECIMAL(%d,%d) %s", oracleDataPrecision, oracleDataScale, oracleColMeta)})

			fixedMsg = fmt.Sprintf("ALTER TABLE %s.%s MODIFY COLUMN %s %s %s;\n",
				targetSchema,
				tableName,
				columnName,
				fmt.Sprintf("DECIMAL(%d,%d)", oracleDataPrecision, oracleDataScale),
				oracleColMeta,
			)
		case oracleDataScale == 0:
			switch {
			case oracleDataPrecision == 0 && oracleDataScale == 0:
				// MySQL column type  NUMERIC would convert to DECIMAL(11,0)
				// buildInColumnType = "NUMERIC"
				if mysqlDataType == "DECIMAL" && mysqlDataPrecision == 11 && mysqlDataScale == oracleDataScale && oracleColMeta == mysqlColMeta {
					return "", nil, nil
				}

				tableRows = append(tableRows, table.Row{columnName,
					fmt.Sprintf("NUMBER %s", oracleColMeta),
					fmt.Sprintf("%s(%d,%d) %s", mysqlDataType, mysqlDataPrecision, mysqlDataScale, mysqlColMeta),
					fmt.Sprintf("DECIMAL(11,0) %s", oracleColMeta)})

				fixedMsg = fmt.Sprintf("ALTER TABLE %s.%s MODIFY COLUMN %s %s %s;\n",
					targetSchema,
					tableName,
					columnName,
					"DECIMAL(11,0)",
					oracleColMeta,
				)
			case oracleDataPrecision >= 1 && oracleDataPrecision < 3:
				if mysqlDataType == "TINYINT" && mysqlDataPrecision >= 3 && mysqlDataScale == oracleDataScale && oracleColMeta == mysqlColMeta {
					return "", nil, nil
				}

				tableRows = append(tableRows, table.Row{columnName,
					fmt.Sprintf("NUMBER(%d) %s", oracleDataPrecision, oracleColMeta),
					fmt.Sprintf("%s(%d) %s", mysqlDataType, mysqlDataPrecision, mysqlColMeta),
					fmt.Sprintf("TINYINT %s", oracleColMeta)})

				fixedMsg = fmt.Sprintf("ALTER TABLE %s.%s MODIFY COLUMN %s %s %s;\n",
					targetSchema,
					tableName,
					columnName,
					"TINYINT",
					oracleColMeta,
				)
			case oracleDataPrecision >= 3 && oracleDataPrecision < 5:
				if mysqlDataType == "SMALLINT" && mysqlDataPrecision >= 5 && mysqlDataScale == oracleDataScale && oracleColMeta == mysqlColMeta {
					return "", nil, nil
				}
				tableRows = append(tableRows, table.Row{
					columnName,
					fmt.Sprintf("NUMBER(%d) %s", oracleDataPrecision, oracleColMeta),
					fmt.Sprintf("%s(%d) %s", mysqlDataType, mysqlDataPrecision, mysqlColMeta),
					fmt.Sprintf("SMALLINT %s", oracleColMeta)})

				fixedMsg = fmt.Sprintf("ALTER TABLE %s.%s MODIFY COLUMN %s %s %s;\n",
					targetSchema,
					tableName,
					columnName,
					"SMALLINT",
					oracleColMeta,
				)
			case oracleDataPrecision >= 5 && oracleDataPrecision < 9:
				if mysqlDataType == "INT" && mysqlDataPrecision >= 9 && mysqlDataScale == oracleDataScale && oracleColMeta == mysqlColMeta {
					return "", nil, nil
				}
				tableRows = append(tableRows, table.Row{
					columnName,
					fmt.Sprintf("NUMBER(%d) %s", oracleDataPrecision, oracleColMeta),
					fmt.Sprintf("%s(%d) %s", mysqlDataType, mysqlDataPrecision, mysqlColMeta),
					fmt.Sprintf("INT %s", oracleColMeta)})

				fixedMsg = fmt.Sprintf("ALTER TABLE %s.%s MODIFY COLUMN %s %s %s;\n",
					targetSchema,
					tableName,
					columnName,
					"INT",
					oracleColMeta,
				)
			case oracleDataPrecision >= 9 && oracleDataPrecision < 19:
				if mysqlDataType == "BIGINT" && mysqlDataPrecision >= 19 && mysqlDataScale == oracleDataScale && oracleColMeta == mysqlColMeta {
					return "", nil, nil
				}
				tableRows = append(tableRows, table.Row{
					columnName,
					fmt.Sprintf("NUMBER(%d) %s", oracleDataPrecision, oracleColMeta),
					fmt.Sprintf("%s(%d) %s", mysqlDataType, mysqlDataPrecision, mysqlColMeta),
					fmt.Sprintf("BIGINT %s", oracleColMeta)})

				fixedMsg = fmt.Sprintf("ALTER TABLE %s.%s MODIFY COLUMN %s %s %s;\n",
					targetSchema,
					tableName,
					columnName,
					"BIGINT",
					oracleColMeta,
				)
			case oracleDataPrecision >= 19 && oracleDataPrecision <= 38:
				if mysqlDataType == "DECIMAL" && mysqlDataPrecision >= 19 && mysqlDataPrecision <= 38 && mysqlDataScale == oracleDataScale && oracleColMeta == mysqlColMeta {
					return "", nil, nil
				}
				tableRows = append(tableRows, table.Row{
					columnName,
					fmt.Sprintf("NUMBER(%d) %s", oracleDataPrecision, oracleColMeta),
					fmt.Sprintf("%s(%d) %s", mysqlDataType, mysqlDataPrecision, mysqlColMeta),
					fmt.Sprintf("DECIMAL(%d) %s", oracleDataPrecision, oracleColMeta)})

				fixedMsg = fmt.Sprintf("ALTER TABLE %s.%s MODIFY COLUMN %s %s %s;\n",
					targetSchema,
					tableName,
					columnName,
					fmt.Sprintf("DECIMAL(%d)", oracleDataPrecision),
					oracleColMeta,
				)
			default:
				if mysqlDataType == "DECIMAL" && mysqlDataPrecision == oracleDataPrecision && mysqlDataScale == 4 && oracleColMeta == mysqlColMeta {
					return "", nil, nil
				}
				tableRows = append(tableRows, table.Row{
					columnName,
					fmt.Sprintf("NUMBER(%d) %s", oracleDataPrecision, oracleColMeta),
					fmt.Sprintf("%s(%d,%d) %s", mysqlDataType, mysqlDataPrecision, mysqlDataScale, mysqlColMeta),
					fmt.Sprintf("DECIMAL(%d,4) %s", oracleDataPrecision, oracleColMeta)})

				fixedMsg = fmt.Sprintf("ALTER TABLE %s.%s MODIFY COLUMN %s %s %s;\n",
					targetSchema,
					tableName,
					columnName,
					fmt.Sprintf("DECIMAL(%d,4)", oracleDataPrecision),
					oracleColMeta,
				)
			}
		}
		return fixedMsg, tableRows, nil
	case "DECIMAL":
		switch {
		case oracleDataScale == 0 && oracleDataPrecision == 0:
			if mysqlDataType == "DECIMAL" && mysqlDataPrecision == 10 && mysqlDataScale == oracleDataScale && oracleColMeta == mysqlColMeta {
				return "", nil, nil
			}
			tableRows = append(tableRows, table.Row{
				columnName,
				fmt.Sprintf("DECIMAL %s", oracleColMeta),
				fmt.Sprintf("%s(%d,%d) %s", mysqlDataType, mysqlDataPrecision, mysqlDataScale, mysqlColMeta),
				fmt.Sprintf("DECIMAL %s", oracleColMeta)})

			fixedMsg = fmt.Sprintf("ALTER TABLE %s.%s MODIFY COLUMN %s %s %s;\n",
				targetSchema,
				tableName,
				columnName,
				"DECIMAL",
				oracleColMeta,
			)
			return fixedMsg, tableRows, nil
		default:
			if mysqlDataType == "DECIMAL" && mysqlDataPrecision == oracleDataPrecision && mysqlDataScale == oracleDataScale && oracleColMeta == mysqlColMeta {
				return "", nil, nil
			}
			tableRows = append(tableRows, table.Row{
				columnName,
				fmt.Sprintf("DECIMAL(%d,%d) %s", oracleDataPrecision, oracleDataScale, oracleColMeta),
				fmt.Sprintf("%s(%d,%d) %s", mysqlDataType, mysqlDataPrecision, mysqlDataScale, mysqlColMeta),
				fmt.Sprintf("DECIMAL(%d,%d) %s", oracleDataPrecision, oracleDataScale, oracleColMeta)})

			fixedMsg = fmt.Sprintf("ALTER TABLE %s.%s MODIFY COLUMN %s %s %s;\n",
				targetSchema,
				tableName,
				columnName,
				fmt.Sprintf("DECIMAL(%d,%d)", oracleDataPrecision, oracleDataScale),
				oracleColMeta,
			)
			return fixedMsg, tableRows, nil
		}
	case "DEC":
		switch {
		case oracleDataScale == 0 && oracleDataPrecision == 0:
			if mysqlDataType == "DECIMAL" && mysqlDataPrecision == 10 && mysqlDataScale == oracleDataScale && oracleColMeta == mysqlColMeta {
				return "", nil, nil
			}
			tableRows = append(tableRows, table.Row{
				columnName,
				fmt.Sprintf("DECIMAL %s", oracleColMeta),
				fmt.Sprintf("%s(%d,%d) %s", mysqlDataType, mysqlDataPrecision, mysqlDataScale, mysqlColMeta),
				fmt.Sprintf("DECIMAL %s", oracleColMeta)})

			fixedMsg = fmt.Sprintf("ALTER TABLE %s.%s MODIFY COLUMN %s %s %s;\n",
				targetSchema,
				tableName,
				columnName,
				"DECIMAL",
				oracleColMeta,
			)
			return fixedMsg, tableRows, nil
		default:
			if mysqlDataType == "DECIMAL" && mysqlDataPrecision == oracleDataPrecision && mysqlDataScale == oracleDataScale && oracleColMeta == mysqlColMeta {
				return "", nil, nil
			}
			tableRows = append(tableRows, table.Row{
				columnName,
				fmt.Sprintf("DECIMAL(%d,%d) %s", oracleDataPrecision, oracleDataScale, oracleColMeta),
				fmt.Sprintf("%s(%d,%d) %s", mysqlDataType, mysqlDataPrecision, mysqlDataScale, mysqlColMeta),
				fmt.Sprintf("DECIMAL(%d,%d) %s", oracleDataPrecision, oracleDataScale, oracleColMeta)})

			fixedMsg = fmt.Sprintf("ALTER TABLE %s.%s MODIFY COLUMN %s %s %s;\n",
				targetSchema,
				tableName,
				columnName,
				fmt.Sprintf("DECIMAL(%d,%d)", oracleDataPrecision, oracleDataScale),
				oracleColMeta,
			)
			return fixedMsg, tableRows, nil
		}
	case "DOUBLE PRECISION":
		if mysqlDataType == "DOUBLE" && mysqlDataPrecision == 22 && mysqlDataScale == 0 && oracleColMeta == mysqlColMeta {
			return "", nil, nil
		}
		tableRows = append(tableRows, table.Row{
			columnName,
			fmt.Sprintf("DOUBLE PRECISION %s", oracleColMeta),
			fmt.Sprintf("%s(%d,%d) %s", mysqlDataType, mysqlDataPrecision, mysqlDataScale, mysqlColMeta),
			fmt.Sprintf("DOUBLE PRECISION %s", oracleColMeta)})

		fixedMsg = fmt.Sprintf("ALTER TABLE %s.%s MODIFY COLUMN %s %s %s;\n",
			targetSchema,
			tableName,
			columnName,
			"DOUBLE PRECISION",
			oracleColMeta,
		)
		return fixedMsg, tableRows, nil
	case "FLOAT":
		if oracleDataPrecision == 0 {
			if mysqlDataType == "FLOAT" && mysqlDataPrecision == 12 && mysqlDataScale == 0 && oracleColMeta == mysqlColMeta {
				return "", nil, nil
			}
			tableRows = append(tableRows, table.Row{
				columnName,
				fmt.Sprintf("FLOAT %s", oracleColMeta),
				fmt.Sprintf("%s(%d,%d) %s", mysqlDataType, mysqlDataPrecision, mysqlDataScale, mysqlColMeta),
				fmt.Sprintf("FLOAT %s", oracleColMeta)})

			fixedMsg = fmt.Sprintf("ALTER TABLE %s.%s MODIFY COLUMN %s %s %s;\n",
				targetSchema,
				tableName,
				columnName,
				"FLOAT",
				oracleColMeta,
			)
			return fixedMsg, tableRows, nil
		}
		if mysqlDataType == "DOUBLE" && mysqlDataPrecision == 22 && mysqlDataScale == 0 && oracleColMeta == mysqlColMeta {
			return "", nil, nil
		}
		tableRows = append(tableRows, table.Row{
			columnName,
			fmt.Sprintf("FLOAT %s", oracleColMeta),
			fmt.Sprintf("%s(%d,%d) %s", mysqlDataType, mysqlDataPrecision, mysqlDataScale, mysqlColMeta),
			fmt.Sprintf("DOUBLE %s", oracleColMeta)})

		fixedMsg = fmt.Sprintf("ALTER TABLE %s.%s MODIFY COLUMN %s %s %s;\n",
			targetSchema,
			tableName,
			columnName,
			"DOUBLE",
			oracleColMeta,
		)
		return fixedMsg, tableRows, nil
	case "INTEGER":
		if mysqlDataType == "INT" && mysqlDataPrecision >= 10 && mysqlDataScale == 0 && oracleColMeta == mysqlColMeta {
			return "", nil, nil
		}
		tableRows = append(tableRows, table.Row{
			columnName,
			fmt.Sprintf("INTEGER %s", oracleColMeta),
			fmt.Sprintf("%s(%d,%d) %s", mysqlDataType, mysqlDataPrecision, mysqlDataScale, mysqlColMeta),
			fmt.Sprintf("INT %s", oracleColMeta)})

		fixedMsg = fmt.Sprintf("ALTER TABLE %s.%s MODIFY COLUMN %s %s %s;\n",
			targetSchema,
			tableName,
			columnName,
			"INT",
			oracleColMeta,
		)
		return fixedMsg, tableRows, nil
	case "INT":
		if mysqlDataType == "INT" && mysqlDataPrecision >= 10 && mysqlDataScale == 0 && oracleColMeta == mysqlColMeta {
			return "", nil, nil
		}
		tableRows = append(tableRows, table.Row{
			columnName,
			fmt.Sprintf("INT %s", oracleColMeta),
			fmt.Sprintf("%s(%d,%d) %s", mysqlDataType, mysqlDataPrecision, mysqlDataScale, mysqlColMeta),
			fmt.Sprintf("INT %s", oracleColMeta)})

		fixedMsg = fmt.Sprintf("ALTER TABLE %s.%s MODIFY COLUMN %s %s %s;\n",
			targetSchema,
			tableName,
			columnName,
			"INT",
			oracleColMeta,
		)
		return fixedMsg, tableRows, nil
	case "REAL":
		if mysqlDataType == "DOUBLE" && mysqlDataPrecision == 22 && mysqlDataScale == 0 && oracleColMeta == mysqlColMeta {
			return "", nil, nil
		}
		tableRows = append(tableRows, table.Row{
			columnName,
			fmt.Sprintf("REAL %s", oracleColMeta),
			fmt.Sprintf("%s(%d,%d) %s", mysqlDataType, mysqlDataPrecision, mysqlDataScale, mysqlColMeta),
			fmt.Sprintf("DOUBLE %s", oracleColMeta)})

		fixedMsg = fmt.Sprintf("ALTER TABLE %s.%s MODIFY COLUMN %s %s %s;\n",
			targetSchema,
			tableName,
			columnName,
			"DOUBLE",
			oracleColMeta,
		)
		return fixedMsg, tableRows, nil
	case "NUMERIC":
		if mysqlDataType == "DECIMAL" && mysqlDataPrecision == oracleDataPrecision && mysqlDataScale == oracleDataScale && oracleColMeta == mysqlColMeta {
			return "", nil, nil
		}
		tableRows = append(tableRows, table.Row{
			columnName,
			fmt.Sprintf("NUMERIC(%d,%d) %s", oracleDataPrecision, oracleDataScale, oracleColMeta),
			fmt.Sprintf("%s(%d,%d) %s", mysqlDataType, mysqlDataPrecision, mysqlDataScale, mysqlColMeta),
			fmt.Sprintf("DECIMAL(%d,%d) %s", oracleDataPrecision, oracleDataScale, oracleColMeta)})

		fixedMsg = fmt.Sprintf("ALTER TABLE %s.%s MODIFY COLUMN %s %s %s;\n",
			targetSchema,
			tableName,
			columnName,
			fmt.Sprintf("DECIMAL(%d,%d)", oracleDataPrecision, oracleDataScale),
			oracleColMeta,
		)
		return fixedMsg, tableRows, nil
	case "BINARY_FLOAT":
		if mysqlDataType == "DOUBLE" && mysqlDataPrecision == 22 && mysqlDataScale == 0 && oracleColMeta == mysqlColMeta {
			return "", nil, nil
		}
		tableRows = append(tableRows, table.Row{
			columnName,
			fmt.Sprintf("BINARY_FLOAT %s", oracleColMeta),
			fmt.Sprintf("%s(%d,%d) %s", mysqlDataType, mysqlDataPrecision, mysqlDataScale, mysqlColMeta),
			fmt.Sprintf("DOUBLE %s", oracleColMeta)})

		fixedMsg = fmt.Sprintf("ALTER TABLE %s.%s MODIFY COLUMN %s %s %s;\n",
			targetSchema,
			tableName,
			columnName,
			"DOUBLE",
			oracleColMeta,
		)
		return fixedMsg, tableRows, nil
	case "BINARY_DOUBLE":
		if mysqlDataType == "DOUBLE" && mysqlDataPrecision == 22 && mysqlDataScale == 0 && oracleColMeta == mysqlColMeta {
			return "", nil, nil
		}
		tableRows = append(tableRows, table.Row{
			columnName,
			fmt.Sprintf("BINARY_DOUBLE %s", oracleColMeta),
			fmt.Sprintf("%s(%d,%d) %s", mysqlDataType, mysqlDataPrecision, mysqlDataScale, mysqlColMeta),
			fmt.Sprintf("DOUBLE %s", oracleColMeta)})

		fixedMsg = fmt.Sprintf("ALTER TABLE %s.%s MODIFY COLUMN %s %s %s;\n",
			targetSchema,
			tableName,
			columnName,
			"DOUBLE",
			oracleColMeta,
		)
		return fixedMsg, tableRows, nil
	case "SMALLINT":
		if mysqlDataType == "DECIMAL" && mysqlDataPrecision == 38 && mysqlDataScale == 0 && oracleColMeta == mysqlColMeta {
			return "", nil, nil
		}
		tableRows = append(tableRows, table.Row{
			columnName,
			fmt.Sprintf("SMALLINT %s", oracleColMeta),
			fmt.Sprintf("%s(%d,%d) %s", mysqlDataType, mysqlDataPrecision, mysqlDataScale, mysqlColMeta),
			fmt.Sprintf("DECIMAL(38) %s", oracleColMeta)})

		fixedMsg = fmt.Sprintf("ALTER TABLE %s.%s MODIFY COLUMN %s %s %s;\n",
			targetSchema,
			tableName,
			columnName,
			"DECIMAL(38)",
			oracleColMeta,
		)
		return fixedMsg, tableRows, nil

	// 字符
	case "BFILE":
		if mysqlDataType == "VARCHAR" && mysqlDataLength == 255 && oracleColMeta == mysqlColMeta {
			return "", nil, nil
		}
		tableRows = append(tableRows, table.Row{
			columnName,
			fmt.Sprintf("BFILE %s", oracleColMeta),
			fmt.Sprintf("%s(%d) %s", mysqlDataType, mysqlDataLength, mysqlColMeta),
			fmt.Sprintf("VARCHAR(255) %s", oracleColMeta)})

		fixedMsg = fmt.Sprintf("ALTER TABLE %s.%s MODIFY COLUMN %s %s %s;\n",
			targetSchema,
			tableName,
			columnName,
			"VARCHAR(255)",
			oracleColMeta,
		)
		return fixedMsg, tableRows, nil
	case "CHARACTER":
		if oracleDataLength < 256 {
			if mysqlDataType == "CHAR" && mysqlDataLength == oracleDataLength && oracleColMeta == mysqlColMeta {
				return "", nil, nil
			}
			tableRows = append(tableRows, table.Row{
				columnName,
				fmt.Sprintf("CHARACTER(%d) %s", oracleDataLength, oracleColMeta),
				fmt.Sprintf("%s(%d) %s", mysqlDataType, mysqlDataLength, mysqlColMeta),
				fmt.Sprintf("CHAR(%d) %s", oracleDataLength, oracleColMeta)})

			fixedMsg = fmt.Sprintf("ALTER TABLE %s.%s MODIFY COLUMN %s %s %s;\n",
				targetSchema,
				tableName,
				columnName,
				fmt.Sprintf("CHAR(%d)", oracleDataLength),
				oracleColMeta,
			)
			return fixedMsg, tableRows, nil
		}
		if mysqlDataType == "VARCHAR" && mysqlDataLength == oracleDataLength && oracleColMeta == mysqlColMeta {
			return "", nil, nil
		}
		tableRows = append(tableRows, table.Row{
			columnName,
			fmt.Sprintf("CHARACTER(%d) %s", oracleDataLength, oracleColMeta),
			fmt.Sprintf("%s(%d) %s", mysqlDataType, mysqlDataLength, mysqlColMeta),
			fmt.Sprintf("VARCHAR(%d) %s", oracleDataLength, oracleColMeta)})

		fixedMsg = fmt.Sprintf("ALTER TABLE %s.%s MODIFY COLUMN %s %s %s;\n",
			targetSchema,
			tableName,
			columnName,
			fmt.Sprintf("VARCHAR(%d)", oracleDataLength),
			oracleColMeta,
		)
		return fixedMsg, tableRows, nil
	case "LONG":
		if mysqlDataType == "LONGTEXT" && mysqlDataLength == 4294967295 && oracleColMeta == mysqlColMeta {
			return "", nil, nil
		}
		tableRows = append(tableRows, table.Row{
			columnName,
			fmt.Sprintf("LONG %s", oracleColMeta),
			fmt.Sprintf("%s(%d) %s", mysqlDataType, mysqlDataLength, mysqlColMeta),
			fmt.Sprintf("LONGTEXT %s", oracleColMeta)})

		fixedMsg = fmt.Sprintf("ALTER TABLE %s.%s MODIFY COLUMN %s %s %s;\n",
			targetSchema,
			tableName,
			columnName,
			"LONGTEXT",
			oracleColMeta,
		)
		return fixedMsg, tableRows, nil
	case "LONG RAW":
		if mysqlDataType == "LONGBLOB" && mysqlDataLength == 4294967295 && oracleColMeta == mysqlColMeta {
			return "", nil, nil
		}
		tableRows = append(tableRows, table.Row{
			columnName,
			fmt.Sprintf("LONG RAW %s", oracleColMeta),
			fmt.Sprintf("%s(%d) %s", mysqlDataType, mysqlDataLength, mysqlColMeta),
			fmt.Sprintf("LONGBLOB %s", oracleColMeta)})

		fixedMsg = fmt.Sprintf("ALTER TABLE %s.%s MODIFY COLUMN %s %s %s;\n",
			targetSchema,
			tableName,
			columnName,
			"LONGBLOB",
			oracleColMeta,
		)
		return fixedMsg, tableRows, nil
	case "NCHAR VARYING":
		if mysqlDataType == "NCHAR VARYING" && mysqlDataLength == oracleDataLength && oracleColMeta == mysqlColMeta {
			return "", nil, nil
		}
		tableRows = append(tableRows, table.Row{
			columnName,
			fmt.Sprintf("NCHAR VARYING %s", oracleColMeta),
			fmt.Sprintf("%s(%d) %s", mysqlDataType, mysqlDataLength, mysqlColMeta),
			fmt.Sprintf("NCHAR VARYING(%d) %s", oracleDataLength, oracleColMeta)})

		fixedMsg = fmt.Sprintf("ALTER TABLE %s.%s MODIFY COLUMN %s %s %s;\n",
			targetSchema,
			tableName,
			columnName,
			fmt.Sprintf("NCHAR VARYING(%d)", oracleDataLength),
			oracleColMeta,
		)
		return fixedMsg, tableRows, nil
	case "NCLOB":
		if mysqlDataType == "TEXT" && mysqlDataLength == 65535 && oracleColMeta == mysqlColMeta {
			return "", nil, nil
		}
		tableRows = append(tableRows, table.Row{
			columnName,
			fmt.Sprintf("NCLOB %s", oracleColMeta),
			fmt.Sprintf("%s(%d) %s", mysqlDataType, mysqlDataLength, mysqlColMeta),
			fmt.Sprintf("TEXT %s", oracleColMeta)})

		fixedMsg = fmt.Sprintf("ALTER TABLE %s.%s MODIFY COLUMN %s %s %s;\n",
			targetSchema,
			tableName,
			columnName,
			"TEXT",
			oracleColMeta,
		)
		return fixedMsg, tableRows, nil
	case "RAW":
		if oracleDataLength < 256 {
			if mysqlDataType == "BINARY" && mysqlDataLength == oracleDataLength && oracleColMeta == mysqlColMeta {
				return "", nil, nil
			}
			tableRows = append(tableRows, table.Row{
				columnName,
				fmt.Sprintf("RAW(%d) %s", oracleDataLength, oracleColMeta),
				fmt.Sprintf("%s(%d) %s", mysqlDataType, mysqlDataLength, mysqlColMeta),
				fmt.Sprintf("BINARY(%d) %s", oracleDataLength, oracleColMeta)})

			fixedMsg = fmt.Sprintf("ALTER TABLE %s.%s MODIFY COLUMN %s %s %s;\n",
				targetSchema,
				tableName,
				columnName,
				fmt.Sprintf("BINARY(%d)", oracleDataLength),
				oracleColMeta,
			)
			return fixedMsg, tableRows, nil
		}

		if mysqlDataType == "VARBINARY" && mysqlDataLength == oracleDataLength && oracleColMeta == mysqlColMeta {
			return "", nil, nil
		}
		tableRows = append(tableRows, table.Row{
			columnName,
			fmt.Sprintf("RAW(%d) %s", oracleDataLength, oracleColMeta),
			fmt.Sprintf("%s(%d) %s", mysqlDataType, mysqlDataLength, mysqlColMeta),
			fmt.Sprintf("VARBINARY(%d) %s", oracleDataLength, oracleColMeta)})

		fixedMsg = fmt.Sprintf("ALTER TABLE %s.%s MODIFY COLUMN %s %s %s;\n",
			targetSchema,
			tableName,
			columnName,
			fmt.Sprintf("VARBINARY(%d)", oracleDataLength),
			oracleColMeta,
		)
		return fixedMsg, tableRows, nil
	case "ROWID":
		if mysqlDataType == "CHAR" && mysqlDataLength == 10 && oracleColMeta == mysqlColMeta {
			return "", nil, nil
		}
		tableRows = append(tableRows, table.Row{
			columnName,
			fmt.Sprintf("ROWID %s", oracleColMeta),
			fmt.Sprintf("%s(%d) %s", mysqlDataType, mysqlDataLength, mysqlColMeta),
			fmt.Sprintf("CHAR(10) %s", oracleColMeta)})

		fixedMsg = fmt.Sprintf("ALTER TABLE %s.%s MODIFY COLUMN %s %s %s;\n",
			targetSchema,
			tableName,
			columnName,
			"CHAR(10)",
			oracleColMeta,
		)
		return fixedMsg, tableRows, nil
	case "UROWID":
		if mysqlDataType == "VARCHAR" && mysqlDataLength == oracleDataLength && oracleColMeta == mysqlColMeta {
			return "", nil, nil
		}
		tableRows = append(tableRows, table.Row{
			columnName,
			fmt.Sprintf("UROWID %s", oracleColMeta),
			fmt.Sprintf("%s(%d) %s", mysqlDataType, mysqlDataLength, mysqlColMeta),
			fmt.Sprintf("VARCHAR(%d) %s", oracleDataLength, oracleColMeta)})

		fixedMsg = fmt.Sprintf("ALTER TABLE %s.%s MODIFY COLUMN %s %s %s;\n",
			targetSchema,
			tableName,
			columnName,
			fmt.Sprintf("VARCHAR(%d)", oracleDataLength),
			oracleColMeta,
		)
		return fixedMsg, tableRows, nil
	case "VARCHAR":
		if mysqlDataType == "VARCHAR" && mysqlDataLength == oracleDataLength && oracleColMeta == mysqlColMeta {
			return "", nil, nil
		}
		tableRows = append(tableRows, table.Row{
			columnName,
			fmt.Sprintf("VARCHAR(%d) %s", oracleDataLength, oracleColMeta),
			fmt.Sprintf("%s(%d) %s", mysqlDataType, mysqlDataLength, mysqlColMeta),
			fmt.Sprintf("VARCHAR(%d) %s", oracleDataLength, oracleColMeta)})

		fixedMsg = fmt.Sprintf("ALTER TABLE %s.%s MODIFY COLUMN %s %s %s;\n",
			targetSchema,
			tableName,
			columnName,
			fmt.Sprintf("VARCHAR(%d)", oracleDataLength),
			oracleColMeta,
		)
		return fixedMsg, tableRows, nil
	case "XMLTYPE":
		if mysqlDataType == "LONGTEXT" && mysqlDataLength == 4294967295 && oracleColMeta == mysqlColMeta {
			return "", nil, nil
		}
		tableRows = append(tableRows, table.Row{
			columnName,
			fmt.Sprintf("XMLTYPE %s", oracleColMeta),
			fmt.Sprintf("%s(%d) %s", mysqlDataType, mysqlDataLength, mysqlColMeta),
			fmt.Sprintf("LONGTEXT %s", oracleColMeta)})

		fixedMsg = fmt.Sprintf("ALTER TABLE %s.%s MODIFY COLUMN %s %s %s;\n",
			targetSchema,
			tableName,
			columnName,
			"LONGTEXT",
			oracleColMeta,
		)
		return fixedMsg, tableRows, nil

	// 二进制
	case "CLOB":
		if mysqlDataType == "LONGTEXT" && mysqlDataLength == 4294967295 && oracleColMeta == mysqlColMeta {
			return "", nil, nil
		}
		tableRows = append(tableRows, table.Row{
			columnName,
			fmt.Sprintf("CLOB %s", oracleColMeta),
			fmt.Sprintf("%s(%d) %s", mysqlDataType, mysqlDataLength, mysqlColMeta),
			fmt.Sprintf("LONGTEXT %s", oracleColMeta)})

		fixedMsg = fmt.Sprintf("ALTER TABLE %s.%s MODIFY COLUMN %s %s %s;\n",
			targetSchema,
			tableName,
			columnName,
			"LONGTEXT",
			oracleColMeta,
		)
		return fixedMsg, tableRows, nil
	case "BLOB":
		if mysqlDataType == "BLOB" && mysqlDataLength == 65535 && oracleColMeta == mysqlColMeta {
			return "", nil, nil
		}
		tableRows = append(tableRows, table.Row{
			columnName,
			fmt.Sprintf("BLOB %s", oracleColMeta),
			fmt.Sprintf("%s(%d) %s", mysqlDataType, mysqlDataLength, mysqlColMeta),
			fmt.Sprintf("BLOB %s", oracleColMeta)})

		fixedMsg = fmt.Sprintf("ALTER TABLE %s.%s MODIFY COLUMN %s %s %s;\n",
			targetSchema,
			tableName,
			columnName,
			"BLOB",
			oracleColMeta,
		)
		return fixedMsg, tableRows, nil

	// 时间
	case "DATE":
		if mysqlDataType == "DATETIME" && mysqlDataLength == 0 && mysqlDataPrecision == 0 && mysqlDataScale == 0 && oracleColMeta == mysqlColMeta {
			return "", nil, nil
		}
		tableRows = append(tableRows, table.Row{
			columnName,
			fmt.Sprintf("DATE %s", oracleColMeta),
			fmt.Sprintf("%s(%d,%d) %s", mysqlDataType, mysqlDataPrecision, mysqlDataScale, mysqlColMeta),
			fmt.Sprintf("DATETIME %s", oracleColMeta)})

		fixedMsg = fmt.Sprintf("ALTER TABLE %s.%s MODIFY COLUMN %s %s %s;\n",
			targetSchema,
			tableName,
			columnName,
			"DATETIME",
			oracleColMeta,
		)
		return fixedMsg, tableRows, nil

	// oracle 字符类型 bytes/char 判断 B/C
	// CHAR、NCHAR、VARCHAR2、NVARCHAR2( oracle 字符类型 B/C)
	// mysql 同等长度（data_length） char 字符类型 > oracle bytes 字节类型
	case "CHAR":
		if oracleDataLength < 256 {
			if mysqlDataType == "CHAR" && mysqlDataLength == oracleDataLength && oracleColMeta == mysqlColMeta && oracleColumnCharUsed == "char" {
				return "", nil, nil
			}
			tableRows = append(tableRows, table.Row{
				columnName,
				fmt.Sprintf("CHAR(%d %s) %s", oracleDataLength, oracleColumnCharUsed, oracleColMeta),
				fmt.Sprintf("%s(%d) %s", mysqlDataType, mysqlDataLength, mysqlColMeta),
				fmt.Sprintf("CHAR(%d) %s", oracleDataLength, oracleColMeta)})

			if mysqlDataType == "CHAR" && mysqlDataLength != oracleDataLength && oracleColMeta != mysqlColMeta {
				fixedMsg = fmt.Sprintf("ALTER TABLE %s.%s MODIFY COLUMN %s %s %s;\n",
					targetSchema,
					tableName,
					columnName,
					fmt.Sprintf("CHAR(%d)", oracleDataLength),
					oracleColMeta,
				)
			}

			return fixedMsg, tableRows, nil
		}
		if mysqlDataType == "VARCHAR" && mysqlDataLength == oracleDataLength && oracleColMeta == mysqlColMeta && oracleColumnCharUsed == "char" {
			return "", nil, nil
		}
		tableRows = append(tableRows, table.Row{
			columnName,
			fmt.Sprintf("CHAR(%d %s) %s", oracleDataLength, oracleColumnCharUsed, oracleColMeta),
			fmt.Sprintf("%s(%d) %s", mysqlDataType, mysqlDataLength, mysqlColMeta),
			fmt.Sprintf("VARCHAR(%d) %s", oracleDataLength, oracleColMeta)})

		if mysqlDataType == "VARCHAR" && mysqlDataLength != oracleDataLength && oracleColMeta != mysqlColMeta {
			fixedMsg = fmt.Sprintf("ALTER TABLE %s.%s MODIFY COLUMN %s %s %s;\n",
				targetSchema,
				tableName,
				columnName,
				fmt.Sprintf("VARCHAR(%d)", oracleDataLength),
				oracleColMeta,
			)
		}
		return fixedMsg, tableRows, nil
	case "NCHAR":
		if oracleDataLength < 256 {
			if mysqlDataType == "NCHAR" && mysqlDataLength == oracleDataLength && oracleColMeta == mysqlColMeta && oracleColumnCharUsed == "char" {
				return "", nil, nil
			}
			tableRows = append(tableRows, table.Row{
				columnName,
				fmt.Sprintf("NCHAR(%d %s) %s", oracleDataLength, oracleColumnCharUsed, oracleColMeta),
				fmt.Sprintf("%s(%d) %s", mysqlDataType, mysqlDataLength, mysqlColMeta),
				fmt.Sprintf("NCHAR(%d) %s", oracleDataLength, oracleColMeta)})

			if mysqlDataType == "NCHAR" && mysqlDataLength != oracleDataLength && oracleColMeta != mysqlColMeta {
				fixedMsg = fmt.Sprintf("ALTER TABLE %s.%s MODIFY COLUMN %s %s %s;\n",
					targetSchema,
					tableName,
					columnName,
					fmt.Sprintf("NCHAR(%d)", oracleDataLength),
					oracleColMeta,
				)
			}
			return fixedMsg, tableRows, nil
		}
		if mysqlDataType == "NVARCHAR" && mysqlDataLength == oracleDataLength && oracleColMeta == mysqlColMeta && oracleColumnCharUsed == "char" {
			return "", nil, nil
		}
		tableRows = append(tableRows, table.Row{
			columnName,
			fmt.Sprintf("NVARCHAR(%d %s) %s", oracleDataLength, oracleColumnCharUsed, oracleColMeta),
			fmt.Sprintf("%s(%d) %s", mysqlDataType, mysqlDataLength, mysqlColMeta),
			fmt.Sprintf("NVARCHAR(%d) %s", oracleDataLength, oracleColMeta)})

		if mysqlDataType == "NVARCHAR" && mysqlDataLength != oracleDataLength && oracleColMeta != mysqlColMeta {
			fixedMsg = fmt.Sprintf("ALTER TABLE %s.%s MODIFY COLUMN %s %s %s;\n",
				targetSchema,
				tableName,
				columnName,
				fmt.Sprintf("NVARCHAR(%d)", oracleDataLength),
				oracleColMeta,
			)
		}
		return fixedMsg, tableRows, nil
	case "VARCHAR2":
		if mysqlDataType == "VARCHAR" && mysqlDataLength == oracleDataLength && oracleColMeta == mysqlColMeta && oracleColumnCharUsed == "char" {
			return "", nil, nil
		}
		tableRows = append(tableRows, table.Row{
			columnName,
			fmt.Sprintf("VARCHAR2(%d %s) %s", oracleDataLength, oracleColumnCharUsed, oracleColMeta),
			fmt.Sprintf("%s(%d) %s", mysqlDataType, mysqlDataLength, mysqlColMeta),
			fmt.Sprintf("VARCHAR(%d) %s", oracleDataLength, oracleColMeta)})

		if mysqlDataType == "VARCHAR" && mysqlDataLength != oracleDataLength && oracleColMeta != mysqlColMeta {
			fixedMsg = fmt.Sprintf("ALTER TABLE %s.%s MODIFY COLUMN %s %s %s;\n",
				targetSchema,
				tableName,
				columnName,
				fmt.Sprintf("VARCHAR(%d)", oracleDataLength),
				oracleColMeta,
			)
		}
		return fixedMsg, tableRows, nil
	case "NVARCHAR2":
		if mysqlDataType == "NVARCHAR" && mysqlDataLength == oracleDataLength && oracleColMeta == mysqlColMeta && oracleColumnCharUsed == "char" {
			return "", nil, nil
		}
		tableRows = append(tableRows, table.Row{
			columnName,
			fmt.Sprintf("NVARCHAR2(%d %s) %s", oracleDataLength, oracleColumnCharUsed, oracleColMeta),
			fmt.Sprintf("%s(%d) %s", mysqlDataType, mysqlDataLength, mysqlColMeta),
			fmt.Sprintf("NVARCHAR(%d) %s", oracleDataLength, oracleColMeta)})

		if mysqlDataType == "NVARCHAR" && mysqlDataLength != oracleDataLength && oracleColMeta != mysqlColMeta {
			fixedMsg = fmt.Sprintf("ALTER TABLE %s.%s MODIFY COLUMN %s %s %s;\n",
				targetSchema,
				tableName,
				columnName,
				fmt.Sprintf("NVARCHAR(%d)", oracleDataLength),
				oracleColMeta,
			)
		}
		return fixedMsg, tableRows, nil

	// 默认其他类型
	default:
		if strings.Contains(oracleDataType, "INTERVAL") {
			if mysqlDataType == "VARCHAR" && mysqlDataLength == 30 && oracleColMeta == mysqlColMeta {
				return "", nil, nil
			}
			tableRows = append(tableRows, table.Row{
				columnName,
				fmt.Sprintf("%s %s", oracleDataType, oracleColMeta),
				fmt.Sprintf("%s(%d) %s", mysqlDataType, mysqlDataLength, mysqlColMeta),
				fmt.Sprintf("VARCHAR(30) %s", oracleColMeta)})

			fixedMsg = fmt.Sprintf("ALTER TABLE %s.%s MODIFY COLUMN %s %s %s;\n",
				targetSchema,
				tableName,
				columnName,
				"VARCHAR(30)",
				oracleColMeta,
			)
			return fixedMsg, tableRows, nil
		} else if strings.Contains(oracleDataType, "TIMESTAMP") {
			if oracleDataScale == 0 {
				if mysqlDataType == "TIMESTAMP" && mysqlDataLength == 0 && mysqlDataPrecision == 0 && mysqlDataScale == 0 && mysqlDatetimePrecision == 0 && oracleColMeta == mysqlColMeta {
					return "", nil, nil
				}
				tableRows = append(tableRows, table.Row{
					columnName,
					fmt.Sprintf("%s %s", oracleDataType, oracleColMeta),
					fmt.Sprintf("%s(%d,%d) %s", mysqlDataType, mysqlDataPrecision, mysqlDataScale, mysqlColMeta),
					fmt.Sprintf("TIMESTAMP %s", oracleColMeta)})

				fixedMsg = fmt.Sprintf("ALTER TABLE %s.%s MODIFY COLUMN %s %s %s;\n",
					targetSchema,
					tableName,
					columnName,
					"TIMESTAMP",
					oracleColMeta,
				)
				return fixedMsg, tableRows, nil
			}
			if mysqlDataType == "TIMESTAMP" && mysqlDataScale == 0 && mysqlDataLength == 0 && mysqlDataPrecision == 0 && mysqlDatetimePrecision <= 6 && oracleColMeta == mysqlColMeta {
				return "", nil, nil
			}
			tableRows = append(tableRows, table.Row{
				columnName,
				fmt.Sprintf("%s %s", oracleDataType, oracleColMeta),
				fmt.Sprintf("%s(%d) %s", mysqlDataType, mysqlDatetimePrecision, mysqlColMeta),
				fmt.Sprintf("TIMESTAMP(%d) %s", oracleDataScale, oracleColMeta)})

			fixedMsg = fmt.Sprintf("ALTER TABLE %s.%s MODIFY COLUMN %s %s %s;\n",
				targetSchema,
				tableName,
				columnName,
				fmt.Sprintf("TIMESTAMP(%d)", oracleDataScale),
				oracleColMeta,
			)
			return fixedMsg, tableRows, nil
		} else {
			if mysqlDataType == "TEXT" && mysqlDataLength == 65535 && mysqlDataScale == 0 && oracleColMeta == mysqlColMeta {
				return "", nil, nil
			}
			tableRows = append(tableRows, table.Row{
				columnName,
				fmt.Sprintf("%s %s", oracleDataType, oracleColMeta),
				fmt.Sprintf("%s(%d) %s", mysqlDataType, mysqlDataLength, mysqlColMeta),
				fmt.Sprintf("TEXT %s", oracleColMeta)})

			fixedMsg = fmt.Sprintf("ALTER TABLE %s.%s MODIFY COLUMN %s %s %s;\n",
				targetSchema,
				tableName,
				columnName,
				"TEXT",
				oracleColMeta,
			)
			return fixedMsg, tableRows, nil
		}
	}
}
