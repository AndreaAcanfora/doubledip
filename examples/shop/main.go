// A tiny shop API with the race conditions real apps have, and the fixed
// version of each endpoint next to it. Point doubledip at it:
//
//	doubledip --db postgres://postgres@localhost:5432/shop \
//	  --app 'go run ./examples/shop' --ready http://localhost:8080/health \
//	  'curl -fsS -X POST "localhost:8080/redeem?code=WELCOME&user=$DOUBLEDIP_N"'
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const schema = `
CREATE TABLE IF NOT EXISTS coupons (
	id serial PRIMARY KEY,
	code text NOT NULL UNIQUE,
	max_uses int NOT NULL,
	uses int NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS redemptions (
	id serial PRIMARY KEY,
	coupon_id int NOT NULL REFERENCES coupons,
	user_name text NOT NULL,
	created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS accounts (
	id int PRIMARY KEY,
	balance int NOT NULL
);
CREATE TABLE IF NOT EXISTS ledger (
	id serial PRIMARY KEY,
	account_id int NOT NULL REFERENCES accounts,
	amount int NOT NULL
);
CREATE TABLE IF NOT EXISTS users (
	id serial PRIMARY KEY,
	email text NOT NULL
);
CREATE TABLE IF NOT EXISTS users_safe (
	id serial PRIMARY KEY,
	email text NOT NULL UNIQUE
);
INSERT INTO coupons (code, max_uses) VALUES ('WELCOME', 1), ('SAFE', 1) ON CONFLICT DO NOTHING;
INSERT INTO accounts VALUES (1, 100), (2, 100) ON CONFLICT DO NOTHING;
`

var db *pgxpool.Pool

func main() {
	ctx := context.Background()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		url = "postgres://postgres@localhost:5432/shop"
	}
	var err error
	if db, err = pgxpool.New(ctx, url); err != nil {
		log.Fatal(err)
	}
	if _, err := db.Exec(ctx, schema); err != nil {
		log.Fatal(err)
	}

	http.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	http.HandleFunc("POST /redeem", redeem)
	http.HandleFunc("POST /redeem-safe", redeemSafe)
	http.HandleFunc("POST /withdraw", withdraw)
	http.HandleFunc("POST /withdraw-safe", withdrawSafe)
	http.HandleFunc("POST /signup", signup)
	http.HandleFunc("POST /signup-safe", signupSafe)
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = "localhost:8080"
	}
	log.Printf("shop listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}

// redeem checks the coupon, then records the use. Two requests can both
// see uses = 0 and both redeem a single-use coupon.
func redeem(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tx, err := db.Begin(ctx)
	if err != nil {
		fail(w, err)
		return
	}
	defer tx.Rollback(ctx)
	var id, maxUses, uses int
	err = tx.QueryRow(ctx, `SELECT id, max_uses, uses FROM coupons WHERE code = $1`, r.FormValue("code")).Scan(&id, &maxUses, &uses)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "no such coupon", http.StatusNotFound)
		return
	} else if err != nil {
		fail(w, err)
		return
	}
	if uses >= maxUses {
		http.Error(w, "coupon already used", http.StatusConflict)
		return
	}
	if _, err := tx.Exec(ctx, `INSERT INTO redemptions (coupon_id, user_name) VALUES ($1, $2)`, id, r.FormValue("user")); err != nil {
		fail(w, err)
		return
	}
	if _, err := tx.Exec(ctx, `UPDATE coupons SET uses = $1 WHERE id = $2`, uses+1, id); err != nil {
		fail(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		fail(w, err)
		return
	}
	fmt.Fprintln(w, "redeemed")
}

