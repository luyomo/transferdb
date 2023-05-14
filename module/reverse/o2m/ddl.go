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
	"encoding/json"
	"fmt"
	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/wentaojin/transferdb/common"
	"github.com/wentaojin/transferdb/module/reverse"
	"go.uber.org/zap"
	"strings"
)

type DDL struct {
	SourceSchemaName   string   `json:"source_schema"`
	SourceTableName    string   `json:"source_table_name"`
	SourceTableType    string   `json:"source_table_type"`
	SourceTableDDL     string   `json:"-"` // 忽略
	TargetSchemaName   string   `json:"target_schema"`
	TargetTableName    string   `json:"target_table_name"`
	TargetDBType       string   `json:"target_db_type"`
	TargetDBVersion    string   `json:"target_db_version"`
	TablePrefix        string   `json:"table_prefix"`
	TableColumns       []string `json:"table_columns"`
	TableKeys          []string `json:"table_keys"`
	TableSuffix        string   `json:"table_suffix"`
	TableComment       string   `json:"table_comment"`
	TableCheckKeys     []string `json:"table_check_keys""`
	TableForeignKeys   []string `json:"table_foreign_keys"`
	TableCompatibleDDL []string `json:"table_compatible_ddl"`
    TablePartitions    []string `json:"table_partitions"`
    TablePartitionKeys string   `json:"table_partition_key"`
    TablePartitionType string   `json:"table_partition_type"`
}

func (d *DDL) Write(w *reverse.Write) (string, error) {
	if w.Cfg.ReverseConfig.DirectWrite {
		errSql, err := d.WriteDB(w)
		if err != nil {
			return errSql, err
		}
	} else {
		errSql, err := d.WriteFile(w)
		if err != nil {
			return errSql, err
		}
	}
	return "", nil
}

func (d *DDL) WriteFile(w *reverse.Write) (string, error) {

	revDDLS, compDDLS := d.GenDDLStructure()

	var (
		sqlRev  strings.Builder
		sqlComp strings.Builder
	)

	// 表 with 主键
	sqlRev.WriteString("/*\n")
	sqlRev.WriteString(" oracle table reverse sql \n")

	sw := table.NewWriter()
	sw.SetStyle(table.StyleLight)
	sw.AppendHeader(table.Row{"#", "ORACLE TABLE TYPE", "ORACLE", "MYSQL", "SUGGEST"})
	sw.AppendRows([]table.Row{
		{"TABLE", d.SourceTableType, fmt.Sprintf("%s.%s", d.SourceSchemaName, d.SourceTableName), fmt.Sprintf("%s.%s", d.TargetSchemaName, d.TargetTableName), "Create Table"},
	})
	sqlRev.WriteString(fmt.Sprintf("%v\n", sw.Render()))
	sqlRev.WriteString(fmt.Sprintf("ORIGIN DDL:%v\n", d.SourceTableDDL))
	sqlRev.WriteString("*/\n")

	sqlRev.WriteString(strings.Join(revDDLS, "\n"))

	// 兼容项处理
	if len(compDDLS) > 0 {
		sqlComp.WriteString("/*\n")
		sqlComp.WriteString(" oracle table index or consrtaint maybe mysql has compatibility, skip\n")
		tw := table.NewWriter()
		tw.SetStyle(table.StyleLight)
		tw.AppendHeader(table.Row{"#", "ORACLE", "MYSQL", "SUGGEST"})
		tw.AppendRows([]table.Row{
			{"TABLE", fmt.Sprintf("%s.%s", d.SourceSchemaName, d.SourceTableName), fmt.Sprintf("%s.%s", d.TargetSchemaName, d.TargetTableName), "Create Index Or Constraints"}})

		sqlComp.WriteString(fmt.Sprintf("%v\n", tw.Render()))
		sqlComp.WriteString("*/\n")

		sqlComp.WriteString(strings.Join(compDDLS, "\n"))
	}

	// 数据写入
	if sqlRev.String() != "" {
		if _, err := w.RWriteFile(sqlRev.String()); err != nil {
			return sqlRev.String(), err
		}
	}
	if sqlComp.String() != "" {
		if _, err := w.CWriteFile(sqlComp.String()); err != nil {
			return sqlComp.String(), err
		}
	}
	return "", nil
}

func (d *DDL) WriteDB(w *reverse.Write) (string, error) {

	revDDLS, compDDLS := d.GenDDLStructure()

	var (
		sqlRev  strings.Builder
		sqlComp strings.Builder
	)

	// 表 with 主键
	sqlRev.WriteString(strings.Join(revDDLS, "\n"))

	// 兼容项处理
	if len(compDDLS) > 0 {
		sqlComp.WriteString("/*\n")
		sqlComp.WriteString(" oracle table index or consrtaint maybe mysql has compatibility, skip\n")
		tw := table.NewWriter()
		tw.SetStyle(table.StyleLight)
		tw.AppendHeader(table.Row{"#", "ORACLE", "MYSQL", "SUGGEST"})
		tw.AppendRows([]table.Row{
			{"TABLE", fmt.Sprintf("%s.%s", d.SourceSchemaName, d.SourceTableName), fmt.Sprintf("%s.%s", d.TargetSchemaName, d.TargetTableName), "Create Index Or Constraints"}})

		sqlComp.WriteString(fmt.Sprintf("%v\n", tw.Render()))
		sqlComp.WriteString("*/\n")

		sqlComp.WriteString(strings.Join(compDDLS, "\n"))
	}

	// 数据写入
	if sqlRev.String() != "" {
		if err := w.RWriteDB(sqlRev.String()); err != nil {
			return sqlRev.String(), err
		}
	}
	if sqlComp.String() != "" {
		if _, err := w.CWriteFile(sqlComp.String()); err != nil {
			return sqlComp.String(), err
		}
	}
	return "", nil
}

