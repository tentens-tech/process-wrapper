# The process-wrapper image with tools used by the e2e tests: curl, python3
# (examples/probes.yaml, examples/memory-limit.yaml) and the fake services (fake/).
# The user stays non-root.
ARG IMAGE=process-wrapper:e2e

FROM golang:1.27-alpine AS fake
WORKDIR /src
COPY fake/main.go .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /e2e-fake main.go

FROM ${IMAGE}
USER 0
RUN apk add --no-cache curl python3
COPY --from=fake /e2e-fake /usr/local/bin/e2e-fake
USER 1010:1010
