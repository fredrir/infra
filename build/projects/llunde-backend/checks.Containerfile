FROM docker.io/library/eclipse-temurin@sha256:dcf835e52330939b6c9f90ecab8aafcbcaa8fbf48423db44de884cf978c10144 AS jdk25
FROM docker.io/library/eclipse-temurin@sha256:1f79c73404fb0cccf9a3459eda22892f368d994b1028d6fb1ae871c1f49749a6 AS jdk21
FROM docker.io/valkey/valkey@sha256:fea8b3e67b15729d4bb70589eb03367bab9ad1ee89c876f54327fc7c6e618571 AS valkey
FROM docker.io/library/postgres@sha256:051f7b7b3abdd564d5d1bd1e8c4b9c1b6e77087d1dd22020ede611c096a272e0 AS integration
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
