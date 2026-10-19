"""Generates and patches a Synapse config for development and testing.

Shared by scripts/dev-migrate.sh and the integration test's Compose setup, so
they don't drift apart. Runs in the Synapse image. Generates homeserver.yaml
(and a signing key) if missing, then applies the patches below, which are
idempotent, so it's safe to run before every start.

The config is patched as YAML rather than with sed, because the generated
layout changes between Synapse versions: newer versions write
bind_addresses as a block list, which sed patches silently stopped matching.
"""

import argparse
import pathlib
import subprocess
import sys

import yaml


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("--data-dir", type=pathlib.Path, default=pathlib.Path("/data"))
    parser.add_argument("--server-name", required=True)
    parser.add_argument("--db-host", required=True)
    parser.add_argument("--db-user", required=True)
    parser.add_argument("--db-password", required=True)
    parser.add_argument("--db-name", default="synapse")
    parser.add_argument("--public-baseurl")
    parser.add_argument(
        "--log-to-console", action="store_true",
        help="log to stdout instead of homeserver.log in the data directory")
    parser.add_argument("--log-level", default="INFO")
    args = parser.parse_args()

    config_path = args.data_dir / "homeserver.yaml"
    if not config_path.exists():
        subprocess.run(
            [sys.executable, "-m", "synapse.app.homeserver",
             f"--server-name={args.server_name}", f"--config-path={config_path}",
             "--generate-config", "--report-stats=no", f"--data-directory={args.data_dir}"],
            check=True, stdout=subprocess.DEVNULL,
        )

    config = yaml.safe_load(config_path.read_text())
    if config["server_name"] != args.server_name:
        raise SystemExit(f"{config_path} is for {config['server_name']}, not {args.server_name}")
    config["database"] = {
        "name": "psycopg2",
        "args": {
            "user": args.db_user,
            "password": args.db_password,
            "host": args.db_host,
            "database": args.db_name,
        },
    }
    # Reachable from other containers, e.g. the client test.
    for listener in config["listeners"]:
        listener["bind_addresses"] = ["0.0.0.0"]
    if args.public_baseurl:
        config["public_baseurl"] = args.public_baseurl
    config_path.write_text(yaml.safe_dump(config))

    log_config_path = pathlib.Path(config["log_config"])
    log_config = yaml.safe_load(log_config_path.read_text())
    # The generated file handler writes to the working directory, which may
    # not be writable.
    log_config["handlers"]["file"]["filename"] = str(args.data_dir / "homeserver.log")
    log_config["root"]["handlers"] = ["console"] if args.log_to_console else ["buffer"]
    log_config["root"]["level"] = args.log_level
    log_config_path.write_text(yaml.safe_dump(log_config))


if __name__ == "__main__":
    main()
