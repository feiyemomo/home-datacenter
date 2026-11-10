"""
Round 4 (R4-20): Real Multi-Process Cross-Language Lease Competition Test Suite.
Spawns real OS Go (cmd/lease-probe) and Python subprocesses competing against
the SAME SQLite WAL database file without any in-process threading shortcuts or mocks.
Covers all 12 R4-20 scenarios.
"""

import json
import os
import pathlib
import sqlite3
import subprocess
import sys
import tempfile
import time
import unittest
from concurrent.futures import ThreadPoolExecutor

CURRENT_DIR = os.path.dirname(os.path.abspath(__file__))
AGENT_DIR = os.path.dirname(CURRENT_DIR)
REPO_ROOT = os.path.dirname(os.path.dirname(AGENT_DIR))
API_DIR = os.path.join(REPO_ROOT, "services", "api")

sys.path.insert(0, AGENT_DIR)
sys.path.insert(0, CURRENT_DIR)

from schema_helper import init_ops_v4_schema


PY_WORKER_SNIPPET = r"""
import json, sys
sys.path.insert(0, sys.argv[1])
from config import OpsAgentConfig
from main import HomeOpsAgent

action = sys.argv[2]
db_path = sys.argv[3]
comp = sys.argv[4]
ttl_sec = float(sys.argv[5])
token = sys.argv[6]
gen = int(sys.argv[7])

cfg = OpsAgentConfig(api_base_url="http://127.0.0.1:8080", internal_token="t" * 32, db_path=db_path)
agent = HomeOpsAgent(cfg)

if action == "acquire":
    ok, tok, fg = agent._acquire_recovery_lease_grant(comp, db_path, lease_duration=ttl_sec)
    print(json.dumps({"acquired": ok, "token": tok, "fencing_generation": fg}))
    sys.exit(0 if ok else 1)
elif action == "hold":
    hold_sec = float(sys.argv[8])
    ok, tok, fg = agent._acquire_recovery_lease_grant(comp, db_path, lease_duration=ttl_sec)
    print(json.dumps({"acquired": ok, "token": tok, "fencing_generation": fg}), flush=True)
    if not ok:
        sys.exit(1)
    import time
    time.sleep(hold_sec)
    sys.exit(0)
elif action == "renew":
    ok, exp_ms = agent._renew_recovery_lease(comp, token, gen, db_path, lease_duration=ttl_sec)
    print(json.dumps({"renewed": ok, "expires_at_ms": exp_ms}))
    sys.exit(0 if ok else 1)
elif action == "verify":
    ok = agent._verify_recovery_lease(comp, token, gen, db_path)
    print(json.dumps({"valid": ok, "token": token, "fencing_generation": gen}))
    sys.exit(0 if ok else 1)
elif action == "release":
    ok = agent._release_recovery_lease(comp, token, db_path, fencing_gen=gen)
    print(json.dumps({"released": ok}))
    sys.exit(0 if ok else 1)
"""


