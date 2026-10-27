#!/bin/sh
# Daily consistent copy of the ngmgmt database; keeps the last 7.
set -eu
dst=/var/backups/ngmgmt
mkdir -p "$dst"
chmod 700 "$dst"
f="$dst/ngmgmt-$(date +%Y%m%d).db"
sqlite3 /var/lib/ngmgmt/ngmgmt.db ".backup '$f'"
gzip -f "$f"
ls -1t "$dst"/ngmgmt-*.db.gz | tail -n +8 | xargs -r rm -f
