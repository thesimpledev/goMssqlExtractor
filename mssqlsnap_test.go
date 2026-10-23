package main

import (
	"database/sql/driver"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildFilter(t *testing.T) {
	inc, full, err := buildFilter("", "")
	if err != nil || !full {
		t.Fatalf("no filter: full=%v err=%v", full, err)
	}
	for _, c := range categories {
		if !inc[c] {
			t.Errorf("no filter: %s should be enabled", c)
		}
	}

	inc, full, err = buildFilter("tables, Procedures", "")
	if err != nil || full {
		t.Fatalf("include filter: full=%v err=%v", full, err)
	}
	if !inc["tables"] || !inc["procedures"] || inc["views"] || inc["schemas"] {
		t.Errorf("include filter wrong: %v", inc)
	}

	inc, _, err = buildFilter("", "triggers")
	if err != nil {
		t.Fatal(err)
	}
	if inc["triggers"] || !inc["tables"] {
		t.Errorf("exclude filter wrong: %v", inc)
	}

	if _, _, err = buildFilter("tables", "views"); err == nil {
		t.Error("include+exclude should be rejected")
	}
	if _, _, err = buildFilter("bogus", ""); err == nil {
		t.Error("unknown category should be rejected")
	}
}

func TestSplitServer(t *testing.T) {
	if h, p := splitServer("db.example.com", 1433); h != "db.example.com" || p != "1433" {
		t.Errorf("got %s %s", h, p)
	}
	if h, p := splitServer("db.example.com,14330", 1433); h != "db.example.com" || p != "14330" {
		t.Errorf("got %s %s", h, p)
	}
}

func TestTypeString(t *testing.T) {
	cases := []struct {
		spec typeSpec
		want string
	}{
		{typeSpec{name: "int", schema: "sys", maxLen: 4, precision: 10}, "int"},
		{typeSpec{name: "decimal", schema: "sys", maxLen: 9, precision: 18, scale: 2}, "decimal(18,2)"},
		{typeSpec{name: "float", schema: "sys", maxLen: 8, precision: 53}, "float"},
		{typeSpec{name: "float", schema: "sys", maxLen: 4, precision: 24}, "float(24)"},
		{typeSpec{name: "datetime2", schema: "sys", maxLen: 8, precision: 27, scale: 7}, "datetime2(7)"},
		{typeSpec{name: "varchar", schema: "sys", maxLen: 255}, "varchar(255)"},
		{typeSpec{name: "varchar", schema: "sys", maxLen: -1}, "varchar(max)"},
		{typeSpec{name: "nvarchar", schema: "sys", maxLen: 100}, "nvarchar(50)"},
		{typeSpec{name: "nvarchar", schema: "sys", maxLen: -1}, "nvarchar(max)"},
		{typeSpec{name: "binary", schema: "sys", maxLen: 16}, "binary(16)"},
		{typeSpec{name: "uniqueidentifier", schema: "sys", maxLen: 16}, "uniqueidentifier"},
		{typeSpec{name: "PhoneNumber", schema: "dbo", userDefined: true, maxLen: 20}, "[dbo].[PhoneNumber]"},
	}
	for _, c := range cases {
		got := typeString(c.spec)
		if got != c.want {
			t.Errorf("typeString(%s): got %q want %q", c.spec.name, got, c.want)
		}
	}
}

func TestSummaryLine(t *testing.T) {
	inc := map[string]bool{"tables": true, "views": true, "schemas": false}
	counts := map[string]int{"tables": 2, "views": 1, "schemas": 9}
	if got := summaryLine("db", inc, counts); got != "db: 2 tables, 1 views" {
		t.Errorf("got %q", got)
	}
}

func TestRequireConnFlags(t *testing.T) {
	if err := requireConnFlags("h", "d", "u"); err != nil {
		t.Errorf("complete flags should pass: %v", err)
	}
	cases := []struct{ host, db, user, want string }{
		{"", "d", "u", "no server"},
		{"h", "", "u", "no database"},
		{"h", "d", "", "no user"},
	}
	for _, c := range cases {
		err := requireConnFlags(c.host, c.db, c.user)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("requireConnFlags(%q,%q,%q) = %v, want %q", c.host, c.db, c.user, err, c.want)
		}
	}
}

