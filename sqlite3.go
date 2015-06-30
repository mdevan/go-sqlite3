// Copyright (C) 2014 Yasuhiro Matsumoto <mattn.jp@gmail.com>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
// ---
// Further opinionated, backward-incompatible changes by Mahadevan.

package sqlite3

/*
#cgo CFLAGS: -I.
// code generation
#cgo CFLAGS: -Wall -O2 -std=c99
// next line depends on gcc version, unfortunately
// #cgo amd64 CFLAGS: -msse4.2 -march=corei7 -mtune=corei7
// platform configuration
#cgo CFLAGS: -DHAVE_FDATASYNC
#cgo CFLAGS: -DHAVE_GMTIME_R
#cgo CFLAGS: -DHAVE_ISNAN
#cgo CFLAGS: -DHAVE_LOCALTIME_R
#cgo linux CFLAGS: -DHAVE_STRCHRNUL
#cgo CFLAGS: -DHAVE_USLEEP
#cgo CFLAGS: -DHAVE_UTIME
// feature configuration
#cgo CFLAGS: -DSQLITE_DEFAULT_AUTOMATIC_INDEX=0
#cgo CFLAGS: -DSQLITE_DEFAULT_PAGE_SIZE=4096
#cgo CFLAGS: -DSQLITE_THREADSAFE=1
#cgo CFLAGS: -DSQLITE_ENABLE_STAT4
#cgo CFLAGS: -DSQLITE_DISABLE_DIRSYNC
#cgo CFLAGS: -DSQLITE_DEFAULT_FOREIGN_KEYS=1
// linker
#cgo linux LDFLAGS: -ldl

#include <sqlite3.h>
#include <stdlib.h>
#include <string.h>
#include <regex.h>

#ifdef __CYGWIN__
# include <errno.h>
#endif

static int
_sqlite3_open_v2(const char *filename, sqlite3 **ppDb, int flags, const char *zVfs) {
#ifdef SQLITE_OPEN_URI
  return sqlite3_open_v2(filename, ppDb, flags | SQLITE_OPEN_URI, zVfs);
#else
  return sqlite3_open_v2(filename, ppDb, flags, zVfs);
#endif
}

static int
_sqlite3_bind_text(sqlite3_stmt *stmt, int n, char *p, int np) {
  return sqlite3_bind_text(stmt, n, p, np, SQLITE_TRANSIENT);
}

static int
_sqlite3_bind_blob(sqlite3_stmt *stmt, int n, void *p, int np) {
  return sqlite3_bind_blob(stmt, n, p, np, SQLITE_TRANSIENT);
}

#include <stdio.h>
#include <stdint.h>

static long
_sqlite3_last_insert_rowid(sqlite3* db) {
  return (long) sqlite3_last_insert_rowid(db);
}

static long
_sqlite3_changes(sqlite3* db) {
  return (long) sqlite3_changes(db);
}

static void
regex_func(sqlite3_context * ctx, int argc, sqlite3_value ** argv)
{
	const char * expr = (const char *)sqlite3_value_text(argv[0]);
	const char * val  = (const char *)sqlite3_value_text(argv[1]);

	// TODO: cache the compiled regex at connection scope

	regex_t preg;
	int rc = regcomp(&preg, expr, REG_EXTENDED|REG_ICASE|REG_NOSUB);
	if (rc != 0) {
		char tmp[1024] = {0};
		regerror(rc, &preg, tmp, sizeof(tmp));
		sqlite3_result_error(ctx, tmp, -1);
	} else {
		int result = (regexec(&preg, val, 0, 0, 0) == 0);
		regfree(&preg);
		sqlite3_result_int64(ctx, result);
	}
}

static int
register_regex_func(sqlite3 * db)
{
	return sqlite3_create_function(db, "regexp", 2,
		SQLITE_UTF8|SQLITE_DETERMINISTIC, 0, regex_func, 0, 0);
}

*/
import "C"
import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unsafe"
)

// Timestamp formats understood by both this module and SQLite.
// The first format in the slice will be used when saving time values
// into the database. When parsing a string from a timestamp or
// datetime column, the formats are tried in order.
var SQLiteTimestampFormats = []string{
	"2006-01-02 15:04:05.999999999",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02 15:04:05",
	"2006-01-02T15:04:05",
	"2006-01-02 15:04",
	"2006-01-02T15:04",
	"2006-01-02",
	"2006-01-02 15:04:05-07:00",
}

func init() {
	sql.Register("sqlite3", &SQLiteDriver{})
}

