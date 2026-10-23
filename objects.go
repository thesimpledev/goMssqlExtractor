package main

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
)

// schema_id < 16384 excludes the schemas owned by the fixed database
// roles (db_owner and friends).
const qSchemas = `
SELECT s.name, dp.name
FROM sys.schemas AS s
JOIN sys.database_principals AS dp ON dp.principal_id = s.principal_id
WHERE s.schema_id < 16384
  AND s.name NOT IN ('dbo', 'guest', 'sys', 'INFORMATION_SCHEMA')
ORDER BY s.name;`

func scriptSchemas(db *sql.DB, root string, counts map[string]int) error {
	rows, err := db.Query(qSchemas)
	if err != nil {
		return fmt.Errorf("reading schemas: %w", err)
	}
	defer closeQuietly(rows)
	for rows.Next() {
		var name, owner string
		if err := rows.Scan(&name, &owner); err != nil {
			return fmt.Errorf("reading schemas: %w", err)
		}
		text := "CREATE SCHEMA " + bracket(name)
		if owner != "dbo" {
			text += " AUTHORIZATION " + bracket(owner)
		}
		text += ";"
		path := filepath.Join(root, "schemas", safeName(name)+".sql")
		if err := writeSQLFile(path, text); err != nil {
			return err
		}
		counts["schemas"]++
	}
	return rows.Err()
}

func scriptTypes(db *sql.DB, root string, cat *tableCatalog, counts map[string]int) error {
	if err := scriptAliasTypes(db, root, counts); err != nil {
		return err
	}
	return scriptTableTypes(db, root, cat, counts)
}

const qAliasTypes = `
SELECT SCHEMA_NAME(ty.schema_id), ty.name, TYPE_NAME(ty.system_type_id),
       ty.max_length, ty.precision, ty.scale, ty.is_nullable
FROM sys.types AS ty
WHERE ty.is_user_defined = 1 AND ty.is_table_type = 0
ORDER BY SCHEMA_NAME(ty.schema_id), ty.name;`

func scriptAliasTypes(db *sql.DB, root string, counts map[string]int) error {
	rows, err := db.Query(qAliasTypes)
	if err != nil {
		return fmt.Errorf("reading user-defined types: %w", err)
	}
	defer closeQuietly(rows)
	for rows.Next() {
		var (
			schema, name, baseName   string
			maxLen, precision, scale int
			nullable                 bool
		)
		if err := rows.Scan(&schema, &name, &baseName, &maxLen, &precision, &scale, &nullable); err != nil {
			return fmt.Errorf("reading user-defined types: %w", err)
		}
		nullability := "NOT NULL"
		if nullable {
			nullability = "NULL"
		}
		text := fmt.Sprintf("CREATE TYPE %s FROM %s %s;",
			qualified(schema, name),
			typeString(typeSpec{name: baseName, maxLen: maxLen, precision: precision, scale: scale}),
			nullability)
		path := filepath.Join(root, safeName(schema), "types", safeName(name)+".sql")
		if err := writeSQLFile(path, text); err != nil {
			return err
		}
		counts["types"]++
	}
	return rows.Err()
}

const qTableTypes = `
SELECT SCHEMA_NAME(tt.schema_id), tt.name, tt.type_table_object_id
FROM sys.table_types AS tt
WHERE tt.is_user_defined = 1
ORDER BY SCHEMA_NAME(tt.schema_id), tt.name;`

func scriptTableTypes(db *sql.DB, root string, cat *tableCatalog, counts map[string]int) error {
	rows, err := db.Query(qTableTypes)
	if err != nil {
		return fmt.Errorf("reading table types: %w", err)
	}
	defer closeQuietly(rows)
	for rows.Next() {
		var (
			schema, name string
			tableObjID   int64
		)
		if err := rows.Scan(&schema, &name, &tableObjID); err != nil {
			return fmt.Errorf("reading table types: %w", err)
		}
		text, err := buildTableTypeSQL(schema, name, tableObjID, cat)
		if err != nil {
			return err
		}
		path := filepath.Join(root, safeName(schema), "types", safeName(name)+".sql")
		if err := writeSQLFile(path, text); err != nil {
			return err
		}
		counts["types"]++
	}
	return rows.Err()
}

// Sequence bounds go through CONVERT(varchar) because they are
// sql_variant values sized by the sequence's own type.
const qSequences = `
SELECT SCHEMA_NAME(sq.schema_id), sq.name,
       ty.name, SCHEMA_NAME(ty.schema_id), ty.is_user_defined,
       ty.max_length, ty.precision, ty.scale,
       CONVERT(varchar(48), sq.start_value),
       CONVERT(varchar(48), sq.increment),
       CONVERT(varchar(48), sq.minimum_value),
       CONVERT(varchar(48), sq.maximum_value),
       sq.is_cycling, sq.is_cached, ISNULL(sq.cache_size, 0)
FROM sys.sequences AS sq
JOIN sys.types AS ty ON ty.user_type_id = sq.user_type_id
WHERE sq.is_ms_shipped = 0
ORDER BY SCHEMA_NAME(sq.schema_id), sq.name;`

func scriptSequences(db *sql.DB, root string, counts map[string]int) error {
	rows, err := db.Query(qSequences)
	if err != nil {
		return fmt.Errorf("reading sequences: %w", err)
	}
	defer closeQuietly(rows)
	for rows.Next() {
		var (
			schema, name             string
			typeName, typeSchema     string
			userDefined              bool
			maxLen, precision, scale int
			start, increment         string
			minVal, maxVal           string
			cycling, cached          bool
			cacheSize                int
		)
		if err := rows.Scan(&schema, &name, &typeName, &typeSchema, &userDefined,
			&maxLen, &precision, &scale,
			&start, &increment, &minVal, &maxVal,
			&cycling, &cached, &cacheSize); err != nil {
			return fmt.Errorf("reading sequences: %w", err)
		}
		lines := []string{
			"CREATE SEQUENCE " + qualified(schema, name),
			"    AS " + typeString(typeSpec{name: typeName, schema: typeSchema, userDefined: userDefined,
				maxLen: maxLen, precision: precision, scale: scale}),
			"    START WITH " + start,
			"    INCREMENT BY " + increment,
			"    MINVALUE " + minVal,
			"    MAXVALUE " + maxVal,
		}
		if cycling {
			lines = append(lines, "    CYCLE")
		} else {
			lines = append(lines, "    NO CYCLE")
		}
		switch {
		case !cached:
			lines = append(lines, "    NO CACHE")
		case cacheSize > 0:
			lines = append(lines, fmt.Sprintf("    CACHE %d", cacheSize))
		default:
			lines = append(lines, "    CACHE")
		}
		text := strings.Join(lines, "\n") + ";"
		path := filepath.Join(root, safeName(schema), "sequences", safeName(name)+".sql")
		if err := writeSQLFile(path, text); err != nil {
			return err
		}
		counts["sequences"]++
	}
	return rows.Err()
}
