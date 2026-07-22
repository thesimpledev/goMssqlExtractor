package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// moduleCategory maps sys.objects.type codes to output categories.
var moduleCategory = map[string]string{
	"P":  "procedures",
	"V":  "views",
	"FN": "functions", // scalar
	"IF": "functions", // inline table-valued
	"TF": "functions", // multi-statement table-valued
	"TR": "triggers",
}

const qModules = `
SELECT SCHEMA_NAME(o.schema_id), o.name, RTRIM(o.type),
       m.definition, ISNULL(OBJECTPROPERTY(o.object_id, 'IsEncrypted'), 0)
FROM sys.sql_modules AS m
JOIN sys.objects AS o ON o.object_id = m.object_id
WHERE o.is_ms_shipped = 0
  AND o.type IN ('P', 'V', 'FN', 'IF', 'TF', 'TR')
ORDER BY SCHEMA_NAME(o.schema_id), RTRIM(o.type), o.name;`

// scriptModules writes procedures, views, functions and triggers using
// the original source text from sys.sql_modules, verbatim.
func scriptModules(db *sql.DB, root string, inc map[string]bool, counts map[string]int) error {
	rows, err := db.Query(qModules)
	if err != nil {
		return fmt.Errorf("reading sys.sql_modules: %w", err)
	}
	defer rows.Close()

	var missing []string
	for rows.Next() {
		var (
			schema, name, objType string
			definition            sql.NullString
			encrypted             int
		)
		if err := rows.Scan(&schema, &name, &objType, &definition, &encrypted); err != nil {
			return fmt.Errorf("reading sys.sql_modules: %w", err)
		}
		category := moduleCategory[objType]
		if !inc[category] {
			continue
		}
		obj := qualified(schema, name)
		if encrypted != 0 {
			fmt.Fprintf(os.Stderr, "mssqlsnap: warning: skipping %s: created WITH ENCRYPTION, definition not readable\n", obj)
			continue
		}
		if !definition.Valid {
			missing = append(missing, obj)
			continue
		}
		path := filepath.Join(root, safeName(schema), category, safeName(name)+".sql")
		if err := writeSQLFile(path, definition.String); err != nil {
			return err
		}
		counts[category]++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reading sys.sql_modules: %w", err)
	}
	if len(missing) > 0 {
		examples := missing
		if len(examples) > 3 {
			examples = examples[:3]
		}
		return fmt.Errorf("%d object definition(s) came back NULL (e.g. %s): the login lacks VIEW DEFINITION on the database; fix with GRANT VIEW DEFINITION TO <user> (see README)",
			len(missing), strings.Join(examples, ", "))
	}
	return nil
}
