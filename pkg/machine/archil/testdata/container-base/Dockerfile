# The base TestSystemdInAContainer runs the fake's sandboxes from: the
# Taskfile's Debian, with no systemd, whose apt reads sid at the Taskfile's
# snapshot (the root DESIGN.md, Debian), so the install Create makes in it is
# the same on every run. go tool task test:archil:container builds it.
ARG DEBIAN_IMAGE
FROM ${DEBIAN_IMAGE}
ARG DEBIAN_SNAPSHOT
RUN : "${DEBIAN_SNAPSHOT:?build through go tool task, which passes the Debian snapshot}" \
    && sed -i "s|^URIs: .*|URIs: http://snapshot.debian.org/archive/debian/${DEBIAN_SNAPSHOT}/|" /etc/apt/sources.list.d/debian.sources \
    && printf '%s\n' 'Acquire::Check-Valid-Until "false";' 'Acquire::Retries "5";' >/etc/apt/apt.conf.d/10-disco-vm-snapshot
