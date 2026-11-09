// Package sqlinfo extracts what doubledip needs to know about a statement
// (what it reads, what it writes, which locks it takes) using the real
// PostgreSQL parser through libpg_query.
package sqlinfo

import (
	"encoding/json"
	"sort"
	"strings"
	"sync"

	pg_query "github.com/pganalyze/pg_query_go/v6"
)

// Kind is the kind of statement.
type Kind string

const (
	Select   Kind = "select"
	Insert   Kind = "insert"
	Update   Kind = "update"
	Delete   Kind = "delete"
	Begin    Kind = "begin"
	Commit   Kind = "commit"
	Rollback Kind = "rollback"
	SetTx    Kind = "set-transaction"
	Other    Kind = "other"
)

// Stmt describes one statement.
type Stmt struct {
	Kind Kind
	// Target is the table an INSERT, UPDATE or DELETE writes.
	Target string
	// Reads lists the tables the statement reads (FROM, JOIN, subqueries).
	Reads []string
	// Lock is the row lock a SELECT takes ("FOR UPDATE", "FOR SHARE", ...), or "".
	Lock string
	// WhereCols lists the columns a WHERE clause tests.
	WhereCols []string
	// EqCols lists the columns compared with = in the WHERE clause.
	EqCols []string
	// SetCols lists the columns an UPDATE assigns.
	SetCols []string
	// Relative lists the SET columns computed from their own value (x = x - 1).
	Relative []string
	// InsertCols lists the columns an INSERT fills.
	InsertCols []string
	// OnConflict is true for INSERT ... ON CONFLICT.
	OnConflict bool
	// Isolation is the level set by BEGIN ISOLATION LEVEL ... or SET TRANSACTION.
	Isolation string
	// Aggregate is true when a SELECT computes count(), sum() and similar.
	Aggregate bool
}

// Writes reports whether the statement changes rows.
func (s Stmt) Writes() bool { return s.Kind == Insert || s.Kind == Update || s.Kind == Delete }

var (
	cacheMu sync.Mutex
	cache   = map[string][]Stmt{}
	fpCache = map[string]string{}
)

// Fingerprint returns libpg_query's fingerprint: equal for statements that
// differ only in constants and parameter values.
func Fingerprint(sql string) (string, error) {
	cacheMu.Lock()
	if fp, ok := fpCache[sql]; ok {
		cacheMu.Unlock()
		return fp, nil
	}
	cacheMu.Unlock()
	fp, err := pg_query.Fingerprint(sql)
	if err != nil {
		return "", err
	}
	cacheMu.Lock()
	fpCache[sql] = fp
	cacheMu.Unlock()
	return fp, nil
}

// Split splits a query string into its statements. If the string does not
// parse it is returned whole.
func Split(sql string) []string {
	parts, err := pg_query.SplitWithParser(sql, true)
	if err != nil || len(parts) == 0 {
		return []string{sql}
	}
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return []string{sql}
	}
	return out
}

// Analyze describes every statement in sql.
func Analyze(sql string) ([]Stmt, error) {
	cacheMu.Lock()
	if s, ok := cache[sql]; ok {
		cacheMu.Unlock()
		return s, nil
	}
	cacheMu.Unlock()

	js, err := pg_query.ParseToJSON(sql)
	if err != nil {
		return nil, err
	}
	var tree struct {
		Stmts []struct {
			Stmt map[string]any `json:"stmt"`
		} `json:"stmts"`
	}
	if err := json.Unmarshal([]byte(js), &tree); err != nil {
		return nil, err
	}
	var out []Stmt
	for _, raw := range tree.Stmts {
		out = append(out, describe(raw.Stmt))
	}
	cacheMu.Lock()
	cache[sql] = out
	cacheMu.Unlock()
	return out, nil
}

func describe(n map[string]any) Stmt {
	for k, v := range n {
		body, _ := v.(map[string]any)
		switch k {
		case "SelectStmt":
			s := Stmt{Kind: Select, Reads: tables(body)}
			if lc, ok := body["lockingClause"].([]any); ok && len(lc) > 0 {
				s.Lock = lockName(lc[0])
			}
			// Include the WHERE clauses of subqueries: SELECT EXISTS (SELECT ... WHERE ...).
			walk(body, func(k string, v any) {
				if k == "whereClause" {
					all, eq := whereCols(v)
					s.WhereCols = uniq(append(s.WhereCols, all...))
					s.EqCols = uniq(append(s.EqCols, eq...))
				}
			})
			s.Aggregate = hasAggregate(body["targetList"])
			return s
		case "InsertStmt":
			s := Stmt{Kind: Insert, Target: relname(body["relation"])}
			for _, c := range list(body["cols"]) {
				if rt := obj(c, "ResTarget"); rt != nil {
					if name, ok := rt["name"].(string); ok {
						s.InsertCols = append(s.InsertCols, name)
					}
				}
			}
			s.OnConflict = body["onConflictClause"] != nil
			s.Reads = without(tables(body["selectStmt"]), s.Target)
			return s
		case "UpdateStmt":
			s := Stmt{Kind: Update, Target: relname(body["relation"])}
			for _, t := range list(body["targetList"]) {
				rt := obj(t, "ResTarget")
				if rt == nil {
					continue
				}
				name, _ := rt["name"].(string)
				s.SetCols = append(s.SetCols, name)
				for _, c := range columnRefs(rt["val"]) {
					if c == name {
						s.Relative = append(s.Relative, name)
						break
					}
				}
			}
			s.WhereCols, s.EqCols = whereCols(body["whereClause"])
			s.Reads = append(tables(body["fromClause"]), tables(body["whereClause"])...)
			return s
		case "DeleteStmt":
			s := Stmt{Kind: Delete, Target: relname(body["relation"])}
			s.WhereCols, s.EqCols = whereCols(body["whereClause"])
			s.Reads = append(tables(body["usingClause"]), tables(body["whereClause"])...)
			return s
		case "TransactionStmt":
			switch body["kind"] {
			case "TRANS_STMT_BEGIN", "TRANS_STMT_START":
				return Stmt{Kind: Begin, Isolation: isolationOption(body["options"])}
			case "TRANS_STMT_COMMIT", "TRANS_STMT_COMMIT_PREPARED", "TRANS_STMT_PREPARE":
				return Stmt{Kind: Commit}
			case "TRANS_STMT_ROLLBACK", "TRANS_STMT_ROLLBACK_PREPARED":
				return Stmt{Kind: Rollback}
			}
		case "VariableSetStmt":
			if strings.EqualFold(str(body["name"]), "TRANSACTION") {
				return Stmt{Kind: SetTx, Isolation: isolationOption(body["args"])}
			}
		}
	}
	return Stmt{Kind: Other}
}

