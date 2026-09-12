FROM docker.io/restic/restic:0.19.1@sha256:136600b6ff6843d61d355f7f71f460a166429f35de6fd11b568fece3c9a4d510 AS restic
FROM docker.io/library/alpine@sha256:1beb0dc0a51de7ff38e3b5274078a2e0b81113ba5c7535e1a03d5913a5edbda3
RUN apk add --no-cache bash ca-certificates coreutils curl jq mongodb-tools postgresql17-client sqlite tar
COPY --from=restic /usr/bin/restic /usr/local/bin/restic
RUN curl -fsSLo /usr/local/bin/kubectl https://dl.k8s.io/release/v1.36.3/bin/linux/amd64/kubectl \
    && echo 'ebbd080e7c2e275093b55915722043257eb24004363e20acb3c4d71919f88336  /usr/local/bin/kubectl' | sha256sum -c - \
    && chmod 755 /usr/local/bin/kubectl
ENV HOME=/tmp
USER 10001:10001
ENTRYPOINT ["/bin/bash"]
