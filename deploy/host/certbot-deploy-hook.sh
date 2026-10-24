#!/bin/sh
# certbot deploy hook: copies a fresh certificate to where dumbwaiter reads
# it. dumbwaiter rereads the files every server.https.reload_interval (1m by
# default), so there is no restart.
#
#   install -m 0755 certbot-deploy-hook.sh /etc/letsencrypt/renewal-hooks/deploy/dumbwaiter
#
# certbot runs it after every successful issuance or renewal with
# RENEWED_LINEAGE=/etc/letsencrypt/live/<name>. The files land in
# $TLS_DIR/<name>/tls.crt and tls.key. certbot keeps privkey.pem root-only,
# so dumbwaiter cannot read it in place.

set -eu

# systemd: /etc/dumbwaiter/tls and the dumbwaiter group.
# Docker Compose: the certs directory next to compose.tls.yaml and group 65532,
# the nonroot user of the image.
TLS_DIR=/etc/dumbwaiter/tls
TLS_GROUP=dumbwaiter

: "${RENEWED_LINEAGE:?certbot sets RENEWED_LINEAGE, install this as a deploy hook}"

name=$(basename "$RENEWED_LINEAGE")
dest=$TLS_DIR/$name

install -d -m 0750 -o root -g "$TLS_GROUP" "$dest"
# Write next to the old files, then rename: dumbwaiter never reads a
# half-written file. If it rereads between the two renames, the pair does not
# match, the reload fails and the old pair stays until the next attempt.
install -m 0644 -o root -g "$TLS_GROUP" "$RENEWED_LINEAGE/fullchain.pem" "$dest/.tls.crt.new"
install -m 0640 -o root -g "$TLS_GROUP" "$RENEWED_LINEAGE/privkey.pem" "$dest/.tls.key.new"
mv -f "$dest/.tls.crt.new" "$dest/tls.crt"
mv -f "$dest/.tls.key.new" "$dest/tls.key"

echo "dumbwaiter: installed $name into $dest"
