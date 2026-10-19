"""Round-trip tests for the key backup decryption in client_test.py.

These only show that decryption inverts encryption as the spec describes it.
Running client_test.py with a real recovery key is what shows the backup
works with real clients.
"""

import base64
import hashlib
import hmac
import json
import os
import unittest

from cryptography.hazmat.primitives import hashes, padding
from cryptography.hazmat.primitives.asymmetric.x25519 import X25519PrivateKey
from cryptography.hazmat.primitives.ciphers import Cipher, algorithms, modes
from cryptography.hazmat.primitives.kdf.hkdf import HKDF

from tests.client_test import (
    _BASE58, check_secret_storage_key, decode_recovery_key, decrypt_backup_session, secret_storage_decrypt)


def encode_recovery_key(private_key: bytes) -> str:
    raw = b"\x8b\x01" + private_key
    parity = 0
    for b in raw:
        parity ^= b
    raw += bytes([parity])
    n = int.from_bytes(raw, "big")
    out = ""
    while n:
        n, r = divmod(n, 58)
        out = _BASE58[r] + out
    return " ".join(out[i:i + 4] for i in range(0, len(out), 4))


def encrypt_backup_session(public_key: X25519PrivateKey, session: dict) -> dict:
    ephemeral = X25519PrivateKey.generate()
    shared = ephemeral.exchange(public_key.public_key())
    okm = HKDF(algorithm=hashes.SHA256(), length=80, salt=bytes(32), info=b"").derive(shared)
    aes_key, mac_key, iv = okm[:32], okm[32:64], okm[64:]
    padder = padding.PKCS7(128).padder()
    padded = padder.update(json.dumps(session).encode()) + padder.finalize()
    encryptor = Cipher(algorithms.AES(aes_key), modes.CBC(iv)).encryptor()
    ciphertext = encryptor.update(padded) + encryptor.finalize()

    def b64(b: bytes) -> str:
        return base64.b64encode(b).decode().rstrip("=")

    return {
        "ephemeral": b64(ephemeral.public_key().public_bytes_raw()),
        "ciphertext": b64(ciphertext),
        "mac": b64(hmac.new(mac_key, b"", hashlib.sha256).digest()[:8]),
    }


def secret_storage_encrypt(key: bytes, name: str, plaintext: bytes) -> dict:
    okm = HKDF(algorithm=hashes.SHA256(), length=64, salt=bytes(32), info=name.encode()).derive(key)
    iv = bytearray(os.urandom(16))
    # Clients clear bit 63 of the IV, for compatibility with Android.
    iv[8] &= 0x7F
    encryptor = Cipher(algorithms.AES(okm[:32]), modes.CTR(bytes(iv))).encryptor()
    ciphertext = encryptor.update(plaintext) + encryptor.finalize()
    return {
        "iv": base64.b64encode(bytes(iv)).decode(),
        "ciphertext": base64.b64encode(ciphertext).decode(),
        "mac": base64.b64encode(hmac.new(okm[32:], ciphertext, hashlib.sha256).digest()).decode(),
    }


class BackupDecryptionTest(unittest.TestCase):
    def test_secret_storage_round_trip(self) -> None:
        key = os.urandom(32)
        secret = b"backup key in base64"
        self.assertEqual(secret, secret_storage_decrypt(key, "m.megolm_backup.v1",
                                                        secret_storage_encrypt(key, "m.megolm_backup.v1", secret)))

    def test_secret_storage_key_check(self) -> None:
        key = os.urandom(32)
        # Like a key description: only the IV and MAC of encrypted zeros.
        description = {k: v for k, v in secret_storage_encrypt(key, "", bytes(32)).items() if k != "ciphertext"}
        self.assertTrue(check_secret_storage_key(key, description))
        self.assertFalse(check_secret_storage_key(os.urandom(32), description))

    def test_secret_storage_wrong_name(self) -> None:
        key = os.urandom(32)
        with self.assertRaises(ValueError):
            secret_storage_decrypt(key, "other", secret_storage_encrypt(key, "m.megolm_backup.v1", b"x"))

    def test_recovery_key_round_trip(self) -> None:
        private_key = os.urandom(32)
        self.assertEqual(private_key, decode_recovery_key(encode_recovery_key(private_key)))

    def test_recovery_key_bad_parity(self) -> None:
        key = encode_recovery_key(os.urandom(32)).replace(" ", "")
        # Changing the last character changes the parity byte.
        bad = key[:-1] + ("1" if key[-1] != "1" else "2")
        with self.assertRaises(ValueError):
            decode_recovery_key(bad)

    def test_session_round_trip(self) -> None:
        key = X25519PrivateKey.generate()
        session = {"algorithm": "m.megolm.v1.aes-sha2", "session_key": "AQAAAAA"}
        got = decrypt_backup_session(key.private_bytes_raw(), encrypt_backup_session(key, session))
        self.assertEqual(session, got)

    def test_session_wrong_key(self) -> None:
        key = X25519PrivateKey.generate()
        data = encrypt_backup_session(key, {"session_key": "x"})
        with self.assertRaises(ValueError):
            decrypt_backup_session(X25519PrivateKey.generate().private_bytes_raw(), data)


if __name__ == "__main__":
    unittest.main()