func TestScriptCategories(t *testing.T) {
	db, f := openFake(t, emptyResults())
	inc, _, err := buildFilter("", "")
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	if err := scriptCategories(db, t.TempDir(), inc, "", counts); err != nil {
		t.Fatal(err)
	}
	for _, c := range categories {
		if counts[c] != 0 {
			t.Errorf("%s: count %d on an empty database", c, counts[c])
		}
	}
	for _, q := range []string{qSchemas, qTables, qModules, qAliasTypes, qSequences} {
		if !f.wasQueried(q) {
			t.Errorf("query %.30q should run when every category is enabled", q)
		}
	}
}

func TestScriptCategoriesSequencesOnly(t *testing.T) {
	db, f := openFake(t, emptyResults())
	inc, _, err := buildFilter("sequences", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := scriptCategories(db, t.TempDir(), inc, "", map[string]int{}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{qSchemas, qTables, qColumns, qModules, qAliasTypes} {
		if f.wasQueried(q) {
			t.Errorf("query %.30q should not run for a sequences-only export", q)
		}
	}
	if !f.wasQueried(qSequences) {
		t.Error("sequences query should run")
	}
}

func TestBracket(t *testing.T) {
	if got := bracket("Weird]Name"); got != "[Weird]]Name]" {
		t.Errorf("got %q", got)
	}
}

func TestSafeName(t *testing.T) {
	if got := safeName(`a/b\c:d`); got != "a_b_c_d" {
		t.Errorf("got %q", got)
	}
	if got := safeName(".."); got != "_" {
		t.Errorf("got %q", got)
	}
}

func TestSafeNamePathsStayUnderRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "db")
	cases := map[string]string{
		"../x":          ".._x",
		`..\x`:          ".._x",
		"/etc/passwd":   "_etc_passwd",
		`C:\x`:          "C__x",
		"..":            "_",
		".":             "_",
		"a\x00b":        "a_b",
		"ok name":       "ok name",
		"we/ird\\n:ame": "we_ird_n_ame",
	}
	for in, want := range cases {
		if got := safeName(in); got != want {
			t.Errorf("safeName(%q) = %q, want %q", in, got, want)
		}
		p := filepath.Join(root, safeName(in), "tables", safeName(in)+".sql")
		if !strings.HasPrefix(p, root+string(filepath.Separator)) {
			t.Errorf("path for %q escapes root: %s", in, p)
		}
		rel, err := filepath.Rel(root, p)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			t.Errorf("path for %q is not under root: rel=%q err=%v", in, rel, err)
		}
	}
}

type errCloser struct{ closed bool }

func (c *errCloser) Close() error {
	c.closed = true
	return errors.New("close failed")
}

func TestCloseQuietlyIgnoresError(t *testing.T) {
	c := &errCloser{}
	closeQuietly(c)
	if !c.closed {
		t.Error("Close should have been called")
	}
}

// database/sql closes the driver rows itself once Next hits EOF and
// reports a Close error through rows.Err(), so nothing is lost when the
// deferred Close's own return value is discarded.
func TestFetchTablesReportsCloseErrorThroughRowsErr(t *testing.T) {
	db, _ := openFake(t, map[string]*fakeRows{
		qTables: {
			cols:     []string{"object_id", "schema", "name"},
			data:     [][]driver.Value{{int64(1), "dbo", "T"}},
			closeErr: errors.New("close failed"),
		},
	})
	_, err := fetchTables(db)
	if err == nil || !strings.Contains(err.Error(), "close failed") {
		t.Errorf("close error should surface via rows.Err(): %v", err)
	}
}

// On an early return the deferred Close runs with its error discarded,
// and the original error is the one reported.
func TestFetchTablesEarlyReturnKeepsOriginalError(t *testing.T) {
	db, _ := openFake(t, map[string]*fakeRows{
		qTables: {
			cols:     []string{"object_id", "schema", "name"},
			data:     [][]driver.Value{{int64(1), "dbo", nil}},
			closeErr: errors.New("close failed"),
		},
	})
	_, err := fetchTables(db)
	if err == nil || strings.Contains(err.Error(), "close failed") {
		t.Errorf("scan error should be reported, not the close error: %v", err)
	}
}

