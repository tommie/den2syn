"""Checks a migrated Synapse through the client-server API.

Logs in as each local user with their most recently used Dendrite access
token, and compares what a client would see with Dendrite's database.

Run inside the den2syn image, against a running Synapse:

    python -m tests.client_test --synapse http://synapse:8008 \\
        --dendrite-db "host=... dbname=dendrite" --dendrite-media /dendrite-media

If PASSWORD_<localpart> is set (e.g. PASSWORD_tommie), that user also logs in
with the password, which checks that Dendrite's password hash works in
Synapse. The new session is logged out again.

If RECOVERY_KEY_<localpart> is set (e.g. RECOVERY_KEY_tommie), that user's key
backup is also decrypted: one room key is decrypted with the recovery key,
and its session ID checked against the encrypted messages in its room.

Passwords and recovery keys are only read from the environment, never stored
or logged.
"""

import argparse
import base64
import hashlib
import hmac
import json
import os
import pathlib
import sys
import urllib.error
import urllib.parse
import urllib.request
from typing import Any

import psycopg2
import psycopg2.extras
from cryptography.hazmat.primitives import hashes, padding
from cryptography.hazmat.primitives.asymmetric.x25519 import X25519PrivateKey, X25519PublicKey
from cryptography.hazmat.primitives.ciphers import Cipher, algorithms, modes
from cryptography.hazmat.primitives.kdf.hkdf import HKDF


class Client:
    def __init__(self, base_url: str, token: str | None) -> None:
        self.base_url = base_url
        self.token = token

    def request(self, method: str, path: str, body: Any = None, raw: bool = False) -> Any:
        headers = {"Content-Type": "application/json"}
        if self.token is not None:
            headers["Authorization"] = f"Bearer {self.token}"
        req = urllib.request.Request(
            self.base_url + path,
            method=method,
            data=None if body is None else json.dumps(body).encode(),
            headers=headers,
        )
        with urllib.request.urlopen(req, timeout=60) as resp:
            data = resp.read()
        return data if raw else json.loads(data)

    def get(self, path: str, **params: Any) -> Any:
        query = "?" + urllib.parse.urlencode(params) if params else ""
        return self.request("GET", path + query)


class Checks:
    def __init__(self) -> None:
        self.failures: list[str] = []

    def equal(self, what: str, want: Any, got: Any) -> None:
        if want == got:
            print(f"ok    {what}")
        else:
            print(f"FAIL  {what}: want {want!r}, got {got!r}")
            self.failures.append(what)

    def true(self, what: str, cond: bool, detail: str = "") -> None:
        if cond:
            print(f"ok    {what}")
        else:
            print(f"FAIL  {what}{': ' + detail if detail else ''}")
            self.failures.append(what)


def quote(s: str) -> str:
    return urllib.parse.quote(s, safe="")


def query(conn: Any, sql: str, args: tuple[Any, ...] = ()) -> list[dict[str, Any]]:
    with conn.cursor(cursor_factory=psycopg2.extras.RealDictCursor) as cur:
        cur.execute(sql, args)
        return cur.fetchall()


