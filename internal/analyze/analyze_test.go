package analyze

import (
	"strings"
	"testing"

	"github.com/AndreaAcanfora/doubledip/internal/proxy"
)

// run builds a recorded request from (tx, sql, tag) triples.
func run(stmts ...any) []proxy.Exec {
	var out []proxy.Exec
	for i := 0; i < len(stmts); i += 3 {
		out = append(out, proxy.Exec{Seq: int64(i/3 + 1), Tx: int64(stmts[i].(int)), SQL: stmts[i+1].(string), Tag: stmts[i+2].(string)})
	}
	return out
}

func TestLostUpdateInOneTransaction(t *testing.T) {
	steps := Steps(run(
		1, "BEGIN", "BEGIN",
		1, "SELECT id, uses, max_uses FROM coupons WHERE code = $1", "SELECT 1",
		1, "INSERT INTO redemptions (coupon_id, user_name) VALUES ($1, $2)", "INSERT 0 1",
		1, "UPDATE coupons SET uses = $1 WHERE id = $2", "UPDATE 1",
		1, "COMMIT", "COMMIT",
	), "read committed")
	c := Find(steps)
	if len(c) != 1 {
		t.Fatalf("got %d candidates", len(c))
	}
	if c[0].Pattern != LostUpdate || c[0].Write.Stmt.Target != "coupons" || c[0].Gate.Stmt.Target != "redemptions" || c[0].Safe {
		t.Fatalf("%+v", c[0])
	}
}

func TestCheckThenActAcrossTransactions(t *testing.T) {
	c := Find(Steps(run(
		1, "SELECT balance FROM accounts WHERE id = $1", "SELECT 1",
		2, "UPDATE accounts SET balance = balance - $1 WHERE id = $2", "UPDATE 1",
	), ""))
	if len(c) != 1 || c[0].Pattern != CheckThenAct || c[0].Safe {
		t.Fatalf("%+v", c)
	}
	if c[0].Fix[0][:20] != "the read (transactio" {
		t.Fatalf("fix should start with the transaction scope: %q", c[0].Fix)
	}
}

func TestCheckThenInsertSuggestsUniqueIndex(t *testing.T) {
	c := Find(Steps(run(
		1, "SELECT EXISTS (SELECT 1 FROM users WHERE email = $1)", "SELECT 1",
		2, "INSERT INTO users (email) VALUES ($1)", "INSERT 0 1",
	), ""))
	if len(c) != 1 || c[0].Pattern != CheckThenWrite {
		t.Fatalf("%+v", c)
	}
	want := "CREATE UNIQUE INDEX ON users (email), then treat the unique violation (23505) as \"already exists\""
	if c[0].Fix[1] != want {
		t.Fatalf("fix: %q", c[0].Fix)
	}
}

func TestSafePatterns(t *testing.T) {
	for name, execs := range map[string][]proxy.Exec{
		"for update": run(
			1, "BEGIN", "BEGIN",
			1, "SELECT uses FROM coupons WHERE id = 1 FOR UPDATE", "SELECT 1",
			1, "UPDATE coupons SET uses = 1 WHERE id = 1", "UPDATE 1",
		),
		"guarded update": run(
			1, "SELECT balance FROM accounts WHERE id = 1", "SELECT 1",
			2, "UPDATE accounts SET balance = balance - 80 WHERE id = 1 AND balance >= 80", "UPDATE 1",
		),
		"serializable": run(
			1, "BEGIN ISOLATION LEVEL SERIALIZABLE", "BEGIN",
			1, "SELECT stock FROM products WHERE id = 1", "SELECT 1",
			1, "UPDATE products SET stock = 1 WHERE id = 1", "UPDATE 1",
		),
		"repeatable read update": run(
			1, "BEGIN", "BEGIN",
			1, "SET TRANSACTION ISOLATION LEVEL REPEATABLE READ", "SET",
			1, "SELECT stock FROM products WHERE id = 1", "SELECT 1",
			1, "UPDATE products SET stock = 1 WHERE id = 1", "UPDATE 1",
		),
	} {
		c := Find(Steps(execs, ""))
		if len(c) != 1 || !c[0].Safe || c[0].Why == "" {
			t.Errorf("%s: %+v", name, c)
		}
	}
}

func TestNotCandidates(t *testing.T) {
	for name, execs := range map[string][]proxy.Exec{
		"write to another table": run(
			1, "SELECT * FROM settings", "SELECT 3",
			2, "INSERT INTO audit (msg) VALUES ('x')", "INSERT 0 1",
		),
		"no rows written": run(
			1, "SELECT balance FROM accounts WHERE id = 1", "SELECT 1",
			2, "UPDATE accounts SET balance = 0 WHERE id = 1 AND false", "UPDATE 0",
		),
		"failed write": {
			{Tx: 1, SQL: "SELECT 1 FROM users WHERE email = 'a'", Tag: "SELECT 0"},
			{Tx: 2, SQL: "INSERT INTO users (email) VALUES ('a')", Err: "23505 duplicate key"},
		},
		"write before read": run(
			1, "UPDATE accounts SET balance = 1 WHERE id = 1", "UPDATE 1",
			2, "SELECT balance FROM accounts WHERE id = 1", "SELECT 1",
		),
	} {
		if c := Find(Steps(execs, "")); len(c) != 0 {
			t.Errorf("%s: %+v", name, c)
		}
	}
}

