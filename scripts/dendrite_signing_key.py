"""Converts a Dendrite signing key to Synapse's signing key format.

Keeping the server's signing key means other servers can keep verifying the
events and requests it signed, without fetching a new key.

Dendrite stores the key as PEM, with the 32-byte Ed25519 seed as the block
and the key ID in a Key-ID header (setup/config.readKeyPEM). Synapse wants
one line per key: "ed25519 <version> <unpadded base64 seed>".

Usage: python dendrite_signing_key.py < matrix_key.pem > homeserver.signing.key
"""

import base64
import re
import sys


def convert(pem: str) -> str:
    lines = []
    for block in re.finditer(r"-----BEGIN MATRIX PRIVATE KEY-----\n(.*?)-----END MATRIX PRIVATE KEY-----", pem, re.S):
        headers, _, body = block.group(1).partition("\n\n")
        key_id = dict(h.split(": ", 1) for h in headers.splitlines()).get("Key-ID", "")
        if not key_id.startswith("ed25519:"):
            raise ValueError(f"key ID {key_id!r} doesn't start with 'ed25519:'")
        seed = base64.b64decode("".join(body.split()))
        # Dendrite passes the block to ed25519.GenerateKey, which reads a
        # 32-byte seed.
        if len(seed) < 32:
            raise ValueError(f"key {key_id} is {len(seed)} bytes, not a 32-byte seed")
        lines.append(f"ed25519 {key_id.removeprefix('ed25519:')} {base64.b64encode(seed[:32]).decode().rstrip('=')}\n")
    if not lines:
        raise ValueError("no MATRIX PRIVATE KEY block found")
    return "".join(lines)


if __name__ == "__main__":
    sys.stdout.write(convert(sys.stdin.read().replace("\r\n", "\n")))