def check_user(checks: Checks, conn: Any, synapse: str, media: pathlib.Path, user: dict[str, Any]) -> None:
    user_id, device_id = user["user_id"], user["device_id"]
    client = Client(synapse, user["access_token"])
    print(f"--- {user_id} (device {device_id})")

    # 1. The migrated access token logs in as the right user and device.
    whoami = client.get("/_matrix/client/v3/account/whoami")
    checks.equal(f"{user_id}: whoami", (user_id, device_id), (whoami["user_id"], whoami.get("device_id")))

    password = os.environ.get("PASSWORD_" + user["localpart"])
    if password:
        check_password_login(checks, synapse, user, password)
    else:
        print(f"skip  {user_id}: password login (no PASSWORD_{user['localpart']})")

    # 2. Joined rooms match Dendrite's current membership.
    want_rooms = {
        r["room_id"]
        for r in query(conn, """
            SELECT room_id FROM syncapi_current_room_state
            WHERE type = 'm.room.member' AND state_key = %s AND membership = 'join'
            """, (user_id,))
    }
    sync = client.get("/_matrix/client/v3/sync", filter=json.dumps({"room": {"timeline": {"limit": 1}}}))
    checks.equal(f"{user_id}: joined rooms in /sync", sorted(want_rooms), sorted(sync.get("rooms", {}).get("join", {})))

    # 3. History can be paged back, every event is one Dendrite had, and the
    # newest is Dendrite's latest event.
    for room_id in sorted(want_rooms):
        dendrite_events = {
            r["event_id"]
            for r in query(conn, """
                SELECT e.event_id FROM roomserver_events e JOIN roomserver_rooms r USING (room_nid)
                WHERE r.room_id = %s
                """, (room_id,))
        }
        latest = query(conn, """
            SELECT e.event_id FROM roomserver_events e JOIN roomserver_rooms r ON e.event_nid = ANY(r.latest_event_nids)
            WHERE r.room_id = %s
            """, (room_id,))
        seen: list[str] = []
        token = None
        while True:
            params = {"dir": "b", "limit": 1000, **({"from": token} if token else {})}
            page = client.get(f"/_matrix/client/v3/rooms/{quote(room_id)}/messages", **params)
            seen += [e["event_id"] for e in page["chunk"]]
            token = page.get("end")
            if not token or not page["chunk"]:
                break
        checks.true(f"{user_id}: {room_id} history has {len(seen)} events, all from Dendrite",
                    bool(seen) and set(seen) <= dendrite_events,
                    f"{len(set(seen) - dendrite_events)} unknown events")
        checks.equal(f"{user_id}: {room_id} newest event", [r["event_id"] for r in latest], seen[:1])

    # 4. Own devices have keys signed by the self-signing key, and the
    # cross-signing keys are there.
    keys = client.request("POST", "/_matrix/client/v3/keys/query", {"device_keys": {user_id: []}})
    devices = keys.get("device_keys", {}).get(user_id, {})
    want_devices = {
        r["device_id"]
        for r in query(conn, """
            SELECT k.device_id FROM keyserver_device_keys k
            JOIN userapi_devices d ON k.user_id = '@' || d.localpart || ':' || d.server_name AND k.device_id = d.device_id
            WHERE k.user_id = %s AND k.key_json <> ''
            """, (user_id,))
    }
    checks.equal(f"{user_id}: devices with keys", sorted(want_devices), sorted(devices))
    ssk = keys.get("self_signing_keys", {}).get(user_id)
    if ssk is not None:
        (ssk_id,) = ssk["keys"].keys()
        unsigned = sorted(d for d, k in devices.items() if ssk_id not in k.get("signatures", {}).get(user_id, {}))
        checks.equal(f"{user_id}: devices not signed by the self-signing key", [], unsigned)
    has_xsk = bool(query(conn, "SELECT 1 FROM keyserver_cross_signing_keys WHERE user_id = %s", (user_id,)))
    checks.equal(f"{user_id}: has master key", has_xsk, user_id in keys.get("master_keys", {}))

    # 5. The current key backup matches Dendrite's.
    want_backup = query(conn, """
        SELECT v.version, v.etag, v.auth_data, (SELECT COUNT(*) FROM userapi_key_backups k WHERE k.user_id = v.user_id AND k.version = v.version::text) AS count
        FROM userapi_key_backup_versions v WHERE v.user_id = %s AND v.deleted = 0 ORDER BY v.version DESC LIMIT 1
        """, (user_id,))
    if want_backup:
        b = want_backup[0]
        got = client.get("/_matrix/client/v3/room_keys/version")
        checks.equal(f"{user_id}: key backup version, etag and count",
                     (str(b["version"]), b["etag"], b["count"]), (got["version"], got["etag"], got["count"]))
        recovery_key = os.environ.get("RECOVERY_KEY_" + user["localpart"])
        if recovery_key:
            check_backup_decryption(checks, client, conn, user_id, got, recovery_key)
        else:
            print(f"skip  {user_id}: key backup decryption (no RECOVERY_KEY_{user['localpart']})")

    # 6. The avatar downloads as the file Dendrite has.
    avatar = query(conn, """
        SELECT p.avatar_url, m.base64hash FROM userapi_profiles p
        JOIN mediaapi_media_repository m ON p.avatar_url = 'mxc://' || m.media_origin || '/' || m.media_id
        WHERE '@' || p.localpart || ':' || p.server_name = %s
        """, (user_id,))
    if avatar:
        server_name, media_id = avatar[0]["avatar_url"].removeprefix("mxc://").split("/")
        h = avatar[0]["base64hash"]
        want = hashlib.sha256((media / h[0] / h[1] / h[2:] / "file").read_bytes()).hexdigest()
        data = client.request("GET", f"/_matrix/client/v1/media/download/{server_name}/{media_id}", raw=True)
        checks.equal(f"{user_id}: avatar download", want, hashlib.sha256(data).hexdigest())
        thumb = client.request(
            "GET", f"/_matrix/client/v1/media/thumbnail/{server_name}/{media_id}?width=96&height=96&method=crop", raw=True)
        checks.true(f"{user_id}: avatar thumbnail", len(thumb) > 0)


