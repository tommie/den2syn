# Plan

Ordered so each step only depends on earlier ones, and the riskiest part (rooms) is reached early.
Each step is done when its MIGRATED stories in `den2syn/stories.py` hold for the test dump, and the post-migration checks pass.

- [x] **Dev loop**: one command that recreates a scratch Synapse database and runs den2syn against the loaded Dendrite dump.
- [x] **users**: `users`, `profiles`, `devices`, `access_tokens`, `user_ips`, `user_threepids`, `registration_tokens`.
- [x] **account_data**: `account_data`, `room_account_data`, `room_tags` (from `m.tag`), `push_rules` and `push_rules_enable` (from `m.push_rules`), `user_filters`.
- [x] **e2e_keys**: device keys, one-time keys, cross-signing keys and signatures, key backup versions and contents.
- [x] **rooms**: `rooms`, events through `persist_events` with state groups checked against Dendrite's snapshots, aliases, forgotten rooms, published rooms.
- [x] **receipts**: needs event stream orderings.
- [x] **pushers**: start at the last migrated stream ordering.
- [x] **to_device**: undelivered to-device messages for migrated devices.
- [x] **media**: copy local media files, store metadata, generate thumbnails with Synapse's thumbnailer.
- [x] **finalize**: move `federation_stream_position` past migrated events.
- [x] **Client-level integration test**: log in with a migrated token, sync, read room history, keys, key backup and media.
- [x] **Key backup decryption with a real recovery key**: run the client test with `RECOVERY_KEY_<localpart>` set.
- [x] **Cutover runbook**: in README.md.
- [ ] **Rehearse** with a fresh Dendrite dump and media copy, following the runbook.
- [ ] **Cut over.**
