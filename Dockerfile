# Build stage. CGO is off so the result runs on a minimal base and on the ARM
# free-tier VMs this is meant to be deployed to.
#
# The tag must satisfy the `go` directive in go.mod (currently 1.26.0). The
# official Go images set GOTOOLCHAIN=local, so an older base will NOT silently
# download a newer toolchain — it just fails.
FROM golang:1.27-alpine AS build

WORKDIR /src

# Dependencies first, so edits to source do not invalidate the module cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/notifco ./cmd/notifco

# Runtime stage.
FROM alpine:3.21

# ca-certificates for TLS to the UW API and Discord; tzdata because term dates
# are reasoned about in Canada/Eastern.
RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -u 10001 notifco

COPY --from=build /out/notifco /usr/local/bin/notifco

USER notifco
EXPOSE 9090

ENTRYPOINT ["/usr/local/bin/notifco"]
CMD ["all"]
