FROM debian:trixie-slim AS kernel
ARG KERNEL_VERSION=6.18.64
ARG KERNEL_SHA256=020d45510c539972e6f14c5c71566878d3b7ef56fb082b3e09134f2ebaade562
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      build-essential bc bison flex libelf-dev libssl-dev xz-utils curl ca-certificates \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src
RUN curl -fsSLO "https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-$KERNEL_VERSION.tar.xz" \
    && echo "$KERNEL_SHA256  linux-$KERNEL_VERSION.tar.xz" | sha256sum -c - \
    && tar -xJf "linux-$KERNEL_VERSION.tar.xz" --strip-components=1 \
    && rm "linux-$KERNEL_VERSION.tar.xz"
COPY deploy/guestboot/kernel.config .config
# olddefconfig must be a no-op, otherwise the committed config is stale for this kernel version.
RUN cp .config /tmp/committed.config && make olddefconfig \
    && diff /tmp/committed.config .config \
    && for opt in NF_TABLES NFT_COMPAT IP_NF_RAW IP6_NF_RAW NF_CT_NETLINK OVERLAY_FS BRIDGE VETH USER_NS PID_NS SECCOMP \
         VSOCKETS VIRTIO_VSOCKETS VIRTIO_BALLOON EXT4_FS BLK_DEV_INITRD DEVTMPFS_MOUNT SERIO_I8042 KEYBOARD_ATKBD; do \
         grep -qx "CONFIG_$opt=y" .config || { echo "CONFIG_$opt is not built in" >&2; exit 1; }; \
       done \
    && grep -qx '# CONFIG_MODULES is not set' .config
RUN make -j"$(nproc)" vmlinux && strip --strip-debug vmlinux

FROM debian:trixie-slim AS e2fsprogs
ARG E2FSPROGS_VERSION=1.47.4
ARG E2FSPROGS_SHA256=fd5bf388cbdbe006a3d3b318d983b2948382440acc85a87f1e7d108653e8db0b
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      build-essential curl ca-certificates xz-utils \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src
RUN curl -fsSLO "https://mirrors.edge.kernel.org/pub/linux/kernel/people/tytso/e2fsprogs/v$E2FSPROGS_VERSION/e2fsprogs-$E2FSPROGS_VERSION.tar.xz" \
    && echo "$E2FSPROGS_SHA256  e2fsprogs-$E2FSPROGS_VERSION.tar.xz" | sha256sum -c - \
    && tar -xJf "e2fsprogs-$E2FSPROGS_VERSION.tar.xz" --strip-components=1
RUN LDFLAGS=-static ./configure --disable-nls --disable-fuse2fs --disable-defrag --disable-e2initrd-helper \
    && make -j"$(nproc)" \
    && strip -o /mke2fs misc/mke2fs && strip -o /resize2fs resize/resize2fs \
    && ! ldd /mke2fs 2>/dev/null && ! ldd /resize2fs 2>/dev/null

FROM scratch
COPY --from=kernel /src/vmlinux /vmlinux
COPY --from=e2fsprogs /mke2fs /mke2fs
COPY --from=e2fsprogs /resize2fs /resize2fs
