package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
)

// sqliteDB is a minimal reader for SQLite database files, enough to scan
// tables and look rows up through an index. It exists so trail can read
// agents that keep sessions in SQLite without cgo or a driver, and without
// taking locks or creating -shm files next to someone else's database: it
// only ever opens the database and its -wal file for reading.
//
// Pages committed to the write-ahead log override the main file, as they do
// for a real reader. The snapshot is taken when the database is opened. A
// checkpoint running at the same time can leave pages that don't fit
// together; every read is bounds-checked and comes back as an error, and the
// caller tries again on its next scan.
type sqliteDB struct {
	f        *os.File
	pageSize int
	usable   int
	wal      map[uint32][]byte // page number -> latest committed page in the WAL
	schema   []sqliteSchemaRow

	// Recently read pages. Index lookups go through the same interior pages
	// over and over; the cache is simply dropped when it fills.
	cacheMu sync.Mutex
	cache   map[uint32][]byte
}

const sqliteCachePages = 4096

type sqliteSchemaRow struct {
	typ, name, table, sql string
	root                  uint32
}

// sqliteTable is one table's root page and column names, in record order.
type sqliteTable struct {
	db     *sqliteDB
	root   uint32
	cols   []string
	rowid  int // index of an INTEGER PRIMARY KEY column (stored as the rowid), or -1
	byName map[string]int
}

// sqliteRow is one decoded record. Values are nil, int64, float64, string or
// []byte.
type sqliteRow struct {
	t    *sqliteTable
	vals []any
}

var errSQLiteCorrupt = errors.New("sqlite: malformed database (it may have changed while being read)")

func openSQLite(path string) (db *sqliteDB, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			f.Close()
			db = nil
		}
	}()
	var hdr [100]byte
	if _, err := f.ReadAt(hdr[:], 0); err != nil {
		return nil, err
	}
	if string(hdr[:16]) != "SQLite format 3\x00" {
		return nil, fmt.Errorf("sqlite: %s is not a SQLite database", path)
	}
	if enc := binary.BigEndian.Uint32(hdr[56:]); enc > 1 {
		return nil, fmt.Errorf("sqlite: unsupported text encoding %d", enc)
	}
	pageSize := int(binary.BigEndian.Uint16(hdr[16:]))
	if pageSize == 1 {
		pageSize = 65536
	}
	if pageSize < 512 || pageSize&(pageSize-1) != 0 {
		return nil, errSQLiteCorrupt
	}
	db = &sqliteDB{f: f, pageSize: pageSize, usable: pageSize - int(hdr[20])}
	if err := db.readWAL(path + "-wal"); err != nil {
		return nil, err
	}
	err = db.readSchema()
	return db, err
}

func (db *sqliteDB) Close() error { return db.f.Close() }

// readWAL indexes the frames of the write-ahead log up to its last valid
// commit. Frames after that belong to a transaction still being written, or
// to an older generation of the log, and are ignored the same way SQLite
// ignores them: by their salts and the running checksum.
func (db *sqliteDB) readWAL(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if len(b) < 32 {
		return nil
	}
	magic := binary.BigEndian.Uint32(b)
	if magic != 0x377f0682 && magic != 0x377f0683 {
		return nil
	}
	bigEndian := magic&1 == 1
	if int(binary.BigEndian.Uint32(b[8:])) != db.pageSize {
		return nil
	}
	s1, s2 := walChecksum(bigEndian, b[:24], 0, 0)
	if s1 != binary.BigEndian.Uint32(b[24:]) || s2 != binary.BigEndian.Uint32(b[28:]) {
		return nil
	}
	salt := b[16:24]
	pending := map[uint32][]byte{}
	db.wal = map[uint32][]byte{}
	frame := 24 + db.pageSize
	for off := 32; off+frame <= len(b); off += frame {
		h := b[off : off+24]
		if string(h[8:16]) != string(salt) {
			break
		}
		page := b[off+24 : off+frame]
		s1, s2 = walChecksum(bigEndian, h[:8], s1, s2)
		s1, s2 = walChecksum(bigEndian, page, s1, s2)
		if s1 != binary.BigEndian.Uint32(h[16:]) || s2 != binary.BigEndian.Uint32(h[20:]) {
			break
		}
		pending[binary.BigEndian.Uint32(h)] = page
		if binary.BigEndian.Uint32(h[4:]) != 0 { // commit frame
			for n, p := range pending {
				db.wal[n] = p
			}
			clear(pending)
		}
	}
	return nil
}

