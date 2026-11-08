#!/bin/bash
# Tests the server side of dagon.sh with fake docker, sops, curl,
# systemctl, flock, and ss. Run: bash internal/server/dagon_test.sh
set -u

HERE=$(cd "$(dirname "$0")" && pwd)
SCRIPT="$HERE/dagon.sh"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

stdout=""
stderr_file="$TMP/stderr"
fail() {
  printf 'FAIL: %s\n' "$1" >&2
  printf 'stdout: %s\n' "$stdout" >&2
  [[ -f "$stderr_file" ]] && printf 'stderr: %s\n' "$(cat "$stderr_file")" >&2
  exit 1
}

FAKE="$TMP/fake"
DOCKER="$TMP/docker"
LOG="$TMP/log"
mkdir -p "$FAKE" "$DOCKER/containers" "$DOCKER/images"
: > "$LOG"

cat > "$FAKE/docker" <<'EOF'
#!/bin/bash
# A tiny docker: containers and images are directories under $DOCKER.
printf 'docker %s\n' "$*" >> "$LOG"
c="$DOCKER/containers"
i="$DOCKER/images"
key() { printf '%s' "$1" | tr '/:' '__'; }
if [[ "$1" == "--config" ]]; then shift 2; fi
cmd=$1; shift
case "$cmd" in
  login) cat > /dev/null; [[ "${LOGIN_FAIL:-}" == "1" ]] && exit 1; exit 0 ;;
  pull)
    [[ "${PULL_FAIL:-}" == "1" ]] && exit 1
    ref=${@: -1}
    printf 'sha256:%s\n' "$(printf '%s' "$ref" | shasum | cut -c1-12)" > "$i/$(key "$ref")"
    ;;
  image)
    sub=$1; shift
    case "$sub" in
      inspect) ref=${@: -1}; cat "$i/$(key "$ref")" 2>/dev/null || exit 1 ;;
      ls) for f in "$i"/*; do [[ -f "$f" ]] && cat "$f"; done ;;
      rm)
        for d in "$c"/*; do [[ -f "$d/image" ]] && grep -q -x "$1" "$d/image" && exit 1; done
        grep -l -x "$1" "$i"/* 2>/dev/null | xargs rm -f
        ;;
    esac
    ;;
  inspect)
    name=${@: -1}
    [[ -d "$c/$name" ]] || exit 1
    if [[ "$1" == "--format" ]]; then printf '%s %s\n' "$(cat "$c/$name/running")" "$(cat "$c/$name/image")"; fi
    ;;
  run)
    name=""; envf=""; port=""
    while [[ $# -gt 1 ]]; do
      case "$1" in
        --name) name=$2; shift 2 ;;
        --env-file) envf=$2; shift 2 ;;
        -p) port=$2; shift 2 ;;
        *) shift ;;
      esac
    done
    ref=$1
    [[ -d "$c/$name" ]] && exit 1
    [[ "${RUN_FAIL:-}" == "1" ]] && exit 1
    mkdir -p "$c/$name"
    cp "$i/$(key "$ref")" "$c/$name/image"
    cp "$envf" "$c/$name/env"
    printf '%s\n' "$port" > "$c/$name/port"
    echo true > "$c/$name/running"
    ;;
  stop) echo false > "$c/${@: -1}/running" ;;
  start) [[ -d "$c/$1" ]] || exit 1; echo true > "$c/$1/running" ;;
  rename) [[ -d "$c/$1" && ! -e "$c/$2" ]] || exit 1; mv "$c/$1" "$c/$2" ;;
  rm) shift; for n in "$@"; do rm -rf "${c:?}/$n"; done ;;
esac
exit 0
EOF

cat > "$FAKE/sops" <<'EOF'
#!/bin/bash
# Pretend decryption: the file must name the key it was encrypted for.
printf 'sops key=%s\n' "$(basename "$SOPS_AGE_KEY_FILE")" >> "$LOG"
file=${@: -1}
want=$(basename "$SOPS_AGE_KEY_FILE" .key)
grep -q -x "# for $want" "$file" || exit 1
grep -v '^#' "$file"
EOF

cat > "$FAKE/curl" <<'EOF'
#!/bin/bash
[[ "${CURL_FAIL:-}" == "1" ]] && exit 1
exit 0
EOF

cat > "$FAKE/systemctl" <<'EOF'
#!/bin/bash
printf 'systemctl %s\n' "$*" >> "$LOG"
[[ "${RELOAD_FAIL:-}" == "1" ]] && exit 1
exit 0
EOF

printf '#!/bin/bash\nexit 0\n' > "$FAKE/flock"
printf '#!/bin/bash\n[[ -n "${PORT_BUSY:-}" && "$*" == *":${PORT_BUSY}"* ]] && echo busy\nexit 0\n' > "$FAKE/ss"
chmod +x "$FAKE"/*

ETC="$TMP/etc"
APP=demo
mkdir -p "$ETC/apps/$APP/age"
printf '%s\n' example.com > "$ETC/apps/$APP/apex"
printf '%s\n' ghcr.io/acme/demo > "$ETC/apps/$APP/image"
for env in production staging preview; do printf 'key\n' > "$ETC/apps/$APP/age/$env.key"; done

SHA1=1111111111111111111111111111111111111111
SHA2=2222222222222222222222222222222222222222
SECRETS="$TMP/secrets"

serve() {
  input=$1
  shift
  stdout=$(
    PATH="$FAKE:$PATH" DOCKER="$DOCKER" LOG="$LOG" \
      DAGON_ETC="$ETC" DAGON_STATE="$TMP/state" DAGON_CADDY="$TMP/caddy" DAGON_RUN="$TMP/run" \
      bash "$SCRIPT" serve "$@" < "$input" 2>"$stderr_file"
  )
  code=$?
}

secrets() {
  { printf 'gh-user gh-token\n'; printf '%s\n' "$@"; } > "$SECRETS"
}

expect() {
  [[ "$code" -eq "$1" ]] || fail "$3: exit $code, want $1"
  [[ "$stdout" == "$2" ]] || fail "$3: stdout"
}

c="$DOCKER/containers"

# 1. First deploy publishes, decrypts with the staging key, and routes.
secrets '# for staging' 'API_KEY=one'
serve "$SECRETS" "$APP" deploy staging "$SHA1"
expect 0 "published staging $SHA1 https://staging.example.com" "case 1"
[[ "$(cat "$c/demo_staging/running")" == "true" ]] || fail "case 1 running"
grep -q -x 'API_KEY=one' "$c/demo_staging/env" || fail "case 1 env"
[[ "$(cat "$c/demo_staging/port")" == "127.0.0.1:20000:3000" ]] || fail "case 1 port"
grep -q 'reverse_proxy 127.0.0.1:20000' "$TMP/caddy/sites/demo_staging.caddy" || fail "case 1 route"
grep -q '^staging.example.com {' "$TMP/caddy/sites/demo_staging.caddy" || fail "case 1 host"
grep -q -x 'sops key=staging.key' "$LOG" || fail "case 1 key"
grep -q 'systemctl reload caddy' "$LOG" || fail "case 1 reload"
[[ -z "$(ls -A "$TMP/run")" ]] || fail "case 1 left decrypted files in run"

# 2. Same image and same secrets: unchanged, and no new container.
: > "$LOG"
serve "$SECRETS" "$APP" deploy staging "$SHA1"
expect 0 "unchanged staging $SHA1 https://staging.example.com" "case 2"
grep -q 'docker run' "$LOG" && fail "case 2 ran a container"
grep -q 'systemctl reload' "$LOG" && fail "case 2 reloaded caddy"

# 3. Changed secrets restart the container with the new values.
secrets '# for staging' 'API_KEY=two'
serve "$SECRETS" "$APP" deploy staging "$SHA1"
expect 0 "published staging $SHA1 https://staging.example.com" "case 3"
grep -q -x 'API_KEY=two' "$c/demo_staging/env" || fail "case 3 env"
[[ ! -e "$c/demo_staging_prev" ]] || fail "case 3 prev left"

# 4. A failed health check keeps the old container serving.
old_image=$(cat "$c/demo_staging/image")
CURL_FAIL=1 serve "$SECRETS" "$APP" deploy staging "$SHA2"
expect 1 "rejected staging $SHA2 https://staging.example.com" "case 4"
[[ "$(cat "$c/demo_staging/image")" == "$old_image" ]] || fail "case 4 old image"
[[ "$(cat "$c/demo_staging/running")" == "true" ]] || fail "case 4 old running"
[[ "$(cat "$c/demo_staging_failed/running")" == "false" ]] || fail "case 4 failed kept stopped"

# 5. A pull failure changes nothing.
PULL_FAIL=1 serve "$SECRETS" "$APP" deploy staging "$SHA2"
expect 1 "rejected staging $SHA2 https://staging.example.com" "case 5"
[[ "$(cat "$c/demo_staging/image")" == "$old_image" ]] || fail "case 5 old image"

# 6. A preview cannot use staging or production secrets.
secrets '# for production' 'DB=prod'
serve "$SECRETS" "$APP" deploy pr-7 "$SHA1"
expect 1 "rejected pr-7 $SHA1 https://pr-7.example.com" "case 6"
[[ ! -e "$c/demo_pr-7" ]] || fail "case 6 started"
grep -q -x 'sops key=preview.key' "$LOG" || fail "case 6 key"

# 7. A preview with preview secrets gets the next free port, skipping a busy one.
secrets '# for preview' 'DB=preview'
PORT_BUSY=20001 serve "$SECRETS" "$APP" deploy pr-7 "$SHA1"
expect 0 "published pr-7 $SHA1 https://pr-7.example.com" "case 7"
[[ "$(cat "$c/demo_pr-7/port")" == "127.0.0.1:20002:3000" ]] || fail "case 7 port"

# 8. No secrets file means an empty environment.
printf 'gh-user gh-token\n' > "$SECRETS"
serve "$SECRETS" "$APP" deploy production "$SHA1"
expect 0 "published production $SHA1 https://example.com" "case 8"
[[ ! -s "$c/demo_production/env" ]] || fail "case 8 env not empty"
grep -q '^example.com {' "$TMP/caddy/sites/demo_production.caddy" || fail "case 8 host"

# 9. Remove deletes the preview, its route, and its port.
: > "$LOG"
serve /dev/null "$APP" remove pr-7
expect 0 "removed pr-7 https://pr-7.example.com" "case 9"
[[ ! -e "$c/demo_pr-7" && ! -e "$TMP/caddy/sites/demo_pr-7.caddy" && ! -e "$ETC/ports/demo_pr-7" ]] || fail "case 9 left files"
grep -q 'systemctl reload caddy' "$LOG" || fail "case 9 reload"
serve /dev/null "$APP" remove pr-7
expect 0 "removed pr-7 https://pr-7.example.com" "case 9 again"
serve /dev/null "$APP" remove staging
[[ "$code" -eq 2 ]] || fail "case 9 staging removable"

# 10. Bad input is refused before anything runs.
for args in "nope deploy staging $SHA1" "$APP deploy preview $SHA1" "$APP deploy pr-0 $SHA1" \
  "$APP deploy staging abc" "$APP deploy staging $SHA1 extra" "$APP shell" "Demo deploy staging $SHA1"; do
  # shellcheck disable=SC2086
  serve "$SECRETS" $args
  [[ "$code" -eq 2 ]] || fail "case 10 '$args' exit $code"
  [[ -z "$stdout" ]] || fail "case 10 '$args' stdout"
done

# 11. The SSH entry wrapper passes the app from authorized_keys, not from the command.
ENTRY="$TMP/entry"
bash -c "$(sed '$d' "$SCRIPT"); render_entry '$SCRIPT'" > "$ENTRY"
printf '#!/bin/bash\n[[ "$1" == "-n" ]] && shift\nexec bash "$@"\n' > "$FAKE/sudo"
chmod +x "$FAKE/sudo"
secrets '# for staging' 'API_KEY=two'
stdout=$(
  PATH="$FAKE:$PATH" DOCKER="$DOCKER" LOG="$LOG" \
    DAGON_ETC="$ETC" DAGON_STATE="$TMP/state" DAGON_CADDY="$TMP/caddy" DAGON_RUN="$TMP/run" \
    SSH_ORIGINAL_COMMAND="deploy staging $SHA1" bash "$ENTRY" "$APP" < "$SECRETS" 2>"$stderr_file"
)
code=$?
expect 0 "unchanged staging $SHA1 https://staging.example.com" "case 11"
stdout=$(PATH="$FAKE:$PATH" SSH_ORIGINAL_COMMAND='deploy staging $(id)' bash "$ENTRY" "$APP" < /dev/null 2>"$stderr_file")
code=$?
[[ "$code" -eq 2 ]] || fail "case 11 injection exit $code"

# 12. Install refuses bad arguments before it touches the system.
for args in "Demo example.com ghcr.io/acme/demo $TMP" "demo nodot ghcr.io/acme/demo $TMP" \
  "demo example.com docker.io/acme/demo $TMP" "demo example.com ghcr.io/acme/demo"; do
  # shellcheck disable=SC2086
  stdout=$(bash "$SCRIPT" install $args 2>"$stderr_file" < /dev/null)
  code=$?
  [[ "$code" -eq 2 ]] || fail "case 12 '$args' exit $code"
done

# 13. Each rollback steps one release further back.
SHA3=3333333333333333333333333333333333333333
SHA4=4444444444444444444444444444444444444444
serve /dev/null "$APP" previous production
[[ "$code" -eq 1 && -z "$stdout" ]] || fail "case 13 previous with one deploy: exit $code"
printf 'gh-user gh-token\n' > "$SECRETS"
serve "$SECRETS" "$APP" deploy production "$SHA2"
serve "$SECRETS" "$APP" deploy production "$SHA2"
expect 0 "unchanged production $SHA2 https://example.com" "case 13 unchanged"
[[ "$(wc -l < "$TMP/state/$APP/production/history" | tr -d ' ')" == "2" ]] || fail "case 13 unchanged wrote history"
serve "$SECRETS" "$APP" deploy production "$SHA3"
serve /dev/null "$APP" previous production
expect 0 "$SHA2" "case 13 previous of 3"
serve "$SECRETS" "$APP" deploy production "$SHA2" rollback
expect 0 "published production $SHA2 https://example.com" "case 13 rollback to 2"
serve /dev/null "$APP" previous production
expect 0 "$SHA1" "case 13 a second rollback goes to 1, not back to 3"
serve "$SECRETS" "$APP" deploy production "$SHA1" rollback
serve /dev/null "$APP" previous production
[[ "$code" -eq 1 ]] || fail "case 13 rolled back past the first release"
serve "$SECRETS" "$APP" deploy production "$SHA4"
serve /dev/null "$APP" previous production
expect 0 "$SHA3" "case 13 after a new deploy, rollback goes to the release before it"
secrets '# for production' 'DB=rotated'
serve "$SECRETS" "$APP" deploy production "$SHA4"
expect 0 "published production $SHA4 https://example.com" "case 13 secrets change"
serve /dev/null "$APP" previous production
expect 0 "$SHA3" "case 13 a secrets-only redeploy is not a release to roll back to"
printf 'gh-user gh-token\n' > "$SECRETS"
serve "$SECRETS" "$APP" deploy production "$SHA4" extra
[[ "$code" -eq 2 ]] || fail "case 13 accepted a bad deploy kind"
CURL_FAIL=1 serve "$SECRETS" "$APP" deploy production "$SHA1"
serve /dev/null "$APP" previous production
expect 0 "$SHA3" "case 13 a rejected deploy is not history"
serve /dev/null "$APP" previous pr-7
[[ "$code" -eq 2 ]] || fail "case 13 previews have no rollback"
for i in $(seq 1 25); do
  serve "$SECRETS" "$APP" deploy staging "$(printf '%040x' "$i")"
done
[[ "$(wc -l < "$TMP/state/$APP/staging/history" | tr -d ' ')" == "20" ]] || fail "case 13 history is not capped at 20"

# 14. Sweep removes previews not deployed for 24 hours and keeps the rest.
sweep() {
  stdout=$(
    PATH="$FAKE:$PATH" DOCKER="$DOCKER" LOG="$LOG" \
      DAGON_ETC="$ETC" DAGON_STATE="$TMP/state" DAGON_CADDY="$TMP/caddy" DAGON_RUN="$TMP/run" \
      bash "$SCRIPT" sweep 2>"$stderr_file"
  )
  code=$?
}
secrets '# for preview' 'DB=preview'
serve "$SECRETS" "$APP" deploy pr-8 "$SHA1"
serve "$SECRETS" "$APP" deploy pr-9 "$SHA2"
old=$(date -v-25H +%Y%m%d%H%M 2>/dev/null || date -d '25 hours ago' +%Y%m%d%H%M)
touch -t "$old" "$TMP/state/$APP/pr-8/deployed" "$TMP/state/$APP/production/deployed"
sweep
expect 0 "expired pr-8 https://pr-8.example.com" "case 14"
[[ ! -e "$c/demo_pr-8" && ! -e "$TMP/caddy/sites/demo_pr-8.caddy" && ! -d "$TMP/state/$APP/pr-8" ]] || fail "case 14 left pr-8"
[[ -e "$c/demo_pr-9" && -e "$c/demo_production" ]] || fail "case 14 removed a live slot"
touch -t "$old" "$TMP/state/$APP/pr-9/deployed"
serve "$SECRETS" "$APP" deploy pr-9 "$SHA2"
expect 0 "unchanged pr-9 $SHA2 https://pr-9.example.com" "case 14 redeploy"
sweep
expect 0 "" "case 14 an unchanged deploy resets the clock"
bash "$SCRIPT" sweep extra 2>/dev/null
[[ $? -eq 2 ]] || fail "case 14 sweep took arguments"

printf '%s\n' ok
