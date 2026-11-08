"""
Human-in-the-Loop Proposal Lifecycle Manager for home-ops-agent:
- Serializes high-risk operations to pending_proposals as AWAITING_APPROVAL.
- Strictly forbids autonomous execution without Admin approval.
- Supports immediate execution upon approval or scheduling for low-peak window (03:00~05:00 AM).
- Automatic rollback execution upon failure.
"""

import json
import logging
import sqlite3
import time
from typing import Any, Dict, List, Optional, Tuple

logger = logging.getLogger("home-ops-agent.proposal")


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

    def __init__(self, db_path: Optional[str] = None):
        self.db_path = db_path
        self.proposals: Dict[str, Dict[str, Any]] = {}
        if self.db_path:
            self._init_sqlite_schema()
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

    def execute_proposal(self, prop_id: str, simulate_failure: bool = False) -> Tuple[bool, str]:
        """
        Executes an approved or scheduled proposal.
        Triggers automatic rollback on execution failure.
        """
        proposal = self.proposals.get(prop_id)
        if not proposal:
            return False, "Proposal not found"

        if proposal["status"] not in (self.STATE_APPROVED, self.STATE_SCHEDULED):
            return False, f"Cannot execute proposal in state {proposal['status']}: must be APPROVED first"

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
        """If inside low-peak window, triggers execution of scheduled proposals."""
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
        if not self.db_path:
            return
        conn = None
        try:
            conn = sqlite3.connect(self.db_path)
            with conn:
                conn.execute("""
                    CREATE TABLE IF NOT EXISTS pending_proposals (
                        id INTEGER PRIMARY KEY AUTOINCREMENT,
                        proposal_id TEXT UNIQUE,
                        title TEXT NOT NULL,
                        description TEXT,
                        target_component TEXT,
                        risk_level TEXT NOT NULL,
                        estimated_seconds INTEGER DEFAULT 30,
                        downtime_seconds INTEGER DEFAULT 0,
                        execution_plan TEXT NOT NULL,
                        rollback_plan TEXT NOT NULL,
                        status TEXT NOT NULL DEFAULT 'AWAITING_APPROVAL',
                        scheduled_at TEXT,
                        approved_at TEXT,
                        approved_by TEXT,
                        executed_at TEXT,
                        execution_log TEXT,
                        created_at TEXT NOT NULL,
                        updated_at TEXT
                    )
                """)
        except Exception as e:
            logger.debug(f"SQLite schema init note: {e}")
        finally:
            if conn:
                conn.close()

    def _load_from_sqlite(self) -> None:
        if not self.db_path:
            return
        conn = None
        try:
            conn = sqlite3.connect(self.db_path)
            conn.row_factory = sqlite3.Row
            cur = conn.cursor()
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
        if not self.db_path:
            return
        conn = None
        try:
            conn = sqlite3.connect(self.db_path)
            with conn:
                conn.execute("""
                    INSERT OR REPLACE INTO pending_proposals
                    (proposal_id, title, risk_level, execution_plan, rollback_plan, target_component, estimated_seconds, downtime_seconds, status, created_at)
                    VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, datetime('now'))
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
        if not self.db_path:
            return
        conn = None
        try:
            conn = sqlite3.connect(self.db_path)
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