func walChecksum(bigEndian bool, b []byte, s1, s2 uint32) (uint32, uint32) {
	order := binary.ByteOrder(binary.LittleEndian)
	if bigEndian {
		order = binary.BigEndian
	}
	for i := 0; i+8 <= len(b); i += 8 {
		s1 += order.Uint32(b[i:]) + s2
		s2 += order.Uint32(b[i+4:]) + s1
	}
	return s1, s2
}

func (db *sqliteDB) page(n uint32) ([]byte, error) {
	if n == 0 {
		return nil, errSQLiteCorrupt
	}
	if p, ok := db.wal[n]; ok {
		return p, nil
	}
	db.cacheMu.Lock()
	p, ok := db.cache[n]
	db.cacheMu.Unlock()
	if ok {
		return p, nil
	}
	p = make([]byte, db.pageSize)
	if _, err := db.f.ReadAt(p, int64(n-1)*int64(db.pageSize)); err != nil {
		return nil, errSQLiteCorrupt
	}
	db.cacheMu.Lock()
	if db.cache == nil || len(db.cache) >= sqliteCachePages {
		db.cache = map[uint32][]byte{}
	}
	db.cache[n] = p
	db.cacheMu.Unlock()
	return p, nil
}

func (db *sqliteDB) readSchema() error {
	t := &sqliteTable{db: db, root: 1, cols: []string{"type", "name", "tbl_name", "rootpage", "sql"}, rowid: -1}
	return t.Scan(func(r sqliteRow) error {
		db.schema = append(db.schema, sqliteSchemaRow{
			typ: r.Str("type"), name: r.Str("name"), table: r.Str("tbl_name"),
			root: uint32(r.Int("rootpage")), sql: r.Str("sql"),
		})
		return nil
	})
}

// Table returns the named table, or nil if the database doesn't have it.
func (db *sqliteDB) Table(name string) *sqliteTable {
	for _, s := range db.schema {
		if s.typ == "table" && strings.EqualFold(s.name, name) {
			t := &sqliteTable{db: db, root: s.root, rowid: -1, byName: map[string]int{}}
			for i, def := range sqlColumnDefs(s.sql) {
				name, rest := sqlIdent(def)
				t.cols = append(t.cols, name)
				t.byName[strings.ToLower(name)] = i
				if r := strings.ToLower(strings.Join(strings.Fields(rest), " ")); strings.HasPrefix(r, "integer primary key") {
					t.rowid = i
				}
			}
			return t
		}
	}
	return nil
}

// indexOn returns the root page of an index on table whose first column is
// col, or 0 if there is none.
func (db *sqliteDB) indexOn(table, col string) uint32 {
	for _, s := range db.schema {
		if s.typ != "index" || !strings.EqualFold(s.table, table) {
			continue
		}
		first := ""
		if s.sql == "" {
			// An automatic index backs a PRIMARY KEY or UNIQUE constraint.
			// For the tables trail reads that is the text "id" column.
			if t := db.Table(table); t != nil && t.rowid < 0 && strings.HasPrefix(s.name, "sqlite_autoindex_") && strings.HasSuffix(s.name, "_1") {
				first = sqlPrimaryKey(db, table)
			}
		} else if i := strings.Index(s.sql, "("); i >= 0 {
			first, _ = sqlIdent(strings.SplitN(s.sql[i+1:], ",", 2)[0])
		}
		if strings.EqualFold(first, col) {
			return s.root
		}
	}
	return 0
}

func sqlPrimaryKey(db *sqliteDB, table string) string {
	for _, s := range db.schema {
		if s.typ == "table" && strings.EqualFold(s.name, table) {
			for _, def := range sqlColumnDefs(s.sql) {
				name, rest := sqlIdent(def)
				if strings.Contains(strings.ToLower(rest), "primary key") {
					return name
				}
			}
		}
	}
	return ""
}

// sqlColumnDefs splits a CREATE TABLE statement into its column definitions,
// leaving out table constraints.
func sqlColumnDefs(sql string) []string {
	i, j := strings.Index(sql, "("), strings.LastIndex(sql, ")")
	if i < 0 || j <= i {
		return nil
	}
	var defs []string
	depth, start := 0, i+1
	var quote byte
	for k := i + 1; k <= j; k++ {
		c := sql[k]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '`' || c == '\'':
			quote = c
		case c == '[':
			quote = ']'
		case c == '(':
			depth++
		case c == ')' && depth > 0:
			depth--
		case c == ',' && depth == 0, k == j:
			def := strings.TrimSpace(sql[start:k])
			start = k + 1
			word := strings.ToUpper(strings.SplitN(def+" ", " ", 2)[0])
			switch word {
			case "", "CONSTRAINT", "PRIMARY", "FOREIGN", "UNIQUE", "CHECK":
				continue
			}
			defs = append(defs, def)
		}
	}
	return defs
}