func TestIsolationSources(t *testing.T) {
	steps := Steps([]proxy.Exec{
		{Tx: 1, SQL: "SELECT 1 FROM a", SessionIsolation: "repeatable read"},
		{Tx: 2, SQL: "SELECT 1 FROM a"},
	}, "serializable")
	if steps[0].Isolation != "repeatable read" || steps[1].Isolation != "serializable" {
		t.Fatalf("%s / %s", steps[0].Isolation, steps[1].Isolation)
	}
	if steps[0].TxIndex != 1 || steps[1].TxIndex != 2 {
		t.Fatal("tx index")
	}
}

func TestOneCandidatePerGate(t *testing.T) {
	c := Find(Steps(run(
		1, "SELECT uses FROM coupons WHERE id = 1", "SELECT 1",
		1, "SELECT count(*) FROM coupons", "SELECT 1",
		1, "UPDATE coupons SET uses = 1 WHERE id = 1", "UPDATE 1",
	), ""))
	if len(c) != 1 {
		t.Fatalf("got %d candidates, want 1 (same gate)", len(c))
	}
}

// conns builds a recorded request from (conn, tx, sql, tag) quadruples.
func conns(stmts ...any) []proxy.Exec {
	var out []proxy.Exec
	for i := 0; i < len(stmts); i += 4 {
		out = append(out, proxy.Exec{Seq: int64(i/4 + 1), Conn: stmts[i].(int), Tx: int64(stmts[i+1].(int)), SQL: stmts[i+2].(string), Tag: stmts[i+3].(string)})
	}
	return out
}

func summary(cs []Candidate) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Read.Stmt.Reads[0]+"->"+c.Write.Stmt.Target+"@"+c.Gate.Stmt.Target)
	}
	return out
}

// One request runs two steps in parallel on two pooled connections. Whatever
// way their statements interleave, the candidates are the same.
func TestParallelConnectionsGiveStableCandidates(t *testing.T) {
	stock := []any{
		1, 1, "SELECT reserved, stocked FROM inventory_level WHERE item_id = $1", "SELECT 1",
		1, 1, "UPDATE inventory_level SET reserved = $1 WHERE item_id = $2", "UPDATE 1",
	}
	order := []any{
		2, 2, "SELECT id FROM carts WHERE id = $1", "SELECT 1",
		2, 2, "INSERT INTO orders (cart_id) VALUES ($1)", "INSERT 0 1",
		2, 2, "UPDATE carts SET completed_at = now() WHERE id = $1", "UPDATE 1",
	}
	a := append(append([]any{}, stock...), order...)
	b := append(append([]any{}, order[:4]...), stock[:4]...)
	b = append(b, order[4:]...)
	b = append(b, stock[4:]...)
	ca, cb := summary(Find(Steps(conns(a...), ""))), summary(Find(Steps(conns(b...), "")))
	if len(ca) != 2 || strings.Join(ca, " ") != strings.Join(cb, " ") {
		t.Fatalf("candidates depend on the interleaving:\n%v\n%v", ca, cb)
	}
	for _, s := range ca {
		if s == "inventory_level->inventory_level@orders" {
			t.Fatalf("gate taken from another connection: %v", ca)
		}
	}
}

// Two reads that share a gate are one test: the one nothing rules out is
// kept, and candidates that are not safe come first.
func TestSharedGateKeepsUnsafeCandidate(t *testing.T) {
	c := Find(Steps(run(
		1, "BEGIN", "BEGIN",
		1, "SELECT uses FROM coupons WHERE id = 1 FOR UPDATE", "SELECT 1",
		1, "SELECT balance FROM accounts WHERE id = 1", "SELECT 1",
		1, "UPDATE coupons SET uses = 1 WHERE id = 1", "UPDATE 1",
		1, "UPDATE accounts SET balance = 0 WHERE id = 1", "UPDATE 1",
		1, "COMMIT", "COMMIT",
	), ""))
	if len(c) != 1 || c[0].Safe || c[0].Write.Stmt.Target != "accounts" {
		t.Fatalf("%v", summary(c))
	}
}

func TestCheckThenDeleteHintHasNoAmount(t *testing.T) {
	c := Find(Steps(run(
		1, "SELECT token FROM checkouts WHERE token = $1", "SELECT 1",
		2, "DELETE FROM checkouts WHERE token IN ($1)", "DELETE 1",
	), ""))
	if len(c) != 1 || c[0].Pattern != CheckThenAct {
		t.Fatalf("%+v", c)
	}
	for _, f := range c[0].Fix {
		if strings.Contains(f, "$amount") {
			t.Fatalf("hint compares a key with an amount: %q", f)
		}
	}
}
