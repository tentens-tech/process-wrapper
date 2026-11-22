# The process-wrapper image with tools used by the e2e configs (curl, python3).
# The user stays non-root.
ARG IMAGE=process-wrapper:e2e
FROM ${IMAGE}
USER 0
RUN apk add --no-cache curl python3
USER 65532:65532