def check_password_login(checks: Checks, synapse: str, user: dict[str, Any], password: str) -> None:
    """Logs in with the password from Dendrite's hash, then logs that session out."""

    user_id = user["user_id"]
    try:
        login = Client(synapse, None).request("POST", "/_matrix/client/v3/login", {
            "type": "m.login.password",
            "identifier": {"type": "m.id.user", "user": user["localpart"]},
            "password": password,
            "initial_device_display_name": "den2syn client test",
        })
    except urllib.error.HTTPError as e:
        checks.true(f"{user_id}: password login", False, f"HTTP {e.code}: {e.read()[:200]!r}")
        return
    checks.equal(f"{user_id}: password login", user_id, login["user_id"])
    # Don't leave a device behind.
    Client(synapse, login["access_token"]).request("POST", "/_matrix/client/v3/logout", {})


_BASE58 = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"


def decode_recovery_key(recovery_key: str) -> bytes:
    """Returns the backup private key from a Matrix recovery key (spec: Key representation)."""

    n = 0
    for c in recovery_key.replace(" ", ""):
        n = n * 58 + _BASE58.index(c)
    raw = n.to_bytes(35, "big")
    if raw[:2] != b"\x8b\x01":
        raise ValueError("not a recovery key (bad prefix)")
    parity = 0
    for b in raw:
        parity ^= b
    if parity != 0:
        raise ValueError("not a recovery key (bad parity)")
    return raw[2:34]


def decrypt_backup_session(private_key: bytes, session_data: dict[str, str]) -> dict[str, Any]:
    """Decrypts m.megolm_backup.v1.curve25519-aes-sha2 session data."""

    def b64(s: str) -> bytes:
        return base64.b64decode(s + "=" * (-len(s) % 4))

    shared = X25519PrivateKey.from_private_bytes(private_key).exchange(
        X25519PublicKey.from_public_bytes(b64(session_data["ephemeral"])))
    okm = HKDF(algorithm=hashes.SHA256(), length=80, salt=bytes(32), info=b"").derive(shared)
    aes_key, mac_key, iv = okm[:32], okm[32:64], okm[64:]
    ciphertext = b64(session_data["ciphertext"])
    # libolm computes the MAC over an empty string, a quirk the spec keeps.
    mac = hmac.new(mac_key, b"", hashlib.sha256).digest()[:8]
    if not hmac.compare_digest(mac, b64(session_data["mac"])):
        raise ValueError("backup MAC mismatch")
    decryptor = Cipher(algorithms.AES(aes_key), modes.CBC(iv)).decryptor()
    padded = decryptor.update(ciphertext) + decryptor.finalize()
    unpadder = padding.PKCS7(128).unpadder()
    return json.loads(unpadder.update(padded) + unpadder.finalize())


def backup_public_key(private_key: bytes) -> str:
    return base64.b64encode(
        X25519PrivateKey.from_private_bytes(private_key).public_key().public_bytes_raw()).decode().rstrip("=")


def _b64decode(s: str) -> bytes:
    return base64.b64decode(s + "=" * (-len(s) % 4))


def secret_storage_decrypt(key: bytes, name: str, encrypted: dict[str, str]) -> bytes:
    """Decrypts an m.secret_storage.v1.aes-hmac-sha2 secret (spec: Secret storage)."""

    okm = HKDF(algorithm=hashes.SHA256(), length=64, salt=bytes(32), info=name.encode()).derive(key)
    aes_key, mac_key = okm[:32], okm[32:]
    ciphertext = _b64decode(encrypted["ciphertext"])
    mac = hmac.new(mac_key, ciphertext, hashlib.sha256).digest()
    if not hmac.compare_digest(mac, _b64decode(encrypted["mac"])):
        raise ValueError(f"secret storage MAC mismatch for {name or 'the key check'}")
    decryptor = Cipher(algorithms.AES(aes_key), modes.CTR(_b64decode(encrypted["iv"]))).decryptor()
    return decryptor.update(ciphertext) + decryptor.finalize()


def check_secret_storage_key(key: bytes, description: dict[str, str]) -> bool:
    """Returns whether a key belongs to a secret storage key description.

    The description holds the MAC of 32 zero bytes, encrypted with the
    description's IV and an empty secret name, but not the ciphertext.
    """

    okm = HKDF(algorithm=hashes.SHA256(), length=64, salt=bytes(32), info=b"").derive(key)
    encryptor = Cipher(algorithms.AES(okm[:32]), modes.CTR(_b64decode(description["iv"]))).encryptor()
    ciphertext = encryptor.update(bytes(32)) + encryptor.finalize()
    mac = hmac.new(okm[32:], ciphertext, hashlib.sha256).digest()
    return hmac.compare_digest(mac, _b64decode(description["mac"]))


