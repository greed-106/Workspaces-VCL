# 工作区镜像:在基础镜像上内置固定版本的 code-server(浏览器版 VS Code)。
# 不预装任何 VS Code 插件:插件由用户在浏览器里自行安装,并随 /home/coder 持久化。
#
# 通过 --build-arg BASE_IMAGE=... 复用同一份构建逻辑产出多个镜像:
#   基础版   → docker.1panel.live/library/ubuntu:24.04
#   CUDA 版 → docker.1panel.live/nvidia/cuda:12.8.1-devel-ubuntu24.04(官方镜像)
#
# code-server 压缩包由 build.sh 预先下载到本目录(GitHub release 直连会超时),
# 构建过程不访问 GitHub,只访问 apt 源。
ARG BASE_IMAGE=docker.1panel.live/library/ubuntu:24.04
FROM ${BASE_IMAGE}

ARG CODE_SERVER_VERSION=4.139.1
ARG CODE_SERVER_SHA256=53029be6c5781b7bca49b815fcc9a2a3fc111813ad8c9965b2c0f0d2985a0674

USER root
SHELL ["/bin/bash", "-o", "pipefail", "-c"]

# 换成国内 apt 源,加速依赖安装(24.04 用 ubuntu.sources,旧格式用 sources.list)。
RUN set -eux; \
    for f in /etc/apt/sources.list.d/ubuntu.sources /etc/apt/sources.list; do \
      if [ -f "$f" ]; then \
        sed -i 's|http://archive.ubuntu.com/ubuntu|http://cn.archive.ubuntu.com/ubuntu|g; \
                s|http://security.ubuntu.com/ubuntu|http://cn.archive.ubuntu.com/ubuntu|g' "$f"; \
      fi; \
    done

# code-server(VS Code)运行所需的图形库 + 基本开发工具。
RUN set -eux; \
    apt-get update; \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      ca-certificates curl wget git sudo less nano vim-tiny jq unzip zip procps \
      locales tzdata openssh-client \
      libnss3 libatk1.0-0t64 libatk-bridge2.0-0t64 libcups2t64 libdrm2 libxkbcommon0 \
      libxcomposite1 libxdamage1 libxfixes3 libxrandr2 libgbm1 libpango-1.0-0 \
      libcairo2 libasound2t64 libxshmfence1 libsecret-1-0; \
    rm -rf /var/lib/apt/lists/*

# 统一运行身份:coder(uid/gid 1000)。官方 CUDA 镜像里 uid 1000 是 ubuntu 用户,
# 这里改名并把家目录迁到 /home/coder,保证两类镜像对模板来说完全一致。
RUN set -eux; \
    if getent passwd 1000 >/dev/null; then \
      old_user="$(getent passwd 1000 | cut -d: -f1)"; \
      old_group="$(id -gn "${old_user}")"; \
      if [ "${old_user}" != "coder" ]; then \
        [ "${old_group}" != "coder" ] && groupmod -n coder "${old_group}" || true; \
        usermod -l coder -d /home/coder -m "${old_user}"; \
      fi; \
    else \
      groupadd -g 1000 coder; \
      useradd -u 1000 -g 1000 -m -d /home/coder -s /bin/bash coder; \
    fi; \
    echo 'coder ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/coder; \
    chmod 0440 /etc/sudoers.d/coder; \
    mkdir -p /home/coder; \
    chown -R 1000:1000 /home/coder; \
    id coder

COPY code-server.tar.gz /tmp/code-server.tar.gz

RUN set -eux; \
    echo "${CODE_SERVER_SHA256}  /tmp/code-server.tar.gz" | sha256sum -c -; \
    tar -xzf /tmp/code-server.tar.gz -C /usr/local/lib; \
    mv "/usr/local/lib/code-server-${CODE_SERVER_VERSION}-linux-amd64" /usr/local/lib/code-server; \
    ln -sf /usr/local/lib/code-server/bin/code-server /usr/local/bin/code-server; \
    rm -f /tmp/code-server.tar.gz; \
    code-server --version

ENV HOME=/home/coder \
    SHELL=/bin/bash \
    LANG=C.UTF-8
WORKDIR /home/coder
USER coder

# 由模板里的 coder_script 启动(只监听 localhost,经 Coder 隧道访问):
#   code-server --bind-addr 127.0.0.1:13337 --auth none /home/coder
EXPOSE 13337
