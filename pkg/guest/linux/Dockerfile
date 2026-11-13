# check=skip=InvalidDefaultArgInFrom
# disco-vm's Linux guest image (ADR 26-10-31-990): a base vz boots with
# VZLinuxBootLoader, and hcs will with direct kernel boot.
#
# It is discobox's vm-image pipeline (discobox ADR 0062 §6, §8; ADR 0101) with
# disco-vm's content: a general Debian machine with systemd and nftables,
# whose root is the instance's own and writable, and which runs the disco-vm
# agent a boot hands it in its initrd. No Docker, nothing of discobox's, and no
# agent baked in: a boot appends the host's, so the protocol ships to both ends
# at once.
#
# The final stage is the artifact set: vmlinux (uncompressed), initrd.img, and
# root.ext4 (raw). Built for linux/arm64 (vz on Apple silicon) and linux/amd64
# (hcs; an Intel Mac), with no per-architecture branch but the kernel's form.
# The amd64 variant also carries krun/vmlinux, the kernel libkrun boots (ADR
# 26-11-03-530 §5): libkrunfw's kernel source, patches, and configuration at a
# pinned release, with what this guest needs added (krun-kernel.config) and
# one patch of disco-vm's (krun-kernel-poweroff.patch: libkrun's machine has
# no ACPI, so a power-off is a reset, its one way out), built on the build
# platform. libkrun boots no kernel without libkrunfw's patches,
# so it cannot boot Debian's.
#
# Every stage is Debian sid at one snapshot (the root DESIGN.md, Debian), and
# the kernel vz and hcs boot is sid's linux-image on the sid root: the owner's
# choice of a 7.x kernel, 2026-11-13. libkrun's kernel is the one exception:
# libkrunfw's, built here on sid. Build through go tool task, which passes
# DEBIAN_IMAGE and DEBIAN_SNAPSHOT.
# DEBIAN_IMAGE has no default, so the Taskfile names it alone (the check
# directive above skips the warning that gives).
ARG DEBIAN_IMAGE

# The libkrun kernel, cross-compiled on the build platform. libkrun on arm64
# is deferred (ADR 26-11-03-530 §10), so another target gets no krun/.
FROM --platform=$BUILDPLATFORM ${DEBIAN_IMAGE} AS krun-kernel
ARG TARGETARCH
ARG BUILDARCH
# libkrunfw and the kernel it names, each pinned by its SHA-256.
ARG LIBKRUNFW_VERSION=5.6.2
ARG LIBKRUNFW_SHA256=df45d649fcbd07a4d0ca03fa836b2f640fdcf9b40187f3eb7023259c6e83d582
ARG KERNEL_VERSION=6.12.109
ARG KERNEL_SHA256=5484e552a334e15019f4aeba89e5b58f04651cf2f4e24e04de9f152f1c38e3fa
ARG DEBIAN_SNAPSHOT
# apt reads sid at the Taskfile's snapshot, and the stage is upgraded to it
# whole, so a build is the same on any day (the root DESIGN.md, Debian).
RUN : "${DEBIAN_SNAPSHOT:?build through go tool task, which passes the Debian snapshot}" \
    && sed -i "s|^URIs: .*|URIs: http://snapshot.debian.org/archive/debian/${DEBIAN_SNAPSHOT}/|" /etc/apt/sources.list.d/debian.sources \
    && printf '%s\n' 'Acquire::Check-Valid-Until "false";' 'Acquire::Retries "5";' >/etc/apt/apt.conf.d/10-disco-vm-snapshot \
    && apt-get update \
    && apt-get -y full-upgrade \
    && apt-get install -y --no-install-recommends \
        bc bison ca-certificates curl flex gcc libc6-dev libelf-dev libssl-dev make patch python3 xz-utils \
        $([ "$BUILDARCH" = amd64 ] || echo gcc-x86-64-linux-gnu) \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*
