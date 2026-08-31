#!/bin/sh
set -eu

# pg_basebackup connects to the special replication pseudo-database, which is
# not covered by the ordinary `host all all` rule produced by initdb. Keep the
# rule scoped to the demo database owner and require the same MD5 credentials
# used by the rest of the local compose stack.
cat >> "$PGDATA/pg_hba.conf" <<'EOF'
host replication playhack 0.0.0.0/0 md5
host replication playhack ::/0 md5
EOF