// Return SQLite library Version information.
func Version() (libVersion string, libVersionNumber int, sourceId string) {
	libVersion = C.GoString(C.sqlite3_libversion())
	libVersionNumber = int(C.sqlite3_libversion_number())
	sourceId = C.GoString(C.sqlite3_sourceid())
	return libVersion, libVersionNumber, sourceId
}

// Driver struct.
type SQLiteDriver struct {
}

// Conn struct.
type SQLiteConn struct {
	db *C.sqlite3
}

// Tx struct.
type SQLiteTx struct {
	c *SQLiteConn
}

// Stmt struct.
type SQLiteStmt struct {
	c      *SQLiteConn
	s      *C.sqlite3_stmt
	t      string
	closed bool
	cls    bool
}

// Result struct.
type SQLiteResult struct {
	id      int64
	changes int64
}

// Rows struct.
type SQLiteRows struct {
	s        *SQLiteStmt
	nc       int
	cols     []string
	decltype []string
	cls      bool
}

// Commit transaction.
func (tx *SQLiteTx) Commit() error {
	_, err := tx.c.exec("COMMIT")
	return err
}

// Rollback transaction.
func (tx *SQLiteTx) Rollback() error {
	_, err := tx.c.exec("ROLLBACK")
	return err
}

// AutoCommit return which currently auto commit or not.
func (c *SQLiteConn) AutoCommit() bool {
	return int(C.sqlite3_get_autocommit(c.db)) != 0
}

func (c *SQLiteConn) lastError() Error {
	return Error{
		Code:         ErrNo(C.sqlite3_errcode(c.db)),
		ExtendedCode: ErrNoExtended(C.sqlite3_extended_errcode(c.db)),
		err:          C.GoString(C.sqlite3_errmsg(c.db)),
	}
}

// Implements Execer
func (c *SQLiteConn) Exec(query string, args []driver.Value) (driver.Result, error) {
	if len(args) == 0 {
		return c.exec(query)
	}

	for {
		s, err := c.Prepare(query)
		if err != nil {
			return nil, err
		}
		var res driver.Result
		if s.(*SQLiteStmt).s != nil {
			na := s.NumInput()
			if len(args) < na {
				return nil, fmt.Errorf("Not enough args to execute query. Expected %d, got %d.", na, len(args))
			}
			res, err = s.Exec(args[:na])
			if err != nil && err != driver.ErrSkip {
				s.Close()
				return nil, err
			}
			args = args[na:]
		}
		tail := s.(*SQLiteStmt).t
		s.Close()
		if tail == "" {
			return res, nil
		}
		query = tail
	}
}

// Implements Queryer
func (c *SQLiteConn) Query(query string, args []driver.Value) (driver.Rows, error) {
	for {
		s, err := c.Prepare(query)
		if err != nil {
			return nil, err
		}
		s.(*SQLiteStmt).cls = true
		na := s.NumInput()
		if len(args) < na {
			return nil, fmt.Errorf("Not enough args to execute query. Expected %d, got %d.", na, len(args))
		}
		rows, err := s.Query(args[:na])
		if err != nil && err != driver.ErrSkip {
			s.Close()
			return nil, err
		}
		args = args[na:]
		tail := s.(*SQLiteStmt).t
		if tail == "" {
			return rows, nil
		}
		rows.Close()
		s.Close()
		query = tail
	}
}

func (c *SQLiteConn) exec(cmd string) (driver.Result, error) {
	pcmd := C.CString(cmd)
	defer C.free(unsafe.Pointer(pcmd))
	rv := C.sqlite3_exec(c.db, pcmd, nil, nil, nil)
	if rv != C.SQLITE_OK {
		return nil, c.lastError()
	}
	return &SQLiteResult{
		int64(C._sqlite3_last_insert_rowid(c.db)),
		int64(C._sqlite3_changes(c.db)),
	}, nil
}

// Begin transaction.
func (c *SQLiteConn) Begin() (driver.Tx, error) {
	if _, err := c.exec("BEGIN"); err != nil {
		return nil, err
	}
	return &SQLiteTx{c}, nil
}

func errorString(err Error) string {
	return C.GoString(C.sqlite3_errstr(C.int(err.Code)))
}