// redeemSafe is redeem with the coupon row locked while it is checked.
func redeemSafe(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tx, err := db.Begin(ctx)
	if err != nil {
		fail(w, err)
		return
	}
	defer tx.Rollback(ctx)
	var id, maxUses, uses int
	err = tx.QueryRow(ctx, `SELECT id, max_uses, uses FROM coupons WHERE code = $1 FOR UPDATE`, r.FormValue("code")).Scan(&id, &maxUses, &uses)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "no such coupon", http.StatusNotFound)
		return
	} else if err != nil {
		fail(w, err)
		return
	}
	if uses >= maxUses {
		http.Error(w, "coupon already used", http.StatusConflict)
		return
	}
	if _, err := tx.Exec(ctx, `INSERT INTO redemptions (coupon_id, user_name) VALUES ($1, $2)`, id, r.FormValue("user")); err != nil {
		fail(w, err)
		return
	}
	if _, err := tx.Exec(ctx, `UPDATE coupons SET uses = uses + 1 WHERE id = $1`, id); err != nil {
		fail(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		fail(w, err)
		return
	}
	fmt.Fprintln(w, "redeemed")
}

// withdraw checks the balance, then subtracts. No transaction at all: two
// withdrawals of 80 from 100 both pass the check.
func withdraw(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	account, amount := atoi(r.FormValue("account")), atoi(r.FormValue("amount"))
	var balance int
	if err := db.QueryRow(ctx, `SELECT balance FROM accounts WHERE id = $1`, account).Scan(&balance); err != nil {
		fail(w, err)
		return
	}
	if balance < amount {
		http.Error(w, "insufficient funds", http.StatusConflict)
		return
	}
	if _, err := db.Exec(ctx, `UPDATE accounts SET balance = balance - $1 WHERE id = $2`, amount, account); err != nil {
		fail(w, err)
		return
	}
	if _, err := db.Exec(ctx, `INSERT INTO ledger (account_id, amount) VALUES ($1, $2)`, account, -amount); err != nil {
		fail(w, err)
		return
	}
	fmt.Fprintf(w, "withdrew %d\n", amount)
}

// withdrawSafe reads the balance too, but the UPDATE checks it again.
func withdrawSafe(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	account, amount := atoi(r.FormValue("account")), atoi(r.FormValue("amount"))
	var balance int
	if err := db.QueryRow(ctx, `SELECT balance FROM accounts WHERE id = $1`, account).Scan(&balance); err != nil {
		fail(w, err)
		return
	}
	if balance < amount {
		http.Error(w, "insufficient funds", http.StatusConflict)
		return
	}
	tag, err := db.Exec(ctx, `UPDATE accounts SET balance = balance - $1 WHERE id = $2 AND balance >= $1`, amount, account)
	if err != nil {
		fail(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		http.Error(w, "insufficient funds", http.StatusConflict)
		return
	}
	if _, err := db.Exec(ctx, `INSERT INTO ledger (account_id, amount) VALUES ($1, $2)`, account, -amount); err != nil {
		fail(w, err)
		return
	}
	fmt.Fprintf(w, "withdrew %d\n", amount)
}

// signup checks that the email is free, then inserts it. Nothing in the
// database stops a duplicate.
func signup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	email := r.FormValue("email")
	var exists bool
	if err := db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE email = $1)`, email).Scan(&exists); err != nil {
		fail(w, err)
		return
	}
	if exists {
		http.Error(w, "email taken", http.StatusConflict)
		return
	}
	if _, err := db.Exec(ctx, `INSERT INTO users (email) VALUES ($1)`, email); err != nil {
		fail(w, err)
		return
	}
	fmt.Fprintln(w, "welcome")
}

// signupSafe has the same check, backed by a unique index.
func signupSafe(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	email := r.FormValue("email")
	var exists bool
	if err := db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users_safe WHERE email = $1)`, email).Scan(&exists); err != nil {
		fail(w, err)
		return
	}
	if exists {
		http.Error(w, "email taken", http.StatusConflict)
		return
	}
	_, err := db.Exec(ctx, `INSERT INTO users_safe (email) VALUES ($1)`, email)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		http.Error(w, "email taken", http.StatusConflict)
		return
	} else if err != nil {
		fail(w, err)
		return
	}
	fmt.Fprintln(w, "welcome")
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

func fail(w http.ResponseWriter, err error) {
	log.Print(err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}