func moduleRows(data ...[]driver.Value) *fakeRows {
	return &fakeRows{
		cols: []string{"schema", "name", "type", "definition", "encrypted"},
		data: data,
	}
}

func TestScriptModules(t *testing.T) {
	root := t.TempDir()
	db, _ := openFake(t, map[string]*fakeRows{
		qModules: moduleRows(
			[]driver.Value{"dbo", "P1", "P", "CREATE PROC P1 AS SELECT 1", int64(0)},
			[]driver.Value{"dbo", "V1", "V", "CREATE VIEW V1 AS SELECT 1", int64(0)},
			[]driver.Value{"dbo", "P2", "P", "CREATE PROC P2 AS SELECT 2", int64(1)},
			[]driver.Value{"sales", "FN1", "FN", "CREATE FUNCTION FN1() RETURNS int AS BEGIN RETURN 1 END", int64(0)},
		),
	})
	inc := map[string]bool{"procedures": true, "functions": true}
	counts := map[string]int{}
	if err := scriptModules(db, root, inc, counts); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(root, "dbo", "procedures", "P1.sql"))
	if err != nil || string(b) != "CREATE PROC P1 AS SELECT 1\n" {
		t.Errorf("P1: %q err=%v", b, err)
	}
	if _, err := os.Stat(filepath.Join(root, "sales", "functions", "FN1.sql")); err != nil {
		t.Error("FN1 should be written under sales/functions")
	}
	if _, err := os.Stat(filepath.Join(root, "dbo", "views")); !os.IsNotExist(err) {
		t.Error("views are filtered out and must not be written")
	}
	if _, err := os.Stat(filepath.Join(root, "dbo", "procedures", "P2.sql")); !os.IsNotExist(err) {
		t.Error("encrypted module must be skipped")
	}
	if counts["procedures"] != 1 || counts["functions"] != 1 || counts["views"] != 0 {
		t.Errorf("counts: %v", counts)
	}
}

func TestScriptModulesReportsMissingDefinitions(t *testing.T) {
	db, _ := openFake(t, map[string]*fakeRows{
		qModules: moduleRows(
			[]driver.Value{"dbo", "P1", "P", "CREATE PROC P1 AS SELECT 1", int64(0)},
			[]driver.Value{"dbo", "P3", "P", nil, int64(0)},
		),
	})
	inc := map[string]bool{"procedures": true}
	err := scriptModules(db, t.TempDir(), inc, map[string]int{})
	if err == nil || !strings.Contains(err.Error(), "1 object definition(s)") || !strings.Contains(err.Error(), "[dbo].[P3]") {
		t.Errorf("missing definition should be reported with the object name: %v", err)
	}
}

func TestColumnLine(t *testing.T) {
	line, err := columnLine("[dbo].[T]", column{
		name: "Id", typeStr: "int", identity: "IDENTITY(1,1)",
	})
	if err != nil || line != "[Id] int IDENTITY(1,1) NOT NULL" {
		t.Errorf("identity column: %q err=%v", line, err)
	}

	line, err = columnLine("[dbo].[T]", column{
		name: "Name", typeStr: "nvarchar(100)", nullable: true,
		hasDefault: true, defName: "DF_T_Name", defDef: "(N'')",
	})
	want := "[Name] nvarchar(100) NULL CONSTRAINT [DF_T_Name] DEFAULT (N'')"
	if err != nil || line != want {
		t.Errorf("default column: %q err=%v", line, err)
	}

	line, err = columnLine("[dbo].[T]", column{
		name: "Total", isComputed: true, computeDef: "([a]+[b])", persisted: true,
	})
	if err != nil || line != "[Total] AS ([a]+[b]) PERSISTED" {
		t.Errorf("computed column: %q err=%v", line, err)
	}

	// NULL definition (missing VIEW DEFINITION) must error, not emit
	// broken DDL.
	if _, err = columnLine("[dbo].[T]", column{name: "X", isComputed: true}); err == nil {
		t.Error("computed column with NULL definition should error")
	}
	if _, err = columnLine("[dbo].[T]", column{name: "X", typeStr: "int", hasDefault: true}); err == nil {
		t.Error("default with NULL definition should error")
	}
}