// Open database and return a new connection.
// You can specify DSN string with URI filename.
//   test.db
//   file:test.db?cache=shared&mode=memory
//   :memory:
//   file::memory:
func (d *SQLiteDriver) Open(dsn string) (driver.Conn, error) {
	if C.sqlite3_threadsafe() == 0 {
		return nil, errors.New("sqlite library was not compiled for thread-safe operation")
	}

	var db *C.sqlite3
	name := C.CString(dsn)
	defer C.free(unsafe.Pointer(name))
	rv := C._sqlite3_open_v2(name, &db,
		C.SQLITE_OPEN_FULLMUTEX|
			C.SQLITE_OPEN_READWRITE|
			C.SQLITE_OPEN_CREATE,
		nil)
	if rv != 0 {
		return nil, Error{Code: ErrNo(rv)}
	}
	if db == nil {
		return nil, errors.New("sqlite succeeded without returning a database")
	}

	rv = C.sqlite3_busy_timeout(db, 5000)
	if rv != C.SQLITE_OK {
		return nil, Error{Code: ErrNo(rv)}
	}

	conn := &SQLiteConn{db}

	rv = C.register_regex_func(db)
	if rv != C.SQLITE_OK {
		return nil, errors.New(C.GoString(C.sqlite3_errmsg(db)))
	}

	runtime.SetFinalizer(conn, (*SQLiteConn).Close)
	return conn, nil
}

// Close the connection.
func (c *SQLiteConn) Close() error {
	rv := C.sqlite3_close_v2(c.db)
	if rv != C.SQLITE_OK {
		return c.lastError()
	}
	c.db = nil
	runtime.SetFinalizer(c, nil)
	return nil
}

// Prepare query string. Return a new statement.
func (c *SQLiteConn) Prepare(query string) (driver.Stmt, error) {
	pquery := C.CString(query)
	defer C.free(unsafe.Pointer(pquery))
	var s *C.sqlite3_stmt
	var tail *C.char
	rv := C.sqlite3_prepare_v2(c.db, pquery, -1, &s, &tail)
	if rv != C.SQLITE_OK {
		return nil, c.lastError()
	}
	var t string
	if tail != nil && C.strlen(tail) > 0 {
		t = strings.TrimSpace(C.GoString(tail))
	}
	ss := &SQLiteStmt{c: c, s: s, t: t}
	runtime.SetFinalizer(ss, (*SQLiteStmt).Close)
	return ss, nil
}

// Close the statement.
func (s *SQLiteStmt) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	if s.c == nil || s.c.db == nil {
		return errors.New("sqlite statement with already closed database connection")
	}
	rv := C.sqlite3_finalize(s.s)
	if rv != C.SQLITE_OK {
		return s.c.lastError()
	}
	runtime.SetFinalizer(s, nil)
	return nil
}

// Return a number of parameters.
func (s *SQLiteStmt) NumInput() int {
	return int(C.sqlite3_bind_parameter_count(s.s))
}

func (s *SQLiteStmt) bind(args []driver.Value) error {
	rv := C.sqlite3_reset(s.s)
	if rv != C.SQLITE_ROW && rv != C.SQLITE_OK && rv != C.SQLITE_DONE {
		return s.c.lastError()
	}

	for i, v := range args {
		n := C.int(i + 1)
		switch v := v.(type) {
		case nil:
			rv = C.sqlite3_bind_null(s.s, n)
		case string:
			if len(v) == 0 {
				b := []byte{0}
				rv = C._sqlite3_bind_text(s.s, n, (*C.char)(unsafe.Pointer(&b[0])), C.int(0))
			} else {
				b := []byte(v)
				rv = C._sqlite3_bind_text(s.s, n, (*C.char)(unsafe.Pointer(&b[0])), C.int(len(b)))
			}
		case int64:
			rv = C.sqlite3_bind_int64(s.s, n, C.sqlite3_int64(v))
		case bool:
			if bool(v) {
				rv = C.sqlite3_bind_int(s.s, n, 1)
			} else {
				rv = C.sqlite3_bind_int(s.s, n, 0)
			}
		case float64:
			rv = C.sqlite3_bind_double(s.s, n, C.double(v))
		case []byte:
			var p *byte
			if len(v) > 0 {
				p = &v[0]
			}
			rv = C._sqlite3_bind_blob(s.s, n, unsafe.Pointer(p), C.int(len(v)))
		case time.Time:
			b := []byte(v.UTC().Format(SQLiteTimestampFormats[0]))
			rv = C._sqlite3_bind_text(s.s, n, (*C.char)(unsafe.Pointer(&b[0])), C.int(len(b)))
		}
		if rv != C.SQLITE_OK {
			return s.c.lastError()
		}
	}
	return nil
}

// Query the statement with arguments. Return records.
func (s *SQLiteStmt) Query(args []driver.Value) (driver.Rows, error) {
	if err := s.bind(args); err != nil {
		return nil, err
	}
	return &SQLiteRows{s, int(C.sqlite3_column_count(s.s)), nil, nil, s.cls}, nil
}

// Return last inserted ID.
func (r *SQLiteResult) LastInsertId() (int64, error) {
	return r.id, nil
}

