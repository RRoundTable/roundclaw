# The agent container holds the `claude` CLI and nothing of roundclaw's.
#
# That is the whole point of dropping the Agent SDK: with no in-process
# callbacks to host, there is no reason to ship a runner process into the
# image. roundclaw drives this container purely through argv and stream-json on
# stdout.
FROM node:22-slim

# python3 is here for agents that run Python tools — e.g. the migrated stock
# bot, which is stdlib-only, so the interpreter alone is enough (no pip).
# curl lets an agent granted an API tool (the admin agent, or a team member
# delegating) call the roundclaw HTTP API directly, rather than improvising with
# python's urllib.
RUN apt-get update && apt-get install -y --no-install-recommends \
        git ca-certificates ripgrep python3 curl \
    && rm -rf /var/lib/apt/lists/*

RUN npm install -g @anthropic-ai/claude-code

# /workspace is the agent's working directory and must be the cwd for every
# turn: `claude --resume` scopes its session lookup to the current directory,
# so running a turn from anywhere else silently fails to find the session.
#
# /home/node/.claude holds session transcripts and is bind-mounted from the
# host. The container is --rm, so without that mount --resume would have
# nothing to resume.
RUN mkdir -p /workspace /home/node/.claude && chown -R node:node /workspace /home/node/.claude

USER node
WORKDIR /workspace

ENTRYPOINT []
