"""Creates a placeholder Dendrite media folder for development.

Writes a small file for every row in mediaapi_media_repository, in Dendrite's
layout, and updates the rows' hash and size to match, so den2syn's checks
pass. Images get a real JPEG, so thumbnailing is exercised.

Only run this against the scratch copy of the Dendrite database: it rewrites
media metadata, and leaves a den2syn_dev_placeholder_media table as a marker.
Files are derived from the media ID, so reruns are idempotent.

Usage: python dev_media_placeholders.py <libpq connection string> <media dir>
"""

import base64
import hashlib
import io
import pathlib
import sys

import psycopg2
from PIL import Image


def content(media_id: str, content_type: str) -> bytes:
    seed = hashlib.sha256(media_id.encode()).digest()
    if content_type.startswith("image/"):
        buf = io.BytesIO()
        Image.new("RGB", (640, 480), tuple(seed[:3])).save(buf, "JPEG")
        return buf.getvalue()
    return seed * 32


def main() -> None:
    dsn, media_dir = sys.argv[1], pathlib.Path(sys.argv[2])
    with psycopg2.connect(dsn) as conn, conn.cursor() as cur:
        cur.execute("SELECT media_id, media_origin, content_type FROM mediaapi_media_repository")
        rows = cur.fetchall()
        for media_id, origin, content_type in rows:
            data = content(media_id, content_type)
            # Dendrite's fileutils: unpadded URL-safe base64 of the SHA-256.
            h = base64.urlsafe_b64encode(hashlib.sha256(data).digest()).decode().rstrip("=")
            path = media_dir / h[0] / h[1] / h[2:] / "file"
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(data)
            cur.execute(
                "UPDATE mediaapi_media_repository SET base64hash = %s, file_size_bytes = %s WHERE media_id = %s AND media_origin = %s",
                (h, len(data), media_id, origin),
            )
        # Tells dev-migrate.sh to reload the dump before using real media.
        cur.execute("CREATE TABLE IF NOT EXISTS den2syn_dev_placeholder_media ()")
        print(f"Wrote {len(rows)} placeholder files to {media_dir}.")


if __name__ == "__main__":
    main()
