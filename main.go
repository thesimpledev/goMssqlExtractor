// Command mssqlsnap connects to a Microsoft SQL Server database with a
// read-only login and writes every schema object's definition to disk as
// one .sql file per object, suitable for committing to git and diffing
// over time.
//
// The only SQL this program issues is SELECT statements against system
// catalog views (sys.*, DB_NAME, DATABASEPROPERTYEX). It never executes
// or modifies anything in the database.
package main

import (
	"bufio"
	"context"
	"database/sql"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "github.com/microsoft/go-mssqldb"
	"golang.org/x/term"
)

// categories lists every object category the tool can script, in the
// order they appear in the summary line. The names double as the
// -include/-exclude filter values and as output directory names.
var categories = []string{"schemas", "tables", "views", "procedures", "functions", "triggers", "types", "sequences"}

const qDatabaseInfo = `
SELECT DB_NAME(),
       ISNULL(CONVERT(nvarchar(128), DATABASEPROPERTYEX(DB_NAME(), 'Collation')), '');`

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "mssqlsnap: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		server   = flag.String("server", os.Getenv("MSSQL_HOST"), "SQL Server `host` or host,port (env MSSQL_HOST)")
		port     = flag.Int("port", 1433, "TCP `port`, ignored when -server is host,port")
		database = flag.String("db", os.Getenv("MSSQL_DB"), "database `name` (env MSSQL_DB)")
		user     = flag.String("user", os.Getenv("MSSQL_USER"), "SQL login `name` (env MSSQL_USER)")
		outDir   = flag.String("out", ".", "output `directory`; files are written under <out>/<database>/")
		include  = flag.String("include", "", "comma-separated object `categories` to script; default all of: "+strings.Join(categories, ","))
		exclude  = flag.String("exclude", "", "comma-separated object `categories` to skip")
	)
	flag.Parse()
	if flag.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q (all options are flags)", flag.Arg(0))
	}

	inc, full, err := buildFilter(*include, *exclude)
	if err != nil {
		return err
	}
	host, portStr := splitServer(*server, *port)
	if err := requireConnFlags(host, *database, *user); err != nil {
		return err
	}
	password, err := resolvePassword(*user, host)
	if err != nil {
		return err
	}

	db, err := connect(host, portStr, *database, *user, password)
	if err != nil {
		return err
	}
	defer closeQuietly(db)

	var dbName, dbCollation string
	if err := db.QueryRow(qDatabaseInfo).Scan(&dbName, &dbCollation); err != nil {
		return fmt.Errorf("reading database info: %w", err)
	}

	root := filepath.Join(*outDir, safeName(dbName))
	if err := wipeOutput(root, inc, full); err != nil {
		return fmt.Errorf("cleaning %s: %w", root, err)
	}
	counts := make(map[string]int)
	if err := scriptCategories(db, root, inc, dbCollation, counts); err != nil {
		return err
	}
	if err := removeEmptyDirs(root); err != nil {
		return err
	}
	fmt.Println(summaryLine(dbName, inc, counts))
	return nil
}

func requireConnFlags(host, database, user string) error {
	switch {
	case host == "":
		return fmt.Errorf("no server: use -server or set MSSQL_HOST")
	case database == "":
		return fmt.Errorf("no database: use -db or set MSSQL_DB")
	case user == "":
		return fmt.Errorf("no user: use -user or set MSSQL_USER")
	}
	return nil
}

// scriptCategories writes every enabled object category under root and
// tallies the objects written per category into counts.
func scriptCategories(db *sql.DB, root string, inc map[string]bool, dbCollation string, counts map[string]int) error {
	if inc["schemas"] {
		if err := scriptSchemas(db, root, counts); err != nil {
			return err
		}
	}
	cat, err := catalogIfNeeded(db, inc, dbCollation)
	if err != nil {
		return err
	}
	steps := []struct {
		on  bool
		run func() error
	}{
		{inc["tables"], func() error { return scriptTables(db, root, cat, counts) }},
		{wantsModules(inc), func() error { return scriptModules(db, root, inc, counts) }},
		{inc["types"], func() error { return scriptTypes(db, root, cat, counts) }},
		{inc["sequences"], func() error { return scriptSequences(db, root, counts) }},
	}
	for _, s := range steps {
		if !s.on {
			continue
		}
		if err := s.run(); err != nil {
			return err
		}
	}
	return nil
}

