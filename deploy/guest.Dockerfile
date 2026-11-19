FROM --platform=linux/amd64 ubuntu:24.04@sha256:73b999393d07f2bf65a40373778a44dc9a6d54ad7718d29247d8ff9413c4d6a8

SHELL ["/bin/bash", "-euxo", "pipefail", "-c"]
ENV DEBIAN_FRONTEND=noninteractive

ARG APT_SNAPSHOT=20261117T000000Z
# The base image has no CA bundle, which the https snapshot service needs.
RUN apt-get -y -q update \
 && apt-get -y -q install --no-install-recommends ca-certificates \
 && apt-get -y -q --snapshot "$APT_SNAPSHOT" update \
 && apt-get -y -q --snapshot "$APT_SNAPSHOT" install --no-install-recommends \
    bash build-essential ca-certificates curl dbus git gnupg iproute2 iptables jq openssh-client procps \
    python3 python3-venv ripgrep sudo systemd systemd-sysv tar unzip xterm xvfb xz-utils \
    fonts-liberation libasound2t64 libatk-bridge2.0-0t64 libatk1.0-0t64 libatspi2.0-0t64 libcairo2 \
    libcups2t64 libdbus-1-3 libgbm1 libglib2.0-0t64 libgtk-3-0t64 libnspr4 libnss3 libpango-1.0-0 \
    libxcomposite1 libxdamage1 libxfixes3 libxkbcommon0 libxrandr2 \
 && rm -rf /var/lib/apt/lists/*

# Only the listed versions may come from a third-party repo; every other package of it is pinned out.
RUN install -d -m 0755 /etc/apt/keyrings; \
    addrepo() { \
      name=$1 url=$2 suite=$3 component=$4 keyurl=$5 fpr=$6; shift 6; \
      curl -fsSL --proto =https --proto-redir =https --retry 3 -o "/tmp/$name.key" "$keyurl"; \
      primary=$(gpg --show-keys --with-colons "/tmp/$name.key" | awk -F: '/^pub:/ { p = 1; next } p && /^fpr:/ { print $10; p = 0 }'); \
      echo "$primary" | grep -qx "$fpr" || { echo "$name key fingerprint is not $fpr (got $primary)" >&2; exit 1; }; \
      gpg --batch --quiet --import "/tmp/$name.key"; \
      gpg --batch --export "$fpr" > "/etc/apt/keyrings/$name.gpg"; \
      echo "deb [signed-by=/etc/apt/keyrings/$name.gpg] $url $suite $component" > "/etc/apt/sources.list.d/$name.list"; \
      host=${url#https://}; host=${host%%/*}; \
      printf 'Package: *\nPin: origin %s\nPin-Priority: -1\n' "$host" > "/etc/apt/preferences.d/$name"; \
      for p in "$@"; do \
        printf '\nPackage: %s\nPin: version %s\nPin-Priority: 1001\n' "${p%%=*}" "${p#*=}" >> "/etc/apt/preferences.d/$name"; \
      done; \
    }; \
    docker_pkgs=(containerd.io=2.4.2-4~ubuntu.24.04~noble docker-buildx-plugin=0.39.0-1~ubuntu.24.04~noble \
      docker-ce=5:29.10.0-1~ubuntu.24.04~noble docker-ce-cli=5:29.10.0-1~ubuntu.24.04~noble \
      docker-compose-plugin=5.8.0-1~ubuntu.24.04~noble); \
    xpra_pkgs=(xpra xpra-client xpra-client-gtk3 xpra-codecs xpra-common xpra-server xpra-x11); \
    xpra_pkgs=("${xpra_pkgs[@]/%/=6.5.5-r0-1}"); \
    addrepo docker https://download.docker.com/linux/ubuntu noble stable https://download.docker.com/linux/ubuntu/gpg \
      9DC858229FC7DD38854AE2D88D81803C0EBFCD88 "${docker_pkgs[@]}"; \
    addrepo xpra https://xpra.org noble main https://xpra.org/xpra.asc \
      B4993B57323148E37977E5D873254CAD17978FAF "${xpra_pkgs[@]}"; \
    gpgconf --kill all; \
    apt-get -y -q --snapshot "$APT_SNAPSHOT" update; \
    apt-get -y -q --snapshot "$APT_SNAPSHOT" install --no-install-recommends "${docker_pkgs[@]}" "${xpra_pkgs[@]}"; \
    apt-mark hold "${docker_pkgs[@]%%=*}" "${xpra_pkgs[@]%%=*}"; \
    rm -rf /var/lib/apt/lists/* /tmp/* /root/.gnupg

ARG NODE_VERSION=26.13.1
ARG NODE_SHA256=5c063e46bab0fbc1c17cc445d7ff129cbdcd0471be6ed83b6ab864f8143bc864
ARG CLAUDE_VERSION=2.1.311
ARG CLAUDE_SHA256=86dc1fcc69498159eaf15f446613e847010f9036bfbbdffd4c49350be8489ada
ARG CODEX_VERSION=0.170.0
ARG CODEX_SHA256=d054916ccd28a53820376b8d2f7c875184ca8f40fcd44bbabeb8ee240ce247f5
ARG UV_VERSION=0.13.9
ARG UV_SHA256=a7fbaff2b9a271d85c1bdb540941090fd1ce011797b4abeb48e4dcd39c2471d5
ARG GH_VERSION=2.105.0
ARG GH_SHA256=79e7ab5db738c48777b696e374f8b428f0647debfb424f44d2f426370407fe2f
ARG CHROME_VERSION=158.0.8127.36
ARG CHROME_SHA256=dba9cc838e1ce269c33ea68170881127bd15d078fc3ca5891c90802819a4d7c7

RUN fetch() { \
      curl -fsSL --proto =https --proto-redir =https --retry 3 -o "/tmp/$1" "$2"; \
      echo "$3  /tmp/$1" | sha256sum -c --quiet -; \
    }; \
    fetch node "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-x64.tar.xz" "$NODE_SHA256"; \
    tar -xJf /tmp/node -C /usr/local --strip-components=1 --no-same-owner; \
    fetch claude "https://downloads.claude.ai/claude-code-releases/${CLAUDE_VERSION}/linux-x64/claude" "$CLAUDE_SHA256"; \
    install -m 0755 /tmp/claude /usr/local/bin/claude; \
    fetch codex "https://github.com/openai/codex/releases/download/rust-v${CODEX_VERSION}/codex-x86_64-unknown-linux-musl.tar.gz" "$CODEX_SHA256"; \
    tar -xzf /tmp/codex -C /tmp; \
    install -m 0755 /tmp/codex-x86_64-unknown-linux-musl /usr/local/bin/codex; \
    fetch uv "https://github.com/astral-sh/uv/releases/download/${UV_VERSION}/uv-x86_64-unknown-linux-gnu.tar.gz" "$UV_SHA256"; \
    tar -xzf /tmp/uv -C /tmp; \
    install -m 0755 /tmp/uv-x86_64-unknown-linux-gnu/uv /tmp/uv-x86_64-unknown-linux-gnu/uvx /usr/local/bin/; \
    fetch gh "https://github.com/cli/cli/releases/download/v${GH_VERSION}/gh_${GH_VERSION}_linux_amd64.tar.gz" "$GH_SHA256"; \
    tar -xzf /tmp/gh -C /tmp; \
    install -m 0755 "/tmp/gh_${GH_VERSION}_linux_amd64/bin/gh" /usr/bin/gh; \
    fetch chrome "https://storage.googleapis.com/chrome-for-testing-public/${CHROME_VERSION}/linux64/chrome-linux64.zip" "$CHROME_SHA256"; \
    unzip -q /tmp/chrome -d /tmp; \
    mkdir -p /opt/vmm; \
    mv /tmp/chrome-linux64 /opt/vmm/chrome; \
    chmod -R go-w /opt/vmm/chrome; \
    rm -rf /tmp/*

COPY deploy/guest/agent-browser/package.json deploy/guest/agent-browser/package-lock.json /opt/vmm/agent-browser/
RUN cd /opt/vmm/agent-browser \
 && HOME=/tmp npm ci --omit=dev --ignore-scripts --no-audit --no-fund \
 && mkdir -p bin \
 && ln -sfn ../node_modules/@agent-browser/cli-linux-x64-gnu/bin/agent-browser bin/agent-browser \
 && rm -rf /tmp/* /root/.npm

COPY deploy/guest/docker-daemon.json /etc/docker/daemon.json

# The init renames ubuntu (uid 1000) to dev, which keeps its groups.
# The xpra package enables a system proxy on TCP 14500; the desktop is dev's own xpra, reached over SSH only.
RUN systemctl enable docker.socket \
 && systemctl disable docker.service containerd.service \
 && usermod -aG docker ubuntu \
 && systemctl mask xpra-server.socket xpra-server.service \
 && echo 'prefix=${HOME}/.local' > /usr/local/etc/npmrc \
 && echo 'DISABLE_AUTOUPDATER=1' >> /etc/environment

# Verifies the pinned versions without running a daemon.
RUN node --version | grep -F "$NODE_VERSION" \
 && claude --version | grep -F "$CLAUDE_VERSION" \
 && codex --version | grep -F "$CODEX_VERSION" \
 && uv --version | grep -F "$UV_VERSION" \
 && gh --version | grep -F "$GH_VERSION" \
 && docker --version | grep -F 29.10.0 \
 && xpra --version | grep -F 6.5.5 \
 && /opt/vmm/chrome/chrome --no-sandbox --version | grep -F "$CHROME_VERSION" \
 && setpriv --reuid=nobody --regid=nogroup --clear-groups env HOME=/nonexistent /opt/vmm/agent-browser/bin/agent-browser --version | grep -F 0.43.0
