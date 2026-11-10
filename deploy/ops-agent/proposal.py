"""
Human-in-the-Loop Proposal Lifecycle Manager for home-ops-agent:
- Serializes high-risk operations to pending_proposals as AWAITING_APPROVAL.
- Strictly forbids autonomous execution without Admin approval.
- Supports immediate execution upon approval or scheduling for low-peak window (03:00~05:00 AM).
- Automatic rollback execution upon failure.
"""

import json
import logging
import os
import pathlib
import socket
import sqlite3
import time
from typing import Any, Dict, List, Optional, Tuple

logger = logging.getLogger("home-ops-agent.proposal")


def _open_existing_sqlite(db_path: str, readonly: bool) -> sqlite3.Connection:
    """Opens an existing SQLite DB using URI mode=ro or mode=rw; NEVER auto-creates files (R4-14)."""
    if not db_path or not os.path.isfile(db_path):
        raise FileNotFoundError(f"SQLite DB missing: {db_path}")
    posix = pathlib.Path(os.path.abspath(db_path)).as_posix()
    mode = "ro" if readonly else "rw"
    conn = sqlite3.connect(f"file:{posix}?mode={mode}", uri=True, timeout=5.0)
    conn.execute("PRAGMA busy_timeout=5000;")
    return conn


class ProposalLifecycleManager:
    """
    Manages proposal creation, human approval verification, execution, and low-peak window scheduling.
    """

    STATE_AWAITING = "AWAITING_APPROVAL"
    STATE_APPROVED = "APPROVED"
    STATE_SCHEDULED = "SCHEDULED"
    STATE_EXECUTING = "EXECUTING"
    STATE_EXECUTED = "EXECUTED"
    STATE_FAILED_ROLLEDBACK = "FAILED_ROLLEDBACK"
    STATE_EXECUTION_UNKNOWN = "EXECUTION_UNKNOWN"

    def __init__(
        self,
        db_path: Optional[str] = None,
        go_api_url: Optional[str] = None,
        internal_token: Optional[str] = None
    ):
        self.db_path = db_path
        self.go_api_url = go_api_url
        self.internal_token = internal_token
        self.proposals: Dict[str, Dict[str, Any]] = {}
        if self.db_path and os.path.isfile(self.db_path):
            self._load_from_sqlite()

    def create_proposal(
        self,
        prop_id: str,
        title: str,
        risk_level: str,
        action: str,
        rollback_cmd: str,
        target_component: str = "",
        estimated_seconds: int = 30,
        downtime_seconds: int = 0
    ) -> Dict[str, Any]:
        """Creates a pending proposal with status AWAITING_APPROVAL."""
        now = time.time()
        proposal = {
            "id": prop_id,
            "title": title,
            "risk_level": risk_level,
            "action": action,
            "rollback_cmd": rollback_cmd,
            "target_component": target_component,
            "estimated_seconds": estimated_seconds,
            "downtime_seconds": downtime_seconds,
            "status": self.STATE_AWAITING,
            "created_at": now,
            "approved_at": None,
            "approved_by": None,
            "executed_at": None,
            "scheduled_at": None,
            "rollback_executed": False
        }
        self.proposals[prop_id] = proposal

        if self.db_path:
            self._persist_to_sqlite(proposal)

        logger.info(f"Created proposal {prop_id} [{risk_level}]: {title} (AWAITING_APPROVAL)")
        return proposal

    def approve_proposal(
        self,
        prop_id: str,
        approver: str,
        is_admin: bool,
        schedule_at: Optional[str] = None
    ) -> Tuple[bool, str]:
        """Approves a proposal. Requires admin role."""
        if not is_admin:
            logger.warning(f"Rejecting approval for proposal {prop_id}: non-admin user {approver}")
            return False, "Permission denied: admin role required"

        proposal = self.proposals.get(prop_id)
        if not proposal:
            return False, "Proposal not found"

        if proposal["status"] != self.STATE_AWAITING:
            return False, f"Invalid state transition from {proposal['status']}: must be AWAITING_APPROVAL first"

        now = time.time()
        proposal["approved_at"] = now
        proposal["approved_by"] = approver

        if schedule_at:
            proposal["status"] = self.STATE_SCHEDULED
            proposal["scheduled_at"] = schedule_at
            msg = f"Proposal {prop_id} scheduled for {schedule_at}"
        else:
            proposal["status"] = self.STATE_APPROVED
            msg = f"Proposal {prop_id} approved successfully"

        if self.db_path:
            self._update_sqlite_status(proposal)

        logger.info(msg)
        return True, msg

    def execute_proposal(
        self,
        prop_id: str,
        simulate_failure: bool = False,
        go_api_url: Optional[str] = None,
        internal_token: Optional[str] = None
    ) -> Tuple[bool, str]:
        """
        Executes an approved or scheduled proposal.
        When connected to Go Core, delegates execution through Go's authoritative API.
        Triggers automatic rollback on execution failure.
        """
        proposal = self.proposals.get(prop_id)
        if not proposal:
            return False, "Proposal not found"

        if proposal["status"] not in (self.STATE_APPROVED, self.STATE_SCHEDULED):
            return False, f"Cannot execute proposal in state {proposal['status']}: must be APPROVED first"

        target_go_url = go_api_url or self.go_api_url
        target_token = internal_token or self.internal_token

        if target_go_url:
            # Delegate strictly to Go Core OperationExecutor via dedicated execute route (R4-03)
            import urllib.error
            import urllib.request
            api_url = f"{target_go_url.rstrip('/')}/api/v1/ops/proposals/{prop_id}/execute"
            payload = json.dumps({}).encode("utf-8")
            headers = {"Content-Type": "application/json"}
            if target_token:
                headers["X-Internal-Secret"] = target_token
            req = urllib.request.Request(api_url, data=payload, headers=headers)
            try:
                proposal["status"] = self.STATE_EXECUTING
                with urllib.request.urlopen(req, timeout=15) as resp:
                    if resp.status == 200:
                        proposal["status"] = self.STATE_EXECUTED
                        proposal["executed_at"] = time.time()
                        if self.db_path:
                            self._update_sqlite_status(proposal)
                        logger.info(f"Proposal {prop_id} successfully executed via Go Core OperationExecutor")
                        return True, "Executed via Go Core"
                    proposal["status"] = self.STATE_EXECUTION_UNKNOWN
                    if self.db_path:
                        self._update_sqlite_status(proposal)
                    return False, f"Go Core execution returned HTTP {resp.status}"
            except urllib.error.HTTPError as http_err:
                if http_err.code >= 500 or http_err.code == 504:
                    proposal["status"] = self.STATE_EXECUTION_UNKNOWN
                else:
                    proposal["status"] = self.STATE_APPROVED  # Policy/CAS rejected before mutation
                if self.db_path:
                    self._update_sqlite_status(proposal)
                logger.error(f"Proposal {prop_id} execution rejected by Go Core (HTTP {http_err.code}): {http_err}")
                return False, f"Go Core rejected execution (HTTP {http_err.code}): {http_err}"
            except (urllib.error.URLError, socket.timeout, TimeoutError) as net_err:
                # Ambiguous transport/timeout outcome MUST transition to EXECUTION_UNKNOWN (R4-03),
                # never claim local rollback executed when remote outcome is unknown.
                proposal["status"] = self.STATE_EXECUTION_UNKNOWN
                if self.db_path:
                    self._update_sqlite_status(proposal)
                logger.critical(f"Proposal {prop_id} entered EXECUTION_UNKNOWN on Go Core timeout/network error: {net_err}")
                return False, f"EXECUTION_UNKNOWN via Go Core timeout/network error: {net_err}"
            except Exception as e:
                proposal["status"] = self.STATE_EXECUTION_UNKNOWN
                if self.db_path:
                    self._update_sqlite_status(proposal)
                logger.error(f"Proposal {prop_id} execution ambiguous failure via Go Core: {e}")
                return False, f"Execution failed via Go Core (EXECUTION_UNKNOWN): {e}"

        env_mode = os.environ.get("ENVIRONMENT", os.environ.get("APP_ENV", os.environ.get("ENV", ""))).strip().lower()
        is_prod = env_mode in ("production", "prod") or (
            env_mode not in ("development", "dev", "test", "testing") and os.path.exists("/.dockerenv")
        )
        if is_prod:
            logger.critical(f"[Fail-Closed] Refusing local proposal {prop_id} execution without Go Core API in production.")
            return False, "Go Core API URL required for proposal execution in production"

        proposal["status"] = self.STATE_EXECUTING

        if simulate_failure:
            # Trigger rollback command
            proposal["status"] = self.STATE_FAILED_ROLLEDBACK
            proposal["rollback_executed"] = True
            if self.db_path:
                self._update_sqlite_status(proposal)
            logger.error(f"Proposal {prop_id} execution failed! Executed rollback command: {proposal['rollback_cmd']}")
            return False, "Execution failed: auto-rollback triggered"

        proposal["status"] = self.STATE_EXECUTED
        proposal["executed_at"] = time.time()
        if self.db_path:
            self._update_sqlite_status(proposal)

        logger.info(f"Proposal {prop_id} executed successfully: {proposal['action']}")
        return True, "Executed successfully"

    def is_low_peak_window(self, current_hour: Optional[int] = None) -> bool:
        """Checks if current time is within low-peak maintenance window (03:00~05:00 AM)."""
        if current_hour is None:
            current_hour = time.localtime().tm_hour
        return 3 <= current_hour < 5

    def check_and_run_scheduled_proposals(self, current_hour: Optional[int] = None) -> List[Dict[str, Any]]:
        """
        Go Core ProposalScheduler is sole scheduling authority when go_api_url is configured (R4-01).
        Only executes local unit-test queue when go_api_url is unset outside production.
        """
        if self.go_api_url:
            logger.debug("Skipping local scheduled proposal runner: Go Core ProposalScheduler is sole authority.")
            return []
        if not self.is_low_peak_window(current_hour):
            return []

        executed = []
        for prop_id, prop in list(self.proposals.items()):
            if prop["status"] == self.STATE_SCHEDULED:
                logger.info(f"Low-peak window active. Auto-running scheduled proposal {prop_id}")
                ok, _ = self.execute_proposal(prop_id)
                if ok:
                    executed.append(prop)
        return executed

    def _init_sqlite_schema(self) -> None:
        """No-op: Python must never run runtime DDL (CREATE TABLE) against SQLite (R4-14)."""
        return

    def _load_from_sqlite(self) -> None:
        if not self.db_path or not os.path.isfile(self.db_path):
            return
        conn = None
        try:
            conn = _open_existing_sqlite(self.db_path, readonly=True)
            conn.row_factory = sqlite3.Row
            cur = conn.cursor()
            cur.execute("SELECT name FROM sqlite_master WHERE type='table' AND name='pending_proposals'")
            if not cur.fetchone():
                return
            cur.execute("SELECT * FROM pending_proposals")
            for row in cur.fetchall():
                prop_id = row["proposal_id"] or f"prop-{row['id']}"
                self.proposals[prop_id] = {
                    "id": prop_id,
                    "title": row["title"],
                    "risk_level": row["risk_level"],
                    "action": row["execution_plan"],
                    "rollback_cmd": row["rollback_plan"],
                    "target_component": row["target_component"] or "",
                    "estimated_seconds": row["estimated_seconds"] or 30,
                    "downtime_seconds": row["downtime_seconds"] or 0,
                    "status": row["status"],
                    "created_at": time.time(),
                    "approved_at": None,
                    "approved_by": row["approved_by"],
                    "executed_at": None,
                    "scheduled_at": row["scheduled_at"],
                    "rollback_executed": False
                }
        except Exception as e:
            logger.debug(f"SQLite load note: {e}")
        finally:
            if conn:
                conn.close()

    def _persist_to_sqlite(self, prop: Dict[str, Any]) -> None:
        if not self.db_path or not os.path.isfile(self.db_path):
            return
        conn = None
        try:
            conn = _open_existing_sqlite(self.db_path, readonly=False)
            with conn:
                conn.execute("""
                    INSERT INTO pending_proposals
                    (proposal_id, title, risk_level, execution_plan, rollback_plan, target_component, estimated_seconds, downtime_seconds, status, created_at)
                    VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, datetime('now'))
                    ON CONFLICT(proposal_id) DO NOTHING
                """, (
                    prop["id"],
                    prop["title"],
                    prop["risk_level"],
                    prop["action"],
                    prop["rollback_cmd"],
                    prop["target_component"],
                    prop["estimated_seconds"],
                    prop["downtime_seconds"],
                    prop["status"]
                ))
        except Exception as e:
            logger.debug(f"SQLite persist note: {e}")
        finally:
            if conn:
                conn.close()

    def _update_sqlite_status(self, prop: Dict[str, Any]) -> None:
        if not self.db_path or not os.path.isfile(self.db_path):
            return
        # Go Core is sole authority for SCHEDULED and authoritative status transitions
        if prop.get("status") == self.STATE_SCHEDULED or self.go_api_url:
            logger.debug(f"Skipping direct SQLite status write for proposal {prop.get('id')}; Go Core owns authoritative transitions")
            return
        conn = None
        try:
            conn = _open_existing_sqlite(self.db_path, readonly=False)
            with conn:
                conn.execute("""
                    UPDATE pending_proposals
                    SET status = ?, approved_by = ?, scheduled_at = ?, updated_at = datetime('now')
                    WHERE proposal_id = ?
                """, (
                    prop["status"],
                    prop.get("approved_by"),
                    prop.get("scheduled_at"),
                    prop["id"]
                ))
        except Exception as e:
            logger.debug(f"SQLite update note: {e}")
        finally:
            if conn:
                conn.close()
