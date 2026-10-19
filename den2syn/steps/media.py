"""Migrates media uploaded by local users.

Files are copied through Synapse's media storage, so they also reach any
configured storage providers, and keep their media IDs, so existing mxc://
URIs keep working. Synapse's thumbnailer then generates thumbnails, since
Synapse only serves thumbnails it has recorded (unless dynamic_thumbnails is
set).

Remote media isn't migrated; Synapse downloads it again on demand.
"""

import base64
import hashlib
import logging
import pathlib

from synapse.media._base import FileInfo
from synapse.server import HomeServer
from synapse.types import UserID

from den2syn.dendrite import Dendrite, VerificationError, verify_equal

logger = logging.getLogger(__name__)


def _dendrite_path(base: pathlib.Path, base64_hash: str) -> pathlib.Path:
    """Returns where Dendrite stores a file (fileutils.GetPathFromBase64Hash)."""

    return base / base64_hash[0] / base64_hash[1] / base64_hash[2:] / "file"


async def run(hs: HomeServer, dendrite: Dendrite) -> None:
    if dendrite.media_path is None:
        raise ValueError("--dendrite-media is required to migrate media")

    store = hs.get_datastores().main
    media_repo = hs.get_media_repository()
    rows = dendrite.query(
        """
        SELECT media_id, content_type, file_size_bytes, creation_ts, upload_name, base64hash, user_id
        FROM mediaapi_media_repository
        WHERE media_origin = %s
        ORDER BY creation_ts
        """,
        (dendrite.server_name,),
    )

    # Checked up front, listing every missing file: a wrong --dendrite-media
    # would otherwise show up as every file missing, and an incomplete copy of
    # the folder should be fixed before cutover.
    missing = [
        f"{r['media_id']} ({_dendrite_path(dendrite.media_path, r['base64hash'])})"
        for r in rows
        if not _dendrite_path(dendrite.media_path, r["base64hash"]).is_file()
    ]
    if missing:
        raise VerificationError(
            f"{len(missing)} of {len(rows)} media files are missing from {dendrite.media_path}: " + ", ".join(missing))

    migrated = []
    for r in rows:
        path = _dendrite_path(dendrite.media_path, r["base64hash"])
        data = path.read_bytes()
        sha256 = hashlib.sha256(data)
        # Dendrite names files by the unpadded URL-safe base64 of the SHA-256.
        verify_equal(f"hash of media {r['media_id']}", r["base64hash"],
                     base64.urlsafe_b64encode(sha256.digest()).decode().rstrip("="))
        verify_equal(f"size of media {r['media_id']}", r["file_size_bytes"], len(data))

        # Dendrite stores no content type as "". The spec's default for
        # unknown content is application/octet-stream.
        media_type = r["content_type"] or "application/octet-stream"
        with path.open("rb") as f:
            await media_repo.media_storage.store_file(f, FileInfo(None, r["media_id"]))
        await store.store_local_media(
            media_id=r["media_id"],
            media_type=media_type,
            time_now_ms=r["creation_ts"],
            upload_name=r["upload_name"] or None,
            media_length=len(data),
            user_id=UserID.from_string(r["user_id"]),
            sha256=sha256.hexdigest(),
        )
        # Returns None for media it can't thumbnail, like encrypted attachments.
        await media_repo._generate_thumbnails(None, r["media_id"], r["media_id"], media_type)
        migrated.append((r["media_id"], sha256.hexdigest()))

    got = await store.db_pool.simple_select_list(
        "local_media_repository", None, ("media_id", "sha256"), desc="den2syn_verify_media")
    verify_equal("local media", sorted(migrated), sorted(got))
    for media_id, sha256_hex in migrated:
        stored = pathlib.Path(media_repo.filepaths.local_media_filepath(media_id))
        verify_equal(f"stored file of media {media_id}", sha256_hex, hashlib.sha256(stored.read_bytes()).hexdigest())

    ((thumbnails,),) = await store.db_pool.execute(
        "den2syn_count_thumbnails", "SELECT COUNT(*) FROM local_media_repository_thumbnails")
    logger.info("Migrated %d media files, with %d thumbnails.", len(migrated), thumbnails)