COPY krun-kernel.config krun-kernel-poweroff.patch /src/
RUN set -eux; \
    mkdir -p /out; \
    [ "$TARGETARCH" = amd64 ] || exit 0; \
    cd /src; \
    curl -fsSL -o libkrunfw.tar.gz "https://github.com/containers/libkrunfw/archive/refs/tags/v${LIBKRUNFW_VERSION}.tar.gz"; \
    echo "$LIBKRUNFW_SHA256  libkrunfw.tar.gz" | sha256sum -c -; \
    tar xzf libkrunfw.tar.gz; \
    fw="/src/libkrunfw-${LIBKRUNFW_VERSION}"; \
    kernel="linux-${KERNEL_VERSION}"; \
    [ "$(sed -n 's/^KERNEL_VERSION = //p' "$fw/Makefile")" = "$kernel" ] || { echo "krun kernel: libkrunfw ${LIBKRUNFW_VERSION} names another kernel than ${kernel}" >&2; exit 1; }; \
    curl -fsSL -o kernel.tar.xz "https://cdn.kernel.org/pub/linux/kernel/v6.x/${kernel}.tar.xz"; \
    echo "$KERNEL_SHA256  kernel.tar.xz" | sha256sum -c -; \
    tar xJf kernel.tar.xz; \
    cd "$kernel"; \
    for patch in $(find "$fw/patches" -name '0*.patch' | sort) /src/krun-kernel-poweroff.patch; do patch -s -p1 <"$patch"; done; \
    cross=; [ "$BUILDARCH" = amd64 ] || cross=x86_64-linux-gnu-; \
    cp "$fw/config-libkrunfw_x86_64" .config; \
    ARCH=x86_64 scripts/kconfig/merge_config.sh -m .config /src/krun-kernel.config; \
    make ARCH=x86_64 CROSS_COMPILE="$cross" olddefconfig; \
    grep -E '^(CONFIG_|# CONFIG_.* is not set$)' /src/krun-kernel.config | while read -r want; do \
        grep -qx "$want" .config || { echo "krun kernel: olddefconfig changed $want" >&2; exit 1; }; \
    done; \
    make ARCH=x86_64 CROSS_COMPILE="$cross" KBUILD_BUILD_TIMESTAMP="libkrunfw ${LIBKRUNFW_VERSION}" \
        KBUILD_BUILD_USER=disco-vm KBUILD_BUILD_HOST=disco-vm -j"$(nproc)" vmlinux; \
    mkdir -p /out/krun; cp vmlinux /out/krun/vmlinux; \
    "${cross}strip" --strip-debug /out/krun/vmlinux; \
    echo "${kernel#linux-} libkrunfw ${LIBKRUNFW_VERSION}" >/out/krun/version

FROM ${DEBIAN_IMAGE} AS rootfs
ARG TARGETARCH

RUN printf '%s\n' \
        'path-exclude /usr/share/doc/*' \
        'path-exclude /usr/share/info/*' \
        'path-exclude /usr/share/locale/*' \
        'path-exclude /usr/share/man/*' \
        > /etc/dpkg/dpkg.cfg.d/01-disco-vm-nodoc

ARG DEBIAN_SNAPSHOT
# apt reads sid at the Taskfile's snapshot, and the stage is upgraded to it
# whole, so a build is the same on any day (the root DESIGN.md, Debian).
RUN : "${DEBIAN_SNAPSHOT:?build through go tool task, which passes the Debian snapshot}" \
    && sed -i "s|^URIs: .*|URIs: http://snapshot.debian.org/archive/debian/${DEBIAN_SNAPSHOT}/|" /etc/apt/sources.list.d/debian.sources \
    && printf '%s\n' 'Acquire::Check-Valid-Until "false";' 'Acquire::Retries "5";' >/etc/apt/apt.conf.d/10-disco-vm-snapshot \
    && apt-get update \
    && apt-get -y full-upgrade \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        dbus \
        e2fsprogs \
        initramfs-tools \
        iproute2 \
        kmod \
        linux-image-${TARGETARCH} \
        nftables \
        systemd \
        systemd-resolved \
        systemd-sysv \
        udev \
        util-linux \
        zstd \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/* \
    && sed -i "s|^URIs: .*|URIs: http://deb.debian.org/debian|" /etc/apt/sources.list.d/debian.sources \
    && rm /etc/apt/apt.conf.d/10-disco-vm-snapshot

# Debian's kernel carries a module for every device Debian supports. This
# guest's hardware is virtio on vz and Hyper-V's synthetic devices on hcs, so
# whole classes describe hardware that cannot appear. drivers/scsi stays:
# Hyper-V's disk (hv_storvsc) is there.
RUN set -eux; \
    modules=$(ls -d /usr/lib/modules/*); \
    for class in \
        drivers/ata drivers/atm drivers/bcma drivers/bluetooth drivers/firewire \
        drivers/gnss drivers/gpu drivers/iio drivers/infiniband drivers/isdn \
        drivers/leds drivers/media drivers/memstick drivers/mmc drivers/mtd \
        drivers/net/ethernet drivers/net/wireless drivers/nvme drivers/parport \
        drivers/pcmcia drivers/platform drivers/ssb drivers/staging \
        drivers/thunderbolt drivers/usb drivers/w1 sound; \
    do \
        rm -rf "$modules/kernel/$class"; \
    done; \
    depmod -a "$(basename "$modules")"
RUN rm -rf /usr/lib/udev/hwdb.bin /usr/lib/udev/hwdb.d

# The initrd mounts an ext4 root on a virtio disk (vz) or a Hyper-V one (hcs),
# and carries the hook that moves the agent a boot appends. Names built into
# the kernel on an architecture are ignored there. It is zstd, which both
# kernels unpack: update-initramfs falls back to gzip, with only a warning,
# when the root has no zstd, so the package list carries it and verify checks.
COPY --chmod=0755 disco-vm-agent /etc/initramfs-tools/scripts/init-bottom/disco-vm-agent
COPY --chmod=0755 disco-vm-agent-hook /etc/initramfs-tools/hooks/disco-vm-agent
RUN set -eux; \
    printf '%s\n' ext4 virtio_blk virtio_console virtio_mmio virtio_pci \
        hv_vmbus hv_storvsc \
        > /etc/initramfs-tools/modules; \
    sed -i 's/^MODULES=.*/MODULES=list/; s/^COMPRESS=.*/COMPRESS=zstd/' /etc/initramfs-tools/initramfs.conf; \
    grep -q '^MODULES=list$' /etc/initramfs-tools/initramfs.conf; \
    if ls /boot/initrd.img-* >/dev/null 2>&1; then update-initramfs -u -k all; else update-initramfs -c -k all; fi

