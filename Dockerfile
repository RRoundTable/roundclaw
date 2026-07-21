# roundclaw's own binaries. Not the agent image — that is container/Dockerfile.
FROM golang:1.26-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# CGO_ENABLED=0 because the SQLite driver is pure Go; the result runs on a
# minimal base with no libc to match.
RUN CGO_ENABLED=0 go build -trimpath -o /out/worker  ./cmd/worker && \
    CGO_ENABLED=0 go build -trimpath -o /out/gateway ./cmd/gateway

FROM alpine:3.20

# The worker shells out to the container runtime to start each agent turn, so
# it needs the client. It talks to the host's daemon over the mounted socket —
# agent containers are siblings of the worker, not children of it.
#
# git is here for the worker too: a conversation's workspace is a `git worktree`
# of the agent's repository, and that is created by the worker on the host
# filesystem, not inside the agent container.
RUN apk add --no-cache docker-cli git ca-certificates tzdata

COPY --from=build /out/worker  /usr/local/bin/worker
COPY --from=build /out/gateway /usr/local/bin/gateway

# Compose overrides this with the host uid so the workspace stays writable by
# both the containers and the person debugging them.
USER 1000:1000

ENTRYPOINT ["/usr/local/bin/worker"]