func (d *DDL) GenDDLStructure() ([]string, []string) {
	var (
		reverseDDLS   []string
		compDDLS      []string
		tableDDL      string
		checkKeyDDL   []string
		foreignKeyDDL []string
	)
    // fmt.Printf("------------------- I am here \n")

	// 表 with 主键
	var structDDL string
	if len(d.TableKeys) > 0 {
		structDDL = fmt.Sprintf("%s (\n%s,\n%s\n)",
			d.TablePrefix,
			strings.Join(d.TableColumns, ",\n"),
			strings.Join(d.TableKeys, ",\n"))
        // fmt.Printf("Primary key: <%s> \n", structDDL)
	} else {
		structDDL = fmt.Sprintf("%s (\n%s\n)",
			d.TablePrefix,
			strings.Join(d.TableColumns, ",\n"))
	}

    partDDL := ""
    if len(d.TablePartitions) > 0 {
        fmt.Printf("The partitions are: <%#v> \n", d.TablePartitions)
        fmt.Printf("The partitions type is : <%#v> \n", d.TablePartitionType)
        fmt.Printf("The partitions keys is : <%#v> \n", d.TablePartitionKeys)


        if d.TablePartitionType == "LIST" {
             partDDL = fmt.Sprintf("PARTITION BY LIST COLUMNS(%s) (%s)", d.TablePartitionKeys, strings.Join(d.TablePartitions, ", "))
        } else if d.TablePartitionType == "RANGE" {

        }
        tableDDL = tableDDL + partDDL 
        // fmt.Printf(" ******** The partitio DDLn: %s \n", partDDL)
    }

	if strings.EqualFold(d.TableComment, "") {
        if partDDL == "" {
		    tableDDL = fmt.Sprintf("%s %s;", structDDL, d.TableSuffix)
        }else {
		    tableDDL = fmt.Sprintf("%s %s %s;", structDDL, d.TableSuffix, partDDL)
        }
	} else {
        if partDDL == "" {
		    tableDDL = fmt.Sprintf("%s %s %s;", structDDL, d.TableSuffix, d.TableComment)
        }else {
		    tableDDL = fmt.Sprintf("%s %s %s %s;", structDDL, d.TableSuffix, d.TableComment, partDDL)
        }
	}


	zap.L().Info("reverse oracle table structure",
		zap.String("schema", d.TargetSchemaName),
		zap.String("table", d.TargetTableName),
		zap.String("sql", tableDDL))

	reverseDDLS = append(reverseDDLS, tableDDL+"\n")

	// foreign and check key sql ddl
	if len(d.TableForeignKeys) > 0 {
		for _, fk := range d.TableForeignKeys {
            // fmt.Printf("There is foreign keys here : <%s> \n", fk );
            // eleFK := strings.Split(fk, " ")
            // tiFK := strings.Join(eleFK[2:], " ")
			fkSQL := fmt.Sprintf("ALTER TABLE `%s`.`%s` ADD %s;",
			 	d.TargetSchemaName, d.TargetTableName, fk)
			zap.L().Info("reverse oracle table foreign key",
				zap.String("schema", d.TargetSchemaName),
				zap.String("table", d.TargetTableName),
				zap.String("fk sql", fkSQL))
			foreignKeyDDL = append(foreignKeyDDL, fkSQL)
		}
        //fmt.Printf("foreign keys: <%#v> \n", foreignKeyDDL)
	}
	if len(d.TableCheckKeys) > 0 {
		for _, ck := range d.TableCheckKeys {
			ckSQL := fmt.Sprintf("ALTER TABLE `%s`.`%s` ADD %s;",
				d.TargetSchemaName, d.TargetTableName, ck)
			zap.L().Info("reverse oracle table check key",
				zap.String("schema", d.TargetSchemaName),
				zap.String("table", d.TargetTableName),
				zap.String("ck sql", ckSQL))
			checkKeyDDL = append(checkKeyDDL, ckSQL)
		}
	}

	// 外键约束、检查约束
	if d.TargetDBType != common.DatabaseTypeTiDB {
		if len(foreignKeyDDL) > 0 {
			for _, sql := range foreignKeyDDL {
				reverseDDLS = append(reverseDDLS, sql)
			}
		}

		if common.VersionOrdinal(d.TargetDBVersion) > common.VersionOrdinal(common.MySQLCheckConsVersion) {
			if len(checkKeyDDL) > 0 {
				for _, sql := range checkKeyDDL {
					reverseDDLS = append(reverseDDLS, sql)
				}
			}
		} else {
			// 增加不兼容性语句
			if len(checkKeyDDL) > 0 {
				for _, sql := range checkKeyDDL {
					compDDLS = append(compDDLS, sql)
				}
			}
		}
		// 增加不兼容性语句
		if len(d.TableCompatibleDDL) > 0 {
			for _, sql := range d.TableCompatibleDDL {
				compDDLS = append(compDDLS, sql)
			}
		}

		return reverseDDLS, compDDLS
	}

	// TiDB 增加不兼容性语句
	if len(foreignKeyDDL) > 0 {
		for _, sql := range foreignKeyDDL {
			compDDLS = append(compDDLS, sql)
		}
	}
	if len(checkKeyDDL) > 0 {
		for _, sql := range checkKeyDDL {
			compDDLS = append(compDDLS, sql)
		}
	}
	if len(d.TableCompatibleDDL) > 0 {
		for _, sql := range d.TableCompatibleDDL {
			compDDLS = append(compDDLS, sql)
		}
	}

	return reverseDDLS, compDDLS
}

func (d *DDL) String() string {
	jsonBytes, _ := json.Marshal(d)
	return string(jsonBytes)
}
