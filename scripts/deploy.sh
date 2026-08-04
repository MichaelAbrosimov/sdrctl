#!/bin/sh
# Builds and deploys sdrctl to a node. Run from the repo on the dev machine:
#
#   make deploy                 # default host
#   make deploy HOST=wyse-sdr   # explicit
#
# The node gets a compiled static binary, not a git clone: it is an
# appliance with no Go toolchain, and the version string embeds the commit
# (git describe), so what runs is still traceable to source.
#
# The only interactive step is the sudo password for the install itself.
set -eu

HOST=${HOST:-wyse-sdr}
REPO=$(cd "$(dirname "$0")/.." && pwd)
cd "$REPO"

# A dirty tree produces a "-dirty" version that matches no commit — that is
# fine for testing, but say so out loud so it is never a surprise later.
if ! git diff-index --quiet HEAD -- 2>/dev/null; then
    printf '\033[33mwarning:\033[0m working tree is dirty — deploying an unversioned build\n' >&2
fi

echo "==> building linux/amd64"
make --no-print-directory build-linux

# Derive the staging path from the REMOTE home instead of hardcoding it:
# this repo has already been bitten once by an ssh alias that changed which
# user it logs in as.
echo "==> checking $HOST"
remote_id=$(ssh -o ConnectTimeout=8 "$HOST" 'echo "$(whoami) $HOME"')
remote_user=${remote_id%% *}
remote_home=${remote_id##* }
stage="$remote_home/sdrctl-deploy"
echo "    $remote_user@$HOST, staging in $stage"

echo "==> staging"
# COPYFILE_DISABLE keeps macOS from shipping ._AppleDouble junk.
COPYFILE_DISABLE=1 tar -C "$REPO" -cf - \
    --exclude '.*' \
    -s '|^bin/sdrctl-linux-amd64$|sdrctl|' \
    -s '|^systemd/||' -s '|^polkit/||' -s '|^scripts/||' \
    bin/sdrctl-linux-amd64 \
    systemd/sdrctl-agent.service systemd/rtl-tcp.service systemd/rtl-433.service \
    polkit/50-sdrctl.rules scripts/apply.sh 2>/dev/null \
  | ssh "$HOST" "rm -rf '$stage' && mkdir -p '$stage' && tar -C '$stage' -xf - && chmod 700 '$stage' && chmod +x '$stage/apply.sh'"

echo "==> applying (sudo password for $remote_user@$HOST)"
ssh -t "$HOST" "sudo '$stage/apply.sh'"

echo "==> node state"
ssh "$HOST" 'sdrctl status | grep -E "Agent|Version|rtl-sdr" || true'
