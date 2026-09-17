ARG BASE
FROM ${BASE}
ARG FORMAT
ARG PACKAGE
ARG BINARY
COPY site /repo
COPY smoke.sh /usr/local/bin/smoke
RUN sh /usr/local/bin/smoke "$FORMAT" "$PACKAGE" "$BINARY"
