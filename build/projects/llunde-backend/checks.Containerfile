FROM docker.io/library/eclipse-temurin@sha256:8c0a84ea11c8f6ed52600fc19f1040121f2a162998e9f50a5faebbbad9172dcc AS jdk25
FROM docker.io/library/eclipse-temurin@sha256:8c0a84ea11c8f6ed52600fc19f1040121f2a162998e9f50a5faebbbad9172dcc AS jdk21
FROM docker.io/valkey/valkey@sha256:fea8b3e67b15729d4bb70589eb03367bab9ad1ee89c876f54327fc7c6e618571 AS valkey
FROM docker.io/library/postgres@sha256:5a5a84b19854a9ffaa54082c166ff4ec27473a361e496e5ea167f298f2da9722 AS integration
COPY --from=jdk25 /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
RUN sed -i 's|http://|https://|g' /etc/apt/sources.list.d/debian.sources /etc/apt/sources.list.d/pgdg.list \
    && apt-get update && apt-get install -y --no-install-recommends ca-certificates git libstdc++6 && rm -rf /var/lib/apt/lists/*
COPY --from=jdk25 /opt/java/openjdk /opt/java/jdk25
COPY --from=jdk21 /opt/java/openjdk /opt/java/jdk21
COPY --from=valkey /usr/local/bin/valkey-server /usr/local/bin/valkey-server
ENV JAVA_HOME=/opt/java/jdk25 PATH=/opt/java/jdk25/bin:$PATH
WORKDIR /src
COPY . .
RUN --mount=type=bind,source=.infra-artifacts/infra,target=/usr/local/bin/infra \
    --mount=type=cache,id=llunde-gradle,target=/root/.gradle,sharing=locked \
    --mount=type=cache,id=llunde-gradle-build,target=/src/build,sharing=locked \
    --mount=type=cache,id=llunde-gradle-project,target=/src/.gradle,sharing=locked \
    infra ci project check --project llunde-backend --suite integration
