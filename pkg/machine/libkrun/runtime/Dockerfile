# disco-vm's libkrun runtime (ADR 26-11-03-530 §3): the host side of a libkrun
# VM, as one directory that brings its own libc, so a Linux host needs only a
# kernel with KVM.
#
#   bin/krun-launch   the launcher (launch.c), linked against libkrun
#   bin/passt         passt, static
#   lib/              libkrun.so.1, every library it and the launcher need,
#                     glibc, and its loader, ld-linux-x86-64.so.2
#   runtime.json      the runtime's version, libkrun's and passt's, and the
#                     launcher's configuration version
#
# The driver runs the launcher as `lib/ld-linux-x86-64.so.2 --inhibit-cache
# --library-path lib bin/krun-launch`, never through the host's loader. The verify stage runs it
# that way in an otherwise empty filesystem, so a runtime missing a library
# fails here. linux/amd64 only (§10).

# Debian 13's own rustc is older than libkrun's dependencies need.
FROM public.ecr.aws/docker/library/rust:1.90-slim-trixie AS build
# Every source is pinned by its SHA-256: a tag can move, and a tarball can
# change under its name.
ARG LIBKRUN_VERSION=1.19.7
ARG LIBKRUN_SHA256=d4541fcd959ad43fb073bff61bc91b645ea6413a10b8319e9b61664a6102d20c
ARG PASST_VERSION=2026_10_02.cba3570
ARG PASST_SHA256=7b81892b1070521ed217de672cdce43d7a37436e6f850cd04f2b63919ac484b6
ARG RUNTIME_VERSION=dev
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates curl gcc libc6-dev libclang-dev make \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src
RUN curl -fsSL -o libkrun.tar.gz "https://github.com/containers/libkrun/archive/refs/tags/v${LIBKRUN_VERSION}.tar.gz" \
    && curl -fsSL -o passt.tar.gz "https://passt.top/passt/snapshot/passt-${PASST_VERSION}.tar.gz" \
    && printf '%s  %s\n' "$LIBKRUN_SHA256" libkrun.tar.gz "$PASST_SHA256" passt.tar.gz | sha256sum -c - \
    && tar xzf libkrun.tar.gz && tar xzf passt.tar.gz && rm libkrun.tar.gz passt.tar.gz
# libkrun with a virtio-blk disk and a virtio-net device, and no init of its
# own (the guest's systemd is init) and no libkrunfw (the kernel is the
# guest image's).
RUN cd "libkrun-${LIBKRUN_VERSION}" \
    && make BLK=1 NET=1 INIT_BLOB=0 \
    && make BLK=1 NET=1 INIT_BLOB=0 PREFIX=/usr/local install
RUN cd "passt-${PASST_VERSION}" && make static && cp passt /usr/local/bin/passt
COPY launch.c /src/launch.c
RUN set -eux; \
    mkdir -p /out/bin /out/lib; \
    gcc -O2 -Wall -Wextra -Werror -o /out/bin/krun-launch /src/launch.c \
        -I/usr/local/include -L/usr/local/lib64 -lkrun; \
    cp /usr/local/bin/passt /out/bin/passt; \
    if ldd /out/bin/passt 2>/dev/null | grep -q '=>'; then echo "passt is not static" >&2; exit 1; fi; \
    cp -L /usr/local/lib64/libkrun.so.1 /out/lib/; \
    for lib in $(LD_LIBRARY_PATH=/usr/local/lib64 ldd /out/bin/krun-launch | awk '/=>/ { print $3 } /^\t\/.*ld-linux/ { print $1 }'); do \
        cp -L "$lib" /out/lib/; \
    done; \
    test -f /out/lib/ld-linux-x86-64.so.2; \
    strip --strip-unneeded /out/bin/krun-launch /out/lib/libkrun.so.1; \
    printf '{\n  "version": "%s",\n  "libkrun": "%s",\n  "passt": "%s",\n  "config": "%s"\n}\n' \
        "$RUNTIME_VERSION" "$LIBKRUN_VERSION" "$PASST_VERSION" \
        "$(sed -n 's/^#define CONFIG_VERSION "\(.*\)"$/\1/p' /src/launch.c)" > /out/runtime.json; \
    ls -la /out/bin /out/lib; cat /out/runtime.json

# Nothing here but the runtime, so a library it lacks is a failure.
FROM scratch AS verify
COPY --from=build /out/ /runtime/
RUN ["/runtime/lib/ld-linux-x86-64.so.2", "--library-path", "/runtime/lib", "/runtime/bin/krun-launch", "--version"]
RUN ["/runtime/bin/passt", "--version"]

FROM scratch
COPY --from=verify /runtime/ /
