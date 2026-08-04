#!/bin/sh
# Installs a staged sdrctl update on the node. Runs as root, invoked by
# scripts/deploy.sh — not meant to be run by hand.
#
# Idempotent by construction: every managed file is compared with what is
# installed and only written when it differs, and only the services whose
# inputs actually changed are restarted. Running it twice in a row is a
# no-op, which is what makes it safe to run after every commit.
#
# Deliberately NOT managed:
#   /etc/sdrctl/config.yaml   node-local settings (device ids, MQTT, ports)
#   /etc/sdrctl/secrets.yaml  agent-only credentials, 0600 root:root
#   udev rules / BindsTo drop-in   one-time setup, see docs/install-*.md
# Deploying those would overwrite decisions that belong to the node.
#
# The SDR mode is never switched. If a mode's unit file changed while that
# mode is running, the change is installed and reported — restarting it
# would interrupt reception, and that is the operator's call.
set -eu

D=$(cd "$(dirname "$0")" && pwd)
TS=$(date +%Y%m%d-%H%M%S)
changed_units=0 changed_agent=0 changed_polkit=0
summary=""

note() { summary="${summary}  $1\n"; }

# install_if_changed <staged> <target> <mode> -> returns 0 when it wrote
install_if_changed() {
    staged=$1 target=$2 mode=$3
    [ -f "$staged" ] || return 1
    if [ -f "$target" ] && cmp -s "$staged" "$target"; then
        return 1
    fi
    if [ -f "$target" ]; then
        cp -a "$target" "$target.bak-$TS"
    fi
    install -m "$mode" -o root -g root "$staged" "$target"
    return 0
}

if install_if_changed "$D/sdrctl" /usr/local/bin/sdrctl 0755; then
    changed_agent=1
    note "binary        → $(/usr/local/bin/sdrctl version)"
fi

if install_if_changed "$D/sdrctl-agent.service" /etc/systemd/system/sdrctl-agent.service 0644; then
    changed_units=1 changed_agent=1
    note "agent unit    → updated"
fi

for u in rtl-tcp.service rtl-433.service spyserver.service; do
    [ -f "$D/$u" ] || continue
    # A mode unit is only installed if the node already has it: deploy
    # updates what exists, it does not enable new modes behind your back.
    [ -f "/etc/systemd/system/$u" ] || continue
    if install_if_changed "$D/$u" "/etc/systemd/system/$u" 0644; then
        changed_units=1
        state=$(systemctl is-active "$u" 2>/dev/null || true)
        if [ "$state" = "active" ]; then
            note "$u → updated (RUNNING — restart it yourself when convenient)"
        else
            note "$u → updated"
        fi
    fi
done

if install_if_changed "$D/50-sdrctl.rules" /etc/polkit-1/rules.d/50-sdrctl.rules 0644; then
    changed_polkit=1
    note "polkit rule   → updated"
fi

[ "$changed_units" = 1 ] && systemctl daemon-reload
[ "$changed_polkit" = 1 ] && systemctl restart polkit
if [ "$changed_agent" = 1 ]; then
    systemctl restart sdrctl-agent
    sleep 2
    systemctl is-active --quiet sdrctl-agent || {
        echo "ERROR: sdrctl-agent did not come back — see journalctl -u sdrctl-agent"
        exit 1
    }
    note "agent         → restarted, $(systemctl is-active sdrctl-agent)"
fi

if [ -z "$summary" ]; then
    echo "nothing to do — node already matches this build"
    exit 0
fi
printf "applied:\n%b" "$summary"
echo "rollback: files were backed up with suffix .bak-$TS"
