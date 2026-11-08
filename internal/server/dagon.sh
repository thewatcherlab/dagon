#!/bin/bash
# dagon server: runs on the VPS as root.
#
#   install <app> <apex> <image> <work>   Called by `dagon setup`. Installs Docker,
#                                         Caddy, and sops, and registers one app.
#   serve <app> deploy <slot> <sha> [rollback]
#                                         Called by GitHub Actions through the SSH
#   serve <app> remove pr-<n>             entry wrapper of the app's deploy key.
#   serve <app> previous <slot>           Prints the commit to roll back to.
#   sweep                                 Called hourly by dagon-sweep.timer. Removes
#                                         previews not deployed in the last 24 hours.
set -u
set -o pipefail

ENVIRONMENTS="production staging preview"
SBIN=/usr/local/sbin/dagon
SOPS_VERSION=3.13.3
SOPS_SHA256_AMD64=e5bec3346a873ae91d871550f3e698c1aad962aff462a080e40f25fde17fef6b
SOPS_SHA256_ARM64=53b0abacd38ef1b12a66d6c100956691b9cefce018d91f81e73ddf7438b94d77
PORT_FIRST=20000
PORT_LAST=29999
HISTORY_KEEP=20
PREVIEW_TTL_MINUTES=${DAGON_PREVIEW_TTL_MINUTES:-1440}

# Server paths. Tests override them; sudo clears these variables on the VPS.
etc_dir() { printf '%s\n' "${DAGON_ETC:-/etc/dagon}"; }
state_dir() { printf '%s\n' "${DAGON_STATE:-/var/lib/dagon}"; }
caddy_dir() { printf '%s\n' "${DAGON_CADDY:-/etc/caddy}"; }
run_dir() { printf '%s\n' "${DAGON_RUN:-/run/dagon}"; }

say() { printf 'dagon: %s\n' "$1" >&2; }
die() { say "$1"; exit "${2:-1}"; }
bad_args() { die "bad arguments" 2; }

is_app() { [[ "$1" =~ ^[a-z][a-z0-9-]{0,38}[a-z0-9]$ ]]; }
is_preview() { [[ "$1" =~ ^pr-[1-9][0-9]{0,5}$ ]]; }
is_slot() { [[ "$1" == "staging" || "$1" == "production" ]] || is_preview "$1"; }
is_sha() { [[ "$1" =~ ^[0-9a-f]{40}$ ]]; }
is_apex() { [[ "$1" =~ ^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$ ]]; }

env_of() {
  if is_preview "$1"; then
    printf '%s\n' preview
  else
    printf '%s\n' "$1"
  fi
}

hostname_of() {
  if [[ "$1" == "production" ]]; then
    printf '%s\n' "$2"
  else
    printf '%s\n' "$1.$2"
  fi
}

read_line() {
  [[ -f "$1" ]] || return 1
  head -n 1 "$1" | tr -d '\r\n'
}

hash_stdin() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum | cut -d' ' -f1
  else
    shasum -a 256 | cut -d' ' -f1
  fi
}

# ---------------------------------------------------------------------------
# serve: runs on the VPS as root. The deploy user reaches it only through the
# entry wrapper, which passes the app name from authorized_keys.

