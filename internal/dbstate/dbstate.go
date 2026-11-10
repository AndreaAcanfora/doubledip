// Package dbstate saves and restores the contents of a development
// database, so every run of a request starts from the same data, and
// compares the states that runs leave behind.
package dbstate

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Column is a table column.
type Column struct {
	Name      string
	Type      string
	Generated bool // GENERATED ... STORED: cannot be inserted
	Identity  bool // GENERATED ... AS IDENTITY
	Volatile  bool // default from a sequence, clock or random generator
}

// Table is a user table.
type Table struct {
	Schema, Name string
	Columns      []Column
	DependsOn    []string // tables it references with foreign keys
}

// QName is the quoted, schema-qualified name.
func (t Table) QName() string { return pgx.Identifier{t.Schema, t.Name}.Sanitize() }

// Key is the name used in reports: "name", or "schema.name" outside public.
func (t Table) Key() string {
	if t.Schema == "public" {
		return t.Name
	}
	return t.Schema + "." + t.Name
}

// Snapshot is the full contents of the user tables and sequences.
type Snapshot struct {
	Tables    []Table // in foreign key order: referenced tables first
	Rows      map[string][]string
	Sequences map[string][2]string // qualified name -> last_value, is_called
}

// DB is a direct connection to the upstream database (not through the proxy).
type DB struct {
	conn *pgx.Conn
}

// Open connects to the database.
func Open(ctx context.Context, url string) (*DB, error) {
	c, err := pgx.Connect(ctx, url)
	if err != nil {
		return nil, err
	}
	return &DB{conn: c}, nil
}

func (d *DB) Close() { d.conn.Close(context.Background()) }

// DefaultIsolation returns default_transaction_isolation.
func (d *DB) DefaultIsolation(ctx context.Context) string {
	var s string
	if err := d.conn.QueryRow(ctx, "SHOW default_transaction_isolation").Scan(&s); err != nil {
		return "read committed"
	}
	return s
}

// LockWaiters returns how many other sessions of this database are waiting
// for a lock held by another session.
func (d *DB) LockWaiters(ctx context.Context) (int, error) {
	var n int
	err := d.conn.QueryRow(ctx, `
		SELECT count(*) FROM pg_stat_activity
		WHERE datname = current_database() AND pid <> pg_backend_pid()
		  AND cardinality(pg_blocking_pids(pid)) > 0`).Scan(&n)
	return n, err
}