func TestBuildTableSQL(t *testing.T) {
	cat := &tableCatalog{
		columns: map[int64][]column{1: {
			{name: "Id", typeStr: "int", identity: "IDENTITY(1,1)"},
			{name: "Email", typeStr: "varchar(255)", nullable: true},
		}},
		keys: map[int64][]keyConstraint{1: {
			{name: "PK_T", isPK: true, clustered: true, cols: []string{"[Id] ASC"}},
		}},
		fks: map[int64][]foreignKey{1: {
			{name: "FK_T_U", refSchema: "dbo", refTable: "U",
				cols: []string{"[Id]"}, refCols: []string{"[Id]"},
				onDelete: "CASCADE", onUpdate: "NO_ACTION"},
		}},
		checks: map[int64][]checkConstraint{1: {
			{name: "CK_T", definition: "([Id]>(0))"},
		}},
		indexes: map[int64][]index{1: {
			{name: "IX_T_Email", typ: 2, unique: true,
				keyCols: []string{"[Email] ASC"}, filter: "([Email] IS NOT NULL)"},
		}},
	}
	got, err := buildTableSQL("dbo", "T", 1, cat)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"CREATE TABLE [dbo].[T] (",
		"    [Id] int IDENTITY(1,1) NOT NULL,",
		"    [Email] varchar(255) NULL,",
		"    CONSTRAINT [PK_T] PRIMARY KEY CLUSTERED ([Id] ASC)",
		");",
		"",
		"ALTER TABLE [dbo].[T] ADD CONSTRAINT [FK_T_U] FOREIGN KEY ([Id]) REFERENCES [dbo].[U] ([Id]) ON DELETE CASCADE;",
		"",
		"ALTER TABLE [dbo].[T] ADD CONSTRAINT [CK_T] CHECK ([Id]>(0));",
		"",
		"CREATE UNIQUE NONCLUSTERED INDEX [IX_T_Email] ON [dbo].[T] ([Email] ASC) WHERE ([Email] IS NOT NULL);",
	}, "\n")
	if got != want {
		t.Errorf("table SQL mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestWriteSQLFileNormalizes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "x.sql")
	if err := writeSQLFile(path, "line1\r\nline2\rline3"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "line1\nline2\nline3\n" {
		t.Errorf("got %q", b)
	}
}

// writeStub creates a placeholder file under root and returns its path.
func writeStub(t *testing.T, root string, parts ...string) string {
	t.Helper()
	p := filepath.Join(append([]string{root}, parts...)...)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestWipeOutputSelective(t *testing.T) {
	root := t.TempDir()
	kept := writeStub(t, root, "dbo", "tables", "T.sql")
	wiped := writeStub(t, root, "dbo", "procedures", "P.sql")
	inc := map[string]bool{"procedures": true}
	if err := wipeOutput(root, inc, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(kept); err != nil {
		t.Error("tables file should survive a procedures-only wipe")
	}
	if _, err := os.Stat(wiped); !os.IsNotExist(err) {
		t.Error("procedures file should be wiped")
	}

	if err := wipeOutput(root, nil, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Error("full wipe should remove the whole subtree")
	}
}

func TestWipeOutputSchemas(t *testing.T) {
	root := t.TempDir()
	schemaFile := writeStub(t, root, "schemas", "sales.sql")
	kept := writeStub(t, root, "sales", "tables", "T.sql")
	wiped := writeStub(t, root, "sales", "views", "V.sql")
	if err := wipeOutput(root, map[string]bool{"schemas": true, "views": true}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(schemaFile); !os.IsNotExist(err) {
		t.Error("schemas directory should be wiped when schemas are included")
	}
	if _, err := os.Stat(wiped); !os.IsNotExist(err) {
		t.Error("views file should be wiped")
	}
	if _, err := os.Stat(kept); err != nil {
		t.Error("tables file should survive a schemas+views wipe")
	}
}

func TestRemoveEmptyDirs(t *testing.T) {
	root := t.TempDir()
	deep := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	full := filepath.Join(root, "keep")
	if err := os.MkdirAll(full, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(full, "f.sql"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := removeEmptyDirs(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "a")); !os.IsNotExist(err) {
		t.Error("empty tree should be pruned")
	}
	if _, err := os.Stat(full); err != nil {
		t.Error("non-empty dir should survive")
	}
}
