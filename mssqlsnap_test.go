package main

import (
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
		name                     string
		schema                   string
		udt                      bool
		maxLen, precision, scale int
		want                     string
	}{
		{"int", "sys", false, 4, 10, 0, "int"},
		{"decimal", "sys", false, 9, 18, 2, "decimal(18,2)"},
		{"float", "sys", false, 8, 53, 0, "float"},
		{"float", "sys", false, 4, 24, 0, "float(24)"},
		{"datetime2", "sys", false, 8, 27, 7, "datetime2(7)"},
		{"varchar", "sys", false, 255, 0, 0, "varchar(255)"},
		{"varchar", "sys", false, -1, 0, 0, "varchar(max)"},
		{"nvarchar", "sys", false, 100, 0, 0, "nvarchar(50)"},
		{"nvarchar", "sys", false, -1, 0, 0, "nvarchar(max)"},
		{"binary", "sys", false, 16, 0, 0, "binary(16)"},
		{"uniqueidentifier", "sys", false, 16, 0, 0, "uniqueidentifier"},
		{"PhoneNumber", "dbo", true, 20, 0, 0, "[dbo].[PhoneNumber]"},
	}
	for _, c := range cases {
		got := typeString(c.name, c.schema, c.udt, c.maxLen, c.precision, c.scale)
		if got != c.want {
			t.Errorf("typeString(%s): got %q want %q", c.name, got, c.want)
		}
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

func TestWipeOutputSelective(t *testing.T) {
	root := t.TempDir()
	mk := func(parts ...string) string {
		p := filepath.Join(append([]string{root}, parts...)...)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	kept := mk("dbo", "tables", "T.sql")
	wiped := mk("dbo", "procedures", "P.sql")
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