// UniqueIndexes returns, for each table key, the column lists of its unique
// indexes and constraints (primary keys included).
func (d *DB) UniqueIndexes(ctx context.Context) (map[string][][]string, error) {
	rows, err := d.conn.Query(ctx, `
		SELECT n.nspname, c.relname, array_agg(a.attname ORDER BY k.ord)
		FROM pg_index i
		JOIN pg_class c ON c.oid = i.indrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		CROSS JOIN LATERAL unnest(i.indkey) WITH ORDINALITY AS k(attnum, ord)
		JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = k.attnum
		WHERE i.indisunique AND i.indpred IS NULL
		  AND n.nspname NOT IN ('pg_catalog', 'information_schema') AND n.nspname NOT LIKE 'pg_toast%'
		GROUP BY n.nspname, c.relname, i.indexrelid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][][]string{}
	for rows.Next() {
		var schema, name string
		var cols []string
		if err := rows.Scan(&schema, &name, &cols); err != nil {
			return nil, err
		}
		key := Table{Schema: schema, Name: name}.Key()
		out[key] = append(out[key], cols)
	}
	return out, rows.Err()
}

func (d *DB) tables(ctx context.Context) ([]Table, error) {
	rows, err := d.conn.Query(ctx, `
		SELECT n.nspname, c.relname, a.attname, format_type(a.atttypid, a.atttypmod),
		       a.attgenerated <> '', a.attidentity <> '',
		       coalesce(pg_get_expr(ad.adbin, ad.adrelid), '')
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
		LEFT JOIN pg_attrdef ad ON ad.adrelid = c.oid AND ad.adnum = a.attnum
		WHERE c.relkind = 'r'
		  AND n.nspname NOT IN ('pg_catalog', 'information_schema') AND n.nspname NOT LIKE 'pg_toast%'
		  AND n.nspname NOT LIKE 'pg_temp%'
		ORDER BY n.nspname, c.relname, a.attnum`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Table
	for rows.Next() {
		var schema, name, col, typ, def string
		var gen, ident bool
		if err := rows.Scan(&schema, &name, &col, &typ, &gen, &ident, &def); err != nil {
			return nil, err
		}
		if len(out) == 0 || out[len(out)-1].Schema != schema || out[len(out)-1].Name != name {
			out = append(out, Table{Schema: schema, Name: name})
		}
		d := strings.ToLower(def)
		volatile := ident || strings.Contains(d, "nextval(") || strings.Contains(d, "now()") ||
			strings.Contains(d, "current_timestamp") || strings.Contains(d, "clock_timestamp") ||
			strings.Contains(d, "random") || strings.Contains(d, "uuid")
		out[len(out)-1].Columns = append(out[len(out)-1].Columns, Column{Name: col, Type: typ, Generated: gen, Identity: ident, Volatile: volatile})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	fks, err := d.conn.Query(ctx, `
		SELECT sn.nspname, s.relname, tn.nspname, t.relname
		FROM pg_constraint k
		JOIN pg_class s ON s.oid = k.conrelid JOIN pg_namespace sn ON sn.oid = s.relnamespace
		JOIN pg_class t ON t.oid = k.confrelid JOIN pg_namespace tn ON tn.oid = t.relnamespace
		WHERE k.contype = 'f'`)
	if err != nil {
		return nil, err
	}
	defer fks.Close()
	idx := map[string]int{}
	for i, t := range out {
		idx[t.Key()] = i
	}
	for fks.Next() {
		var ss, sn, ts, tn string
		if err := fks.Scan(&ss, &sn, &ts, &tn); err != nil {
			return nil, err
		}
		from, to := Table{Schema: ss, Name: sn}.Key(), Table{Schema: ts, Name: tn}.Key()
		if i, ok := idx[from]; ok && from != to {
			out[i].DependsOn = append(out[i].DependsOn, to)
		}
	}
	return topo(out), fks.Err()
}

// topo orders tables so that referenced tables come first.
func topo(ts []Table) []Table {
	byKey := map[string]Table{}
	for _, t := range ts {
		byKey[t.Key()] = t
	}
	var out []Table
	state := map[string]int{}
	var visit func(string)
	visit = func(k string) {
		if state[k] != 0 {
			return
		}
		state[k] = 1
		for _, dep := range byKey[k].DependsOn {
			if _, ok := byKey[dep]; ok {
				visit(dep)
			}
		}
		state[k] = 2
		out = append(out, byKey[k])
	}
	for _, t := range ts {
		visit(t.Key())
	}
	return out
}

// Save reads every user table and sequence.
func (d *DB) Save(ctx context.Context) (*Snapshot, error) {
	ts, err := d.tables(ctx)
	if err != nil {
		return nil, err
	}
	s := &Snapshot{Tables: ts, Rows: map[string][]string{}, Sequences: map[string][2]string{}}
	tx, err := d.conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	for _, t := range ts {
		rows, err := tx.Query(ctx, fmt.Sprintf("SELECT t::text FROM %s t", t.QName()))
		if err != nil {
			return nil, err
		}
		vals, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return nil, err
		}
		s.Rows[t.Key()] = vals
	}
	seqs, err := tx.Query(ctx, `
		SELECT quote_ident(schemaname) || '.' || quote_ident(sequencename) FROM pg_sequences
		WHERE schemaname NOT IN ('pg_catalog', 'information_schema')`)
	if err != nil {
		return nil, err
	}
	names, err := pgx.CollectRows(seqs, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	for _, n := range names {
		var last, called string
		if err := tx.QueryRow(ctx, fmt.Sprintf("SELECT last_value::text, is_called::text FROM %s", n)).Scan(&last, &called); err != nil {
			return nil, err
		}
		s.Sequences[n] = [2]string{last, called}
	}
	return s, tx.Commit(ctx)
}

// Restore puts the database back to the snapshot: every user table is
// emptied and refilled and every sequence reset.
func (d *DB) Restore(ctx context.Context, s *Snapshot) error {
	tx, err := d.conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout = '10s'"); err != nil {
		return err
	}
	// Skip foreign key and user triggers while refilling if we may.
	if _, err := tx.Exec(ctx, "SAVEPOINT r"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "SET LOCAL session_replication_role = replica"); err != nil {
		if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT r"); err != nil {
			return err
		}
	}
	if len(s.Tables) > 0 {
		names := make([]string, len(s.Tables))
		for i, t := range s.Tables {
			names[i] = t.QName()
		}
		if _, err := tx.Exec(ctx, "TRUNCATE "+strings.Join(names, ", ")); err != nil {
			return fmt.Errorf("restore: %w", err)
		}
	}
	for _, t := range s.Tables {
		rows := s.Rows[t.Key()]
		if len(rows) == 0 {
			continue
		}
		var cols, sel []string
		for _, c := range t.Columns {
			if c.Generated {
				continue
			}
			id := pgx.Identifier{c.Name}.Sanitize()
			cols = append(cols, id)
			sel = append(sel, "(x::"+t.QName()+")."+id)
		}
		q := fmt.Sprintf("INSERT INTO %s (%s) OVERRIDING SYSTEM VALUE SELECT %s FROM unnest($1::text[]) AS x",
			t.QName(), strings.Join(cols, ", "), strings.Join(sel, ", "))
		if _, err := tx.Exec(ctx, q, rows); err != nil {
			return fmt.Errorf("restore %s: %w", t.Key(), err)
		}
	}
	for name, v := range s.Sequences {
		if _, err := tx.Exec(ctx, fmt.Sprintf("SELECT setval('%s', %s, %s)", strings.ReplaceAll(name, "'", "''"), v[0], v[1])); err != nil {
			return fmt.Errorf("restore %s: %w", name, err)
		}
	}
	return tx.Commit(ctx)
}

// State is what a run left in the database, reduced to what should not
// change between equivalent runs: rows as text without generated ids,
// timestamps and random values, sorted per table.
type State map[string][]string

// Capture reads the comparable state of the given tables (all when nil).
func (d *DB) Capture(ctx context.Context, s *Snapshot, ignore map[string]bool) (State, error) {
	st := State{}
	for _, t := range s.Tables {
		var cols []string
		for _, c := range t.Columns {
			if c.Volatile || ignore[t.Key()+"."+c.Name] || isClockType(c.Type) {
				continue
			}
			cols = append(cols, pgx.Identifier{c.Name}.Sanitize())
		}
		if len(cols) == 0 {
			cols = []string{"1"}
		}
		rows, err := d.conn.Query(ctx, fmt.Sprintf("SELECT ROW(%s)::text FROM %s", strings.Join(cols, ", "), t.QName()))
		if err != nil {
			return nil, err
		}
		vals, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return nil, err
		}
		sort.Strings(vals)
		st[t.Key()] = vals
	}
	return st, nil
}

func isClockType(t string) bool {
	return strings.HasPrefix(t, "timestamp") || strings.HasPrefix(t, "time ") || t == "time" || t == "interval"
}

// TableDiff describes how one table differs between two states.
type TableDiff struct {
	Table        string
	Count        [2]int
	OnlyA, OnlyB []string
}

// Diff returns the tables that differ between a and b, skipping those in skip.
func Diff(a, b State, skip map[string]bool) []TableDiff {
	keys := map[string]bool{}
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}
	var names []string
	for k := range keys {
		if !skip[k] {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	var out []TableDiff
	for _, k := range names {
		onlyA, onlyB := multisetDiff(a[k], b[k])
		if len(onlyA) == 0 && len(onlyB) == 0 {
			continue
		}
		out = append(out, TableDiff{Table: k, Count: [2]int{len(a[k]), len(b[k])}, OnlyA: onlyA, OnlyB: onlyB})
	}
	return out
}

func multisetDiff(a, b []string) (onlyA, onlyB []string) {
	n := map[string]int{}
	for _, x := range a {
		n[x]++
	}
	for _, x := range b {
		n[x]--
	}
	for _, x := range a {
		if n[x] > 0 {
			onlyA = append(onlyA, x)
			n[x]--
		}
	}
	for k, v := range n {
		for ; v < 0; v++ {
			onlyB = append(onlyB, k)
		}
	}
	sort.Strings(onlyB)
	return onlyA, onlyB
}
