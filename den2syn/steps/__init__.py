"""Migration steps, in the order they run.

Each MIGRATED story in stories.py names one of these steps. The order matters:
rooms need users to exist, receipts and reports need the events they refer to,
pushers need event stream orderings, and federation positions must be moved
past everything that was persisted.

Each step verifies what it migrated before returning.
"""

from dataclasses import dataclass
from typing import Awaitable, Callable

from synapse.server import HomeServer

from den2syn.dendrite import Dendrite
from den2syn.steps import account_data, e2e_keys, finalize, media, pushers, receipts, reports, rooms, to_device, users

StepFunc = Callable[[HomeServer, Dendrite], Awaitable[None]]


@dataclass(frozen=True)
class Step:
    name: str
    run: StepFunc


STEPS = [
    Step("users", users.run),
    Step("account_data", account_data.run),
    Step("e2e_keys", e2e_keys.run),
    Step("rooms", rooms.run),
    Step("receipts", receipts.run),
    Step("reports", reports.run),
    Step("pushers", pushers.run),
    Step("to_device", to_device.run),
    Step("media", media.run),
    Step("finalize", finalize.run),
]
