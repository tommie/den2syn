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
from den2syn.steps import users

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
    *(
        Step(name, _todo(name))
        for name in (
            "account_data",
            "e2e_keys",
            "rooms",
            "receipts",
            "pushers",
            "to_device",
            "media",
            "finalize",
        )
    ),
]