class TestCrossProcessLeaseR4(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.build_dir = tempfile.TemporaryDirectory()
        exe_name = "lease-probe.exe" if os.name == "nt" else "lease-probe"
        cls.go_probe_bin = os.path.join(cls.build_dir.name, exe_name)
        res = subprocess.run(
            ["go", "build", "-o", cls.go_probe_bin, "./cmd/lease-probe"],
            cwd=API_DIR,
            capture_output=True,
            text=True,
            timeout=120,
        )
        if res.returncode != 0:
            raise RuntimeError(f"Failed to build Go cmd/lease-probe: {res.stderr}\n{res.stdout}")

    @classmethod
    def tearDownClass(cls):
        try:
            cls.build_dir.cleanup()
        except Exception:
            pass

    def setUp(self):
        self.tmp_dir = tempfile.TemporaryDirectory()
        self.db_path = os.path.join(self.tmp_dir.name, "shared_wal.db")
        init_ops_v4_schema(self.db_path)

    def tearDown(self):
        try:
            self.tmp_dir.cleanup()
        except Exception:
            pass

    def _run_go(self, action, component="home-web", holder="go-core", token="", gen=0, ttl_ms=5000):
        cmd = [
            self.go_probe_bin,
            "-db", self.db_path,
            "-action", action,
            "-component", component,
            "-holder", holder,
            "-token", token,
            "-gen", str(gen),
            "-ttl-ms", str(ttl_ms),
        ]
        res = subprocess.run(cmd, capture_output=True, text=True, timeout=30)
        data = json.loads(res.stdout.strip().splitlines()[-1]) if res.stdout.strip() else {}
        return res.returncode, data

    def _run_py(self, action, component="home-web", token="", gen=0, ttl_sec=5.0):
        cmd = [
            sys.executable, "-c", PY_WORKER_SNIPPET,
            AGENT_DIR, action, self.db_path, component, str(ttl_sec), token, str(gen),
        ]
        res = subprocess.run(cmd, capture_output=True, text=True, timeout=30)
        data = json.loads(res.stdout.strip().splitlines()[-1]) if res.stdout.strip() else {}
        return res.returncode, data

    # 1. Go vs Go concurrent OS processes
    def test_01_go_vs_go_multiprocess_race(self):
        with ThreadPoolExecutor(max_workers=6) as pool:
            futs = [pool.submit(self._run_go, "acquire", "home-web", f"go-proc-{i}", "", 0, 10000) for i in range(6)]
            results = [f.result() for f in futs]
        winners = [d for rc, d in results if d.get("acquired")]
        self.assertEqual(len(winners), 1, f"Expected strictly 1 Go winner, got {results}")

    # 2. Python vs Python concurrent OS processes
    def test_02_python_vs_python_multiprocess_race(self):
        with ThreadPoolExecutor(max_workers=6) as pool:
            futs = [pool.submit(self._run_py, "acquire", "home-api", "", 0, 10.0) for _ in range(6)]
            results = [f.result() for f in futs]
        winners = [d for rc, d in results if d.get("acquired")]
        self.assertEqual(len(winners), 1, f"Expected strictly 1 Python winner, got {results}")

    # 3. Go vs Python simultaneous OS process competition
    def test_03_go_vs_python_simultaneous_competition(self):
        for round_idx in range(6):
            comp = f"home-web"
            # Wait/clear previous lease if any
            with ThreadPoolExecutor(max_workers=4) as pool:
                f_go1 = pool.submit(self._run_go, "acquire", comp, f"go-{round_idx}", "", 0, 8000)
                f_py1 = pool.submit(self._run_py, "acquire", "web", "", 0, 8.0)  # alias for home-web
                f_go2 = pool.submit(self._run_go, "acquire", "nginx", f"go2-{round_idx}", "", 0, 8000)
                f_py2 = pool.submit(self._run_py, "acquire", "/home-web", "", 0, 8.0)
                outcomes = [f.result() for f in (f_go1, f_py1, f_go2, f_py2)]
            winners = [d for _, d in outcomes if d.get("acquired")]
            self.assertEqual(len(winners), 1, f"Round {round_idx}: split-brain! {outcomes}")
            win_tok = winners[0]["token"]
            if win_tok.startswith("py-"):
                rc, rel = self._run_py("release", comp, win_tok, winners[0]["fencing_generation"])
            else:
                rc, rel = self._run_go("release", comp, "go", win_tok, 0)
            self.assertEqual(rc, 0, f"Winner cleanup release failed: {rel}")

    # 4. Same-holder re-entry strictly forbidden in BOTH Go and Python (R4-04)
    def test_04_same_holder_reentry_forbidden_cross_language(self):
        rc1, d1 = self._run_go("acquire", "frigate", "go-core", ttl_ms=10000)
        self.assertTrue(d1.get("acquired"))
        rc2, d2 = self._run_go("acquire", "home-frigate", "go-core", ttl_ms=10000)
        self.assertFalse(d2.get("acquired"), "Same Go holder re-entry MUST be rejected while active")
        self._run_go("release", "frigate", "go-core", d1["token"])

        rc3, d3 = self._run_py("acquire", "frigate", ttl_sec=10.0)
        self.assertTrue(d3.get("acquired"))
        rc4, d4 = self._run_py("acquire", "go2rtc", ttl_sec=10.0)
        self.assertFalse(d4.get("acquired"), "Same Python holder re-entry MUST be rejected while active")

    # 5. Expiration takeover & monotonic fencing_generation across Go <-> Python
    def test_05_expiration_takeover_monotonic_fencing_generation(self):
        _, g1 = self._run_go("acquire", "home-vision", "go-core", ttl_ms=120)
        self.assertTrue(g1.get("acquired"))
        gen1 = g1["fencing_generation"]
        time.sleep(0.25)  # Expire

        _, p2 = self._run_py("acquire", "home-vision", ttl_sec=0.12)
        self.assertTrue(p2.get("acquired"))
        gen2 = p2["fencing_generation"]
        self.assertGreater(gen2, gen1, f"Python fencing generation {gen2} must exceed Go generation {gen1}")
        time.sleep(0.25)

        _, g3 = self._run_go("acquire", "home-vision", "go-core", ttl_ms=5000)
        self.assertTrue(g3.get("acquired"))
        gen3 = g3["fencing_generation"]
        self.assertGreater(gen3, gen2, f"Go takeover fencing generation {gen3} must exceed Python generation {gen2}")

    # 6. Renewal concurrency & token check
    def test_06_lease_renewal_and_wrong_token_rejected(self):
        _, p1 = self._run_py("acquire", "home-api", ttl_sec=1.5)
        self.assertTrue(p1["acquired"])
        tok, gen = p1["token"], p1["fencing_generation"]

        # Valid Go-compatible renewal from Python
        rc_ok, d_ok = self._run_py("renew", "home-api", tok, gen, ttl_sec=5.0)
        self.assertTrue(d_ok.get("renewed"))

        # Wrong token renew from Go MUST fail
        rc_bad, d_bad = self._run_go("renew", "home-api", token="wrong-token", ttl_ms=5000)
        self.assertFalse(d_bad.get("renewed"))

    # 7. Release with wrong token fails; right token succeeds
    def test_07_release_token_verification(self):
        _, g1 = self._run_go("acquire", "openresty", "go-core", ttl_ms=5000)
        tok = g1["token"]
        rc_bad, d_bad = self._run_py("release", "openresty", "forged-token", g1["fencing_generation"])
        self.assertFalse(d_bad.get("released"))
        rc_ok, d_ok = self._run_go("release", "1panel-openresty-wudu", "go-core", tok)
        self.assertTrue(d_ok.get("released"))

    # 8 & 9. Stale token & stale fencing generation rejected in verify gate
    def test_08_09_stale_token_and_stale_fencing_rejected(self):
        _, g1 = self._run_go("acquire", "home-web", "go-core", ttl_ms=100)
        tok1, gen1 = g1["token"], g1["fencing_generation"]
        time.sleep(0.2)
        _, p2 = self._run_py("acquire", "home-web", ttl_sec=5.0)
        self.assertTrue(p2["acquired"])
        tok2, gen2 = p2["token"], p2["fencing_generation"]
        self.assertGreater(gen2, gen1)

        # Verify stale tok1/gen1 from BOTH Go and Python MUST reject!
        _, vg = self._run_go("verify", "home-web", token=tok1, gen=gen1)
        self.assertFalse(vg.get("valid"))
        _, vp = self._run_py("verify", "home-web", token=tok1, gen=gen1)
        self.assertFalse(vp.get("valid"))
        # Also verify current token with forged old gen1 MUST reject!
        _, vp_gen = self._run_py("verify", "home-web", token=tok2, gen=gen1)
        self.assertFalse(vp_gen.get("valid"))
        # Fresh token + fresh gen2 MUST verify in BOTH Go and Python!
        _, vp_ok = self._run_py("verify", "home-web", token=tok2, gen=gen2)
        self.assertTrue(vp_ok.get("valid"))
        _, vg_ok = self._run_go("verify", "home-web", token=tok2, gen=gen2)
        self.assertTrue(vg_ok.get("valid"))

    # 10. SQLITE_BUSY heavy contention burst across Go & Python OS processes
    def test_10_sqlite_busy_multiprocess_burst(self):
        def task(i):
            if i % 2 == 0:
                return self._run_go("acquire", "home-cloudflared", f"go-b-{i}", "", 0, 15000)
            return self._run_py("acquire", "cloudflared", "", 0, 15.0)

        with ThreadPoolExecutor(max_workers=10) as pool:
            res = list(pool.map(task, range(10)))
        winners = [d for _, d in res if d.get("acquired")]
        self.assertEqual(len(winners), 1, f"Expected strictly 1 survivor in 10-process WAL burst, got {winners}")

    # 11. Process crash / SIGKILL takeover after TTL expiry
    def test_11_process_kill_crash_takeover_after_ttl(self):
        proc = subprocess.Popen(
            [
                self.go_probe_bin,
                "-db", self.db_path,
                "-action", "hold",
                "-component", "home-api",
                "-holder", "go-crashing",
                "-ttl-ms", "900",
                "-hold-ms", "10000",
            ],
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        )
        line = proc.stdout.readline().strip()
        proc.kill()
        proc.wait(timeout=5)
        if proc.stdout:
            proc.stdout.close()
        if proc.stderr:
            proc.stderr.close()
        held = json.loads(line)
        self.assertTrue(held["acquired"])

        # Immediately while TTL (900ms) hasn't expired -> Python must still respect lease
        _, imm = self._run_py("acquire", "home-api", ttl_sec=5.0)
        self.assertFalse(imm["acquired"], "Python must NOT steal lease before killed holder TTL expires")
        time.sleep(1.0)
        _, after = self._run_py("acquire", "home-api", ttl_sec=5.0)
        self.assertTrue(after["acquired"])
        self.assertGreater(after["fencing_generation"], held["fencing_generation"])

    # 12. Malformed/corrupt lease timestamps or state -> BOTH Go & Python fail closed
    def test_12_malformed_lease_record_fails_closed(self):
        conn = sqlite3.connect(self.db_path)
        with conn:
            conn.execute(
                """
                INSERT INTO ops_recovery_leases (
                    component, holder, lease_token, fencing_generation,
                    acquired_at, expires_at, acquired_at_ms, expires_at_ms, state
                ) VALUES ('home-web', 'corrupt', 'tok', 1, 'bad-ts', 'bad-ts', 5000, 1000, 'ACTIVE')
                """
            )
        conn.close()

        _, py_res = self._run_py("acquire", "home-web", ttl_sec=5.0)
        self.assertFalse(py_res.get("acquired"), "Python must fail closed on reversed/malformed lease timestamps")
        _, go_res = self._run_go("acquire", "home-web", "go-core", ttl_ms=5000)
        self.assertFalse(go_res.get("acquired"), "Go must fail closed on reversed/malformed lease timestamps")


if __name__ == "__main__":
    unittest.main()
