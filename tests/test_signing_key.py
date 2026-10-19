"""Tests scripts/dendrite_signing_key.py against Synapse's key loading."""

import base64
import importlib.util
import os
import pathlib
import unittest

import nacl.signing
from signedjson.key import get_verify_key, read_signing_keys

_spec = importlib.util.spec_from_file_location(
    "dendrite_signing_key", pathlib.Path(__file__).parent.parent / "scripts" / "dendrite_signing_key.py")
dendrite_signing_key = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(dendrite_signing_key)


def dendrite_pem(key_id: str, seed: bytes) -> str:
    """Returns a key file like Dendrite's generate-keys writes."""

    body = base64.b64encode(seed).decode()
    return f"-----BEGIN MATRIX PRIVATE KEY-----\nKey-ID: {key_id}\n\n{body}\n-----END MATRIX PRIVATE KEY-----\n"


class SigningKeyTest(unittest.TestCase):
    def test_convert(self) -> None:
        seed = os.urandom(32)
        (key,) = read_signing_keys(dendrite_signing_key.convert(dendrite_pem("ed25519:abc123", seed)).splitlines())
        self.assertEqual("abc123", key.version)
        self.assertEqual(
            bytes(nacl.signing.SigningKey(seed).verify_key), bytes(get_verify_key(key)))

    def test_wrong_key_id(self) -> None:
        with self.assertRaises(ValueError):
            dendrite_signing_key.convert(dendrite_pem("abc123", os.urandom(32)))

    def test_no_key(self) -> None:
        with self.assertRaises(ValueError):
            dendrite_signing_key.convert("")


if __name__ == "__main__":
    unittest.main()