// sqlIdent reads a possibly quoted identifier from the start of s.
func sqlIdent(s string) (name, rest string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", ""
	}
	if end := map[byte]byte{'"': '"', '`': '`', '[': ']', '\'': '\''}[s[0]]; end != 0 {
		if k := strings.IndexByte(s[1:], end); k >= 0 {
			return s[1 : k+1], s[k+2:]
		}
	}
	k := strings.IndexAny(s, " \t\n\r(),")
	if k < 0 {
		return s, ""
	}
	return s[:k], s[k:]
}

// Scan calls fn for every row of the table in rowid order.
func (t *sqliteTable) Scan(fn func(sqliteRow) error) (err error) {
	defer recoverCorrupt(&err)
	return t.db.walkTable(t.root, func(rowid int64, payload []byte) error {
		return fn(t.row(rowid, payload))
	})
}

// Get returns the row with the given rowid.
func (t *sqliteTable) Get(rowid int64) (row sqliteRow, ok bool, err error) {
	defer recoverCorrupt(&err)
	n := t.root
	for depth := 0; depth < 64; depth++ {
		p, err := t.db.page(n)
		if err != nil {
			return row, false, err
		}
		h := t.db.header(n, p)
		switch p[h] {
		case 0x05: // interior table page
			cells := t.db.cells(n, p)
			next := binary.BigEndian.Uint32(p[h+8:])
			for _, c := range cells {
				key, _ := sqliteVarint(p[c+4:])
				if rowid <= key {
					next = binary.BigEndian.Uint32(p[c:])
					break
				}
			}
			n = next
		case 0x0d: // leaf table page
			for _, c := range t.db.cells(n, p) {
				size, k := sqliteVarint(p[c:])
				id, k2 := sqliteVarint(p[c+k:])
				if id == rowid {
					payload, err := t.db.payload(p, c+k+k2, size, false)
					if err != nil {
						return row, false, err
					}
					return t.row(id, payload), true, nil
				}
			}
			return row, false, nil
		default:
			return row, false, errSQLiteCorrupt
		}
	}
	return row, false, errSQLiteCorrupt
}

// Lookup returns the rows whose column col equals key, using an index on
// that column when there is one and scanning the table otherwise.
func (t *sqliteTable) Lookup(col, key string) ([]sqliteRow, error) {
	var name string
	for _, s := range t.db.schema {
		if s.typ == "table" && s.root == t.root {
			name = s.name
		}
	}
	root := t.db.indexOn(name, col)
	if root == 0 {
		var out []sqliteRow
		err := t.Scan(func(r sqliteRow) error {
			if r.Str(col) == key {
				out = append(out, r)
			}
			return nil
		})
		return out, err
	}
	ids, err := t.db.indexRowids(root, key)
	if err != nil {
		return nil, err
	}
	out := make([]sqliteRow, 0, len(ids))
	for _, id := range ids {
		r, ok, err := t.Get(id)
		if err != nil {
			return nil, err
		}
		if ok && r.Str(col) == key {
			out = append(out, r)
		}
	}
	return out, nil
}

// indexRowids returns the rowids of index entries whose first column is the
// text key, in index order.
func (db *sqliteDB) indexRowids(root uint32, key string) (ids []int64, err error) {
	defer recoverCorrupt(&err)
	seen := map[uint32]bool{}
	var walk func(n uint32, depth int) error
	visit := func(payload []byte) {
		vals := sqliteRecord(payload)
		if len(vals) >= 2 {
			if s, ok := vals[0].(string); ok && s == key {
				if id, ok := vals[len(vals)-1].(int64); ok {
					ids = append(ids, id)
				}
			}
		}
	}
	walk = func(n uint32, depth int) error {
		if depth > 64 || seen[n] {
			return errSQLiteCorrupt
		}
		seen[n] = true
		p, err := db.page(n)
		if err != nil {
			return err
		}
		h := db.header(n, p)
		switch p[h] {
		case 0x0a: // leaf index page
			for _, c := range db.cells(n, p) {
				size, k := sqliteVarint(p[c:])
				payload, err := db.payload(p, c+k, size, true)
				if err != nil {
					return err
				}
				visit(payload)
			}
			return nil
		case 0x02: // interior index page: each cell's left child holds keys <= the cell's key
			for _, c := range db.cells(n, p) {
				size, k := sqliteVarint(p[c+4:])
				payload, err := db.payload(p, c+4+k, size, true)
				if err != nil {
					return err
				}
				cmp := compareFirstText(payload, key)
				if cmp >= 0 {
					if err := walk(binary.BigEndian.Uint32(p[c:]), depth+1); err != nil {
						return err
					}
				}
				if cmp == 0 {
					visit(payload)
				}
				if cmp > 0 {
					return nil
				}
			}
			return walk(binary.BigEndian.Uint32(p[h+8:]), depth+1)
		}
		return errSQLiteCorrupt
	}
	err = walk(root, 0)
	return ids, err
}

