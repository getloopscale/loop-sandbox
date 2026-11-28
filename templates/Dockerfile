# The images behind the built-in templates that LoopSandbox builds itself.
#
# A sandbox runs an image as nobody (65534), on a read-only root filesystem, with
# HOME=/workspace. Whatever a template needs is therefore installed here, under
# /usr/local, at build time; nothing may count on writing outside /workspace or /tmp
# when the sandbox runs.
#
# Targets: base, agent (one coding agent CLI from npm) and desktop.

FROM node:22-bookworm-slim@sha256:be979e5523c76fa4d5ca4ebd3099078f220aa47ec39c48ad5b5df9314debd6eb AS base
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates curl git jq less openssh-client procps python3 python3-pip python3-venv ripgrep unzip \
 && rm -rf /var/lib/apt/lists/* \
 && ln -s /usr/bin/python3 /usr/local/bin/python
# pip installs into the home of the sandbox user without asking for a virtual environment.
ENV PIP_BREAK_SYSTEM_PACKAGES=1 \
    PIP_USER=1 \
    PATH=/workspace/.local/bin:$PATH \
    NPM_CONFIG_UPDATE_NOTIFIER=false
WORKDIR /workspace

# One coding agent CLI, at an exact version, installed for every user.
FROM base AS agent
ARG PACKAGE
ARG VERSION
RUN test -n "$PACKAGE" && test -n "$VERSION" \
 && npm install --global "${PACKAGE}@${VERSION}" \
 && npm cache clean --force

# A graphical desktop on a virtual display. start-desktop brings it up on DISPLAY=:0;
# a screenshot (scrot) and input (xdotool) then work from a command.
FROM base AS desktop
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      dbus-x11 scrot x11-utils x11-xserver-utils xdotool xfce4 xfce4-terminal xvfb \
 && rm -rf /var/lib/apt/lists/*
COPY start-desktop /usr/local/bin/start-desktop
RUN chmod 0755 /usr/local/bin/start-desktop
ENV DISPLAY=:0
