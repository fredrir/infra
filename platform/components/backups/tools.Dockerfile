FROM docker.io/library/golang:1.27.1-alpine@sha256:cf6fca6641884b8433441b2b0652976f975e1d0fdd26d177eaaf8596087f3125 AS restic
RUN apk add --no-cache git
WORKDIR /src
RUN git init -q . \
    && git remote add origin https://github.com/restic/restic.git \
    && git fetch -q --depth 1 origin 6099d4b5c4376368d42b9982c7d3e93ba13d83b1 \
    && git checkout -q FETCH_HEAD
RUN go get golang.org/x/crypto@v0.57.0 golang.org/x/net@v0.59.0 golang.org/x/text@v0.42.0 google.golang.org/grpc@v1.83.2 \
    && go mod tidy
RUN CGO_ENABLED=0 go build -trimpath -ldflags '-s -w -X main.version=0.19.1' -o /usr/local/bin/restic ./cmd/restic

FROM docker.io/library/alpine:3.24.1@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b
RUN sed -i 's|http://|https://|g' /etc/apk/repositories \
    && apk upgrade --no-cache \
    && apk add --no-cache bash ca-certificates coreutils curl jq mongodb-tools postgresql17-client sqlite tar
COPY --from=restic /usr/local/bin/restic /usr/local/bin/restic
RUN curl -fsSLo /usr/local/bin/kubectl https://dl.k8s.io/release/v1.37.0/bin/linux/amd64/kubectl \
    && echo '6129359f4e1f3848a5572ccb0b26cf28b8ca08cef38c95a765b2f64a2c961a2f  /usr/local/bin/kubectl' | sha256sum -c - \
    && chmod 755 /usr/local/bin/kubectl
ENV HOME=/tmp
USER 10001:10001
ENTRYPOINT ["/bin/bash"]
