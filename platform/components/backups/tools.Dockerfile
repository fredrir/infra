FROM docker.io/library/golang:1.27.2-alpine@sha256:f92b6ef800e499660581efdabdf25d9d817a9d124eaf900924f0504e7e27e12d AS rclone
RUN apk add --no-cache git
WORKDIR /src
RUN git init -q . \
    && git remote add origin https://github.com/rclone/rclone.git \
    && git fetch -q --depth 1 origin tag v1.75.1 \
    && git checkout -q v1.75.1 \
    && test "$(git rev-parse HEAD)" = 687d264b689b8c49a67e2e52a8a5e0caa01c04ce
RUN go get google.golang.org/grpc@v1.84.0-dev.0.20260825144003-d5a41119e0e3 golang.org/x/net@v0.61.0 \
    && go mod tidy
RUN CGO_ENABLED=0 go build -trimpath -ldflags '-s -w -X github.com/rclone/rclone/fs.Version=v1.75.1' -o /usr/local/bin/rclone .

FROM docker.io/library/golang:1.27.2-alpine@sha256:f92b6ef800e499660581efdabdf25d9d817a9d124eaf900924f0504e7e27e12d AS restic
RUN apk add --no-cache git
WORKDIR /src
RUN git init -q . \
    && git remote add origin https://github.com/restic/restic.git \
    && git fetch -q --depth 1 origin 6099d4b5c4376368d42b9982c7d3e93ba13d83b1 \
    && git checkout -q FETCH_HEAD
RUN go get golang.org/x/crypto@v0.58.0 golang.org/x/net@v0.61.0 golang.org/x/text@v0.43.0 google.golang.org/grpc@v1.83.2 \
    && go mod tidy
RUN CGO_ENABLED=0 go build -trimpath -ldflags '-s -w -X main.version=0.19.1' -o /usr/local/bin/restic ./cmd/restic

FROM docker.io/library/golang:1.27.2-alpine@sha256:f92b6ef800e499660581efdabdf25d9d817a9d124eaf900924f0504e7e27e12d AS kubectl
RUN apk add --no-cache git
WORKDIR /src
RUN git init -q . \
    && git remote add origin https://github.com/kubernetes/kubernetes.git \
    && git fetch -q --depth 1 origin tag v1.37.1 \
    && git checkout -q v1.37.1 \
    && test "$(git rev-parse HEAD)" = f78e722310e50bcaca9276be22276d9e91d91308
ENV GOWORK=off
RUN go get golang.org/x/net@v0.61.0
RUN CGO_ENABLED=0 go build -mod=readonly -trimpath -ldflags '-s -w -X k8s.io/client-go/pkg/version.gitVersion=v1.37.1 -X k8s.io/component-base/version.gitVersion=v1.37.1' -o /usr/local/bin/kubectl ./cmd/kubectl

FROM docker.io/library/alpine:3.24.1@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b
RUN sed -i 's|http://|https://|g' /etc/apk/repositories \
    && apk upgrade --no-cache \
    && apk add --no-cache bash ca-certificates coreutils curl jq mongodb-tools postgresql17-client sqlite tar
COPY --from=restic /usr/local/bin/restic /usr/local/bin/restic
COPY --from=rclone /usr/local/bin/rclone /usr/local/bin/rclone
COPY --from=kubectl /usr/local/bin/kubectl /usr/local/bin/kubectl
COPY .infra-artifacts/infra /usr/local/bin/infra
COPY platform/components/object-store-trust/ca.crt /usr/local/share/object-store/ca.crt
ENV HOME=/tmp
USER 10001:10001
ENTRYPOINT ["/usr/local/bin/infra"]
