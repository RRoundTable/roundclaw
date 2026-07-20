# A fake agent image for roundclaw's end-to-end tests.
#
# Identical in shape to container/Dockerfile — same paths, same user, same
# working directory — but `claude` is a shell script that emits scripted
# stream-json. That keeps the tested code path identical to production while
# removing the Anthropic credential and the token spend.
FROM alpine:3.20

RUN adduser -D -u 1000 node \
    && mkdir -p /workspace /home/node/.claude \
    && chown -R node:node /workspace /home/node/.claude

COPY claude /usr/local/bin/claude
RUN chmod 0755 /usr/local/bin/claude

USER node
WORKDIR /workspace

ENTRYPOINT []
