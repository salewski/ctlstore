package mysql

import (
	"context"
	"database/sql"

	"github.com/segmentio/ctlstore/pkg/schema"
	"github.com/segmentio/ctlstore/pkg/sqlgen"
)

type MySQLDBInfo struct {
	Db *sql.DB
}

func (m *MySQLDBInfo) GetColumnInfo(ctx context.Context, tableNames []string) ([]schema.DBColumnInfo, error) {
	if len(tableNames) == 0 {
		return nil, nil
	}

	qs := sqlgen.SqlSprintf(
		"SELECT table_name, ordinal_position, column_name, data_type, column_key "+
			"FROM information_schema.columns "+
			"WHERE table_name IN ($1) "+
			"AND table_schema = DATABASE() "+
			"ORDER BY table_name, ordinal_position ASC",
		sqlgen.SQLPlaceholderSet(len(tableNames)))

	// []interface{} below won't accept []string
	ptrTableNames := []interface{}{}
	for _, tableName := range tableNames {
		ptrTableNames = append(ptrTableNames, tableName)
	}

	rows, err := m.Db.QueryContext(ctx, qs, ptrTableNames...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	columnInfos := []schema.DBColumnInfo{}

	for rows.Next() {
		var tableName string
		var index int
		var colName string
		var dataType string
		var colKey string

		err = rows.Scan(
			&tableName,
			&index,
			&colName,
			&dataType,
			&colKey,
		)
		if err != nil {
			return nil, err
		}

		columnInfos = append(columnInfos, schema.DBColumnInfo{
			TableName:    tableName,
			Index:        index,
			ColumnName:   colName,
			DataType:     dataType,
			IsPrimaryKey: (colKey == "PRI"),
		})
	}
	err = rows.Err()
	if err != nil {
		return nil, err
	}

	return columnInfos, nil
}