// catalogIfNeeded loads the table catalog only when a category that
// needs it (tables or table types) is enabled.
func catalogIfNeeded(db *sql.DB, inc map[string]bool, dbCollation string) (*tableCatalog, error) {
	if !inc["tables"] && !inc["types"] {
		return nil, nil
	}
	return loadTableCatalog(db, dbCollation)
}

func wantsModules(inc map[string]bool) bool {
	return inc["views"] || inc["procedures"] || inc["functions"] || inc["triggers"]
}

// summaryLine renders the per-category object counts for the enabled
// categories, in the order categories lists them.
func summaryLine(dbName string, inc map[string]bool, counts map[string]int) string {
	parts := make([]string, 0, len(categories))
	for _, c := range categories {
		if inc[c] {
			parts = append(parts, fmt.Sprintf("%d %s", counts[c], c))
		}
	}
	return dbName + ": " + strings.Join(parts, ", ")
}

// buildFilter turns the -include/-exclude flags into a category set.
// The second result reports whether every category is enabled (no
// filter given), which controls whether the whole output subtree is
// wiped or only the directories being regenerated.
func buildFilter(include, exclude string) (map[string]bool, bool, error) {
	if include != "" && exclude != "" {
		return nil, false, fmt.Errorf("-include and -exclude are mutually exclusive")
	}
	valid := make(map[string]bool, len(categories))
	inc := make(map[string]bool, len(categories))
	for _, c := range categories {
		valid[c] = true
		inc[c] = include == ""
	}
	list, on := include, true
	if exclude != "" {
		list, on = exclude, false
	}
	for name := range strings.SplitSeq(list, ",") {
		name = strings.TrimSpace(strings.ToLower(name))
		if name == "" {
			continue
		}
		if !valid[name] {
			return nil, false, fmt.Errorf("unknown object category %q (valid: %s)", name, strings.Join(categories, ", "))
		}
		inc[name] = on
	}
	return inc, include == "" && exclude == "", nil
}

// splitServer accepts either "host" (paired with the -port flag) or the
// SQL Server conventional "host,port" form.
func splitServer(server string, port int) (string, string) {
	if host, p, ok := strings.Cut(server, ","); ok {
		return strings.TrimSpace(host), strings.TrimSpace(p)
	}
	return strings.TrimSpace(server), strconv.Itoa(port)
}

// resolvePassword reads MSSQL_PASSWORD, falling back to an interactive
// prompt. The password is deliberately not accepted as a flag so it
// never lands in shell history or process listings.
func resolvePassword(user, host string) (string, error) {
	if p := os.Getenv("MSSQL_PASSWORD"); p != "" {
		return p, nil
	}
	fmt.Fprintf(os.Stderr, "Password for %s@%s: ", user, host)
	if term.IsTerminal(int(os.Stdin.Fd())) {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", fmt.Errorf("reading password: %w", err)
		}
		if len(b) == 0 {
			return "", fmt.Errorf("no password: set MSSQL_PASSWORD or enter one at the prompt")
		}
		return string(b), nil
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		if err != nil {
			return "", fmt.Errorf("no password: set MSSQL_PASSWORD or pipe one on stdin")
		}
		return "", fmt.Errorf("no password: set MSSQL_PASSWORD or enter one at the prompt")
	}
	return line, nil
}

func connect(host, port, database, user, password string) (*sql.DB, error) {
	q := url.Values{}
	q.Set("database", database)
	q.Set("app name", "mssqlsnap")
	u := &url.URL{
		Scheme:   "sqlserver",
		User:     url.UserPassword(user, password),
		Host:     net.JoinHostPort(host, port),
		RawQuery: q.Encode(),
	}
	db, err := sql.Open("sqlserver", u.String())
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connecting to %s: %w", net.JoinHostPort(host, port), err)
	}
	return db, nil
}
