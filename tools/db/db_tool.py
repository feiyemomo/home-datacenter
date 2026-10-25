import sys
import sqlite3
import json
import argparse
import subprocess
from pathlib import Path

REMOTE_PY = '''
import sys, sqlite3, json
p = "/vol1/docker/home-datacenter/data/sqlite/app.db"
conn = sqlite3.connect(f"file:{p}?mode=ro", uri=True)
conn.row_factory = sqlite3.Row
c = conn.cursor()
mode = sys.argv[1]
if mode == "tables":
    c.execute("SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name")
    print(json.dumps([r[0] for r in c.fetchall()], indent=2))
elif mode == "query":
    q = sys.argv[2]
    c.execute(q)
    rows = [dict(r) for r in c.fetchall()]
    print(json.dumps(rows, indent=2, ensure_ascii=False, default=str))
elif mode == "summary":
    c.execute("SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name")
    res = {}
    for t in [r[0] for r in c.fetchall()]:
        try:
            c.execute(f'SELECT COUNT(*) FROM "{t}"')
            res[t] = c.fetchone()[0]
        except Exception:
            res[t] = "err"
    print(json.dumps({"remote_db": p, "tables": res}, indent=2, ensure_ascii=False))
'''

def run_local(db_path, query=None, list_tables=False):
    p = Path(db_path)
    if not p.is_file():
        print(f"Error: Local DB '{db_path}' not found.", file=sys.stderr)
        sys.exit(1)
    conn = sqlite3.connect(f"file:{p.resolve()}?mode=ro", uri=True)
    conn.row_factory = sqlite3.Row
    c = conn.cursor()
    if list_tables:
        c.execute("SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name")
        print(json.dumps([r[0] for r in c.fetchall()], indent=2))
    elif query:
        c.execute(query)
        rows = [dict(r) for r in c.fetchall()]
        print(json.dumps(rows, indent=2, ensure_ascii=False, default=str))
    else:
        c.execute("SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name")
        res = {}
        for t in [r[0] for r in c.fetchall()]:
            try:
                c.execute(f'SELECT COUNT(*) FROM "{t}"')
                res[t] = c.fetchone()[0]
            except Exception:
                res[t] = "err"
        print(json.dumps({"local_db": str(p), "tables": res}, indent=2, ensure_ascii=False))

def run_remote(query=None, list_tables=False):
    import base64
    b64 = base64.b64encode(REMOTE_PY.encode('utf-8')).decode('utf-8')
    if list_tables:
        cmd = f"python3 -c \"import base64; exec(base64.b64decode('{b64}').decode('utf-8'))\" tables"
    elif query:
        q_b64 = base64.b64encode(query.encode('utf-8')).decode('utf-8')
        cmd = f"python3 -c \"import base64; exec(base64.b64decode('{b64}').decode('utf-8'))\" query \"$(echo {q_b64} | base64 -d)\""
    else:
        cmd = f"python3 -c \"import base64; exec(base64.b64decode('{b64}').decode('utf-8'))\" summary"

    res = subprocess.run(["ssh", "nas", cmd], capture_output=True, text=True, encoding='utf-8')
    if res.stdout:
        print(res.stdout.strip())
    if res.stderr:
        print(res.stderr.strip(), file=sys.stderr)

def main():
    parser = argparse.ArgumentParser(description="Inspect SQLite DB (default: remote NAS production DB)")
    parser.add_argument("--local", "-l", action="store_true", help="Inspect local app.db instead of remote NAS")
    parser.add_argument("--db", default="D:/Projects/home-datacenter/data/sqlite/app.db", help="Local DB path")
    parser.add_argument("--query", "-q", help="SQL Query to execute (read-only)")
    parser.add_argument("--tables", "-t", action="store_true", help="List tables")
    args = parser.parse_args()

    if args.local:
        run_local(args.db, query=args.query, list_tables=args.tables)
    else:
        run_remote(query=args.query, list_tables=args.tables)

if __name__ == "__main__":
    main()
