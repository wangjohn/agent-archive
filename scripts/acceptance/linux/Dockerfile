# A disposable Ubuntu 24.04 machine with systemd as PID 1, for
# scripts/acceptance/linux/host.sh: what a server running a lingering user's
# systemd manager looks like, plus the tools the acceptance run uses.
FROM ubuntu:24.04
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update \
 && apt-get install -y --no-install-recommends systemd systemd-sysv dbus dbus-user-session sudo git ca-certificates curl less procps sqlite3 \
 && rm -rf /var/lib/apt/lists/*
# Two non-root users (Ubuntu's own "ubuntu" user already has UID 1000) whose
# user managers start at boot (linger), as they do on a server: ada is the
# person setting up, bob is the second user the real-manager tests run as.
RUN useradd -m -s /bin/bash -u 1100 ada && useradd -m -s /bin/bash -u 1101 bob \
 && mkdir -p /var/lib/systemd/linger \
 && touch /var/lib/systemd/linger/ada /var/lib/systemd/linger/bob
# guest.sh refuses to run where this file is not: only this image has it.
RUN echo "the disposable machine of scripts/acceptance/linux/host.sh; guest.sh runs only where this file is" >/etc/agent-archive-acceptance-machine
STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
