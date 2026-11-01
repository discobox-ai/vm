# disco-vm's Linux guest image (ADR 26-10-31-990): a base vz boots with
# VZLinuxBootLoader, and hcs will with direct kernel boot.
#
# It is discobox's vm-image pipeline (discobox ADR 0062 §6, §8; ADR 0101) with
# disco-vm's content: a general Debian machine with systemd and nothing else,
# whose root is the instance's own and writable, and which runs the disco-vm
# agent a boot hands it in its initrd. No Docker, nothing of discobox's, and no
# agent baked in: a boot appends the host's, so the protocol ships to both ends
# at once.
#
# The final stage is the artifact set: vmlinux (uncompressed), initrd.img, and
# root.ext4 (raw). Built for linux/arm64 (vz on Apple silicon) and linux/amd64
# (hcs; an Intel Mac), with no per-architecture branch but the kernel's form.

FROM public.ecr.aws/docker/library/debian:13-slim AS rootfs
ARG TARGETARCH

RUN printf '%s\n' \
        'path-exclude /usr/share/doc/*' \
        'path-exclude /usr/share/info/*' \
        'path-exclude /usr/share/locale/*' \
        'path-exclude /usr/share/man/*' \
        > /etc/dpkg/dpkg.cfg.d/01-disco-vm-nodoc

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        dbus \
        e2fsprogs \
        initramfs-tools \
        iproute2 \
        kmod \
        linux-image-${TARGETARCH} \
        systemd \
        systemd-resolved \
        systemd-sysv \
        udev \
        util-linux \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

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
# the kernel on an architecture are ignored there.
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
# it too, and a preset is a third party's switch.
RUN mkdir -p /var/lib/disco-vm \
    && systemctl enable \
        disco-vm-guest.service \
        disco-vm-timesync.timer \
        systemd-networkd.service \
        systemd-networkd-wait-online.service \
        systemd-resolved.service

FROM public.ecr.aws/docker/library/debian:13-slim AS assemble
ARG TARGETARCH
RUN apt-get update \
    && apt-get install -y --no-install-recommends e2fsprogs gzip initramfs-tools-core lz4 xz-utils zstd \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*
COPY --from=rootfs / /rootfs
COPY --chmod=0755 assemble verify /usr/local/sbin/
RUN TARGETARCH="$TARGETARCH" assemble /rootfs /out && TARGETARCH="$TARGETARCH" verify /out

FROM scratch
COPY --from=assemble /out/ /