func lockName(n any) string {
	lc := obj(n, "LockingClause")
	switch str(lc["strength"]) {
	case "LCS_FORUPDATE":
		return "FOR UPDATE"
	case "LCS_FORNOKEYUPDATE":
		return "FOR NO KEY UPDATE"
	case "LCS_FORSHARE":
		return "FOR SHARE"
	case "LCS_FORKEYSHARE":
		return "FOR KEY SHARE"
	}
	return "FOR UPDATE"
}

func isolationOption(n any) string {
	for _, o := range list(n) {
		de := obj(o, "DefElem")
		if de == nil || str(de["defname"]) != "transaction_isolation" {
			continue
		}
		var lvl string
		walk(de["arg"], func(k string, v any) {
			if k == "sval" {
				if s, ok := v.(string); ok {
					lvl = s
				}
			}
		})
		return strings.ToLower(lvl)
	}
	return ""
}

// tables collects every RangeVar under n.
func tables(n any) []string {
	var out []string
	walk(n, func(k string, v any) {
		if k == "RangeVar" {
			if t := relname(v); t != "" {
				out = append(out, t)
			}
		}
	})
	return uniq(out)
}

func relname(n any) string {
	m, _ := n.(map[string]any)
	if rv, ok := m["RangeVar"].(map[string]any); ok {
		m = rv
	}
	name := str(m["relname"])
	if sch := str(m["schemaname"]); sch != "" && sch != "public" {
		name = sch + "." + name
	}
	return name
}

// whereCols returns the columns a WHERE clause mentions and those compared
// with = at its top level (inside ANDs).
func whereCols(n any) (all, eq []string) {
	all = uniq(columnRefs(n))
	var visit func(any)
	visit = func(x any) {
		if be := obj(x, "BoolExpr"); be != nil {
			if str(be["boolop"]) == "AND_EXPR" {
				for _, a := range list(be["args"]) {
					visit(a)
				}
			}
			return
		}
		if ae := obj(x, "A_Expr"); ae != nil && str(ae["kind"]) == "AEXPR_OP" {
			op := ""
			for _, nm := range list(ae["name"]) {
				op = str(obj(nm, "String")["sval"])
			}
			if op == "=" {
				for _, side := range []any{ae["lexpr"], ae["rexpr"]} {
					if cr := obj(side, "ColumnRef"); cr != nil {
						eq = append(eq, colName(cr))
					}
				}
			}
		}
	}
	visit(n)
	return all, uniq(eq)
}

func columnRefs(n any) []string {
	var out []string
	walk(n, func(k string, v any) {
		if k == "ColumnRef" {
			if c := colName(v.(map[string]any)); c != "" && c != "*" {
				out = append(out, c)
			}
		}
	})
	return out
}

func colName(cr map[string]any) string {
	fields := list(cr["fields"])
	if len(fields) == 0 {
		return ""
	}
	last := fields[len(fields)-1]
	if s := obj(last, "String"); s != nil {
		return str(s["sval"])
	}
	return "*"
}

var aggregates = map[string]bool{"count": true, "sum": true, "min": true, "max": true, "avg": true, "bool_and": true, "bool_or": true, "every": true}

func hasAggregate(n any) bool {
	found := false
	walk(n, func(k string, v any) {
		if k != "FuncCall" {
			return
		}
		for _, nm := range list(v.(map[string]any)["funcname"]) {
			if aggregates[strings.ToLower(str(obj(nm, "String")["sval"]))] {
				found = true
			}
		}
	})
	return found
}

// walk calls fn for every key/value pair in the tree, depth first.
func walk(n any, fn func(k string, v any)) {
	switch x := n.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fn(k, x[k])
			walk(x[k], fn)
		}
	case []any:
		for _, e := range x {
			walk(e, fn)
		}
	}
}

func obj(n any, key string) map[string]any {
	m, _ := n.(map[string]any)
	o, _ := m[key].(map[string]any)
	return o
}

func list(n any) []any {
	l, _ := n.([]any)
	return l
}

func str(n any) string {
	s, _ := n.(string)
	return s
}

func uniq(in []string) []string {
	seen := map[string]bool{}
	out := in[:0:0]
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func without(in []string, drop string) []string {
	var out []string
	for _, s := range in {
		if s != drop {
			out = append(out, s)
		}
	}
	return out
}
