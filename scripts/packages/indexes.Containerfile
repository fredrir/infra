FROM public.ecr.aws/docker/library/buildpack-deps:trixie-curl@sha256:04907bdd423bdac4bdf6b7ce84562eea9f75916b55be1fa279b271879f116802 AS apt
RUN sed -i 's|^URIs: http://|URIs: https://|' /etc/apt/sources.list.d/debian.sources \
    && apt-get update -o APT::Update::Error-Mode=any \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends apt-utils gnupg \
    && rm -rf /var/lib/apt/lists/*
COPY site/deb /out/deb
COPY index/apt.sh /usr/local/bin/index
RUN --mount=type=secret,id=gpg,required=true bash /usr/local/bin/index /out/deb

FROM quay.io/fedora/fedora:44@sha256:e7d398c0a67572b33a43260102f9135ab79b6b8d95efca91faa5179d83b2cdf0 AS rpm
RUN dnf install -y --setopt=install_weak_deps=False createrepo_c gnupg2 \
    && dnf clean all
COPY site/rpm /out/rpm
COPY index/rpm.sh /usr/local/bin/index
RUN --mount=type=secret,id=gpg,required=true bash /usr/local/bin/index /out/rpm

FROM public.ecr.aws/docker/library/alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b AS apk
RUN sed -i 's|http://|https://|g' /etc/apk/repositories \
    && apk add --no-cache abuild bash openssl
COPY site/apk /out/apk
COPY index/apk.sh /usr/local/bin/index
RUN --mount=type=secret,id=apk,required=true bash /usr/local/bin/index /out/apk

FROM scratch
COPY --from=apt /out/deb /deb
COPY --from=rpm /out/rpm /rpm
COPY --from=apk /out/apk /apk
