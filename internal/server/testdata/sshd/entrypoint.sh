#!/bin/sh
set -eu
ssh-keygen -A >/dev/null
mkdir -p /root/.ssh
echo "$AUTHORIZED_KEY" > /root/.ssh/authorized_keys
chmod 700 /root/.ssh && chmod 600 /root/.ssh/authorized_keys
echo "${SANDBOX_CACHE:-}" > /etc/stackwright-sandbox-cache
if [ -n "${NO_DOCKER:-}" ]; then rm -f /usr/bin/docker; fi
exec /usr/sbin/sshd -D -e
