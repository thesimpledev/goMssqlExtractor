package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"slices"
	"testing"
)

// fakeRows is a canned driver.Rows result for one query.
type fakeRows struct {
	cols     []string
	data     [][]driver.Value
	i        int
	closeErr error
}

func (r *fakeRows) Columns() []string { return r.cols }
func (r *fakeRows) Close() error      { return r.closeErr }

func (r *fakeRows) Next(dest []driver.Value) error {
	if r.i >= len(r.data) {
		return io.EOF
	}
	copy(dest, r.data[r.i])
	r.i++
	return nil
}

// fakeDB is a driver.Connector whose connections answer each query with
// the fakeRows registered under that exact query text, and record every
// query text they are asked.
type fakeDB struct {
	results map[string]*fakeRows
	queried []string
}

func (f *fakeDB) Connect(context.Context) (driver.Conn, error) { return &fakeConn{db: f}, nil }
func (f *fakeDB) Driver() driver.Driver                        { return fakeDriver{} }

type fakeDriver struct{}

func (fakeDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("fake driver: open through sql.OpenDB")
}

type fakeConn struct{ db *fakeDB }

func (c *fakeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("fake driver: Prepare not supported")
}
func (c *fakeConn) Close() error { return nil }
func (c *fakeConn) Begin() (driver.Tx, error) {
	return nil, errors.New("fake driver: Begin not supported")
}

func (c *fakeConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	c.db.queried = append(c.db.queried, query)
	r, ok := c.db.results[query]
	if !ok {
		return nil, fmt.Errorf("fake driver: no result registered for query %.60q", query)
	}
	fresh := *r
	fresh.i = 0
	return &fresh, nil
}

func openFake(t *testing.T, results map[string]*fakeRows) (*sql.DB, *fakeDB) {
	t.Helper()
	f := &fakeDB{results: results}
	db := sql.OpenDB(f)
	t.Cleanup(func() { _ = db.Close() })
	return db, f
}

// emptyResults registers a zero-row result for every query the tool
// issues, so any code path can run against the fake without a server.
func emptyResults() map[string]*fakeRows {
	queries := []string{qDatabaseInfo, qSchemas, qAliasTypes, qTableTypes, qSequences, qModules,
		qTables, qColumns, qKeyConstraints, qForeignKeys, qCheckConstraints, qIndexes}
	m := make(map[string]*fakeRows, len(queries))
	for _, q := range queries {
		m[q] = &fakeRows{}
	}
	return m
}

func (f *fakeDB) wasQueried(query string) bool {
	return slices.Contains(f.queried, query)
}

func TestFakeDBReturnsRows(t *testing.T) {
	db, f := openFake(t, map[string]*fakeRows{
		qTables: {
			cols: []string{"object_id", "schema", "name"},
			data: [][]driver.Value{{int64(1), "dbo", "T"}, {int64(2), "dbo", "U"}},
		},
	})
	got, err := fetchTables(db)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].name != "T" || got[1].objectID != 2 {
		t.Errorf("got %+v", got)
	}
	if !f.wasQueried(qTables) {
		t.Error("qTables should be recorded as queried")
	}
	if _, err := db.Query(qSchemas); err == nil {
		t.Error("unregistered query should fail")
	}
}
