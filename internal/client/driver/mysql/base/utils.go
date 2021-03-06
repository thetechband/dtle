/*
 * Copyright (C) 2016-2018. ActionTech.
 * Based on: github.com/hashicorp/nomad, github.com/github/gh-ost .
 * License: MPL version 2: https://www.mozilla.org/en-US/MPL/2.0 .
 */

package base

import (
	"bytes"
	gosql "database/sql"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	sqle "github.com/actiontech/dtle/internal/client/driver/mysql/sqle/inspector"
	"github.com/actiontech/dtle/internal/g"
	"github.com/pingcap/parser/ast"
	parsermysql "github.com/pingcap/parser/mysql"

	"database/sql"

	"github.com/satori/go.uuid"
	gomysql "github.com/siddontang/go-mysql/mysql"
	"github.com/siddontang/go/hack"

	usql "github.com/actiontech/dtle/internal/client/driver/mysql/sql"
	umconf "github.com/actiontech/dtle/internal/config/mysql"
)

var (
	prettifyDurationRegexp = regexp.MustCompile("([.][0-9]+)")
)

func PrettifyDurationOutput(d time.Duration) string {
	if d < time.Second {
		return "0s"
	}
	result := fmt.Sprintf("%s", d)
	result = prettifyDurationRegexp.ReplaceAllString(result, "")
	return result
}

// StringContainsAll returns true if `s` contains all non empty given `substrings`
// The function returns `false` if no non-empty arguments are given.
func StringContainsAll(s string, substrings ...string) bool {
	nonEmptyStringsFound := false
	for _, substring := range substrings {
		if substring == "" {
			continue
		}
		if strings.Contains(s, substring) {
			nonEmptyStringsFound = true
		} else {
			// Immediate failure
			return false
		}
	}
	return nonEmptyStringsFound
}

func GetSelfBinlogCoordinates(db *gosql.DB) (selfBinlogCoordinates *BinlogCoordinatesX, err error) {
	err = usql.QueryRowsMap(db, `show master status`, func(m usql.RowMap) error {
		selfBinlogCoordinates = &BinlogCoordinatesX{
			LogFile: m.GetString("File"),
			LogPos:  m.GetInt64("Position"),
			GtidSet: m.GetString("Executed_Gtid_Set"),
		}
		return nil
	})
	return selfBinlogCoordinates, err
}

func ParseBinlogCoordinatesFromRows(rows *sql.Rows) (selfBinlogCoordinates *BinlogCoordinatesX, err error) {
	err = usql.ScanRowsToMaps(rows, func(m usql.RowMap) error {
		selfBinlogCoordinates = &BinlogCoordinatesX{
			LogFile: m.GetString("File"),
			LogPos:  m.GetInt64("Position"),
			GtidSet: m.GetString("Executed_Gtid_Set"),
		}
		return nil
	})
	return selfBinlogCoordinates, err
}