serve() {
  app=${1:-}
  is_app "$app" || bad_args
  shift
  conf="$(etc_dir)/apps/$app"
  [[ -d "$conf" ]] || die "unknown app" 2
  apex=$(read_line "$conf/apex") || die "app has no apex" 2
  image_repo=$(read_line "$conf/image") || die "app has no image" 2
  case "${1:-}" in
    deploy)
      [[ $# -eq 3 || ( $# -eq 4 && "$4" == "rollback" ) ]] || bad_args
      serve_deploy "$2" "$3" "${4:-deploy}"
      ;;
    remove)
      [[ $# -eq 2 ]] || bad_args
      serve_remove "$2"
      ;;
    previous)
      [[ $# -eq 2 ]] || bad_args
      serve_previous "$2"
      ;;
    *)
      bad_args
      ;;
  esac
}

emit() {
  printf '%s %s %s https://%s\n' "$1" "$slot" "$sha" "$(hostname_of "$slot" "$apex")"
}

reject() {
  [[ -n "${1:-}" ]] && say "$1"
  emit rejected
  exit 1
}

lock_slot() {
  mkdir -p "$(state_dir)/$app/$slot" || return 1
  exec 9>"$(state_dir)/$app/$slot/lock" || return 1
  flock 9
}

# Slots of one app share its images. Deploys hold a shared lock, so a prune
# never deletes an image that another slot has pulled but not started yet.
lock_images() {
  mkdir -p "$(state_dir)/$app" || return 1
  exec 7>"$(state_dir)/$app/images.lock" || return 1
  flock -s 7
}

prune_images() {
  flock -x -n 7 2>/dev/null || return 0
  docker image ls "$image_repo" --format '{{.ID}}' 2>/dev/null | sort -u | while read -r id; do
    docker image rm "$id" >/dev/null 2>&1 || true
  done
}

port_in_use() {
  ss -Hltn "sport = :$1" 2>/dev/null | grep -q .
}

port_for() {
  pdir="$(etc_dir)/ports"
  mkdir -p "$pdir" || return 1
  file="$pdir/$name"
  if [[ -f "$file" ]]; then
    read_line "$file"
    return
  fi
  exec 8>"$(etc_dir)/ports.lock" || return 1
  flock 8 || return 1
  used=$(cat "$pdir"/* 2>/dev/null || true)
  for ((p = PORT_FIRST; p <= PORT_LAST; p++)); do
    printf '%s\n' "$used" | grep -x -q "$p" && continue
    port_in_use "$p" && continue
    printf '%s\n' "$p" > "$file" || return 1
    printf '%s\n' "$p"
    return 0
  done
  return 1
}

pull_image() {
  cfg="$work/docker"
  mkdir -p "$cfg"
  if [[ -n "$registry_token" ]]; then
    if ! printf '%s' "$registry_token" | docker --config "$cfg" login "${image_repo%%/*}" -u "$registry_user" --password-stdin >/dev/null 2>&1; then
      say "registry login failed"
      return 1
    fi
  fi
  if ! docker --config "$cfg" pull -q "$ref" >/dev/null; then
    say "image pull failed for $ref"
    return 1
  fi
  rm -rf "$cfg"
}

decrypt_env() {
  : > "$work/run.env"
  chmod 600 "$work/run.env"
  [[ -s "$work/secrets.enc.env" ]] || return 0
  env=$(env_of "$slot")
  key="$(etc_dir)/apps/$app/age/$env.key"
  [[ -f "$key" ]] || { say "missing age key for $env"; return 1; }
  if ! SOPS_AGE_KEY_FILE="$key" sops --decrypt --input-type dotenv --output-type dotenv "$work/secrets.enc.env" > "$work/run.env" 2>/dev/null; then
    say "secrets for $env could not be decrypted with the $env key"
    return 1
  fi
}

start_container() {
  docker run -d --name "$name" \
    --restart unless-stopped --init \
    --cap-drop ALL --security-opt no-new-privileges \
    --log-opt max-size=10m --log-opt max-file=3 \
    --label "dagon.app=$app" --label "dagon.slot=$slot" --label "dagon.sha=$sha" \
    -p "127.0.0.1:$port:3000" \
    --env-file "$work/run.env" \
    "$ref" >/dev/null
}

healthy() {
  curl -fsS -o /dev/null --retry 30 --retry-delay 1 --retry-all-errors "http://127.0.0.1:$port/"
}

# Stops the running container, starts the new one, and checks it. On failure
# the new container is kept, stopped, as <name>_failed and the old one returns.
switch_container() {
  prev="${name}_prev"
  failed="${name}_failed"
  docker rm -f "$prev" "$failed" >/dev/null 2>&1 || true
  had_old=0
  if docker inspect "$name" >/dev/null 2>&1; then
    docker stop -t 10 "$name" >/dev/null 2>&1 || true
    docker rename "$name" "$prev" || return 1
    had_old=1
  fi
  if start_container && healthy; then
    docker rm -f "$prev" >/dev/null 2>&1 || true
    return 0
  fi
  say "the new container failed its health check; on the VPS run: docker logs $failed"
  docker stop -t 5 "$name" >/dev/null 2>&1 || true
  docker rename "$name" "$failed" >/dev/null 2>&1 || docker rm -f "$name" >/dev/null 2>&1 || true
  if [[ "$had_old" == "1" ]]; then
    { docker rename "$prev" "$name" && docker start "$name" >/dev/null; } || say "the old container did not restart"
  fi
  return 1
}

reload_caddy() {
  systemctl reload caddy >&2
}

route() {
  sites="$(caddy_dir)/sites"
  mkdir -p "$sites" || return 1
  file="$sites/$name.caddy"
  printf '%s {\n  reverse_proxy 127.0.0.1:%s\n}\n' "$(hostname_of "$slot" "$apex")" "$port" > "$file.new" || return 1
  if [[ -f "$file" ]] && cmp -s "$file.new" "$file"; then
    rm -f "$file.new"
    return 0
  fi
  mv -f "$file.new" "$file" || return 1
  if ! reload_caddy; then
    say "caddy reload failed"
    return 1
  fi
}

# stdin: "<registry user> <registry token>" on the first line, then the
# encrypted dotenv file for this slot's environment, which may be empty.
serve_deploy() {
  slot=$1
  sha=$2
  kind=$3
  { is_slot "$slot" && is_sha "$sha"; } || bad_args
  name="${app}_${slot}"
  ref="$image_repo:$sha"
  lock_slot || reject "lock failed"
  lock_images || reject "lock failed"
  mkdir -p -m 700 "$(run_dir)" || reject "no run directory"
  work=$(mktemp -d "$(run_dir)/deploy.XXXXXX") || reject "no work directory"
  trap 'rm -rf "$work"' EXIT
  registry_user=""
  registry_token=""
  IFS=' ' read -r registry_user registry_token || true
  cat > "$work/secrets.enc.env"
  pull_image || reject
  decrypt_env || reject
  image_id=$(docker image inspect --format '{{.Id}}' "$ref" 2>/dev/null) || reject "image inspect failed"
  want="$image_id $(hash_stdin < "$work/run.env")"
  have=$(read_line "$(state_dir)/$app/$slot/state" || true)
  running=$(docker inspect --format '{{.State.Running}} {{.Image}}' "$name" 2>/dev/null || true)
  port=$(port_for) || reject "no free port"
  if [[ "$have" == "$want" && "$running" == "true $image_id" ]]; then
    route || reject
    touch "$(state_dir)/$app/$slot/deployed"
    emit unchanged
    exit 0
  fi
  switch_container || reject
  printf '%s\n' "$want" > "$(state_dir)/$app/$slot/state"
  record_history
  touch "$(state_dir)/$app/$slot/deployed"
  route || reject
  prune_images
  emit published
}

# history has one "<sha> <unix time> <deploy|rollback>" line per published
# deploy, newest last. The last line is the commit that serves now.
record_history() {
  file="$(state_dir)/$app/$slot/history"
  { tail -n $((HISTORY_KEEP - 1)) "$file" 2>/dev/null; printf '%s %s %s\n' "$sha" "$(date +%s)" "$kind"; } > "$file.new" &&
    mv -f "$file.new" "$file"
}

# Rollback targets come from normal deploys only, so each rollback steps one
# release further back instead of returning to the release it replaced.
serve_previous() {
  slot=$1
  [[ "$slot" == "staging" || "$slot" == "production" ]] || bad_args
  file="$(state_dir)/$app/$slot/history"
  current=$(tail -n 1 "$file" 2>/dev/null | cut -d' ' -f1)
  [[ -n "$current" ]] || die "$slot has no deploy to roll back"
  previous=$(awk -v cur="$current" '
    $3 != "rollback" { n++; s[n] = $1 }
    END {
      for (i = n; i > 0; i--) if (s[i] == cur) break
      if (i == 0) { print "unknown"; exit }
      for (j = i - 1; j > 0; j--) if (s[j] != cur) { print s[j]; exit }
    }' "$file")
  [[ "$previous" != "unknown" ]] || die "$slot serves a commit outside its deploy history; roll back to a named commit"
  [[ -n "$previous" ]] || die "$slot has no earlier release to roll back to"
  printf '%s\n' "$previous"
}

serve_remove() {
  slot=$1
  is_preview "$slot" || bad_args
  name="${app}_${slot}"
  host=$(hostname_of "$slot" "$apex")
  lock_slot || { say "lock failed"; printf 'rejected %s https://%s\n' "$slot" "$host"; exit 1; }
  lock_images || true
  docker rm -f "$name" "${name}_prev" "${name}_failed" >/dev/null 2>&1 || true
  file="$(caddy_dir)/sites/$name.caddy"
  if [[ -f "$file" ]]; then
    rm -f "$file"
    if ! reload_caddy; then
      say "caddy reload failed"
      printf 'rejected %s https://%s\n' "$slot" "$host"
      exit 1
    fi
  fi
  rm -f "$(etc_dir)/ports/$name"
  rm -rf "$(state_dir)/$app/$slot"
  prune_images
  printf 'removed %s https://%s\n' "$slot" "$host"
}


# ---------------------------------------------------------------------------
# install: prepares the VPS and registers one app. Safe to run again.

render_entry() {
  cat <<END
#!/bin/bash
# Forced command for a GitHub Actions key. The app name comes from
# authorized_keys; the rest comes from the SSH command line.
set -u
set -f
[[ \$# -eq 1 ]] || exit 2
exec sudo -n $1 serve "\$1" \${SSH_ORIGINAL_COMMAND:-}
END
}

render_sudoers() {
  printf 'deploy ALL=(root) NOPASSWD: %s serve *\n' "$SBIN"
}

render_sweep_units() {
  cat > /etc/systemd/system/dagon-sweep.service <<END
[Unit]
Description=Remove dagon previews not deployed in the last 24 hours

[Service]
Type=oneshot
ExecStart=$SBIN sweep
END
  cat > /etc/systemd/system/dagon-sweep.timer <<END
[Unit]
Description=Run dagon sweep every hour

[Timer]
OnCalendar=hourly
RandomizedDelaySec=5m
Persistent=true

[Install]
WantedBy=timers.target
END
}

render_caddyfile() {
  printf '%s\n' "# Written by dagon. Each app and slot has a file in sites/." "import sites/*.caddy"
}

install_packages() {
  export DEBIAN_FRONTEND=noninteractive
  need=""
  command -v docker >/dev/null 2>&1 || need="$need docker.io"
  command -v age-keygen >/dev/null 2>&1 || need="$need age"
  command -v curl >/dev/null 2>&1 || need="$need curl"
  command -v gpg >/dev/null 2>&1 || need="$need gnupg"
  dpkg -s ca-certificates >/dev/null 2>&1 || need="$need ca-certificates"
  if [[ -n "$need" ]]; then
    apt-get update >&2
    # shellcheck disable=SC2086
    apt-get install -y $need >&2
  fi
  systemctl enable --now docker >&2

  if ! command -v caddy >/dev/null 2>&1; then
    curl -fsSL 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | gpg --dearmor --yes -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
    curl -fsSL 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' > /etc/apt/sources.list.d/caddy-stable.list
    apt-get update >&2
    apt-get install -y caddy >&2
  fi

  if ! sops --version 2>/dev/null | grep -q "sops $SOPS_VERSION"; then
    case "$(uname -m)" in
      x86_64) arch=amd64; sum=$SOPS_SHA256_AMD64 ;;
      aarch64|arm64) arch=arm64; sum=$SOPS_SHA256_ARM64 ;;
      *) die "unsupported architecture $(uname -m)" ;;
    esac
    curl -fsSL -o "$work/sops" "https://github.com/getsops/sops/releases/download/v$SOPS_VERSION/sops-v$SOPS_VERSION.linux.$arch"
    echo "$sum  $work/sops" | sha256sum -c - >&2
    install -o root -g root -m 755 "$work/sops" /usr/local/bin/sops
  fi
}

install_cmd() {
  [[ $# -eq 4 ]] || bad_args
  app=$1 apex=$2 image=$3 work=$4
  is_app "$app" || bad_args
  is_apex "$apex" || bad_args
  [[ "$image" =~ ^ghcr\.io/[a-z0-9._/-]+$ ]] || bad_args
  [[ -f "$work/actions.pub" ]] || die "missing $work/actions.pub"
  [[ "$(id -u)" == "0" ]] || die "install must run as root"
  set -e

  install_packages

  if ! id deploy >/dev/null 2>&1; then
    useradd --system --create-home --shell /bin/bash deploy
  fi
  if id -nG deploy | tr ' ' '\n' | grep -x -q docker; then
    gpasswd -d deploy docker >&2
  fi

  render_entry "$SBIN" > "$work/entry"
  render_sudoers > "$work/sudoers"
  render_caddyfile > "$work/Caddyfile"
  install -o root -g root -m 755 "$0" "$SBIN"
  install -d -o root -g root -m 755 /usr/local/lib/dagon
  install -o root -g root -m 755 "$work/entry" /usr/local/lib/dagon/entry
  visudo -cf "$work/sudoers" >&2
  install -o root -g root -m 440 "$work/sudoers" /etc/sudoers.d/dagon

  # Earlier versions of this tool were called sanbox. Move their state over.
  if [[ -d /etc/sanbox && ! -e /etc/dagon ]]; then mv /etc/sanbox /etc/dagon; fi
  if [[ -d /var/lib/sanbox && ! -e /var/lib/dagon ]]; then mv /var/lib/sanbox /var/lib/dagon; fi
  install -d -o root -g root -m 755 /etc/dagon /etc/dagon/apps /etc/dagon/ports
  install -d -o root -g root -m 700 /var/lib/dagon
  for other in /etc/dagon/apps/*; do
    [[ -d "$other" && "$(basename "$other")" != "$app" ]] || continue
    if [[ "$(head -n 1 "$other/apex" 2>/dev/null)" == "$apex" ]]; then
      die "$apex already belongs to app $(basename "$other")"
    fi
  done
  conf=/etc/dagon/apps/$app
  install -d -o root -g root -m 755 "$conf"
  install -d -o root -g root -m 700 "$conf/age"
  printf '%s\n' "$apex" > "$conf/apex"
  printf '%s\n' "$image" > "$conf/image"
  for env in $ENVIRONMENTS; do
    key="$conf/age/$env.key"
    [[ -f "$key" ]] || age-keygen -o "$key" 2>/dev/null
    chmod 600 "$key"
    printf 'AGE_%s=%s\n' "$env" "$(age-keygen -y "$key")"
  done

  install -d -m 755 /etc/caddy/sites
  touch /etc/caddy/sites/_none.caddy
  if ! cmp -s "$work/Caddyfile" /etc/caddy/Caddyfile 2>/dev/null; then
    if [[ -f /etc/caddy/Caddyfile ]]; then
      cp /etc/caddy/Caddyfile "/etc/caddy/Caddyfile.bak.$(date +%Y%m%d%H%M%S)"
    fi
    install -m 644 "$work/Caddyfile" /etc/caddy/Caddyfile
  fi
  caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile >/dev/null 2>&1 || die "caddy config is not valid"
  systemctl enable caddy >&2
  systemctl reload caddy >&2 || systemctl restart caddy >&2

  render_sweep_units
  systemctl daemon-reload >&2
  systemctl enable --now dagon-sweep.timer >&2

  home=$(getent passwd deploy | cut -d: -f6)
  install -d -o deploy -g deploy -m 700 "$home/.ssh"
  auth="$home/.ssh/authorized_keys"
  touch "$auth"
  {
    grep -v -F -e "command=\"/usr/local/lib/dagon/entry $app\"" -e "command=\"/usr/local/lib/sanbox/entry $app\"" \
      -e 'command="/usr/local/lib/sanbox/pipeline.sh"' "$auth" |
      sed 's#^command="/usr/local/lib/sanbox/entry #command="/usr/local/lib/dagon/entry #' || true
    printf 'command="/usr/local/lib/dagon/entry %s",restrict %s\n' "$app" "$(head -n 1 "$work/actions.pub")"
  } > "$work/authorized_keys"
  install -o deploy -g deploy -m 600 "$work/authorized_keys" "$auth"
  # Every key now uses the dagon entry, so the sanbox files can go.
  rm -rf /usr/local/lib/sanbox /usr/local/sbin/sanbox /etc/sudoers.d/sanbox
  rm -rf "$work"
}

# A preview expires PREVIEW_TTL_MINUTES after its last deploy, published or
# unchanged. Older versions wrote no deployed file, so state stands in for it.
sweep() {
  [[ $# -eq 0 ]] || bad_args
  for dir in "$(state_dir)"/*/pr-*; do
    [[ -d "$dir" ]] || continue
    slot=$(basename "$dir")
    app=$(basename "$(dirname "$dir")")
    { is_app "$app" && is_preview "$slot"; } || continue
    stamp="$dir/deployed"
    [[ -f "$stamp" ]] || stamp="$dir/state"
    [[ -f "$stamp" ]] || continue
    [[ -n "$(find "$stamp" -mmin "+$PREVIEW_TTL_MINUTES" 2>/dev/null)" ]] || continue
    ( serve "$app" remove "$slot" ) | sed 's/^removed /expired /'
  done
}

main() {
  case "${1:-}" in
    serve) shift; serve "$@" ;;
    sweep) shift; sweep "$@" ;;
    install) shift; install_cmd "$@" ;;
    *) bad_args ;;
  esac
}

main "$@"
