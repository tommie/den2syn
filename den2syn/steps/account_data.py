"""Migrates account data, room tags, push rules and sync filters.

Dendrite keeps push rules (m.push_rules) and room tags (m.tag) as account
data, while Synapse has dedicated tables and synthesizes the account data
from them.
"""

import json
import logging
import pathlib
from typing import Any

from canonicaljson import encode_canonical_json

from synapse.push.rulekinds import PRIORITY_CLASS_MAP
from synapse.rest.client.push_rule import RuleSpec, _rule_tuple_from_request_object
from synapse.server import HomeServer
from synapse.synapse_rust.push import get_base_rule_ids
from synapse.types import UserID

from den2syn.dendrite import Dendrite, verify_equal

logger = logging.getLogger(__name__)

_PUSH_RULES = "m.push_rules"
_TAG = "m.tag"


async def run(hs: HomeServer, dendrite: Dendrite) -> None:
    rows = dendrite.query(
        """
        SELECT localpart, room_id, type, content
        FROM userapi_account_datas
        WHERE server_name = %s
        ORDER BY localpart, room_id, type
        """,
        (dendrite.server_name,),
    )

    await _account_data(hs, dendrite, [r for r in rows if r["type"] not in (_PUSH_RULES, _TAG)])
    await _tags(hs, dendrite, [r for r in rows if r["type"] == _TAG])
    for row in rows:
        if row["type"] == _PUSH_RULES:
            await _push_rules(hs, dendrite.user_id(row["localpart"]), json.loads(row["content"]))
    await _filters(hs, dendrite)


async def _account_data(hs: HomeServer, dendrite: Dendrite, rows: list[dict[str, Any]]) -> None:
    store = hs.get_datastores().main
    for row in rows:
        user_id = dendrite.user_id(row["localpart"])
        content = json.loads(row["content"])
        # Dendrite uses "" for global account data.
        if row["room_id"]:
            await store.add_account_data_to_room(user_id, row["room_id"], row["type"], content)
        else:
            # Also updates ignored_users from m.ignored_user_list.
            await store.add_account_data_for_user(user_id, row["type"], content)

    got = await store.db_pool.simple_select_list(
        "account_data", None, ("user_id", "account_data_type", "content"), desc="den2syn_verify_account_data")
    got += [
        (r[0], r[1], r[2], r[3])
        for r in await store.db_pool.simple_select_list(
            "room_account_data", None, ("user_id", "room_id", "account_data_type", "content"),
            desc="den2syn_verify_room_account_data")
    ]
    verify_equal(
        "account data",
        sorted(
            (dendrite.user_id(r["localpart"]), *((r["room_id"],) if r["room_id"] else ()), r["type"], json.loads(r["content"]))
            for r in rows
        ),
        sorted(tuple(json.loads(v) if i == len(g) - 1 else v for i, v in enumerate(g)) for g in got),
    )
    logger.info("Migrated %d account data entries.", len(rows))


async def _tags(hs: HomeServer, dendrite: Dendrite, rows: list[dict[str, Any]]) -> None:
    store = hs.get_datastores().main
    want = []
    for row in rows:
        if not row["room_id"]:
            raise ValueError(f"global {_TAG} account data for {row['localpart']}")
        user_id = dendrite.user_id(row["localpart"])
        for tag, content in json.loads(row["content"]).get("tags", {}).items():
            await store.add_tag_to_room(user_id, row["room_id"], tag, content)
            want.append((user_id, row["room_id"], tag, content))

    got = await store.db_pool.simple_select_list(
        "room_tags", None, ("user_id", "room_id", "tag", "content"), desc="den2syn_verify_room_tags")
    verify_equal("room tags", sorted(want), sorted((u, r, t, json.loads(c)) for u, r, t, c in got))
    logger.info("Migrated %d room tags.", len(want))


def _normalized_actions(actions: list[Any]) -> list[str]:
    """Returns push rule actions in a form where equal meaning compares equal.

    Dendrite's stored rules mix user changes with whatever Dendrite's defaults
    were when the account was created, so we only want to carry over actions
    that actually behave differently from Synapse's defaults.
    """

    out = set()
    for a in actions:
        # Legacy no-ops, per the spec.
        if a == "dont_notify":
            continue
        if a == "coalesce":
            a = "notify"
        if isinstance(a, dict) and a.get("set_tweak") == "highlight":
            # A missing value means true. Dendrite writes a missing value as
            # null.
            if a.get("value") is False:
                continue
            a = {"set_tweak": "highlight"}
        out.add(json.dumps(a, sort_keys=True))
    return sorted(out)


def _cleaned_actions(actions: list[Any]) -> list[Any]:
    """Drops Dendrite's null tweak values, which mean "no value" there."""

    return [
        {k: v for k, v in a.items() if not (k == "value" and v is None)} if isinstance(a, dict) else a
        for a in actions
    ]


def _has_duplicate_tweaks(actions: list[Any]) -> bool:
    tweaks = [a["set_tweak"] for a in actions if isinstance(a, dict) and "set_tweak" in a]
    return len(tweaks) != len(set(tweaks))


def _rules_by_id(ruleset: dict[str, list[dict[str, Any]]]) -> dict[tuple[str, str], dict[str, Any]]:
    return {(kind, rule["rule_id"]): rule for kind, rules in ruleset.items() for rule in rules}


