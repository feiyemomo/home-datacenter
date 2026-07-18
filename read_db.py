import sqlite3
import os

db_path = os.path.join(os.environ.get("TEMP", "/tmp"), "db.bin")
conn = sqlite3.connect(db_path)
cur = conn.cursor()

cur.execute("SELECT name FROM sqlite_master WHERE type='table'")
print("Tables:", [r[0] for r in cur.fetchall()])

print("\nDevices:")
cur.execute("SELECT id, user_id, name, access_key_hash FROM devices")
for r in cur.fetchall():
    print(f"  id={r[0]} user_id={r[1]} name={r[2]} hash={r[3][:20]}...")

print("\nUsers:")
cur.execute("SELECT id, username, is_admin FROM users")
for r in cur.fetchall():
    print(f"  id={r[0]} username={r[1]} admin={r[2]}")

conn.close()
