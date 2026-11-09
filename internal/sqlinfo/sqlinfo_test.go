package sqlinfo

import (
	"reflect"
	"testing"
)

func one(t *testing.T, sql string) Stmt {
	t.Helper()
	s, err := Analyze(sql)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	if len(s) != 1 {
		t.Fatalf("%s: got %d statements", sql, len(s))
	}
	return s[0]
}

func TestSelect(t *testing.T) {
	s := one(t, `SELECT id, uses, max_uses FROM coupons c WHERE c.code = $1 AND active`)
	if s.Kind != Select || !reflect.DeepEqual(s.Reads, []string{"coupons"}) || s.Lock != "" {
		t.Fatalf("%+v", s)
	}
	if !reflect.DeepEqual(s.EqCols, []string{"code"}) || !reflect.DeepEqual(s.WhereCols, []string{"code", "active"}) {
		t.Fatalf("where: %+v", s)
	}
	s = one(t, `select * from accounts where id = 3 for update`)
	if s.Lock != "FOR UPDATE" {
		t.Fatalf("lock: %+v", s)
	}
	s = one(t, `select count(*) from redemptions where coupon_id = $1 and user_id = $2`)
	if !s.Aggregate || !reflect.DeepEqual(s.EqCols, []string{"coupon_id", "user_id"}) {
		t.Fatalf("agg: %+v", s)
	}
	s = one(t, `select 1 from users u join orgs o on o.id = u.org_id where u.email = 'a'`)
	if !reflect.DeepEqual(s.Reads, []string{"users", "orgs"}) && !reflect.DeepEqual(s.Reads, []string{"orgs", "users"}) {
		t.Fatalf("join: %+v", s)
	}
}

func TestWrites(t *testing.T) {
	s := one(t, `UPDATE coupons SET uses = $1, updated_at = now() WHERE id = $2`)
	if s.Kind != Update || s.Target != "coupons" || !reflect.DeepEqual(s.SetCols, []string{"uses", "updated_at"}) || len(s.Relative) != 0 {
		t.Fatalf("%+v", s)
	}
	s = one(t, `update accounts set balance = balance - $1 where id = $2 and balance >= $1`)
	if !reflect.DeepEqual(s.Relative, []string{"balance"}) || !reflect.DeepEqual(s.WhereCols, []string{"id", "balance"}) {
		t.Fatalf("%+v", s)
	}
	s = one(t, `INSERT INTO redemptions (coupon_id, user_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`)
	if s.Kind != Insert || s.Target != "redemptions" || !s.OnConflict || !reflect.DeepEqual(s.InsertCols, []string{"coupon_id", "user_id"}) {
		t.Fatalf("%+v", s)
	}
	s = one(t, `delete from carts where user_id = $1`)
	if s.Kind != Delete || s.Target != "carts" || !reflect.DeepEqual(s.EqCols, []string{"user_id"}) {
		t.Fatalf("%+v", s)
	}
	s = one(t, `insert into app.events select * from staging`)
	if s.Target != "app.events" || !reflect.DeepEqual(s.Reads, []string{"staging"}) {
		t.Fatalf("%+v", s)
	}
}

func TestTransactions(t *testing.T) {
	for sql, want := range map[string]Stmt{
		"BEGIN":                              {Kind: Begin},
		"begin isolation level serializable": {Kind: Begin, Isolation: "serializable"},
		"START TRANSACTION ISOLATION LEVEL REPEATABLE READ": {Kind: Begin, Isolation: "repeatable read"},
		"SET TRANSACTION ISOLATION LEVEL SERIALIZABLE":      {Kind: SetTx, Isolation: "serializable"},
		"commit":                   {Kind: Commit},
		"ROLLBACK":                 {Kind: Rollback},
		"SET search_path = public": {Kind: Other},
	} {
		if got := one(t, sql); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %+v, want %+v", sql, got, want)
		}
	}
}

func TestSplitAndFingerprint(t *testing.T) {
	if got := Split("BEGIN; UPDATE a SET x = 1 WHERE id = 2; COMMIT;"); len(got) != 3 || got[1] != "UPDATE a SET x = 1 WHERE id = 2" {
		t.Fatalf("split: %q", got)
	}
	a, _ := Fingerprint("UPDATE a SET x = 1 WHERE id = 2")
	b, _ := Fingerprint("update a set x = $1 where id = $2")
	c, _ := Fingerprint("UPDATE a SET y = 1 WHERE id = 2")
	if a != b || a == c {
		t.Fatalf("fingerprints: %s %s %s", a, b, c)
	}
}