// Return how many rows affected.
func (r *SQLiteResult) RowsAffected() (int64, error) {
	return r.changes, nil
}

// Execute the statement with arguments. Return result object.
func (s *SQLiteStmt) Exec(args []driver.Value) (driver.Result, error) {
	if err := s.bind(args); err != nil {
		C.sqlite3_reset(s.s)
		return nil, err
	}
	rv := C.sqlite3_step(s.s)
	if rv != C.SQLITE_ROW && rv != C.SQLITE_OK && rv != C.SQLITE_DONE {
		err := s.c.lastError()
		C.sqlite3_reset(s.s)
		return nil, err
	}

	res := &SQLiteResult{
		int64(C._sqlite3_last_insert_rowid(s.c.db)),
		int64(C._sqlite3_changes(s.c.db)),
	}
	return res, nil
}

// Close the rows.
func (rc *SQLiteRows) Close() error {
	if rc.s.closed {
		return nil
	}
	if rc.cls {
		return rc.s.Close()
	}
	rv := C.sqlite3_reset(rc.s.s)
	if rv != C.SQLITE_OK {
		return rc.s.c.lastError()
	}
	return nil
}

// Return column names.
func (rc *SQLiteRows) Columns() []string {
	if rc.nc != len(rc.cols) {
		rc.cols = make([]string, rc.nc)
		for i := 0; i < rc.nc; i++ {
			rc.cols[i] = C.GoString(C.sqlite3_column_name(rc.s.s, C.int(i)))
		}
	}
	return rc.cols
}

// Move cursor to next.
func (rc *SQLiteRows) Next(dest []driver.Value) error {
	rv := C.sqlite3_step(rc.s.s)
	if rv == C.SQLITE_DONE {
		return io.EOF
	}
	if rv != C.SQLITE_ROW {
		rv = C.sqlite3_reset(rc.s.s)
		if rv != C.SQLITE_OK {
			return rc.s.c.lastError()
		}
		return nil
	}

	if rc.decltype == nil {
		rc.decltype = make([]string, rc.nc)
		for i := 0; i < rc.nc; i++ {
			rc.decltype[i] = strings.ToLower(C.GoString(C.sqlite3_column_decltype(rc.s.s, C.int(i))))
		}
	}

	for i := range dest {
		switch C.sqlite3_column_type(rc.s.s, C.int(i)) {
		case C.SQLITE_INTEGER:
			val := int64(C.sqlite3_column_int64(rc.s.s, C.int(i)))
			switch rc.decltype[i] {
			case "timestamp", "datetime", "date":
				unixTimestamp := strconv.FormatInt(val, 10)
				if len(unixTimestamp) == 13 {
					duration, err := time.ParseDuration(unixTimestamp + "ms")
					if err != nil {
						return fmt.Errorf("error parsing %s value %d, %s", rc.decltype[i], val, err)
					}
					epoch := time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)
					dest[i] = epoch.Add(duration)
				} else {
					dest[i] = time.Unix(val, 0).Local()
				}

			case "boolean":
				dest[i] = val > 0
			default:
				dest[i] = val
			}
		case C.SQLITE_FLOAT:
			dest[i] = float64(C.sqlite3_column_double(rc.s.s, C.int(i)))
		case C.SQLITE_BLOB:
			p := C.sqlite3_column_blob(rc.s.s, C.int(i))
			if p == nil {
				dest[i] = nil
				continue
			}
			n := int(C.sqlite3_column_bytes(rc.s.s, C.int(i)))
			switch dest[i].(type) {
			case sql.RawBytes:
				dest[i] = (*[1 << 30]byte)(unsafe.Pointer(p))[0:n]
			default:
				slice := make([]byte, n)
				copy(slice[:], (*[1 << 30]byte)(unsafe.Pointer(p))[0:n])
				dest[i] = slice
			}
		case C.SQLITE_NULL:
			dest[i] = nil
		case C.SQLITE_TEXT:
			var err error
			var timeVal time.Time
			s := C.GoString((*C.char)(unsafe.Pointer(C.sqlite3_column_text(rc.s.s, C.int(i)))))

			switch rc.decltype[i] {
			case "timestamp", "datetime", "date":
				for _, format := range SQLiteTimestampFormats {
					if timeVal, err = time.ParseInLocation(format, s, time.UTC); err == nil {
						dest[i] = timeVal.Local()
						break
					}
				}
				if err != nil {
					// The column is a time value, so return the zero time on parse failure.
					dest[i] = time.Time{}
				}
			default:
				dest[i] = []byte(s)
			}

		}
	}
	return nil
}
