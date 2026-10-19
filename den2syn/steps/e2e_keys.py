"""Migrates end-to-end encryption keys and server-side key backups.

Keys go through Synapse's E2E keys handler, i.e. the same code as the client
upload APIs. That validates them like a client upload would, and records the
device list and signature changes clients need to notice them.
"""

import collections
import copy
import json
import logging
from typing import Any

from synapse.server import HomeServer

from den2syn.dendrite import Dendrite, verify_equal

logger = logging.getLogger(__name__)

# Dendrite's userapi/types KeyTypeIntToPurpose.
_KEY_PURPOSES = {1: "master", 2: "self_signing", 3: "user_signing"}


async def run(hs: HomeServer, dendrite: Dendrite) -> None:
    xsks = _cross_signing_keys(dendrite)
    devices = _device_keys(dendrite)
    await _upload_cross_signing_keys(hs, xsks)
    await _upload_device_keys(hs, dendrite, devices)
    await _upload_signatures(hs, dendrite, xsks, devices)
    await _verify_query(hs, dendrite, xsks, devices)
    await _key_backups(hs, dendrite)


def _cross_signing_keys(dendrite: Dendrite) -> dict[str, dict[str, dict[str, Any]]]:
    """Returns cross-signing key JSON, by user ID and purpose.

    Dendrite stores only the public keys, and rebuilds the JSON on query.
    """

    out: dict[str, dict[str, dict[str, Any]]] = collections.defaultdict(dict)
    for row in dendrite.query(
        "SELECT user_id, key_type, key_data FROM keyserver_cross_signing_keys WHERE user_id LIKE %s",
        ("%:" + dendrite.server_name,),
    ):
        purpose = _KEY_PURPOSES[row["key_type"]]
        out[row["user_id"]][purpose] = {
            "user_id": row["user_id"],
            "usage": [purpose],
            "keys": {"ed25519:" + row["key_data"]: row["key_data"]},
        }
    return out


def _device_keys(dendrite: Dendrite) -> dict[tuple[str, str], dict[str, Any]]:
    """Returns device key JSON for migrated devices, by user ID and device ID.

    Synapse deletes keys along with a device, so keys of devices Dendrite no
    longer has wouldn't be served anyway.
    """

    rows = dendrite.query(
        """
        SELECT k.user_id, k.device_id, k.key_json
        FROM keyserver_device_keys k
        JOIN userapi_devices d ON k.user_id = '@' || d.localpart || ':' || d.server_name AND k.device_id = d.device_id
        WHERE d.server_name = %s AND k.key_json <> ''
        """,
        (dendrite.server_name,),
    )
    return {(r["user_id"], r["device_id"]): json.loads(r["key_json"]) for r in rows}


async def _upload_cross_signing_keys(hs: HomeServer, xsks: dict[str, dict[str, dict[str, Any]]]) -> None:
    handler = hs.get_e2e_keys_handler()
    for user_id, keys in sorted(xsks.items()):
        if "master" not in keys:
            raise ValueError(f"{user_id}: cross-signing keys without a master key")
        # The self-signing and user-signing keys must carry the master key's
        # signature, which Dendrite keeps in its signature table. They're
        # added to the JSON in _upload_signatures, so upload the master key
        # alone first.
        await handler.upload_signing_keys_for_user(user_id, {"master_key": keys["master"]})
    logger.info("Uploaded master keys for %d users.", len(xsks))


