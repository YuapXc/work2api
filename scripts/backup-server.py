#!/usr/bin/env python3
"""Create a consistent, root-only work2api backup during a short maintenance stop.

Run on the Linux server: sudo python3 backup-server.py
This deliberately has no automatic restore or retention deletion.
"""
import datetime
import hashlib
import os
import pathlib
import subprocess
import tarfile


def main():
    if os.geteuid() != 0:
        raise SystemExit("Run as root on the deployment server")
    os.umask(0o077)
    stamp = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    root = pathlib.Path("/opt/work2api/backups")
    root.mkdir(parents=True, exist_ok=True, mode=0o700)
    destination = root / ("full-" + stamp)
    destination.mkdir(mode=0o700)
    current = pathlib.Path("/opt/work2api/current").resolve()
    paths = [
        "/var/lib/work2api", "/etc/work2api", "/etc/systemd/system/work2api.service",
        "/etc/nginx/sites-available/work2api-api", "/etc/nginx/sites-available/work2api-admin",
        "/etc/nginx/snippets/work2api-proxy.conf", "/etc/nginx/conf.d/work2api-admin-origin.conf",
        "/etc/nginx/conf.d/work2api-origin-auth.conf",
        "/etc/nginx/ssl/cloudflare-origin.pem", "/etc/nginx/ssl/cloudflare-origin.key",
        str(current),
    ]
    active = subprocess.run(["systemctl", "is-active", "--quiet", "work2api"]).returncode == 0
    partial = destination / "state-config.tar.gz.partial"
    final = destination / "state-config.tar.gz"
    try:
        if active:
            subprocess.run(["systemctl", "stop", "work2api"], check=True)
        # Include WAL/SHM and all channel secrets together while writers are stopped.
        with tarfile.open(partial, "w:gz", dereference=False) as archive:
            for name in paths:
                path = pathlib.Path(name)
                if path.exists():
                    archive.add(path, arcname=str(path).lstrip("/"))
        partial.rename(final)
    finally:
        if active:
            subprocess.run(["systemctl", "start", "work2api"], check=True)
    with final.open("rb") as handle:
        digest = hashlib.file_digest(handle, "sha256").hexdigest()
    (destination / "state-config.tar.gz.sha256").write_text(digest + "  state-config.tar.gz\n")
    (destination / "version.txt").write_text(str(current) + "\n")
    print("Backup complete:", final)
    print("Restore requires maintenance isolation and matching application/schema versions.")


if __name__ == "__main__":
    main()
