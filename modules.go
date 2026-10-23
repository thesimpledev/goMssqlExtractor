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

// module is one row of sys.sql_modules joined to sys.objects.
type module struct {
	schema, name, category string
	definition             sql.NullString
	encrypted              bool
}

func scanModule(rows *sql.Rows) (module, error) {
	var (
		m         module
		objType   string
		encrypted int
	)
	if err := rows.Scan(&m.schema, &m.name, &objType, &m.definition, &encrypted); err != nil {
		return module{}, fmt.Errorf("reading sys.sql_modules: %w", err)
	}
	m.category = moduleCategory[objType]
	m.encrypted = encrypted != 0
	return m, nil
}

func writeModule(root string, m module, counts map[string]int) error {
	path := filepath.Join(root, safeName(m.schema), m.category, safeName(m.name)+".sql")
	if err := writeSQLFile(path, m.definition.String); err != nil {
		return err
	}
	counts[m.category]++
	return nil
}

// scriptModules writes procedures, views, functions and triggers using
// the original source text from sys.sql_modules, verbatim.
func scriptModules(db *sql.DB, root string, inc map[string]bool, counts map[string]int) error {
	rows, err := db.Query(qModules)
	if err != nil {
		return fmt.Errorf("reading sys.sql_modules: %w", err)
	}
	defer closeQuietly(rows)

	var missing []string
	for rows.Next() {
		m, err := scanModule(rows)
		if err != nil {
			return err
		}
		if !inc[m.category] {
			continue
		}
		if m.encrypted {
			fmt.Fprintf(os.Stderr, "mssqlsnap: warning: skipping %s: created WITH ENCRYPTION, definition not readable\n", qualified(m.schema, m.name))
			continue
		}
		if !m.definition.Valid {
			missing = append(missing, qualified(m.schema, m.name))
			continue
		}
		if err := writeModule(root, m, counts); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reading sys.sql_modules: %w", err)
	}
	return missingDefinitionsError(missing)
}

func missingDefinitionsError(missing []string) error {
	if len(missing) == 0 {
		return nil
	}
	examples := missing
	if len(examples) > 3 {
		examples = examples[:3]
	}
	return fmt.Errorf("%d object definition(s) came back NULL (e.g. %s): the login lacks VIEW DEFINITION on the database; fix with GRANT VIEW DEFINITION TO <user> (see README)",
		len(missing), strings.Join(examples, ", "))
}
