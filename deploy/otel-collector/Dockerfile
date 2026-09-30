# The collector image is built FROM scratch and has no shell, so Docker cannot run a
# health check inside it. A static busybox gives it one wget to probe the collector's
# health_check extension with; nothing else changes.
FROM busybox:1.38.0-musl AS busybox

FROM otel/opentelemetry-collector-contrib:0.161.0
COPY --from=busybox /bin/busybox /busybox