// GetTableColumns reads column list from given table
func GetTableColumns(db usql.QueryAble, databaseName, tableName string) (*umconf.ColumnList, error) {
	query := fmt.Sprintf(`
		show columns from %s.%s
		`,
		umconf.EscapeName(databaseName),
		umconf.EscapeName(tableName),
	)
	columns := []umconf.Column{}
	err := usql.QueryRowsMap(db, query, func(rowMap usql.RowMap) error {
		aColumn := umconf.Column{
			RawName:    rowMap.GetString("Field"),
			ColumnType: rowMap.GetString("Type"),
			Default:    rowMap.GetString("Default"),
			Key:        strings.ToUpper(rowMap.GetString("Key")),
			Nullable:   strings.ToUpper(rowMap.GetString("Null")) == "YES",
		}
		aColumn.EscapedName = umconf.EscapeName(aColumn.RawName)
		columns = append(columns, aColumn)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(columns) == 0 {
		return nil, fmt.Errorf("Found 0 columns on %s.%s. Bailing out",
			umconf.EscapeName(databaseName),
			umconf.EscapeName(tableName),
		)
	}
	return umconf.NewColumnList(columns), nil
}

func ShowCreateTable(db *gosql.DB, databaseName, tableName string, dropTableIfExists bool, addUse bool) (statement []string, err error) {
	var dummy, createTableStatement string
	query := fmt.Sprintf(`show create table %s.%s`, umconf.EscapeName(databaseName), umconf.EscapeName(tableName))
	err = db.QueryRow(query).Scan(&dummy, &createTableStatement)
	if addUse {
		statement = append(statement, fmt.Sprintf("USE %s", umconf.EscapeName(databaseName)))
	}
	if dropTableIfExists {
		statement = append(statement, fmt.Sprintf("DROP TABLE IF EXISTS %s", umconf.EscapeName(tableName)))
	}
	statement = append(statement, createTableStatement)
	return statement, err
}

func ShowCreateView(db *gosql.DB, databaseName, tableName string, dropTableIfExists bool) (createTableStatement string, err error) {
	var dummy, character_set_client, collation_connection string
	query := fmt.Sprintf(`show create table %s.%s`, umconf.EscapeName(databaseName), umconf.EscapeName(tableName))
	err = db.QueryRow(query).Scan(&dummy, &createTableStatement, &character_set_client, &collation_connection)
	statement := fmt.Sprintf("USE %s", umconf.EscapeName(databaseName))
	if dropTableIfExists {
		statement = fmt.Sprintf("%s;DROP TABLE IF EXISTS `%s`", statement, tableName)
	}
	return fmt.Sprintf("%s;%s", statement, createTableStatement), err
}

// Interval is [start, stop), but the GTID string's format is [n] or [n1-n2], closed interval
func parseInterval(str string) (i gomysql.Interval, err error) {
	p := strings.Split(str, "-")
	switch len(p) {
	case 1:
		i.Start, err = strconv.ParseInt(p[0], 10, 64)
		i.Stop = i.Start + 1
	case 2:
		i.Start, err = strconv.ParseInt(p[0], 10, 64)
		i.Stop, err = strconv.ParseInt(p[1], 10, 64)
		i.Stop = i.Stop + 1
	default:
		err = fmt.Errorf("invalid interval format, must n[-n]")
	}

	if err != nil {
		return
	}

	if i.Stop <= i.Start {
		err = fmt.Errorf("invalid interval format, must n[-n] and the end must >= start")
	}

	return
}

// return: normalized GtidSet
func SelectAllGtidExecuted(db usql.QueryAble, jid uuid.UUID) (gtidSet GtidSet, err error) {
	query := fmt.Sprintf(`SELECT source_uuid,interval_gtid FROM %v.%v where job_uuid=?`,
		g.DtleSchemaName, g.GtidExecutedTableV3)

	rows, err := db.Query(query, jid.Bytes())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	gtidSet = make(GtidSet)

	for rows.Next() {
		var sidUUID uuid.UUID
		var interval string
		err = rows.Scan(&sidUUID, &interval)

		if err != nil {
			return nil, err
		}

		item, ok := gtidSet[sidUUID]
		if !ok {
			item = &GtidExecutedItem{
				NRow:      0,
				Intervals: nil,
			}
			gtidSet[sidUUID] = item
		}
		item.NRow += 1
		sep := strings.Split(interval, ":")
		// Handle interval
		for i := 0; i < len(sep); i++ {
			if in, err := parseInterval(sep[i]); err != nil {
				return nil, err
			} else {
				item.Intervals = append(item.Intervals, in)
			}
		}
	}

	for sid, item := range gtidSet {
		gtidSet[sid].Intervals = item.Intervals.Normalize()
	}

	return gtidSet, err
}

func StringInterval(intervals gomysql.IntervalSlice) string {
	buf := new(bytes.Buffer)

	for idx, i := range intervals {
		if idx != 0 {
			buf.WriteString(":")
		}
		buf.WriteString(i.String())
	}

	return hack.String(buf.Bytes())
}

// applyColumnTypes
func ApplyColumnTypes(db usql.QueryAble, databaseName, tableName string, columnsLists ...*umconf.ColumnList) error {
	query := `
		select
				*
			from
				information_schema.columns
			where
				table_schema=?
				and table_name=?
		`
	err := usql.QueryRowsMap(db, query, func(m usql.RowMap) error {
		columnName := m.GetString("COLUMN_NAME")
		columnType := m.GetString("COLUMN_TYPE")
		if strings.Contains(columnType, "unsigned") {
			for _, columnsList := range columnsLists {
				columnsList.SetUnsigned(columnName)
			}
		}
		if strings.Contains(columnType, "mediumint") {
			for _, columnsList := range columnsLists {
				columnsList.GetColumn(columnName).Type = umconf.MediumIntColumnType
			}
		}
		if strings.Contains(columnType, "timestamp") {
			for _, columnsList := range columnsLists {
				columnsList.GetColumn(columnName).Type = umconf.TimestampColumnType
				columnsList.GetColumn(columnName).ColumnType = columnType
			}
		}
		if strings.Contains(columnType, "datetime") {
			for _, columnsList := range columnsLists {
				col := columnsList.GetColumn(columnName)
				col.Type = umconf.DateTimeColumnType
				col.Precision = m.GetInt("DATETIME_PRECISION")
			}
		}
		if strings.HasPrefix(columnType, "enum") {
			for _, columnsList := range columnsLists {
				columnsList.GetColumn(columnName).Type = umconf.EnumColumnType
				columnsList.GetColumn(columnName).ColumnType = columnType
			}
		}
		if strings.HasPrefix(columnType, "binary") {
			for _, columnsList := range columnsLists {
				columnsList.GetColumn(columnName).Type = umconf.BinaryColumnType
				columnsList.GetColumn(columnName).ColumnType = columnType
			}
		}
		if strings.Contains(columnType, "text") {
			for _, columnsList := range columnsLists {
				columnsList.GetColumn(columnName).Type = umconf.TextColumnType
				columnsList.GetColumn(columnName).ColumnType = columnType
			}
		}
		if strings.Contains(columnType, "json") {
			for _, columnsList := range columnsLists {
				columnsList.GetColumn(columnName).Type = umconf.JSONColumnType
			}
		}
		if strings.Contains(columnType, "float") {
			for _, columnsList := range columnsLists {
				columnsList.GetColumn(columnName).Type = umconf.FloatColumnType
			}
		}
		if strings.HasPrefix(columnType, "varbinary") {
			for _, columnsList := range columnsLists {
				columnsList.GetColumn(columnName).Type = umconf.VarbinaryColumnType
				columnsList.GetColumn(columnName).ColumnType = columnType
			}
		}
		if strings.HasPrefix(columnType, "char") {
			for _, columnsList := range columnsLists {
				columnsList.GetColumn(columnName).Type = umconf.CharColumnType
				columnsList.GetColumn(columnName).ColumnType = columnType
			}
		}
		if strings.HasPrefix(columnType, "varchar") {
			for _, columnsList := range columnsLists {
				columnsList.GetColumn(columnName).Type = umconf.VarcharColumnType
				columnsList.GetColumn(columnName).ColumnType = columnType
			}
		}
		if strings.HasPrefix(columnType, "date") {
			for _, columnsList := range columnsLists {
				columnsList.GetColumn(columnName).Type = umconf.DateColumnType
			}
		}
		if strings.HasPrefix(columnType, "year") {
			for _, columnsList := range columnsLists {
				columnsList.GetColumn(columnName).Type = umconf.YearColumnType
			}
		}
		if strings.HasPrefix(columnType, "time") {
			for _, columnsList := range columnsLists {
				col := columnsList.GetColumn(columnName)
				col.Type = umconf.TimeColumnType
				col.Precision = m.GetInt("DATETIME_PRECISION")
			}
		}
		if strings.Contains(columnType, "blob") {
			for _, columnsList := range columnsLists {
				columnsList.GetColumn(columnName).Type = umconf.BlobColumnType
			}
		}
		if strings.HasPrefix(columnType, "bit") {
			for _, columnsList := range columnsLists {
				columnsList.GetColumn(columnName).Type = umconf.BitColumnType
			}
		}
		if strings.HasPrefix(columnType, "int") {
			for _, columnsList := range columnsLists {
				columnsList.GetColumn(columnName).Type = umconf.IntColumnType
				columnsList.GetColumn(columnName).ColumnType = columnType
			}
		}
		if strings.HasPrefix(columnType, "tinyint") {
			for _, columnsList := range columnsLists {
				columnsList.GetColumn(columnName).Type = umconf.TinyintColumnType
				columnsList.GetColumn(columnName).ColumnType = columnType
			}
		}
		if strings.HasPrefix(columnType, "smallint") {
			for _, columnsList := range columnsLists {
				columnsList.GetColumn(columnName).Type = umconf.SmallintColumnType
				columnsList.GetColumn(columnName).ColumnType = columnType
			}
		}
		if strings.HasPrefix(columnType, "bigint") {
			for _, columnsList := range columnsLists {
				columnsList.GetColumn(columnName).Type = umconf.BigIntColumnType
				columnsList.GetColumn(columnName).ColumnType = columnType
			}
		}
		if strings.HasPrefix(columnType, "decimal") {
			for _, columnsList := range columnsLists {
				col := columnsList.GetColumn(columnName)
				col.Type = umconf.DecimalColumnType
				col.ColumnType = columnType
				col.Precision = m.GetInt("NUMERIC_PRECISION")
				col.Scale = m.GetInt("NUMERIC_SCALE")
			}
		}
		if strings.HasPrefix(columnType, "double") {
			for _, columnsList := range columnsLists {
				columnsList.GetColumn(columnName).Type = umconf.DoubleColumnType
				columnsList.GetColumn(columnName).ColumnType = columnType
			}
		}
		if strings.HasPrefix(columnType, "tinytext") {
			for _, columnsList := range columnsLists {
				columnsList.GetColumn(columnName).Type = umconf.TinytextColumnType
				columnsList.GetColumn(columnName).ColumnType = columnType
			}
		}
		if strings.HasPrefix(columnType, "set") {
			for _, columnsList := range columnsLists {
				columnsList.GetColumn(columnName).Type = umconf.SetColumnType
				columnsList.GetColumn(columnName).ColumnType = columnType
			}
		}
		// TODO return err on unknown type?
		if charset := m.GetString("CHARACTER_SET_NAME"); charset != "" {
			for _, columnsList := range columnsLists {
				columnsList.SetCharset(columnName, charset)
			}
		}
		return nil
	}, databaseName, tableName)
	return err
}

func GtidSetDiff(set1 string, set2 string) (string, error) {
	gStartHelper, err := gomysql.ParseMysqlGTIDSet(set2)
	if err != nil {
		return "", err
	}

	gStart, ok := gStartHelper.(*gomysql.MysqlGTIDSet)
	if !ok {
		return "", fmt.Errorf("internal error: cannot cast MysqlGTIDSet")
	}

	gExecutedHelper, err := gomysql.ParseMysqlGTIDSet(set1)
	if err != nil {
		return "", err
	}

	gExecuted, ok := gExecutedHelper.(*gomysql.MysqlGTIDSet)
	if !ok {
		return "", fmt.Errorf("internal error: cannot cast MysqlGTIDSet")
	}

	for sid, startSet := range gStart.Sets {
		// one for each UUID
		if startSet.Intervals.Len() != 1 {
			return "", fmt.Errorf("bad format for GtidStart")
		}
		// only start
		if startSet.Intervals[0].Start+1 != startSet.Intervals[0].Stop {
			return "", fmt.Errorf("bad format for GtidStart")
		}

		startPoint := startSet.Intervals[0].Start
		execSets, ok := gExecuted.Sets[sid]
		if !ok {
			// do nothing
		} else {
			newIntervals := gomysql.IntervalSlice{}
			for i, _ := range execSets.Intervals {
				if execSets.Intervals[i].Start >= startPoint {
					continue
				} else if execSets.Intervals[i].Stop >= startPoint {
					newIntervals = append(newIntervals, gomysql.Interval{
						Start: execSets.Intervals[i].Start,
						Stop:  startPoint,
					})
				} else {
					newIntervals = append(newIntervals, execSets.Intervals[i])
				}
			}
			execSets.Intervals = newIntervals
		}
	}

	return gExecuted.String(), nil
}

func GetTableColumnsSqle(sqleContext *sqle.Context, schema string, table string) (*umconf.ColumnList, error) {
	tableInfo, exists := sqleContext.GetTable(schema, table)
	if !exists {
		return nil, fmt.Errorf("table does not exists in sqle context. table: %v.%v", schema, table)
	}

	cStmt := tableInfo.MergedTable
	if cStmt == nil {
		cStmt = tableInfo.OriginalTable
	}

	columns := []umconf.Column{}

	pks, _ := sqle.GetPrimaryKey(cStmt)

	for _, col := range cStmt.Cols {
		newColumn := umconf.Column{
			RawName:  col.Name.String(),
			Nullable: true, // by default
		}
		newColumn.EscapedName = umconf.EscapeName(newColumn.RawName)
		if _, inPk := pks[newColumn.RawName]; inPk {
			newColumn.Key = "PRI"
		}

		if parsermysql.HasUnsignedFlag(col.Tp.Flag) {
			newColumn.IsUnsigned = true
		}

		newColumn.ColumnType = col.Tp.String()

		switch col.Tp.Tp {
		case parsermysql.TypeDecimal, parsermysql.TypeNewDecimal:
			newColumn.Type = umconf.DecimalColumnType
			newColumn.Precision = col.Tp.Flen
			newColumn.Scale = col.Tp.Decimal
		case parsermysql.TypeTiny:
			newColumn.Type = umconf.TinyintColumnType
		case parsermysql.TypeShort:
			newColumn.Type = umconf.SmallintColumnType
		case parsermysql.TypeLong:
			newColumn.Type = umconf.IntColumnType
		case parsermysql.TypeLonglong:
			newColumn.Type = umconf.BigIntColumnType
		case parsermysql.TypeInt24:
			newColumn.Type = umconf.MediumIntColumnType
		case parsermysql.TypeFloat:
			newColumn.Type = umconf.FloatColumnType
		case parsermysql.TypeDouble:
			newColumn.Type = umconf.DoubleColumnType
		case parsermysql.TypeNull:
			newColumn.Type = umconf.UnknownColumnType
		case parsermysql.TypeTimestamp:
			newColumn.Type = umconf.TimestampColumnType
		case parsermysql.TypeDate:
			newColumn.Type = umconf.DateColumnType
		case parsermysql.TypeDuration:
			newColumn.Type = umconf.TimeColumnType
			newColumn.Precision = col.Tp.Decimal
		case parsermysql.TypeDatetime:
			newColumn.Type = umconf.DateTimeColumnType
			newColumn.Precision = col.Tp.Decimal
		case parsermysql.TypeYear:
			newColumn.Type = umconf.YearColumnType
		case parsermysql.TypeNewDate:
			newColumn.Type = umconf.DateColumnType
		case parsermysql.TypeVarchar:
			newColumn.Type = umconf.VarcharColumnType
		case parsermysql.TypeBit:
			newColumn.Type = umconf.BitColumnType
		case parsermysql.TypeJSON:
			newColumn.Type = umconf.JSONColumnType
		case parsermysql.TypeEnum:
			newColumn.Type = umconf.EnumColumnType
		case parsermysql.TypeSet:
			newColumn.Type = umconf.SetColumnType
		case parsermysql.TypeTinyBlob:
			newColumn.Type = umconf.BlobColumnType
		case parsermysql.TypeMediumBlob:
			newColumn.Type = umconf.BlobColumnType
		case parsermysql.TypeLongBlob:
			newColumn.Type = umconf.BlobColumnType
		case parsermysql.TypeBlob:
			newColumn.Type = umconf.BlobColumnType
		case parsermysql.TypeVarString:
			newColumn.Type = umconf.TextColumnType
		case parsermysql.TypeString:
			newColumn.Type = umconf.VarcharColumnType
		case parsermysql.TypeGeometry:
			newColumn.Type = umconf.UnknownColumnType
		}

		for _, colOpt := range col.Options {
			switch colOpt.Tp {
			case ast.ColumnOptionNoOption:
			case ast.ColumnOptionPrimaryKey:
				// TODO multiple value?
				newColumn.Key = "PRI"
			case ast.ColumnOptionNotNull:
				newColumn.Nullable = false
			case ast.ColumnOptionAutoIncrement:
			case ast.ColumnOptionDefaultValue:
				value, ok := colOpt.Expr.(ast.ValueExpr)
				if !ok {
					newColumn.Default = nil
				} else {
					newColumn.Default = value.GetValue()
				}
			case ast.ColumnOptionUniqKey:
				newColumn.Key = "UNI"
			case ast.ColumnOptionNull:
				newColumn.Nullable = true
				// `not null` and `null` can occurred multiple times and the latter wins
			case ast.ColumnOptionOnUpdate:
			case ast.ColumnOptionFulltext:
			case ast.ColumnOptionComment:
			case ast.ColumnOptionGenerated:
			case ast.ColumnOptionReference:
			}
		}

		columns = append(columns, newColumn)
	}

	for _, cons := range cStmt.Constraints {
		switch cons.Tp {
		case ast.ConstraintPrimaryKey:
		case ast.ConstraintKey:
		case ast.ConstraintIndex:
		case ast.ConstraintUniq:
		case ast.ConstraintUniqKey:
		case ast.ConstraintUniqIndex:
		case ast.ConstraintForeignKey:
		case ast.ConstraintFulltext:
		}
	}

	r := umconf.NewColumnList(columns)
	//r.SetCharset() // TODO
	return r, nil
}
