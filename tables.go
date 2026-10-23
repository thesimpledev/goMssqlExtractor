package main

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
)

type column struct {
	name       string
	typeStr    string
	collation  string // set only when it differs from the database default
	nullable   bool
	identity   string
	rowguid    bool
	isComputed bool
	computeDef string
	persisted  bool
	hasDefault bool
	defName    string // empty for system-named defaults
	defDef     string
}

type keyConstraint struct {
	name        string
	systemNamed bool
	isPK        bool
	clustered   bool
	cols        []string
}

type foreignKey struct {
	name        string
	systemNamed bool
	refSchema   string
	refTable    string
	cols        []string
	refCols     []string
	onDelete    string
	onUpdate    string
}

type checkConstraint struct {
	name        string
	systemNamed bool
	definition  string
}

type index struct {
	name        string
	typ         int // sys.indexes.type: 1/2 rowstore, 5/6 columnstore
	unique      bool
	filter      string
	keyCols     []string
	includeCols []string
}

// tableCatalog holds everything needed to script tables and table
// types, keyed by object_id (the table type's internal table object).
type tableCatalog struct {
	columns map[int64][]column
	keys    map[int64][]keyConstraint
	fks     map[int64][]foreignKey
	checks  map[int64][]checkConstraint
	indexes map[int64][]index
}

func loadTableCatalog(db *sql.DB, dbCollation string) (*tableCatalog, error) {
	cat := &tableCatalog{}
	var err error
	if cat.columns, err = fetchColumns(db, dbCollation); err != nil {
		return nil, err
	}
	if cat.keys, err = fetchKeyConstraints(db); err != nil {
		return nil, err
	}
	if cat.fks, err = fetchForeignKeys(db); err != nil {
		return nil, err
	}
	if cat.checks, err = fetchCheckConstraints(db); err != nil {
		return nil, err
	}
	if cat.indexes, err = fetchIndexes(db); err != nil {
		return nil, err
	}
	return cat, nil
}

// typeSpec is the sys.types / sys.columns description of a data type.
type typeSpec struct {
	name, schema             string
	userDefined              bool
	maxLen, precision, scale int
}

// typeString renders a column/parameter type the way T-SQL is written:
// length for (n)(var)char and (var)binary, precision/scale for
// decimal/numeric, scale for datetime2/datetimeoffset/time, and the
// schema-qualified name for user-defined types. sys.columns stores
// nchar/nvarchar max_length in bytes, so it is halved here.
func typeString(s typeSpec) string {
	if s.userDefined {
		return qualified(s.schema, s.name)
	}
	switch s.name {
	case "decimal", "numeric":
		return fmt.Sprintf("%s(%d,%d)", s.name, s.precision, s.scale)
	case "float":
		if s.precision == 53 {
			return s.name
		}
		return fmt.Sprintf("float(%d)", s.precision)
	case "datetime2", "datetimeoffset", "time":
		return fmt.Sprintf("%s(%d)", s.name, s.scale)
	case "char", "varchar", "binary", "varbinary":
		if s.maxLen == -1 {
			return s.name + "(max)"
		}
		return fmt.Sprintf("%s(%d)", s.name, s.maxLen)
	case "nchar", "nvarchar":
		if s.maxLen == -1 {
			return s.name + "(max)"
		}
		return fmt.Sprintf("%s(%d)", s.name, s.maxLen/2)
	default:
		return s.name
	}
}

// noDefinition reports catalog source text that came back NULL, which
// means the login lacks VIEW DEFINITION.
func noDefinition(what, obj string) error {
	return fmt.Errorf("%s of %s came back NULL: the login lacks VIEW DEFINITION on the database; fix with GRANT VIEW DEFINITION TO <user> (see README)", what, obj)
}

type tableInfo struct {
	objectID int64
	schema   string
	name     string
}

const qTables = `
SELECT t.object_id, SCHEMA_NAME(t.schema_id), t.name
FROM sys.tables AS t
WHERE t.is_ms_shipped = 0
ORDER BY SCHEMA_NAME(t.schema_id), t.name;`