def backup_key_from_secret_storage(checks: Checks, client: Client, user_id: str, key: bytes) -> bytes | None:
    """Returns the backup key held in secret storage, using a secret storage key."""

    def account_data(typ: str) -> dict[str, Any]:
        return client.get(f"/_matrix/client/v3/user/{quote(user_id)}/account_data/{quote(typ)}")

    encrypted = account_data("m.megolm_backup.v1").get("encrypted", {})
    for key_id, secret in sorted(encrypted.items()):
        description = account_data(f"m.secret_storage.key.{key_id}")
        if description.get("algorithm") != "m.secret_storage.v1.aes-hmac-sha2":
            continue
        if not check_secret_storage_key(key, description):
            continue
        print(f"ok    {user_id}: recovery key is secret storage key {key_id}")
        return _b64decode(secret_storage_decrypt(key, "m.megolm_backup.v1", secret).decode())

    checks.true(f"{user_id}: recovery key is the backup key or a secret storage key holding it", False,
                f"secret storage keys holding the backup key: {sorted(encrypted)}")
    return None


def check_backup_decryption(
    checks: Checks, client: Client, conn: Any, user_id: str, version: dict[str, Any], recovery_key: str
) -> None:
    key = decode_recovery_key(recovery_key)
    want_public_key = version["auth_data"]["public_key"]
    if backup_public_key(key) == want_public_key:
        print(f"ok    {user_id}: recovery key is the backup key")
        private_key = key
    else:
        # Clients since secret storage (MSC1946) give users a key for secret
        # storage instead, which holds the backup key.
        private_key = backup_key_from_secret_storage(checks, client, user_id, key)
        if private_key is None:
            return
        checks.equal(f"{user_id}: backup key from secret storage matches the backup",
                     want_public_key, backup_public_key(private_key))
        if backup_public_key(private_key) != want_public_key:
            return

    backup = client.get(f"/_matrix/client/v3/room_keys/keys", version=version["version"])
    for room_id, room in sorted(backup["rooms"].items()):
        for session_id, key in sorted(room["sessions"].items()):
            session = decrypt_backup_session(private_key, key["session_data"])
            # An exported Megolm session is version (1) | index (4) |
            # ratchet (128) | Ed25519 public key (32) | signature; the
            # session ID is the public key.
            exported = base64.b64decode(session["session_key"] + "=" * (-len(session["session_key"]) % 4))
            derived_id = base64.b64encode(exported[133:165]).decode().rstrip("=")
            checks.equal(f"{user_id}: decrypted backup key {session_id[:8]}… has its session ID", session_id, derived_id)
            used = query(conn, """
                SELECT COUNT(*) AS n FROM roomserver_event_json j
                JOIN roomserver_events e USING (event_nid) JOIN roomserver_rooms r USING (room_nid)
                WHERE r.room_id = %s AND j.event_json::jsonb -> 'content' ->> 'session_id' = %s
                """, (room_id, session_id))[0]["n"]
            checks.true(f"{user_id}: backup key {session_id[:8]}… encrypts {used} messages in {room_id}", used > 0)
            return


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("--synapse", required=True, help="Synapse client API base URL")
    parser.add_argument("--dendrite-db", required=True, help="libpq connection string for the Dendrite database")
    parser.add_argument("--dendrite-media", required=True, type=pathlib.Path, help="Dendrite's media_api.base_path")
    args = parser.parse_args()

    conn = psycopg2.connect(args.dendrite_db)
    conn.set_session(readonly=True)
    (server_name,) = {r["server_name"] for r in query(conn, "SELECT DISTINCT server_name FROM userapi_accounts")}
    users = query(conn, """
        SELECT DISTINCT ON (localpart) localpart, '@' || localpart || ':' || server_name AS user_id, device_id, access_token
        FROM userapi_devices WHERE server_name = %s ORDER BY localpart, last_seen_ts DESC
        """, (server_name,))

    checks = Checks()
    for user in users:
        try:
            check_user(checks, conn, args.synapse, args.dendrite_media, user)
        except urllib.error.HTTPError as e:
            checks.true(f"{user['user_id']}: {e.url}", False, f"HTTP {e.code}: {e.read()[:200]!r}")

    print()
    if checks.failures:
        print(f"{len(checks.failures)} checks failed.")
        sys.exit(1)
    print("All checks passed.")


if __name__ == "__main__":
    main()
