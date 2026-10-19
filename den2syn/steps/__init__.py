"""Migration steps, in the order they run.

Each MIGRATED story in stories.py names one of these steps. The order matters:
rooms need users to exist, receipts and pushers need event stream orderings,
and federation positions must be moved past everything that was persisted.

Each step verifies what it migrated before returning.
"""

from dataclasses import dataclass
from typing import Awaitable, Callable

from synapse.server import HomeServer

from den2syn.dendrite import Dendrite
from den2syn.steps import account_data, e2e_keys, pushers, receipts, rooms, users

StepFunc = Callable[[HomeServer, Dendrite], Awaitable[None]]


@dataclass(frozen=True)
class Step:
    name: str
    run: StepFunc


def _todo(name: str) -> StepFunc:
    async def run(hs: HomeServer, dendrite: Dendrite) -> None:
        # TODO: implement the step; see the MIGRATED stories naming it, and PLAN.md.
        raise NotImplementedError(f"step {name!r} is not implemented yet")

    return run


STEPS = [
    Step("users", users.run),
    Step("account_data", account_data.run),
    Step("e2e_keys", e2e_keys.run),
    Step("rooms", rooms.run),
    Step("receipts", receipts.run),
    Step("pushers", pushers.run),
    *(
        Step(name, _todo(name))
        for name in (
            "to_device",
            "media",
            "finalize",
        )
    ),
]