func fetchTables(db *sql.DB) ([]tableInfo, error) {
	rows, err := db.Query(qTables)
	if err != nil {
		return nil, fmt.Errorf("listing tables: %w", err)
	}
	defer closeQuietly(rows)
	var out []tableInfo
	for rows.Next() {
		var t tableInfo
		if err := rows.Scan(&t.objectID, &t.schema, &t.name); err != nil {
			return nil, fmt.Errorf("listing tables: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Identity seed/increment go through CONVERT(varchar) because they are
// sql_variant values that can exceed bigint for decimal identities.
const qColumns = `
SELECT o.object_id, c.name, ty.name, SCHEMA_NAME(ty.schema_id), ty.is_user_defined,
       c.max_length, c.precision, c.scale, c.is_nullable, ISNULL(c.collation_name, ''),
       c.is_identity,
       ISNULL(CONVERT(varchar(48), ic.seed_value), ''),
       ISNULL(CONVERT(varchar(48), ic.increment_value), ''),
       c.is_computed, ISNULL(cc.definition, ''), ISNULL(cc.is_persisted, 0),
       ISNULL(dc.name, ''), ISNULL(dc.is_system_named, 0), ISNULL(dc.definition, ''),
       c.is_rowguidcol
FROM sys.columns AS c
JOIN sys.objects AS o ON o.object_id = c.object_id
JOIN sys.types AS ty ON ty.user_type_id = c.user_type_id
LEFT JOIN sys.identity_columns AS ic ON ic.object_id = c.object_id AND ic.column_id = c.column_id
LEFT JOIN sys.computed_columns AS cc ON cc.object_id = c.object_id AND cc.column_id = c.column_id
LEFT JOIN sys.default_constraints AS dc ON dc.object_id = c.default_object_id
WHERE o.type IN ('U', 'TT') AND o.is_ms_shipped = 0
ORDER BY o.object_id, c.column_id;`

func fetchColumns(db *sql.DB, dbCollation string) (map[int64][]column, error) {
	rows, err := db.Query(qColumns)
	if err != nil {
		return nil, fmt.Errorf("reading columns: %w", err)
	}
	defer closeQuietly(rows)
	byObj := make(map[int64][]column)
	for rows.Next() {
		var (
			objID                    int64
			name, typeName, tySchema string
			userDefined              bool
			maxLen, precision, scale int
			nullable                 bool
			collation                string
			isIdentity               bool
			seed, incr               string
			isComputed               bool
			computeDef               string
			persisted                bool
			defName                  string
			defSystemNamed           bool
			defDef                   string
			rowguid                  bool
		)
		if err := rows.Scan(&objID, &name, &typeName, &tySchema, &userDefined,
			&maxLen, &precision, &scale, &nullable, &collation,
			&isIdentity, &seed, &incr,
			&isComputed, &computeDef, &persisted,
			&defName, &defSystemNamed, &defDef, &rowguid); err != nil {
			return nil, fmt.Errorf("reading columns: %w", err)
		}
		c := column{
			name: name,
			typeStr: typeString(typeSpec{name: typeName, schema: tySchema, userDefined: userDefined,
				maxLen: maxLen, precision: precision, scale: scale}),
			nullable:   nullable,
			rowguid:    rowguid,
			isComputed: isComputed,
			computeDef: computeDef,
			persisted:  persisted,
		}
		if collation != "" && collation != dbCollation {
			c.collation = collation
		}
		if isIdentity {
			c.identity = fmt.Sprintf("IDENTITY(%s,%s)", seed, incr)
		}
		if defName != "" {
			c.hasDefault = true
			c.defDef = defDef
			if !defSystemNamed {
				c.defName = defName
			}
		}
		byObj[objID] = append(byObj[objID], c)
	}
	return byObj, rows.Err()
}

// ORDER BY kc.type puts 'PK' ahead of 'UQ' so primary keys script first.
const qKeyConstraints = `
SELECT kc.parent_object_id, kc.name, kc.is_system_named, RTRIM(kc.type), i.type,
       c.name, ic.is_descending_key
FROM sys.key_constraints AS kc
JOIN sys.objects AS o ON o.object_id = kc.parent_object_id
JOIN sys.indexes AS i ON i.object_id = kc.parent_object_id AND i.index_id = kc.unique_index_id
JOIN sys.index_columns AS ic ON ic.object_id = i.object_id AND ic.index_id = i.index_id AND ic.key_ordinal > 0
JOIN sys.columns AS c ON c.object_id = ic.object_id AND c.column_id = ic.column_id
WHERE o.type IN ('U', 'TT') AND o.is_ms_shipped = 0
ORDER BY kc.parent_object_id, RTRIM(kc.type), kc.name, ic.key_ordinal;`

func fetchKeyConstraints(db *sql.DB) (map[int64][]keyConstraint, error) {
	rows, err := db.Query(qKeyConstraints)
	if err != nil {
		return nil, fmt.Errorf("reading key constraints: %w", err)
	}
	defer closeQuietly(rows)
	byObj := make(map[int64][]keyConstraint)
	for rows.Next() {
		var (
			objID       int64
			name        string
			systemNamed bool
			ctype       string
			idxType     int
			colName     string
			descending  bool
		)
		if err := rows.Scan(&objID, &name, &systemNamed, &ctype, &idxType, &colName, &descending); err != nil {
			return nil, fmt.Errorf("reading key constraints: %w", err)
		}
		col := bracket(colName) + " ASC"
		if descending {
			col = bracket(colName) + " DESC"
		}
		list := byObj[objID]
		if n := len(list); n > 0 && list[n-1].name == name {
			list[n-1].cols = append(list[n-1].cols, col)
		} else {
			list = append(list, keyConstraint{
				name:        name,
				systemNamed: systemNamed,
				isPK:        ctype == "PK",
				clustered:   idxType == 1,
				cols:        []string{col},
			})
		}
		byObj[objID] = list
	}
	return byObj, rows.Err()
}

const qForeignKeys = `
SELECT fk.parent_object_id, fk.name, fk.is_system_named,
       SCHEMA_NAME(ro.schema_id), ro.name,
       pc.name, rc.name,
       fk.delete_referential_action_desc, fk.update_referential_action_desc
FROM sys.foreign_keys AS fk
JOIN sys.objects AS ro ON ro.object_id = fk.referenced_object_id
JOIN sys.foreign_key_columns AS fkc ON fkc.constraint_object_id = fk.object_id
JOIN sys.columns AS pc ON pc.object_id = fkc.parent_object_id AND pc.column_id = fkc.parent_column_id
JOIN sys.columns AS rc ON rc.object_id = fkc.referenced_object_id AND rc.column_id = fkc.referenced_column_id
WHERE fk.is_ms_shipped = 0
ORDER BY fk.parent_object_id, fk.name, fkc.constraint_column_id;`

func fetchForeignKeys(db *sql.DB) (map[int64][]foreignKey, error) {
	rows, err := db.Query(qForeignKeys)
	if err != nil {
		return nil, fmt.Errorf("reading foreign keys: %w", err)
	}
	defer closeQuietly(rows)
	byObj := make(map[int64][]foreignKey)
	for rows.Next() {
		var (
			objID                      int64
			name                       string
			systemNamed                bool
			refSchema, refTable        string
			parentCol, refCol          string
			deleteAction, updateAction string
		)
		if err := rows.Scan(&objID, &name, &systemNamed, &refSchema, &refTable,
			&parentCol, &refCol, &deleteAction, &updateAction); err != nil {
			return nil, fmt.Errorf("reading foreign keys: %w", err)
		}
		list := byObj[objID]
		if n := len(list); n > 0 && list[n-1].name == name {
			list[n-1].cols = append(list[n-1].cols, bracket(parentCol))
			list[n-1].refCols = append(list[n-1].refCols, bracket(refCol))
		} else {
			list = append(list, foreignKey{
				name:        name,
				systemNamed: systemNamed,
				refSchema:   refSchema,
				refTable:    refTable,
				cols:        []string{bracket(parentCol)},
				refCols:     []string{bracket(refCol)},
				onDelete:    deleteAction,
				onUpdate:    updateAction,
			})
		}
		byObj[objID] = list
	}
	return byObj, rows.Err()
}

const qCheckConstraints = `
SELECT cc.parent_object_id, cc.name, cc.is_system_named, ISNULL(cc.definition, '')
FROM sys.check_constraints AS cc
WHERE cc.is_ms_shipped = 0
ORDER BY cc.parent_object_id, cc.name;`

func fetchCheckConstraints(db *sql.DB) (map[int64][]checkConstraint, error) {
	rows, err := db.Query(qCheckConstraints)
	if err != nil {
		return nil, fmt.Errorf("reading check constraints: %w", err)
	}
	defer closeQuietly(rows)
	byObj := make(map[int64][]checkConstraint)
	for rows.Next() {
		var (
			objID int64
			ck    checkConstraint
		)
		if err := rows.Scan(&objID, &ck.name, &ck.systemNamed, &ck.definition); err != nil {
			return nil, fmt.Errorf("reading check constraints: %w", err)
		}
		byObj[objID] = append(byObj[objID], ck)
	}
	return byObj, rows.Err()
}

// LEFT JOIN to index_columns so a clustered columnstore index (which
// has no explicit column list) still comes back as a row. Included
// columns sort after key columns.
const qIndexes = `
SELECT i.object_id, i.name, i.type, i.is_unique, ISNULL(i.filter_definition, ''),
       ISNULL(c.name, ''), ISNULL(ic.is_descending_key, 0), ISNULL(ic.is_included_column, 0)
FROM sys.indexes AS i
JOIN sys.tables AS t ON t.object_id = i.object_id
LEFT JOIN sys.index_columns AS ic ON ic.object_id = i.object_id AND ic.index_id = i.index_id
LEFT JOIN sys.columns AS c ON c.object_id = ic.object_id AND c.column_id = ic.column_id
WHERE t.is_ms_shipped = 0
  AND i.type IN (1, 2, 5, 6)
  AND i.is_primary_key = 0 AND i.is_unique_constraint = 0 AND i.is_hypothetical = 0
ORDER BY i.object_id, i.name, ic.is_included_column, ic.key_ordinal, ic.index_column_id;`

func fetchIndexes(db *sql.DB) (map[int64][]index, error) {
	rows, err := db.Query(qIndexes)
	if err != nil {
		return nil, fmt.Errorf("reading indexes: %w", err)
	}
	defer closeQuietly(rows)
	byObj := make(map[int64][]index)
	for rows.Next() {
		var (
			objID      int64
			name       string
			typ        int
			unique     bool
			filter     string
			colName    string
			descending bool
			included   bool
		)
		if err := rows.Scan(&objID, &name, &typ, &unique, &filter, &colName, &descending, &included); err != nil {
			return nil, fmt.Errorf("reading indexes: %w", err)
		}
		list := byObj[objID]
		if n := len(list); n == 0 || list[n-1].name != name {
			list = append(list, index{name: name, typ: typ, unique: unique, filter: filter})
		}
		ix := &list[len(list)-1]
		switch {
		case colName == "":
			// clustered columnstore: no column list
		case typ >= 5 || included:
			ix.includeCols = append(ix.includeCols, bracket(colName))
		case descending:
			ix.keyCols = append(ix.keyCols, bracket(colName)+" DESC")
		default:
			ix.keyCols = append(ix.keyCols, bracket(colName)+" ASC")
		}
		byObj[objID] = list
	}
	return byObj, rows.Err()
}

func columnLine(obj string, c column) (string, error) {
	if c.isComputed {
		if c.computeDef == "" {
			return "", noDefinition("computed column "+bracket(c.name), obj)
		}
		line := bracket(c.name) + " AS " + c.computeDef
		if c.persisted {
			line += " PERSISTED"
		}
		return line, nil
	}
	line := bracket(c.name) + " " + c.typeStr
	if c.collation != "" {
		line += " COLLATE " + c.collation
	}
	if c.identity != "" {
		line += " " + c.identity
	}
	if c.rowguid {
		line += " ROWGUIDCOL"
	}
	if c.nullable {
		line += " NULL"
	} else {
		line += " NOT NULL"
	}
	if c.hasDefault {
		if c.defDef == "" {
			return "", noDefinition("default for column "+bracket(c.name), obj)
		}
		if c.defName != "" {
			line += " CONSTRAINT " + bracket(c.defName)
		}
		line += " DEFAULT " + c.defDef
	}
	return line, nil
}

func keyConstraintSQL(k keyConstraint) string {
	var b strings.Builder
	if !k.systemNamed {
		b.WriteString("CONSTRAINT " + bracket(k.name) + " ")
	}
	if k.isPK {
		b.WriteString("PRIMARY KEY ")
	} else {
		b.WriteString("UNIQUE ")
	}
	if k.clustered {
		b.WriteString("CLUSTERED ")
	} else {
		b.WriteString("NONCLUSTERED ")
	}
	b.WriteString("(" + strings.Join(k.cols, ", ") + ")")
	return b.String()
}

func foreignKeySQL(schema, table string, fk foreignKey) string {
	s := "ALTER TABLE " + qualified(schema, table) + " ADD "
	if !fk.systemNamed {
		s += "CONSTRAINT " + bracket(fk.name) + " "
	}
	s += fmt.Sprintf("FOREIGN KEY (%s) REFERENCES %s (%s)",
		strings.Join(fk.cols, ", "),
		qualified(fk.refSchema, fk.refTable),
		strings.Join(fk.refCols, ", "))
	if fk.onDelete != "NO_ACTION" {
		s += " ON DELETE " + strings.ReplaceAll(fk.onDelete, "_", " ")
	}
	if fk.onUpdate != "NO_ACTION" {
		s += " ON UPDATE " + strings.ReplaceAll(fk.onUpdate, "_", " ")
	}
	return s + ";"
}

func checkConstraintSQL(schema, table string, ck checkConstraint) (string, error) {
	if ck.definition == "" {
		return "", noDefinition("check constraint "+bracket(ck.name), qualified(schema, table))
	}
	s := "ALTER TABLE " + qualified(schema, table) + " ADD "
	if !ck.systemNamed {
		s += "CONSTRAINT " + bracket(ck.name) + " "
	}
	return s + "CHECK " + ck.definition + ";", nil
}

func indexSQL(schema, table string, ix index) string {
	target := qualified(schema, table)
	switch ix.typ {
	case 5:
		return fmt.Sprintf("CREATE CLUSTERED COLUMNSTORE INDEX %s ON %s;", bracket(ix.name), target)
	case 6:
		return fmt.Sprintf("CREATE NONCLUSTERED COLUMNSTORE INDEX %s ON %s (%s);",
			bracket(ix.name), target, strings.Join(ix.includeCols, ", "))
	}
	s := "CREATE "
	if ix.unique {
		s += "UNIQUE "
	}
	if ix.typ == 1 {
		s += "CLUSTERED "
	} else {
		s += "NONCLUSTERED "
	}
	s += fmt.Sprintf("INDEX %s ON %s (%s)", bracket(ix.name), target, strings.Join(ix.keyCols, ", "))
	if len(ix.includeCols) > 0 {
		s += " INCLUDE (" + strings.Join(ix.includeCols, ", ") + ")"
	}
	if ix.filter != "" {
		s += " WHERE " + ix.filter
	}
	return s + ";"
}

func buildTableSQL(schema, name string, objID int64, cat *tableCatalog) (string, error) {
	obj := qualified(schema, name)
	var lines []string
	for _, c := range cat.columns[objID] {
		line, err := columnLine(obj, c)
		if err != nil {
			return "", err
		}
		lines = append(lines, "    "+line)
	}
	for _, k := range cat.keys[objID] {
		lines = append(lines, "    "+keyConstraintSQL(k))
	}
	stmts := []string{fmt.Sprintf("CREATE TABLE %s (\n%s\n);", obj, strings.Join(lines, ",\n"))}
	for _, fk := range cat.fks[objID] {
		stmts = append(stmts, foreignKeySQL(schema, name, fk))
	}
	for _, ck := range cat.checks[objID] {
		stmt, err := checkConstraintSQL(schema, name, ck)
		if err != nil {
			return "", err
		}
		stmts = append(stmts, stmt)
	}
	for _, ix := range cat.indexes[objID] {
		stmts = append(stmts, indexSQL(schema, name, ix))
	}
	return strings.Join(stmts, "\n\n"), nil
}

// buildTableTypeSQL scripts a user-defined table type. Constraint names
// inside a table type are always system-generated, so they are omitted.
func buildTableTypeSQL(schema, name string, objID int64, cat *tableCatalog) (string, error) {
	obj := qualified(schema, name)
	var lines []string
	for _, c := range cat.columns[objID] {
		c.defName = ""
		line, err := columnLine(obj, c)
		if err != nil {
			return "", err
		}
		lines = append(lines, "    "+line)
	}
	for _, k := range cat.keys[objID] {
		k.systemNamed = true
		lines = append(lines, "    "+keyConstraintSQL(k))
	}
	return fmt.Sprintf("CREATE TYPE %s AS TABLE (\n%s\n);", obj, strings.Join(lines, ",\n")), nil
}

func scriptTables(db *sql.DB, root string, cat *tableCatalog, counts map[string]int) error {
	tables, err := fetchTables(db)
	if err != nil {
		return err
	}
	for _, t := range tables {
		text, err := buildTableSQL(t.schema, t.name, t.objectID, cat)
		if err != nil {
			return err
		}
		path := filepath.Join(root, safeName(t.schema), "tables", safeName(t.name)+".sql")
		if err := writeSQLFile(path, text); err != nil {
			return err
		}
		counts["tables"]++
	}
	return nil
}