// walkIndex calls fn with every entry of an index, in order. The last value
// is the rowid.
func (db *sqliteDB) walkIndex(root uint32, fn func([]any)) (err error) {
	defer recoverCorrupt(&err)
	seen := map[uint32]bool{}
	var walk func(n uint32, depth int) error
	walk = func(n uint32, depth int) error {
		if depth > 64 || seen[n] {
			return errSQLiteCorrupt
		}
		seen[n] = true
		p, err := db.page(n)
		if err != nil {
			return err
		}
		h := db.header(n, p)
		leaf := p[h] == 0x0a
		if !leaf && p[h] != 0x02 {
			return errSQLiteCorrupt
		}
		for _, c := range db.cells(n, p) {
			if !leaf {
				if err := walk(binary.BigEndian.Uint32(p[c:]), depth+1); err != nil {
					return err
				}
				c += 4
			}
			size, k := sqliteVarint(p[c:])
			payload, err := db.payload(p, c+k, size, true)
			if err != nil {
				return err
			}
			if vals := sqliteRecord(payload); len(vals) > 0 {
				fn(vals)
			}
		}
		if !leaf {
			return walk(binary.BigEndian.Uint32(p[h+8:]), depth+1)
		}
		return nil
	}
	return walk(root, 0)
}

// compareFirstText compares an index record's first column with key, using
// SQLite's ordering: NULL and numbers sort before text, text compares
// bytewise.
func compareFirstText(payload []byte, key string) int {
	vals := sqliteRecord(payload)
	if len(vals) == 0 {
		return -1
	}
	switch v := vals[0].(type) {
	case string:
		return strings.Compare(v, key)
	case []byte:
		return 1
	}
	return -1
}

func (db *sqliteDB) walkTable(root uint32, fn func(int64, []byte) error) error {
	seen := map[uint32]bool{}
	var walk func(n uint32, depth int) error
	walk = func(n uint32, depth int) error {
		if depth > 64 || seen[n] {
			return errSQLiteCorrupt // a page reached twice means a cycle
		}
		seen[n] = true
		p, err := db.page(n)
		if err != nil {
			return err
		}
		h := db.header(n, p)
		switch p[h] {
		case 0x05:
			for _, c := range db.cells(n, p) {
				if err := walk(binary.BigEndian.Uint32(p[c:]), depth+1); err != nil {
					return err
				}
			}
			return walk(binary.BigEndian.Uint32(p[h+8:]), depth+1)
		case 0x0d:
			for _, c := range db.cells(n, p) {
				size, k := sqliteVarint(p[c:])
				id, k2 := sqliteVarint(p[c+k:])
				payload, err := db.payload(p, c+k+k2, size, false)
				if err != nil {
					return err
				}
				if err := fn(id, payload); err != nil {
					return err
				}
			}
			return nil
		}
		return errSQLiteCorrupt
	}
	return walk(root, 0)
}

// header is the offset of the b-tree page header; page 1 starts with the
// 100-byte file header.
func (db *sqliteDB) header(n uint32, p []byte) int {
	if n == 1 {
		return 100
	}
	return 0
}

func (db *sqliteDB) cells(n uint32, p []byte) []int {
	h := db.header(n, p)
	hdrLen := 8
	if p[h] == 0x02 || p[h] == 0x05 {
		hdrLen = 12
	}
	count := int(binary.BigEndian.Uint16(p[h+3:]))
	out := make([]int, count)
	for i := range out {
		out[i] = int(binary.BigEndian.Uint16(p[h+hdrLen+2*i:]))
		if out[i] >= len(p) {
			panic(errSQLiteCorrupt)
		}
	}
	return out
}

