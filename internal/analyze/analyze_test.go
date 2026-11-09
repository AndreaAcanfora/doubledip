package analyze

import (
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