def _load_dendrite_defaults() -> dict[tuple[str, str], list[dict[str, Any]]]:
    """Returns every default rule Dendrite has shipped, by (kind, rule ID).

    Generated by scripts/dump-dendrite-push-rules.sh. Only actions and enabled
    flags are compared, so the placeholder user in conditions doesn't matter.
    """

    path = pathlib.Path(__file__).parent.parent / "dendrite_default_push_rules.json"
    out: dict[tuple[str, str], list[dict[str, Any]]] = {}
    for ruleset in json.loads(path.read_text()).values():
        for key, rule in _rules_by_id({k: v or [] for k, v in ruleset.items()}).items():
            out.setdefault(key, []).append(rule)
    return out


_DENDRITE_DEFAULTS = _load_dendrite_defaults()


async def _push_rules(hs: HomeServer, user_id: str, content: dict[str, Any]) -> None:
    store = hs.get_datastores().main
    handler = hs.get_push_rules_handler()
    user = UserID.from_string(user_id)
    base_rule_ids = set(get_base_rule_ids())

    # Synapse's view before any changes: its defaults for this user.
    defaults = _rules_by_id((await handler.push_rules_for_user(user))["global"])
    want: dict[tuple[str, str], dict[str, Any]] = {}

    for kind, rules in content.get("global", {}).items():
        if kind not in PRIORITY_CLASS_MAP:
            continue
        previous = None
        for rule in rules:
            rule_id = rule["rule_id"]
            namespaced_rule_id = f"global/{kind}/{rule_id}"
            enabled = rule.get("enabled", True)
            actions = _cleaned_actions(rule["actions"])

            if rule_id.startswith("."):
                # A default rule Synapse doesn't have.
                if namespaced_rule_id not in base_rule_ids:
                    continue
                default = defaults[(kind, rule_id)]
                dendrite_defaults = _DENDRITE_DEFAULTS.get((kind, rule_id), [])

                # Values matching any default Dendrite has shipped are what the
                # account was created with, not a user's choice, so Synapse's
                # default applies.
                if enabled in (d["enabled"] for d in dendrite_defaults):
                    enabled = default["enabled"]
                if _normalized_actions(actions) in (_normalized_actions(d["actions"]) for d in dendrite_defaults):
                    actions = default["actions"]
                elif _has_duplicate_tweaks(actions):
                    logger.warning(
                        "%s: ignoring %s actions with duplicate tweaks, written by a buggy client "
                        "or server, in favor of Synapse's default: %s",
                        user_id, namespaced_rule_id, json.dumps(actions))
                    actions = default["actions"]

                if enabled != default["enabled"]:
                    await handler.set_rule_attr(user_id, RuleSpec("global", kind, rule_id, "enabled"), enabled)
                if _normalized_actions(actions) != _normalized_actions(default["actions"]):
                    await handler.set_rule_attr(
                        user_id, RuleSpec("global", kind, rule_id, "actions"), {"actions": actions})
                want[(kind, rule_id)] = {"enabled": enabled, "actions": _normalized_actions(actions)}
                continue

            conditions, actions = _rule_tuple_from_request_object(kind, rule_id, {**rule, "actions": actions})
            # Without a position, add_push_rule makes the rule the highest
            # priority in its class. Placing each after the previous keeps
            # Dendrite's order.
            await store.add_push_rule(
                user_id, namespaced_rule_id, PRIORITY_CLASS_MAP[kind], conditions, actions,
                after=previous,
            )
            if not enabled:
                await handler.set_rule_attr(user_id, RuleSpec("global", kind, rule_id, "enabled"), False)
            previous = namespaced_rule_id
            want[(kind, rule_id)] = {
                "enabled": enabled,
                "actions": _normalized_actions(actions),
                # Only override/underride rules have conditions, and only
                # content rules have a pattern.
                **{k: rule[k] for k in ("conditions", "pattern") if k in rule},
            }

    got_ruleset = (await handler.push_rules_for_user(user))["global"]
    got = _rules_by_id(got_ruleset)
    verify_equal(
        f"push rules for {user_id}",
        want,
        {
            key: {
                k: _normalized_actions(v) if k == "actions" else v
                for k, v in got[key].items()
                if k in want[key]
            } if key in got else None
            for key in want
        },
    )
    # Order of user-defined rules within each kind.
    for kind, rules in content.get("global", {}).items():
        if kind not in PRIORITY_CLASS_MAP:
            continue
        verify_equal(
            f"{kind} push rule order for {user_id}",
            [r["rule_id"] for r in rules if not r["rule_id"].startswith(".")],
            [r["rule_id"] for r in got_ruleset.get(kind, []) if not r["rule_id"].startswith(".")],
        )
    logger.info("Migrated push rules for %s.", user_id)


async def _filters(hs: HomeServer, dendrite: Dendrite) -> None:
    store = hs.get_datastores().main
    # syncapi_filter has no server_name column; Dendrite's sync API only
    # serves its own users.
    filters = dendrite.query("SELECT id, localpart, filter FROM syncapi_filter ORDER BY id")

    for f in filters:
        user = UserID(f["localpart"], dendrite.server_name)
        # add_user_filter assigns new IDs, but clients cache the filter IDs
        # they got, so keep Dendrite's.
        await store.db_pool.simple_insert(
            "user_filters",
            {
                "full_user_id": user.to_string(),
                "user_id": user.localpart,
                "filter_id": f["id"],
                # Stored like add_user_filter does.
                "filter_json": bytearray(encode_canonical_json(json.loads(f["filter"]))),
            },
            desc="den2syn_user_filters",
        )

    for f in filters:
        got = await store.get_user_filter(UserID(f["localpart"], dendrite.server_name), f["id"])
        verify_equal(f"filter {f['id']} for {f['localpart']}", json.loads(f["filter"]), got)
    logger.info("Migrated %d sync filters.", len(filters))