// payload assembles a cell's payload of the given size starting at off,
// following overflow pages when it doesn't fit on the page.
func (db *sqliteDB) payload(p []byte, off int, size int64, index bool) ([]byte, error) {
	u := db.usable
	maxLocal := u - 35
	if index {
		maxLocal = (u-12)*64/255 - 23
	}
	if size < 0 || size > 1<<30 {
		return nil, errSQLiteCorrupt
	}
	if size <= int64(maxLocal) {
		if off+int(size) > len(p) {
			return nil, errSQLiteCorrupt
		}
		return p[off : off+int(size)], nil
	}
	minLocal := (u-12)*32/255 - 23
	local := minLocal + int((size-int64(minLocal))%int64(u-4))
	if local > maxLocal {
		local = minLocal
	}
	if off+local+4 > len(p) {
		return nil, errSQLiteCorrupt
	}
	out := make([]byte, 0, size)
	out = append(out, p[off:off+local]...)
	next := binary.BigEndian.Uint32(p[off+local:])
	for int64(len(out)) < size {
		if next == 0 {
			return nil, errSQLiteCorrupt
		}
		op, err := db.page(next)
		if err != nil {
			return nil, err
		}
		next = binary.BigEndian.Uint32(op)
		n := min(int(size)-len(out), u-4)
		out = append(out, op[4:4+n]...)
	}
	return out, nil
}

func (t *sqliteTable) row(rowid int64, payload []byte) sqliteRow {
	vals := sqliteRecord(payload)
	if t.rowid >= 0 && t.rowid < len(vals) && vals[t.rowid] == nil {
		vals[t.rowid] = rowid
	}
	return sqliteRow{t: t, vals: vals}
}

func (r sqliteRow) get(col string) any {
	i, ok := r.t.byName[strings.ToLower(col)]
	if r.t.byName == nil {
		for k, c := range r.t.cols {
			if c == col {
				i, ok = k, true
			}
		}
	}
	if !ok || i >= len(r.vals) {
		return nil // also columns added by ALTER TABLE after this row was written
	}
	return r.vals[i]
}

func (r sqliteRow) Str(col string) string {
	switch v := r.get(col).(type) {
	case string:
		return v
	case []byte:
		return string(v)
	}
	return ""
}

func (r sqliteRow) Int(col string) int64 {
	switch v := r.get(col).(type) {
	case int64:
		return v
	case float64:
		return int64(v)
	}
	return 0
}

func (r sqliteRow) Float(col string) float64 {
	switch v := r.get(col).(type) {
	case int64:
		return float64(v)
	case float64:
		return v
	}
	return 0
}

func (r sqliteRow) Null(col string) bool { return r.get(col) == nil }

// sqliteRecord decodes the record format: a header of serial types, then the
// values. It panics with errSQLiteCorrupt on a malformed record; callers
// recover it into an error.
func sqliteRecord(b []byte) []any {
	hlen, k := sqliteVarint(b)
	if hlen < int64(k) || hlen > int64(len(b)) {
		panic(errSQLiteCorrupt)
	}
	var types []int64
	for i := k; i < int(hlen); {
		t, n := sqliteVarint(b[i:])
		types = append(types, t)
		i += n
	}
	vals := make([]any, len(types))
	off := int(hlen)
	need := func(n int) []byte {
		if off+n > len(b) {
			panic(errSQLiteCorrupt)
		}
		v := b[off : off+n]
		off += n
		return v
	}
	for i, t := range types {
		switch {
		case t == 0:
			vals[i] = nil
		case t >= 1 && t <= 6:
			n := []int{1, 2, 3, 4, 6, 8}[t-1]
			v := need(n)
			x := int64(int8(v[0]))
			for _, c := range v[1:] {
				x = x<<8 | int64(c)
			}
			vals[i] = x
		case t == 7:
			vals[i] = math.Float64frombits(binary.BigEndian.Uint64(need(8)))
		case t == 8:
			vals[i] = int64(0)
		case t == 9:
			vals[i] = int64(1)
		case t >= 12 && t%2 == 0:
			vals[i] = need(int((t - 12) / 2))
		case t >= 13:
			vals[i] = string(need(int((t - 13) / 2)))
		default:
			panic(errSQLiteCorrupt)
		}
	}
	return vals
}

// sqliteVarint reads SQLite's big-endian varint: up to 8 bytes of 7 bits,
// then a full ninth byte.
func sqliteVarint(b []byte) (int64, int) {
	var x uint64
	for i := 0; i < 9; i++ {
		if i >= len(b) {
			panic(errSQLiteCorrupt)
		}
		if i == 8 {
			return int64(x<<8 | uint64(b[i])), 9
		}
		x = x<<7 | uint64(b[i]&0x7f)
		if b[i] < 0x80 {
			return int64(x), i + 1
		}
	}
	return int64(x), 9
}

// recoverCorrupt turns a panic from reading a malformed page (an index out of
// range, or errSQLiteCorrupt) into an error.
func recoverCorrupt(err *error) {
	if r := recover(); r != nil {
		*err = errSQLiteCorrupt
	}
}