async def _upload_device_keys(hs: HomeServer, dendrite: Dendrite, devices: dict[tuple[str, str], dict[str, Any]]) -> None:
    handler = hs.get_e2e_keys_handler()

    otks: dict[tuple[str, str], dict[str, Any]] = collections.defaultdict(dict)
    for row in dendrite.query(
        "SELECT user_id, device_id, key_id, algorithm, key_json FROM keyserver_one_time_keys WHERE user_id LIKE %s",
        ("%:" + dendrite.server_name,),
    ):
        otks[(row["user_id"], row["device_id"])][f"{row['algorithm']}:{row['key_id']}"] = json.loads(row["key_json"])

    # Dendrite keeps one fallback key per algorithm, like Synapse.
    fallbacks: dict[tuple[str, str], dict[str, Any]] = collections.defaultdict(dict)
    used: list[tuple[str, str, str]] = []
    for row in dendrite.query(
        "SELECT user_id, device_id, key_id, algorithm, key_json, used FROM keyserver_fallback_keys WHERE user_id LIKE %s",
        ("%:" + dendrite.server_name,),
    ):
        fallbacks[(row["user_id"], row["device_id"])][f"{row['algorithm']}:{row['key_id']}"] = json.loads(row["key_json"])
        if row["used"]:
            used.append((row["user_id"], row["device_id"], row["algorithm"]))

    orphans = (otks.keys() | fallbacks.keys()) - devices.keys()
    if orphans:
        raise ValueError(f"one-time or fallback keys for devices without device keys: {sorted(orphans)}")

    for (user_id, device_id), device_keys in sorted(devices.items()):
        body: dict[str, Any] = {"device_keys": device_keys}
        if (user_id, device_id) in otks:
            body["one_time_keys"] = otks[(user_id, device_id)]
        if (user_id, device_id) in fallbacks:
            body["fallback_keys"] = fallbacks[(user_id, device_id)]
        await handler.upload_keys_for_user(user_id, device_id, body)

    store = hs.get_datastores().main
    # Uploading marks a fallback key unused. A used one has been handed out
    # to another device, and the client replaces it once sync stops listing
    # its algorithm as unused, so mark it used again the way claiming does.
    if used:
        await store.claim_e2e_fallback_keys([(u, d, alg, True) for u, d, alg in used])

    for (user_id, device_id), keys in sorted(otks.items()):
        counts = await store.count_e2e_one_time_keys(user_id, device_id)
        verify_equal(
            f"one-time key counts for {user_id} {device_id}",
            dict(collections.Counter(k.split(":", 1)[0] for k in keys)),
            {alg: n for alg, n in counts.items() if n},
        )
    for (user_id, device_id), keys in sorted(fallbacks.items()):
        algorithms = {k.split(":", 1)[0] for k in keys}
        verify_equal(
            f"unused fallback key algorithms of {user_id} {device_id}",
            sorted(algorithms - {alg for u, d, alg in used if (u, d) == (user_id, device_id)}),
            sorted(await store.get_e2e_unused_fallback_key_types(user_id, device_id)),
        )
        got = await store.claim_e2e_fallback_keys([(user_id, device_id, alg, False) for alg in algorithms])
        verify_equal(f"fallback keys of {user_id} {device_id}", keys, got.get(user_id, {}).get(device_id, {}))
    logger.info(
        "Uploaded keys for %d devices, %d one-time keys, %d fallback keys (%d used).",
        len(devices), sum(len(k) for k in otks.values()), sum(len(k) for k in fallbacks.values()), len(used))


def _without_signatures(obj: dict[str, Any]) -> dict[str, Any]:
    return {k: v for k, v in obj.items() if k not in ("signatures", "unsigned")}


async def _upload_signatures(
    hs: HomeServer,
    dendrite: Dendrite,
    xsks: dict[str, dict[str, dict[str, Any]]],
    devices: dict[tuple[str, str], dict[str, Any]],
) -> None:
    """Uploads Dendrite's cross-signing signatures, letting Synapse verify them.

    Dendrite's signature table has signatures by and on replaced keys, and
    stores target key IDs both with and without the "ed25519:" prefix.
    Synapse checks each signature against the current keys, and only accepts
    the kinds of signatures the spec defines, so what it rejects couldn't be
    verified by clients either. Those are logged and dropped.
    """

    handler = hs.get_e2e_keys_handler()
    sigs = dendrite.query(
        """
        SELECT origin_user_id, origin_key_id, target_user_id, target_key_id, signature
        FROM keyserver_cross_signing_sigs
        WHERE origin_user_id LIKE %s
        ORDER BY origin_user_id, target_user_id, target_key_id, origin_key_id
        """,
        ("%:" + dendrite.server_name,),
    )

    # Public key to (user, purpose), for resolving signature targets.
    xsk_by_pub = {
        (user_id, pub): purpose
        for user_id, keys in xsks.items()
        for purpose, key in keys.items()
        for pub in key["keys"].values()
    }

    # The master key's signatures on the user's other cross-signing keys are
    # part of those keys' JSON, which must be uploaded together.
    with_master_sig: dict[str, dict[str, Any]] = collections.defaultdict(dict)
    others = []
    for sig in sigs:
        target_id = sig["target_key_id"].removeprefix("ed25519:")
        target_purpose = xsk_by_pub.get((sig["target_user_id"], target_id))
        origin_purpose = xsk_by_pub.get((sig["origin_user_id"], sig["origin_key_id"].removeprefix("ed25519:")))
        if (
            sig["origin_user_id"] == sig["target_user_id"]
            and origin_purpose == "master"
            and target_purpose in ("self_signing", "user_signing")
        ):
            key = copy.deepcopy(xsks[sig["target_user_id"]][target_purpose])
            key["signatures"] = {sig["origin_user_id"]: {sig["origin_key_id"]: sig["signature"]}}
            with_master_sig[sig["target_user_id"]][target_purpose + "_key"] = key
        else:
            others.append((sig, target_id, target_purpose))

    for user_id, keys in sorted(with_master_sig.items()):
        missing = {p + "_key" for p in xsks[user_id] if p != "master"} - keys.keys()
        if missing:
            raise ValueError(f"{user_id}: no master key signature for {sorted(missing)}")
        await handler.upload_signing_keys_for_user(user_id, keys)

    accepted = rejected = 0
    for sig, target_id, target_purpose in others:
        if target_purpose is not None:
            target = xsks[sig["target_user_id"]][target_purpose]
        elif (sig["target_user_id"], target_id) in devices:
            target = devices[(sig["target_user_id"], target_id)]
        else:
            logger.warning("Dropping signature by %s %s on unknown key %s %s.",
                           sig["origin_user_id"], sig["origin_key_id"], sig["target_user_id"], sig["target_key_id"])
            rejected += 1
            continue

        signed = _without_signatures(target)
        signed["signatures"] = {sig["origin_user_id"]: {sig["origin_key_id"]: sig["signature"]}}
        result = await handler.upload_signatures_for_device_keys(
            sig["origin_user_id"], {sig["target_user_id"]: {target_id: signed}})
        if result.get("failures"):
            logger.warning("Synapse rejected signature by %s %s on %s %s: %s",
                           sig["origin_user_id"], sig["origin_key_id"], sig["target_user_id"], target_id,
                           json.dumps(result["failures"]))
            rejected += 1
        else:
            accepted += 1

    logger.info("Uploaded %d cross-signing signatures; Synapse rejected %d, and %d are part of cross-signing keys.",
                accepted, rejected, sum(len(k) for k in with_master_sig.values()))


