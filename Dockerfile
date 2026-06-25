# Multi-stage build for the sluice gateway (CON-005, CARD-011).
#
# Stage 1 compiles a static binary; stage 2 is a distroless-style scratch image
# carrying only the binary + CA certs + the SQL migrations, so the final image
# is tiny and has no shell/package surface.

# ---- build stage ----------------------------------------------------------
FROM golang:1.25-alpine AS build

WORKDIR /src

# Cache module downloads across builds: copy manifests first.
COPY go.mod go.sum ./
RUN go mod download

# Copy the source and build a static binary (CGO disabled).
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath -ldflags="-s -w" \
    -o /out/gateway ./cmd/gateway

# ---- runtime stage --------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot AS runtime

# CA certs come from the distroless base. Carry the gateway binary and the SQL
# migrations (the compose `migrate` step applies them; the image bundles them so
# the artifact is self-describing).
COPY --from=build /out/gateway /usr/local/bin/gateway
COPY migrations /migrations

EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/gateway"]
