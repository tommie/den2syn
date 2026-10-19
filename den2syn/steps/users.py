"""Migrates accounts, profiles, devices and access tokens."""

import logging

from synapse.server import HomeServer
from synapse.types import UserID

from den2syn.dendrite import Dendrite, verify_equal

logger = logging.getLogger(__name__)

# Dendrite's userapi/api AccountType values.
_ACCOUNT_TYPE_USER = 1
_ACCOUNT_TYPE_GUEST = 2
_ACCOUNT_TYPE_ADMIN = 3
_ACCOUNT_TYPE_APPSERVICE = 4


async def run(hs: HomeServer, dendrite: Dendrite) -> None:
    await _accounts(hs, dendrite)
    await _devices(hs, dendrite)
    await _threepids(hs, dendrite)
    await _registration_tokens(hs, dendrite)


async def _accounts(hs: HomeServer, dendrite: Dendrite) -> None:
    store = hs.get_datastores().main
    accounts = dendrite.query(
        """
        SELECT a.localpart, a.created_ts, a.password_hash, a.appservice_id,
            a.is_deactivated, a.account_type, p.display_name, p.avatar_url
        FROM userapi_accounts a
        LEFT JOIN userapi_profiles p USING (localpart, server_name)
        WHERE a.server_name = %s
        ORDER BY a.created_ts
        """,
        (dendrite.server_name,),
    )

    for acc in accounts:
        user_id = dendrite.user_id(acc["localpart"])
        account_type = acc["account_type"]
        if account_type not in (_ACCOUNT_TYPE_USER, _ACCOUNT_TYPE_GUEST, _ACCOUNT_TYPE_ADMIN, _ACCOUNT_TYPE_APPSERVICE):
            raise ValueError(f"{user_id}: unknown Dendrite account type {account_type}")

        # Both use bcrypt, so password hashes carry over as long as Synapse
        # has no password pepper configured.
        await store.register_user(
            user_id,
            password_hash=acc["password_hash"],
            make_guest=account_type == _ACCOUNT_TYPE_GUEST,
            appservice_id=acc["appservice_id"],
            admin=account_type == _ACCOUNT_TYPE_ADMIN,
            # Dendrite has no registration approval, so every account was usable.
            approved=True,
        )
        # Synapse's registration always creates a profile row, but
        # register_user only does so given a display name. Dendrite uses ""
        # for no display name, and Synapse NULL.
        user = UserID.from_string(user_id)
        await store.create_profile(user)
        if acc["display_name"]:
            await store.set_profile_field(user, "displayname", acc["display_name"])
        # register_user stamps the current time.
        await store.db_pool.simple_update_one(
            "users",
            keyvalues={"name": user_id},
            updatevalues={"creation_ts": acc["created_ts"] // 1000},
            desc="den2syn_users_creation_ts",
        )
        if acc["avatar_url"]:
            await store.set_profile_field(user, "avatar_url", acc["avatar_url"])
        if acc["is_deactivated"]:
            await store.set_user_deactivated_status(user_id, True)

    got = await store.db_pool.simple_select_onecol("users", None, "name", desc="den2syn_verify_users")
    verify_equal("users", sorted(dendrite.user_id(acc["localpart"]) for acc in accounts), sorted(got))
    got = await store.db_pool.simple_select_list(
        "profiles", None, ("full_user_id", "displayname", "avatar_url"), desc="den2syn_verify_profiles")
    verify_equal(
        "profiles",
        sorted(
            (dendrite.user_id(acc["localpart"]), acc["display_name"] or None, acc["avatar_url"] or None)
            for acc in accounts
        ),
        sorted(got),
    )
    logger.info("Migrated %d accounts.", len(accounts))


async def _devices(hs: HomeServer, dendrite: Dendrite) -> None:
    store = hs.get_datastores().main
    devices = dendrite.query(
        """
        SELECT access_token, device_id, localpart, display_name, last_seen_ts, ip, user_agent
        FROM userapi_devices
        WHERE server_name = %s
        ORDER BY created_ts
        """,
        (dendrite.server_name,),
    )

    for dev in devices:
        user_id = dendrite.user_id(dev["localpart"])
        # Returns False if the device already exists, which would mean two
        # Dendrite access tokens share a device. Synapse allows that too.
        await store.store_device(user_id, dev["device_id"], dev["display_name"])
        await store.add_access_token_to_user(user_id, dev["access_token"], dev["device_id"], valid_until_ms=None)

        # Synapse's own writer (insert_client_ip) batches live requests in
        # memory, so write the last-seen data directly.
        await store.db_pool.simple_update_one(
            "devices",
            keyvalues={"user_id": user_id, "device_id": dev["device_id"]},
            updatevalues={"last_seen": dev["last_seen_ts"], "ip": dev["ip"], "user_agent": dev["user_agent"]},
            desc="den2syn_devices_last_seen",
        )
        if dev["ip"] is not None:
            await store.db_pool.simple_upsert(
                "user_ips",
                keyvalues={"user_id": user_id, "access_token": dev["access_token"], "ip": dev["ip"]},
                values={
                    "device_id": dev["device_id"],
                    # Synapse records a missing User-Agent header as "".
                    "user_agent": dev["user_agent"] or "",
                    "last_seen": dev["last_seen_ts"],
                },
                desc="den2syn_user_ips",
            )

    got = await store.db_pool.simple_select_list("access_tokens", None, ("token", "user_id", "device_id"), desc="den2syn_verify_tokens")
    verify_equal(
        "access tokens",
        sorted((d["access_token"], dendrite.user_id(d["localpart"]), d["device_id"]) for d in devices),
        sorted(got),
    )
    logger.info("Migrated %d devices.", len(devices))


async def _threepids(hs: HomeServer, dendrite: Dendrite) -> None:
    store = hs.get_datastores().main
    threepids = dendrite.query(
        """
        SELECT t.threepid, t.medium, t.localpart, a.created_ts
        FROM userapi_threepids t
        JOIN userapi_accounts a USING (localpart, server_name)
        WHERE t.server_name = %s
        """,
        (dendrite.server_name,),
    )

    for tp in threepids:
        # Dendrite doesn't record when a threepid was validated or added. The
        # account creation time is the best lower bound we have.
        await store.user_add_threepid(
            dendrite.user_id(tp["localpart"]), tp["medium"], tp["threepid"],
            validated_at=tp["created_ts"], added_at=tp["created_ts"],
        )

    got = await store.db_pool.simple_select_list("user_threepids", None, ("user_id", "medium", "address"), desc="den2syn_verify_threepids")
    verify_equal(
        "threepids",
        sorted((dendrite.user_id(tp["localpart"]), tp["medium"], tp["threepid"]) for tp in threepids),
        sorted(got),
    )
    logger.info("Migrated %d threepids.", len(threepids))


async def _registration_tokens(hs: HomeServer, dendrite: Dendrite) -> None:
    store = hs.get_datastores().main
    tokens = dendrite.query("SELECT token, pending, completed, uses_allowed, expiry_time FROM userapi_registration_tokens")

    for tok in tokens:
        # Dendrite creates tokens with zero counts, so NULL means something
        # we don't understand.
        if tok["pending"] is None or tok["completed"] is None:
            raise ValueError(f"registration token {tok['token']!r} has NULL use counts")
        await store.create_registration_token(tok["token"], tok["uses_allowed"], tok["expiry_time"])
        # create_registration_token always starts the counts at zero.
        await store.db_pool.simple_update_one(
            "registration_tokens",
            keyvalues={"token": tok["token"]},
            updatevalues={"pending": tok["pending"], "completed": tok["completed"]},
            desc="den2syn_registration_token_counts",
        )

    logger.info("Migrated %d registration tokens.", len(tokens))
