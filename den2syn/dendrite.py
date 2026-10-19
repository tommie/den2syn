"""Read access to the Dendrite database."""

import pathlib
from typing import Any

import psycopg2.extensions
import psycopg2.extras


class Dendrite:
    """A read-only connection to Dendrite's database.

    Queries block the reactor thread. That's fine for a batch tool, and keeps
    Dendrite reads out of Synapse's connection pool.
    """

    def __init__(
        self, conn: psycopg2.extensions.connection, server_name: str, media_path: pathlib.Path | None
    ) -> None:
        self.conn = conn
        # Dendrite supports virtual hosts, Synapse doesn't. Only data for the
        # Synapse server name is migrated.
        self.server_name = server_name
        # Dendrite's media_api.base_path.
        self.media_path = media_path

    def query(self, sql: str, args: tuple[Any, ...] = ()) -> list[psycopg2.extras.RealDictRow]:
        with self.conn.cursor(cursor_factory=psycopg2.extras.RealDictCursor) as cur:
            cur.execute(sql, args)
            return cur.fetchall()

    def user_id(self, localpart: str) -> str:
        return f"@{localpart}:{self.server_name}"


class VerificationError(Exception):
    """Synapse doesn't contain what a step migrated."""


def verify_equal(what: str, want: Any, got: Any) -> None:
    if want != got:
        raise VerificationError(f"{what}: Dendrite has {want!r}, Synapse has {got!r}")
