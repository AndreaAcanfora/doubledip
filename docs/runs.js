window.RUNS = {
  "doubledip": "dev",
  "confirmed": 3,
  "scenarios": [
    {
      "name": "redeem a single-use coupon",
      "requests": [
        "curl -sS -X POST \"localhost:8080/redeem?code=WELCOME\u0026user=u$DOUBLEDIP_N\""
      ],
      "statements": [
        {
          "tx": 1,
          "sql": "begin",
          "tag": "BEGIN",
          "isolation": "read committed"
        },
        {
          "tx": 1,
          "sql": "SELECT id, max_uses, uses FROM coupons WHERE code = $1",
          "params": [
            "WELCOME"
          ],
          "tag": "SELECT 1",
          "isolation": "read committed"
        },
        {
          "tx": 1,
          "sql": "INSERT INTO redemptions (coupon_id, user_name) VALUES ($1, $2)",
          "params": [
            "1",
            "u1"
          ],
          "tag": "INSERT 0 1",
          "isolation": "read committed"
        },
        {
          "tx": 1,
          "sql": "UPDATE coupons SET uses = $1 WHERE id = $2",
          "params": [
            "1",
            "1"
          ],
          "tag": "UPDATE 1",
          "isolation": "read committed"
        },
        {
          "tx": 1,
          "sql": "commit",
          "tag": "COMMIT",
          "isolation": "read committed"
        }
      ],
      "findings": [
        {
          "outcome": "confirmed",
          "pattern": "lost update",
          "table": "coupons",
          "read": {
            "tx": 1,
            "sql": "SELECT id, max_uses, uses FROM coupons WHERE code = $1",
            "params": [
              "WELCOME"
            ],
            "tag": "SELECT 1",
            "isolation": "read committed"
          },
          "write": {
            "tx": 1,
            "sql": "UPDATE coupons SET uses = $1 WHERE id = $2",
            "params": [
              "1",
              "1"
            ],
            "tag": "UPDATE 1",
            "isolation": "read committed"
          },
          "held_at": {
            "tx": 1,
            "sql": "INSERT INTO redemptions (coupon_id, user_name) VALUES ($1, $2)",
            "params": [
              "1",
              "u1"
            ],
            "tag": "INSERT 0 1",
            "isolation": "read committed"
          },
          "arrived": 2,
          "diff": [
            {
              "table": "redemptions",
              "serial_rows": 1,
              "concurrent_rows": 2,
              "only_concurrent": [
                "(1,u2)"
              ]
            }
          ],
          "fix": [
            "do the arithmetic in the UPDATE (SET uses = uses ± $n), put the check in its WHERE clause and treat 0 rows as \"rejected\"",
            "or read the row with SELECT ... FOR UPDATE in the same transaction as the UPDATE of coupons"
          ],
          "serial": [
            {
              "exit_code": 0,
              "output": "redeemed\n"
            },
            {
              "exit_code": 0,
              "output": "coupon already used\n"
            }
          ],
          "concurrent": [
            {
              "exit_code": 0,
              "output": "redeemed\n"
            },
            {
              "exit_code": 0,
              "output": "redeemed\n"
            }
          ],
          "trace": [
            {
              "conn": 1,
              "tx": 10,
              "sql": "begin",
              "tag": "BEGIN",
              "start_ms": 9.9,
              "end_ms": 10.3
            },
            {
              "conn": 1,
              "tx": 10,
              "sql": "SELECT id, max_uses, uses FROM coupons WHERE code = $1",
              "params": [
                "WELCOME"
              ],
              "tag": "SELECT 1",
              "start_ms": 10.5,
              "end_ms": 11.2
            },
            {
              "conn": 2,
              "tx": 12,
              "sql": "begin",
              "tag": "BEGIN",
              "start_ms": 14.6,
              "end_ms": 14.8
            },
            {
              "conn": 2,
              "tx": 12,
              "sql": "SELECT id, max_uses, uses FROM coupons WHERE code = $1",
              "params": [
                "WELCOME"
              ],
              "tag": "SELECT 1",
              "start_ms": 15.8,
              "end_ms": 16.6
            },
            {
              "conn": 1,
              "tx": 10,
              "sql": "INSERT INTO redemptions (coupon_id, user_name) VALUES ($1, $2)",
              "params": [
                "1",
                "u1"
              ],
              "tag": "INSERT 0 1",
              "start_ms": 11.3,
              "end_ms": 18.3,
              "held": true
            },
            {
              "conn": 2,
              "tx": 12,
              "sql": "INSERT INTO redemptions (coupon_id, user_name) VALUES ($1, $2)",
              "params": [
                "1",
                "u2"
              ],
              "tag": "INSERT 0 1",
              "start_ms": 17.4,
              "end_ms": 18.4,
              "held": true
            },
            {
              "conn": 1,
              "tx": 10,
              "sql": "UPDATE coupons SET uses = $1 WHERE id = $2",
              "params": [
                "1",
                "1"
              ],
              "tag": "UPDATE 1",
              "start_ms": 18.5,
              "end_ms": 18.8
            },
            {
              "conn": 1,
              "tx": 10,
              "sql": "commit",
              "tag": "COMMIT",
              "start_ms": 19.2,
              "end_ms": 20.2
            },
            {
              "conn": 2,
              "tx": 12,
              "sql": "UPDATE coupons SET uses = $1 WHERE id = $2",
              "params": [
                "1",
                "1"
              ],
              "tag": "UPDATE 1",
              "start_ms": 19.1,
              "end_ms": 20.3
            },
            {
              "conn": 2,
              "tx": 12,
              "sql": "commit",
              "tag": "COMMIT",
              "start_ms": 20.5,
              "end_ms": 21.8
            }
          ]
        }
      ],
      "confirmed": 1
    },
    {
      "name": "redeem a single-use coupon (FOR UPDATE)",
      "requests": [
        "curl -sS -X POST \"localhost:8080/redeem-safe?code=SAFE\u0026user=u$DOUBLEDIP_N\""
      ],
      "statements": [
        {
          "tx": 1,
          "sql": "begin",
          "tag": "BEGIN",
          "isolation": "read committed"
        },
        {
          "tx": 1,
          "sql": "SELECT id, max_uses, uses FROM coupons WHERE code = $1 FOR UPDATE",
          "params": [
            "SAFE"
          ],
          "tag": "SELECT 1",
          "isolation": "read committed"
        },
        {
          "tx": 1,
          "sql": "INSERT INTO redemptions (coupon_id, user_name) VALUES ($1, $2)",
          "params": [
            "2",
            "u1"
          ],
          "tag": "INSERT 0 1",
          "isolation": "read committed"
        },
        {
          "tx": 1,
          "sql": "UPDATE coupons SET uses = uses + 1 WHERE id = $1",
          "params": [
            "2"
          ],
          "tag": "UPDATE 1",
          "isolation": "read committed"
        },
        {
          "tx": 1,
          "sql": "commit",
          "tag": "COMMIT",
          "isolation": "read committed"
        }
      ],
      "findings": [
        {
          "outcome": "serialized",
          "pattern": "check-then-act",
          "table": "coupons",
          "read": {
            "tx": 1,
            "sql": "SELECT id, max_uses, uses FROM coupons WHERE code = $1 FOR UPDATE",
            "params": [
              "SAFE"
            ],
            "tag": "SELECT 1",
            "isolation": "read committed"
          },
          "write": {
            "tx": 1,
            "sql": "UPDATE coupons SET uses = uses + 1 WHERE id = $1",
            "params": [
              "2"
            ],
            "tag": "UPDATE 1",
            "isolation": "read committed"
          },
          "held_at": {
            "tx": 1,
            "sql": "INSERT INTO redemptions (coupon_id, user_name) VALUES ($1, $2)",
            "params": [
              "2",
              "u1"
            ],
            "tag": "INSERT 0 1",
            "isolation": "read committed"
          },
          "arrived": 1,
          "fix": [
            "put the check in the write's WHERE clause (e.g. WHERE ... AND uses \u003e= $amount) and treat 0 rows as \"rejected\"",
            "or read with SELECT ... FOR UPDATE in the same transaction as the write to coupons"
          ],
          "why": "the read takes a row lock (FOR UPDATE) in the same transaction",
          "serial": [
            {
              "exit_code": 0,
              "output": "redeemed\n"
            },
            {
              "exit_code": 0,
              "output": "coupon already used\n"
            }
          ],
          "concurrent": [
            {
              "exit_code": 0,
              "output": "coupon already used\n"
            },
            {
              "exit_code": 0,
              "output": "redeemed\n"
            }
          ],
          "trace": [
            {
              "conn": 2,
              "tx": 21,
              "sql": "begin",
              "tag": "BEGIN",
              "start_ms": 8.8,
              "end_ms": 9.1
            },
            {
              "conn": 1,
              "tx": 13,
              "sql": "begin",
              "tag": "BEGIN",
              "start_ms": 8.8,
              "end_ms": 9.2
            },
            {
              "conn": 2,
              "tx": 21,
              "sql": "SELECT id, max_uses, uses FROM coupons WHERE code = $1 FOR UPDATE",
              "params": [
                "SAFE"
              ],
              "tag": "SELECT 1",
              "start_ms": 9.2,
              "end_ms": 9.8
            },
            {
              "conn": 2,
              "tx": 21,
              "sql": "INSERT INTO redemptions (coupon_id, user_name) VALUES ($1, $2)",
              "params": [
                "2",
                "u2"
              ],
              "tag": "INSERT 0 1",
              "start_ms": 9.9,
              "end_ms": 3011.6,
              "held": true
            },
            {
              "conn": 2,
              "tx": 21,
              "sql": "UPDATE coupons SET uses = uses + 1 WHERE id = $1",
              "params": [
                "2"
              ],
              "tag": "UPDATE 1",
              "start_ms": 3011.9,
              "end_ms": 3012.6
            },
            {
              "conn": 1,
              "tx": 13,
              "sql": "SELECT id, max_uses, uses FROM coupons WHERE code = $1 FOR UPDATE",
              "params": [
                "SAFE"
              ],
              "tag": "SELECT 1",
              "start_ms": 9.9,
              "end_ms": 3014.5
            },
            {
              "conn": 2,
              "tx": 21,
              "sql": "commit",
              "tag": "COMMIT",
              "start_ms": 3013.2,
              "end_ms": 3014.6
            },
            {
              "conn": 1,
              "tx": 13,
              "sql": "rollback",
              "tag": "ROLLBACK",
              "start_ms": 3016.4,
              "end_ms": 3017
            }
          ]
        }
      ],
      "confirmed": 0
    },
    {
      "name": "withdraw 80 from 100",
      "requests": [
        "curl -sS -X POST \"localhost:8080/withdraw?account=1\u0026amount=80\""
      ],
      "statements": [
        {
          "tx": 1,
          "sql": "SELECT balance FROM accounts WHERE id = $1",
          "params": [
            "1"
          ],
          "tag": "SELECT 1",
          "isolation": "read committed"
        },
        {
          "tx": 2,
          "sql": "UPDATE accounts SET balance = balance - $1 WHERE id = $2",
          "params": [
            "80",
            "1"
          ],
          "tag": "UPDATE 1",
          "isolation": "read committed"
        },
        {
          "tx": 3,
          "sql": "INSERT INTO ledger (account_id, amount) VALUES ($1, $2)",
          "params": [
            "1",
            "-80"
          ],
          "tag": "INSERT 0 1",
          "isolation": "read committed"
        }
      ],
      "findings": [
        {
          "outcome": "confirmed",
          "pattern": "check-then-act",
          "table": "accounts",
          "read": {
            "tx": 1,
            "sql": "SELECT balance FROM accounts WHERE id = $1",
            "params": [
              "1"
            ],
            "tag": "SELECT 1",
            "isolation": "read committed"
          },
          "write": {
            "tx": 2,
            "sql": "UPDATE accounts SET balance = balance - $1 WHERE id = $2",
            "params": [
              "80",
              "1"
            ],
            "tag": "UPDATE 1",
            "isolation": "read committed"
          },
          "held_at": {
            "tx": 2,
            "sql": "UPDATE accounts SET balance = balance - $1 WHERE id = $2",
            "params": [
              "80",
              "1"
            ],
            "tag": "UPDATE 1",
            "isolation": "read committed"
          },
          "arrived": 2,
          "diff": [
            {
              "table": "accounts",
              "serial_rows": 2,
              "concurrent_rows": 2,
              "only_serial": [
                "(1,20)"
              ],
              "only_concurrent": [
                "(1,-60)"
              ]
            },
            {
              "table": "ledger",
              "serial_rows": 1,
              "concurrent_rows": 2,
              "only_concurrent": [
                "(1,-80)"
              ]
            }
          ],
          "fix": [
            "the read (transaction 1) and the write (transaction 2) are in separate transactions: put them in one",
            "put the check in the write's WHERE clause (e.g. WHERE ... AND balance \u003e= $amount) and treat 0 rows as \"rejected\"",
            "or read with SELECT ... FOR UPDATE in the same transaction as the write to accounts"
          ],
          "serial": [
            {
              "exit_code": 0,
              "output": "withdrew 80\n"
            },
            {
              "exit_code": 0,
              "output": "insufficient funds\n"
            }
          ],
          "concurrent": [
            {
              "exit_code": 0,
              "output": "withdrew 80\n"
            },
            {
              "exit_code": 0,
              "output": "withdrew 80\n"
            }
          ],
          "trace": [
            {
              "conn": 1,
              "tx": 41,
              "sql": "SELECT balance FROM accounts WHERE id = $1",
              "params": [
                "1"
              ],
              "tag": "SELECT 1",
              "start_ms": 9.8,
              "end_ms": 10.4
            },
            {
              "conn": 2,
              "tx": 43,
              "sql": "SELECT balance FROM accounts WHERE id = $1",
              "params": [
                "1"
              ],
              "tag": "SELECT 1",
              "start_ms": 12.5,
              "end_ms": 12.9
            },
            {
              "conn": 2,
              "tx": 45,
              "sql": "UPDATE accounts SET balance = balance - $1 WHERE id = $2",
              "params": [
                "80",
                "1"
              ],
              "tag": "UPDATE 1",
              "start_ms": 13.7,
              "end_ms": 15.1,
              "held": true
            },
            {
              "conn": 1,
              "tx": 42,
              "sql": "UPDATE accounts SET balance = balance - $1 WHERE id = $2",
              "params": [
                "80",
                "1"
              ],
              "tag": "UPDATE 1",
              "start_ms": 10.8,
              "end_ms": 15.7,
              "held": true
            },
            {
              "conn": 1,
              "tx": 48,
              "sql": "INSERT INTO ledger (account_id, amount) VALUES ($1, $2)",
              "params": [
                "1",
                "-80"
              ],
              "tag": "INSERT 0 1",
              "start_ms": 16,
              "end_ms": 17.3
            },
            {
              "conn": 2,
              "tx": 47,
              "sql": "INSERT INTO ledger (account_id, amount) VALUES ($1, $2)",
              "params": [
                "1",
                "-80"
              ],
              "tag": "INSERT 0 1",
              "start_ms": 15.9,
              "end_ms": 17.8
            }
          ]
        }
      ],
      "confirmed": 1
    },
    {
      "name": "withdraw 80 from 100 (guarded UPDATE)",
      "requests": [
        "curl -sS -X POST \"localhost:8080/withdraw-safe?account=2\u0026amount=80\""
      ],
      "statements": [
        {
          "tx": 1,
          "sql": "SELECT balance FROM accounts WHERE id = $1",
          "params": [
            "2"
          ],
          "tag": "SELECT 1",
          "isolation": "read committed"
        },
        {
          "tx": 2,
          "sql": "UPDATE accounts SET balance = balance - $1 WHERE id = $2 AND balance \u003e= $1",
          "params": [
            "80",
            "2"
          ],
          "tag": "UPDATE 1",
          "isolation": "read committed"
        },
        {
          "tx": 3,
          "sql": "INSERT INTO ledger (account_id, amount) VALUES ($1, $2)",
          "params": [
            "2",
            "-80"
          ],
          "tag": "INSERT 0 1",
          "isolation": "read committed"
        }
      ],
      "findings": [
        {
          "outcome": "safe",
          "pattern": "check-then-act",
          "table": "accounts",
          "read": {
            "tx": 1,
            "sql": "SELECT balance FROM accounts WHERE id = $1",
            "params": [
              "2"
            ],
            "tag": "SELECT 1",
            "isolation": "read committed"
          },
          "write": {
            "tx": 2,
            "sql": "UPDATE accounts SET balance = balance - $1 WHERE id = $2 AND balance \u003e= $1",
            "params": [
              "80",
              "2"
            ],
            "tag": "UPDATE 1",
            "isolation": "read committed"
          },
          "held_at": {
            "tx": 2,
            "sql": "UPDATE accounts SET balance = balance - $1 WHERE id = $2 AND balance \u003e= $1",
            "params": [
              "80",
              "2"
            ],
            "tag": "UPDATE 1",
            "isolation": "read committed"
          },
          "arrived": 2,
          "fix": [
            "the read (transaction 1) and the write (transaction 2) are in separate transactions: put them in one",
            "put the check in the write's WHERE clause (e.g. WHERE ... AND balance \u003e= $amount) and treat 0 rows as \"rejected\"",
            "or read with SELECT ... FOR UPDATE in the same transaction as the write to accounts"
          ],
          "why": "the UPDATE re-checks balance in its own WHERE clause",
          "serial": [
            {
              "exit_code": 0,
              "output": "withdrew 80\n"
            },
            {
              "exit_code": 0,
              "output": "insufficient funds\n"
            }
          ],
          "concurrent": [
            {
              "exit_code": 0,
              "output": "withdrew 80\n"
            },
            {
              "exit_code": 0,
              "output": "insufficient funds\n"
            }
          ],
          "trace": [
            {
              "conn": 1,
              "tx": 49,
              "sql": "SELECT balance FROM accounts WHERE id = $1",
              "params": [
                "2"
              ],
              "tag": "SELECT 1",
              "start_ms": 9.3,
              "end_ms": 9.8
            },
            {
              "conn": 2,
              "tx": 66,
              "sql": "SELECT balance FROM accounts WHERE id = $1",
              "params": [
                "2"
              ],
              "tag": "SELECT 1",
              "start_ms": 9.2,
              "end_ms": 9.9
            },
            {
              "conn": 1,
              "tx": 69,
              "sql": "UPDATE accounts SET balance = balance - $1 WHERE id = $2 AND balance \u003e= $1",
              "params": [
                "80",
                "2"
              ],
              "tag": "UPDATE 1",
              "start_ms": 10.8,
              "end_ms": 12,
              "held": true
            },
            {
              "conn": 2,
              "tx": 68,
              "sql": "UPDATE accounts SET balance = balance - $1 WHERE id = $2 AND balance \u003e= $1",
              "params": [
                "80",
                "2"
              ],
              "tag": "UPDATE 0",
              "start_ms": 10.1,
              "end_ms": 13,
              "held": true
            },
            {
              "conn": 1,
              "tx": 70,
              "sql": "INSERT INTO ledger (account_id, amount) VALUES ($1, $2)",
              "params": [
                "2",
                "-80"
              ],
              "tag": "INSERT 0 1",
              "start_ms": 12.2,
              "end_ms": 13.9
            }
          ]
        }
      ],
      "confirmed": 0
    },
    {
      "name": "sign up with the same email",
      "requests": [
        "curl -sS -X POST \"localhost:8080/signup?email=ada@example.com\""
      ],
      "statements": [
        {
          "tx": 1,
          "sql": "SELECT EXISTS (SELECT 1 FROM users WHERE email = $1)",
          "params": [
            "ada@example.com"
          ],
          "tag": "SELECT 1",
          "isolation": "read committed"
        },
        {
          "tx": 2,
          "sql": "INSERT INTO users (email) VALUES ($1)",
          "params": [
            "ada@example.com"
          ],
          "tag": "INSERT 0 1",
          "isolation": "read committed"
        }
      ],
      "findings": [
        {
          "outcome": "confirmed",
          "pattern": "check-then-insert",
          "table": "users",
          "read": {
            "tx": 1,
            "sql": "SELECT EXISTS (SELECT 1 FROM users WHERE email = $1)",
            "params": [
              "ada@example.com"
            ],
            "tag": "SELECT 1",
            "isolation": "read committed"
          },
          "write": {
            "tx": 2,
            "sql": "INSERT INTO users (email) VALUES ($1)",
            "params": [
              "ada@example.com"
            ],
            "tag": "INSERT 0 1",
            "isolation": "read committed"
          },
          "held_at": {
            "tx": 2,
            "sql": "INSERT INTO users (email) VALUES ($1)",
            "params": [
              "ada@example.com"
            ],
            "tag": "INSERT 0 1",
            "isolation": "read committed"
          },
          "arrived": 2,
          "diff": [
            {
              "table": "users",
              "serial_rows": 1,
              "concurrent_rows": 2,
              "only_concurrent": [
                "(ada@example.com)"
              ]
            }
          ],
          "fix": [
            "the read (transaction 1) and the write (transaction 2) are in separate transactions: put them in one",
            "CREATE UNIQUE INDEX ON users (email), then treat the unique violation (23505) as \"already exists\"",
            "or run the transaction at SERIALIZABLE and retry on 40001"
          ],
          "serial": [
            {
              "exit_code": 0,
              "output": "welcome\n"
            },
            {
              "exit_code": 0,
              "output": "email taken\n"
            }
          ],
          "concurrent": [
            {
              "exit_code": 0,
              "output": "welcome\n"
            },
            {
              "exit_code": 0,
              "output": "welcome\n"
            }
          ],
          "trace": [
            {
              "conn": 1,
              "tx": 85,
              "sql": "SELECT EXISTS (SELECT 1 FROM users WHERE email = $1)",
              "params": [
                "ada@example.com"
              ],
              "tag": "SELECT 1",
              "start_ms": 8.8,
              "end_ms": 9.5
            },
            {
              "conn": 2,
              "tx": 87,
              "sql": "SELECT EXISTS (SELECT 1 FROM users WHERE email = $1)",
              "params": [
                "ada@example.com"
              ],
              "tag": "SELECT 1",
              "start_ms": 9.8,
              "end_ms": 10.3
            },
            {
              "conn": 1,
              "tx": 86,
              "sql": "INSERT INTO users (email) VALUES ($1)",
              "params": [
                "ada@example.com"
              ],
              "tag": "INSERT 0 1",
              "start_ms": 9.7,
              "end_ms": 12.4,
              "held": true
            },
            {
              "conn": 2,
              "tx": 89,
              "sql": "INSERT INTO users (email) VALUES ($1)",
              "params": [
                "ada@example.com"
              ],
              "tag": "INSERT 0 1",
              "start_ms": 11,
              "end_ms": 13,
              "held": true
            }
          ]
        }
      ],
      "confirmed": 1
    },
    {
      "name": "sign up with the same email (unique index)",
      "requests": [
        "curl -sS -X POST \"localhost:8080/signup-safe?email=ada@example.com\""
      ],
      "statements": [
        {
          "tx": 1,
          "sql": "SELECT EXISTS (SELECT 1 FROM users_safe WHERE email = $1)",
          "params": [
            "ada@example.com"
          ],
          "tag": "SELECT 1",
          "isolation": "read committed"
        },
        {
          "tx": 2,
          "sql": "INSERT INTO users_safe (email) VALUES ($1)",
          "params": [
            "ada@example.com"
          ],
          "tag": "INSERT 0 1",
          "isolation": "read committed"
        }
      ],
      "findings": [
        {
          "outcome": "safe",
          "pattern": "check-then-insert",
          "table": "users_safe",
          "read": {
            "tx": 1,
            "sql": "SELECT EXISTS (SELECT 1 FROM users_safe WHERE email = $1)",
            "params": [
              "ada@example.com"
            ],
            "tag": "SELECT 1",
            "isolation": "read committed"
          },
          "write": {
            "tx": 2,
            "sql": "INSERT INTO users_safe (email) VALUES ($1)",
            "params": [
              "ada@example.com"
            ],
            "tag": "INSERT 0 1",
            "isolation": "read committed"
          },
          "held_at": {
            "tx": 2,
            "sql": "INSERT INTO users_safe (email) VALUES ($1)",
            "params": [
              "ada@example.com"
            ],
            "tag": "INSERT 0 1",
            "isolation": "read committed"
          },
          "arrived": 2,
          "fix": [
            "the read (transaction 1) and the write (transaction 2) are in separate transactions: put them in one",
            "CREATE UNIQUE INDEX ON users_safe (email), then treat the unique violation (23505) as \"already exists\"",
            "or run the transaction at SERIALIZABLE and retry on 40001"
          ],
          "serial": [
            {
              "exit_code": 0,
              "output": "welcome\n"
            },
            {
              "exit_code": 0,
              "output": "email taken\n"
            }
          ],
          "concurrent": [
            {
              "exit_code": 0,
              "output": "email taken\n"
            },
            {
              "exit_code": 0,
              "output": "welcome\n"
            }
          ],
          "trace": [
            {
              "conn": 2,
              "tx": 104,
              "sql": "SELECT EXISTS (SELECT 1 FROM users_safe WHERE email = $1)",
              "params": [
                "ada@example.com"
              ],
              "tag": "SELECT 1",
              "start_ms": 8.5,
              "end_ms": 9.1
            },
            {
              "conn": 1,
              "tx": 105,
              "sql": "SELECT EXISTS (SELECT 1 FROM users_safe WHERE email = $1)",
              "params": [
                "ada@example.com"
              ],
              "tag": "SELECT 1",
              "start_ms": 9.1,
              "end_ms": 9.4
            },
            {
              "conn": 1,
              "tx": 108,
              "sql": "INSERT INTO users_safe (email) VALUES ($1)",
              "params": [
                "ada@example.com"
              ],
              "tag": "INSERT 0 1",
              "start_ms": 9.8,
              "end_ms": 10.8,
              "held": true
            },
            {
              "conn": 2,
              "tx": 106,
              "sql": "INSERT INTO users_safe (email) VALUES ($1)",
              "params": [
                "ada@example.com"
              ],
              "error": "23505 duplicate key value violates unique constraint \"users_safe_email_key\"",
              "start_ms": 9.3,
              "end_ms": 11.1,
              "held": true
            }
          ]
        }
      ],
      "confirmed": 0
    }
  ]
}
;
