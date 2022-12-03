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
	"github.com/wentaojin/transferdb/model"
	"github.com/wentaojin/transferdb/module/check"
	"github.com/wentaojin/transferdb/module/query/mysql"
	"github.com/wentaojin/transferdb/module/query/oracle"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type O2M struct {
	ctx    context.Context
	cfg    *config.Config
	mysql  *mysql.MySQL
	oracle *oracle.Oracle
}

func NewO2MCheck(ctx context.Context, cfg *config.Config, oracle *oracle.Oracle, mysql *mysql.MySQL) *O2M {
	return &O2M{
		ctx:    ctx,
		cfg:    cfg,
		mysql:  mysql,
		oracle: oracle,
	}
}

func (r *O2M) NewCheck() error {
	startTime := time.Now()
	zap.L().Info("check oracle and mysql table start",
		zap.String("oracleSchema", r.cfg.OracleConfig.SchemaName),
		zap.String("mysqlSchema", r.cfg.MySQLConfig.SchemaName))

	exporters, err := filterCFGTable(r.cfg, r.oracle)
	if err != nil {
		return err
	}

	// 判断下游数据库是否存在 oracle 表
	mysqlTables, err := r.mysql.GetMySQLTable(r.cfg.MySQLConfig.SchemaName)
	if err != nil {
		return err
	}
	ok, noExistTables := common.IsSubsetString(mysqlTables, exporters)
	if !ok {
		return fmt.Errorf("oracle tables %v isn't exist in the mysqldb schema [%v], please create", noExistTables, r.cfg.MySQLConfig.SchemaName)
	}

	// 判断 table_error_detail 是否存在错误记录，是否可进行 check
	errTotals, err := model.NewTableErrorDetailModel(r.oracle.GormDB).CountsBySchema(r.ctx, &model.TableErrorDetail{
		SourceSchemaName: common.StringUPPER(r.cfg.OracleConfig.SchemaName),
		RunMode:          common.CheckO2MMode,
	})

	if errTotals > 0 || err != nil {
		return fmt.Errorf("check schema [%s] mode [%s] table task failed: %v, table [table_error_detail] exist failed error, please clear and rerunning", strings.ToUpper(r.cfg.OracleConfig.SchemaName), common.CheckO2MMode, err)
	}

	// oracle 环境信息
	beginTime := time.Now()
	oracleDBCharacterSet, err := r.oracle.GetOracleDBCharacterSet()
	if err != nil {
		return err
	}
	if _, ok := common.OracleDBCharacterSetMap[strings.Split(oracleDBCharacterSet, ".")[1]]; !ok {
		return fmt.Errorf("oracle db character set [%v] isn't support", oracleDBCharacterSet)
	}

	// oracle db collation
	nlsSort, err := r.oracle.GetOracleDBCharacterNLSSortCollation()
	if err != nil {
		return err
	}
	nlsComp, err := r.oracle.GetOracleDBCharacterNLSCompCollation()
	if err != nil {
		return err
	}
	if _, ok := common.OracleCollationMap[strings.ToUpper(nlsSort)]; !ok {
		return fmt.Errorf("oracle db nls sort [%s] isn't support", nlsSort)
	}
	if _, ok := common.OracleCollationMap[strings.ToUpper(nlsComp)]; !ok {
		return fmt.Errorf("oracle db nls comp [%s] isn't support", nlsComp)
	}
	if strings.ToUpper(nlsSort) != strings.ToUpper(nlsComp) {
		return fmt.Errorf("oracle db nls_sort [%s] and nls_comp [%s] isn't different, need be equal; because mysql db isn't support", nlsSort, nlsComp)
	}

	// oracle 版本是否存在 collation
	oracleDBVersion, err := r.oracle.GetOracleDBVersion()
	if err != nil {
		return err
	}

	oracleDBCollation := false
	if common.VersionOrdinal(oracleDBVersion) >= common.VersionOrdinal(common.OracleTableColumnCollationDBVersion) {
		oracleDBCollation = true
	}
	finishTime := time.Now()
	zap.L().Info("get oracle db character and version finished",
		zap.String("schema", r.cfg.OracleConfig.SchemaName),
		zap.String("db version", oracleDBVersion),
		zap.String("db character", oracleDBCharacterSet),
		zap.Int("table totals", len(exporters)),
		zap.Bool("table collation", oracleDBCollation),
		zap.String("cost", finishTime.Sub(beginTime).String()))

	var (
		oracleTableCollation  map[string]string
		oracleSchemaCollation string
	)

	if oracleDBCollation {
		beginTime = time.Now()
		oracleSchemaCollation, err = r.oracle.GetOracleSchemaCollation(strings.ToUpper(r.cfg.OracleConfig.SchemaName))
		if err != nil {
			return err
		}
		oracleTableCollation, err = r.oracle.GetOracleSchemaTableCollation(strings.ToUpper(r.cfg.OracleConfig.SchemaName), oracleSchemaCollation)
		if err != nil {
			return err
		}
		finishTime = time.Now()
		zap.L().Info("get oracle schema and table collation finished",
			zap.String("schema", r.cfg.OracleConfig.SchemaName),
			zap.String("db version", oracleDBVersion),
			zap.String("db character", oracleDBCharacterSet),
			zap.Int("table totals", len(exporters)),
			zap.Bool("table collation", oracleDBCollation),
			zap.String("cost", finishTime.Sub(beginTime).String()))
	}

	// 任务检查表
	tasks := GenCheckTaskTable(r.cfg.OracleConfig.SchemaName, r.cfg.MySQLConfig.SchemaName, oracleDBCharacterSet,
		nlsSort, nlsComp, oracleTableCollation, oracleSchemaCollation, oracleDBCollation,
		r.cfg.MySQLConfig.DBType, r.oracle, r.mysql, exporters)

	pwdDir, err := os.Getwd()
	if err != nil {
		return err
	}

	checkFile := filepath.Join(pwdDir, fmt.Sprintf("check_%s.sql", r.cfg.OracleConfig.SchemaName))

	// file writer
	f, err := check.NewWriter(checkFile)
	if err != nil {
		return err
	}

	g := &errgroup.Group{}
	g.SetLimit(r.cfg.AppConfig.Threads)

	for _, task := range tasks {
		t := task
		g.Go(func() error {
			oracleTableInfo, err := t.GenOracleTable()
			if err != nil {
				return err
			}
			mysqlTableInfo, mysqlDBVersion, err := t.GenMySQLTable()
			if err != nil {
				return err
			}
			err = NewChecker(r.ctx, oracleTableInfo, mysqlTableInfo, mysqlDBVersion, r.cfg.MySQLConfig.DBType, r.oracle).Writer(f)
			if err != nil {
				return err
			}
			return nil
		})
	}

	if err = g.Wait(); err != nil {
		return err
	}

	if err = f.Close(); err != nil {
		return err
	}

	checkError, err := model.NewTableErrorDetailModel(r.oracle.GormDB).CountsBySchema(r.ctx, &model.TableErrorDetail{
		SourceSchemaName: common.StringUPPER(r.cfg.OracleConfig.SchemaName),
		RunMode:          common.CheckO2MMode,
	})
	if err != nil {
		return err
	}

	endTime := time.Now()
	zap.L().Info("check", zap.String("output", filepath.Join(pwdDir, fmt.Sprintf("check_%s.sql", r.cfg.OracleConfig.SchemaName))))
	if checkError == 0 {
		zap.L().Info("check table oracle to mysql finished",
			zap.Int("table totals", len(exporters)),
			zap.Int("table success", len(exporters)),
			zap.Int("table failed", 0),
			zap.String("cost", endTime.Sub(startTime).String()))
	} else {
		zap.L().Warn("check table oracle to mysql finished",
			zap.Int("table totals", len(exporters)),
			zap.Int("table success", len(exporters)-int(checkError)),
			zap.Int("check failed", int(checkError)),
			zap.String("failed tips", "failed detail, please see table [table_error_detail]"),
			zap.String("cost", endTime.Sub(startTime).String()))
	}
	return nil
}