# A headless guest has no seats or consoles to log in at; these fail or loop on
# the serial console, the one record a guest that never answered leaves.
RUN systemctl mask \
        console-getty.service \
        getty.target \
        systemd-firstboot.service \
        systemd-logind.service \
        systemd-networkd-persistent-storage.service \
    && : > /etc/machine-id

COPY 10-disco-vm.network /etc/systemd/network/10-disco-vm.network
COPY wait-online.conf /etc/systemd/system/systemd-networkd-wait-online.service.d/10-disco-vm.conf
COPY disco-vm-guest.service /etc/systemd/system/disco-vm-guest.service
COPY --chmod=0755 disco-vm-sync-clock /usr/local/sbin/disco-vm-sync-clock
COPY disco-vm-timesync.service /etc/systemd/system/disco-vm-timesync.service
COPY disco-vm-timesync.timer /etc/systemd/system/disco-vm-timesync.timer
COPY fstab /etc/fstab
# The explicit enable of wait-online is the guarantee: Debian's preset enables
# it too, and a preset is a third party's switch. nftables.service loads
# /etc/nftables.conf before network-pre.target on every boot, so a rule set a
# user of the base saves there loads before the network comes up. A failed load
# does not hold the network back; a user that needs the fence requires the unit.
# Debian's own nftables.conf, which this base keeps, admits everything.
RUN mkdir -p /var/lib/disco-vm \
    && systemctl enable \
        disco-vm-guest.service \
        disco-vm-timesync.timer \
        nftables.service \
        systemd-networkd.service \
        systemd-networkd-wait-online.service \
        systemd-resolved.service

FROM ${DEBIAN_IMAGE} AS assemble
ARG TARGETARCH
ARG DEBIAN_SNAPSHOT
# apt reads sid at the Taskfile's snapshot, and the stage is upgraded to it
# whole, so a build is the same on any day (the root DESIGN.md, Debian).
RUN : "${DEBIAN_SNAPSHOT:?build through go tool task, which passes the Debian snapshot}" \
    && sed -i "s|^URIs: .*|URIs: http://snapshot.debian.org/archive/debian/${DEBIAN_SNAPSHOT}/|" /etc/apt/sources.list.d/debian.sources \
    && printf '%s\n' 'Acquire::Check-Valid-Until "false";' 'Acquire::Retries "5";' >/etc/apt/apt.conf.d/10-disco-vm-snapshot \
    && apt-get update \
    && apt-get -y full-upgrade \
    && apt-get install -y --no-install-recommends e2fsprogs gzip initramfs-tools-core lz4 xz-utils zstd \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*
COPY --from=rootfs / /rootfs
COPY --chmod=0755 assemble verify /usr/local/sbin/
COPY --from=krun-kernel /out/ /krun-out/
RUN TARGETARCH="$TARGETARCH" assemble /rootfs /out \
    && cp -r /krun-out/. /out/ \
    && TARGETARCH="$TARGETARCH" verify /out

FROM scratch
COPY --from=assemble /out/ /
