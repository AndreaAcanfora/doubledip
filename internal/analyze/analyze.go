// Package analyze looks at the statements one request ran and finds the
// reads whose result the request acts on later: the places where a second
// copy of the same request, running at the same time, can slip in between
// the check and the write.
package analyze

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/AndreaAcanfora/doubledip/internal/proxy"
	"github.com/AndreaAcanfora/doubledip/internal/sqlinfo"
)

// Pattern names the kind of race.
type Pattern string

const (
	LostUpdate     Pattern = "lost update"
	CheckThenAct   Pattern = "check-then-act"
	CheckThenWrite Pattern = "check-then-insert"
)

// Step is one statement of the request.
type Step struct {
	proxy.Exec
	Stmt      sqlinfo.Stmt
	Isolation string // effective isolation of its transaction
	TxIndex   int    // 1-based index of its transaction within the request
}

// Candidate is a read followed by a write that depends on it.
type Candidate struct {
	Pattern Pattern
	Read    Step
	Write   Step // the write on the same table that acts on the read
	Gate    Step // first write after the read: where both requests are held
	GateFP  string
	// Safe is set when the statements already rule the race out; Why says how.
	Safe bool
	Why  string
	Fix  []string
}

// Steps annotates the recorded statements of one request with what they
// do and their effective isolation level. defaultIso is the database's
// default_transaction_isolation.
func Steps(execs []proxy.Exec, defaultIso string) []Step {
	steps := make([]Step, 0, len(execs))
	txIso := map[int64]string{}
	txIndex := map[int64]int{}
	for _, e := range execs {
		stmts, err := sqlinfo.Analyze(e.SQL)
		st := sqlinfo.Stmt{Kind: sqlinfo.Other}
		if err == nil && len(stmts) > 0 {
			st = stmts[0]
		}
		if (st.Kind == sqlinfo.Begin || st.Kind == sqlinfo.SetTx) && st.Isolation != "" {
			txIso[e.Tx] = st.Isolation
		}
		if _, ok := txIndex[e.Tx]; !ok {
			txIndex[e.Tx] = len(txIndex) + 1
		}
		steps = append(steps, Step{Exec: e, Stmt: st, TxIndex: txIndex[e.Tx]})
	}
	for i := range steps {
		iso := txIso[steps[i].Tx]
		if iso == "" {
			iso = steps[i].SessionIsolation
		}
		if iso == "" {
			iso = defaultIso
		}
		if iso == "" {
			iso = "read committed"
		}
		steps[i].Isolation = iso
	}
	return steps
}

// Find returns the candidate races in one request, at most one per gate
// statement, in the order the request ran them.
func Find(steps []Step) []Candidate {
	var out []Candidate
	seen := map[string]bool{}
	for i, r := range steps {
		if r.Stmt.Kind != sqlinfo.Select || r.Err != "" || len(r.Stmt.Reads) == 0 {
			continue
		}
		gate := -1
		for j := i + 1; j < len(steps); j++ {
			if steps[j].Stmt.Writes() && steps[j].Err == "" {
				gate = j
				break
			}
		}
		if gate < 0 {
			continue
		}
		for j := gate; j < len(steps); j++ {
			w := steps[j]
			if !w.Stmt.Writes() || w.Err != "" || !contains(r.Stmt.Reads, w.Stmt.Target) || affected(w.Tag) == 0 {
				continue
			}
			fp, err := sqlinfo.Fingerprint(steps[gate].SQL)
			if err != nil || seen[fp] {
				break
			}
			seen[fp] = true
			c := classify(r, w)
			c.Gate, c.GateFP = steps[gate], fp
			out = append(out, c)
			break
		}
	}
	return out
}

func classify(r, w Step) Candidate {
	c := Candidate{Read: r, Write: w}
	sameTx := r.Tx == w.Tx
	t := w.Stmt.Target
	switch w.Stmt.Kind {
	case sqlinfo.Insert:
		c.Pattern = CheckThenWrite
		key := intersect(r.Stmt.EqCols, w.Stmt.InsertCols)
		switch {
		case w.Stmt.OnConflict:
			c.Fix = append(c.Fix, fmt.Sprintf("check that the ON CONFLICT target of the INSERT into %s is a unique index covering what the SELECT checks", t))
		case len(key) > 0 && !r.Stmt.Aggregate:
			c.Fix = append(c.Fix, fmt.Sprintf("CREATE UNIQUE INDEX ON %s (%s), then treat the unique violation (23505) as \"already exists\"", t, strings.Join(key, ", ")))
		default:
			c.Fix = append(c.Fix, "lock a parent row with SELECT ... FOR UPDATE before counting, so the check and the insert run one request at a time")
		}
		c.Fix = append(c.Fix, "or run the transaction at SERIALIZABLE and retry on 40001")
	case sqlinfo.Update, sqlinfo.Delete:
		relative := len(w.Stmt.Relative) > 0
		guarded := relative
		for _, col := range w.Stmt.Relative {
			if !contains(w.Stmt.WhereCols, col) {
				guarded = false
			}
		}
		switch {
		case w.Stmt.Kind == sqlinfo.Update && !relative:
			c.Pattern = LostUpdate
			c.Fix = append(c.Fix,
				fmt.Sprintf("do the arithmetic in the UPDATE (SET %s = %s ± $n), put the check in its WHERE clause and treat 0 rows as \"rejected\"", first(w.Stmt.SetCols), first(w.Stmt.SetCols)),
				fmt.Sprintf("or read the row with SELECT ... FOR UPDATE in the same transaction as the UPDATE of %s", t))
		default:
			c.Pattern = CheckThenAct
			col := first(w.Stmt.Relative)
			if col == "" {
				col = first(w.Stmt.WhereCols)
			}
			c.Fix = append(c.Fix,
				fmt.Sprintf("put the check in the write's WHERE clause (e.g. WHERE ... AND %s >= $amount) and treat 0 rows as \"rejected\"", col),
				fmt.Sprintf("or read with SELECT ... FOR UPDATE in the same transaction as the write to %s", t))
		}
		if guarded && w.Stmt.Kind == sqlinfo.Update {
			c.Safe, c.Why = true, fmt.Sprintf("the UPDATE re-checks %s in its own WHERE clause", strings.Join(w.Stmt.Relative, ", "))
		}
		if sameTx && r.Isolation == "repeatable read" {
			c.Safe, c.Why = true, "REPEATABLE READ aborts the second writer of the same row"
		}
	}
	if !sameTx {
		c.Fix = append([]string{fmt.Sprintf("the read (transaction %d) and the write (transaction %d) are in separate transactions: put them in one", r.TxIndex, w.TxIndex)}, c.Fix...)
	}
	if sameTx && r.Stmt.Lock != "" {
		c.Safe, c.Why = true, "the read takes a row lock ("+r.Stmt.Lock+") in the same transaction"
	}
	if sameTx && r.Isolation == "serializable" {
		c.Safe, c.Why = true, "the transaction runs at SERIALIZABLE"
	}
	return c
}

// affected returns the row count of a command tag, or -1 when it has none.
func affected(tag string) int {
	f := strings.Fields(tag)
	if len(f) < 2 {
		return -1
	}
	n, err := strconv.Atoi(f[len(f)-1])
	if err != nil {
		return -1
	}
	return n
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func intersect(a, b []string) []string {
	var out []string
	for _, x := range a {
		if contains(b, x) {
			out = append(out, x)
		}
	}
	return out
}

func first(l []string) string {
	if len(l) == 0 {
		return ""
	}
	return l[0]
}