async def _verify_query(
    hs: HomeServer,
    dendrite: Dendrite,
    xsks: dict[str, dict[str, dict[str, Any]]],
    devices: dict[tuple[str, str], dict[str, Any]],
) -> None:
    """Checks that /keys/query returns what was migrated, as each user sees it."""

    handler = hs.get_e2e_keys_handler()
    users = sorted({u for u, _ in devices} | xsks.keys())
    for user_id in users:
        res = await handler.query_devices({"device_keys": {u: [] for u in users}}, 0, user_id, None)
        verify_equal(
            f"device keys seen by {user_id}",
            sorted((u, d, json.dumps(_without_signatures(k), sort_keys=True)) for (u, d), k in devices.items()),
            sorted(
                (u, d, json.dumps(_without_signatures(k), sort_keys=True))
                for u, user_devices in res["device_keys"].items()
                for d, k in user_devices.items()
            ),
        )
        for purpose in ("master", "self_signing", "user_signing"):
            want = {
                u: keys[purpose]["keys"]
                for u, keys in xsks.items()
                # Only a user's own user-signing key is visible to them.
                if purpose in keys and (purpose != "user_signing" or u == user_id)
            }
            got = {u: k["keys"] for u, k in res.get(f"{purpose}_keys", {}).items()}
            verify_equal(f"{purpose} keys seen by {user_id}", want, got)


async def _key_backups(hs: HomeServer, dendrite: Dendrite) -> None:
    store = hs.get_datastores().main
    versions = dendrite.query(
        """
        SELECT user_id, version, algorithm, auth_data, etag, deleted
        FROM userapi_key_backup_versions
        WHERE user_id LIKE %s
        ORDER BY user_id, version
        """,
        ("%:" + dendrite.server_name,),
    )

    for v in versions:
        # create_e2e_room_keys_version numbers versions per user, while
        # Dendrite numbers them globally. Clients remember the version they
        # back up to, so keep Dendrite's. Synapse continues from the user's
        # highest version.
        await store.db_pool.simple_insert(
            "e2e_room_keys_versions",
            {
                "user_id": v["user_id"],
                "version": v["version"],
                "algorithm": v["algorithm"],
                "auth_data": v["auth_data"],
                "deleted": v["deleted"],
                "etag": int(v["etag"]),
            },
            desc="den2syn_e2e_room_keys_versions",
        )
        if v["deleted"]:
            continue

        keys = dendrite.query(
            """
            SELECT room_id, session_id, first_message_index, forwarded_count, is_verified, session_data
            FROM userapi_key_backups
            WHERE user_id = %s AND version = %s
            """,
            (v["user_id"], str(v["version"])),
        )
        await store.add_e2e_room_keys(
            v["user_id"],
            str(v["version"]),
            [
                (
                    k["room_id"],
                    k["session_id"],
                    {
                        "first_message_index": k["first_message_index"],
                        "forwarded_count": k["forwarded_count"],
                        "is_verified": k["is_verified"],
                        "session_data": json.loads(k["session_data"]),
                    },
                )
                for k in keys
            ],
        )

        got = await store.get_e2e_room_keys(v["user_id"], str(v["version"]))
        verify_equal(
            f"key backup {v['version']} of {v['user_id']}",
            sorted((k["room_id"], k["session_id"], k["first_message_index"], json.loads(k["session_data"])["ciphertext"]) for k in keys),
            sorted(
                (room_id, session_id, key["first_message_index"], key["session_data"]["ciphertext"])
                for room_id, room in got["rooms"].items()
                for session_id, key in room["sessions"].items()
            ),
        )
        logger.info("Migrated key backup version %s of %s with %d keys.", v["version"], v["user_id"], len(keys))
