"""A small inventory API in plain Python (http.server + psycopg2).

psycopg2 sends SQL with the simple query protocol and inlines the values,
unlike the Go example, which uses prepared statements. doubledip handles
both:

    doubledip --db postgres://postgres@localhost:5432/shop \
      --app 'python3 examples/shop-py/app.py' --ready http://localhost:8091/health \
      'curl -fsS -X POST "localhost:8091/buy?product=1&qty=3"'
"""

import os
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse

import psycopg2
import psycopg2.errors
import psycopg2.extensions

DB = os.environ.get("DATABASE_URL", "postgres://postgres@localhost:5432/shop")

SCHEMA = """
CREATE TABLE IF NOT EXISTS products (id int PRIMARY KEY, name text NOT NULL, stock int NOT NULL);
CREATE TABLE IF NOT EXISTS orders (id serial PRIMARY KEY, product_id int NOT NULL REFERENCES products, qty int NOT NULL);
INSERT INTO products VALUES (1, 'last-pair-of-sneakers', 4), (2, 'limited-vinyl', 4) ON CONFLICT DO NOTHING;
"""


def buy(conn, product, qty):
    """Reads the stock, then writes the new value: two buyers both see 4."""
    with conn, conn.cursor() as cur:
        cur.execute("SELECT stock FROM products WHERE id = %s", (product,))
        (stock,) = cur.fetchone()
        if stock < qty:
            return 409, "out of stock"
        cur.execute("UPDATE products SET stock = %s WHERE id = %s", (stock - qty, product))
        cur.execute("INSERT INTO orders (product_id, qty) VALUES (%s, %s)", (product, qty))
    return 200, "bought"


def buy_safe(conn, product, qty):
    """The same code at SERIALIZABLE, retried when Postgres aborts it."""
    conn.set_session(isolation_level=psycopg2.extensions.ISOLATION_LEVEL_SERIALIZABLE)
    for _ in range(5):
        try:
            return buy(conn, product, qty)
        except psycopg2.errors.SerializationFailure:
            continue
    return 503, "busy, try again"


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        self.reply(200, "ok")

    def do_POST(self):
        url = urlparse(self.path)
        q = {k: v[0] for k, v in parse_qs(url.query).items()}
        handler = {"/buy": buy, "/buy-safe": buy_safe}.get(url.path)
        if handler is None:
            return self.reply(404, "not found")
        conn = psycopg2.connect(DB)
        try:
            self.reply(*handler(conn, int(q.get("product", 1)), int(q.get("qty", 1))))
        finally:
            conn.close()

    def reply(self, code, text):
        body = (text + "\n").encode()
        self.send_response(code)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass


if __name__ == "__main__":
    with psycopg2.connect(DB) as conn, conn.cursor() as cur:
        cur.execute(SCHEMA)
    addr = os.environ.get("ADDR", "localhost:8091").rsplit(":", 1)
    print("shop-py listening on", ":".join(addr), flush=True)
    ThreadingHTTPServer((addr[0], int(addr[1])), Handler).serve_forever()
