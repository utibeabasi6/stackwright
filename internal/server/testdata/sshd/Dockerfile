# A host for the SSH enrolment test: sshd and the Docker CLI, with the host's
# Docker socket mounted in. systemctl is a stand-in.
FROM alpine:3.22.2
RUN apk add --no-cache openssh docker-cli && sed -i 's/^root:!/root:*/' /etc/shadow && mkdir -p /etc/systemd/system
COPY systemctl /usr/bin/systemctl
COPY entrypoint.sh /entrypoint.sh
ENTRYPOINT ["/entrypoint.sh"]
