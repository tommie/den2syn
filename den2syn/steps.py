"""Migration steps, in the order they run.

Each MIGRATED story in stories.py names one of these steps. The order matters:
rooms need users to exist, receipts and pushers need event stream orderings,
and federation positions must be moved past everything that was persisted.
"""

from dataclasses import dataclass
from typing import Any, Awaitable, Callable

from synapse.server import HomeServer


@dataclass(frozen=True)
class Step:
    name: str
    run: Callable[[HomeServer, Any], Awaitable[None]]


def _todo(name: str) -> Callable[[HomeServer, Any], Awaitable[None]]:
    async def run(hs: HomeServer, dendrite: Any) -> None:
        # TODO: implement the step; see the MIGRATED stories naming it.
        raise NotImplementedError(f"step {name!r} is not implemented yet")

    return run


STEPS = [
    Step(name, _todo(name))
    for name in (
        "users",
        "account_data",
        "e2e_keys",
        "rooms",
        "receipts",
        "pushers",
        "to_device",
        "media",
        "finalize",
    )
]
