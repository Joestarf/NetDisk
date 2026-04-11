#!/usr/bin/env bash
set -euo pipefail

# NetDisk NFS end-to-end regression
# Covers: mkdir, upload(write), overwrite, cross-dir rename, delete, OSS migrated read

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

API_PORT="${API_PORT:-8080}"
API_BASE="${API_BASE:-http://127.0.0.1:${API_PORT}}"
NFS_ADDR="${NFS_ADDR:-:2049}"
NFS_OWNER_ID="${NFS_OWNER_ID:-1}"
MOUNT_DIR="${MOUNT_DIR:-/tmp/netdisk-nfs-e2e}"
TEST_USERNAME="${TEST_USERNAME:-}"
TEST_PASSWORD="${TEST_PASSWORD:-}"
AUTO_KILL_PORTS="${AUTO_KILL_PORTS:-0}"

: "${MYSQL_DSN:?MYSQL_DSN is required}"
: "${TEST_USERNAME:?TEST_USERNAME is required (must match NFS_OWNER_ID user)}"
: "${TEST_PASSWORD:?TEST_PASSWORD is required (must match TEST_USERNAME)}"

echo "[INFO] root=${ROOT_DIR}"
echo "[INFO] api=${API_BASE} nfs_addr=${NFS_ADDR} owner_id=${NFS_OWNER_ID}"

require_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "[ERROR] command not found: $1" >&2
    exit 1
  fi
}

require_cmd curl
require_cmd python3
require_cmd go
require_cmd mount
require_cmd umount
require_cmd ss

resolve_owner_id() {
  if [[ "${NFS_OWNER_ID}" =~ ^[0-9]+$ ]]; then
    return 0
  fi

  local lookup_username="${NFS_OWNER_ID}"
  if [[ -z "${lookup_username}" || "${lookup_username}" == "auto" ]]; then
    lookup_username="${TEST_USERNAME}"
  fi

  echo "[INFO] resolving NFS owner id by username=${lookup_username}"
  local tmp_go_file="/tmp/netdisk_owner_id_lookup_$$.go"
  cat >"${tmp_go_file}" <<'EOF'
package main

import (
  "database/sql"
  "fmt"
  "log"
  "os"

  _ "github.com/go-sql-driver/mysql"
)

func main() {
  dsn := os.Getenv("MYSQL_DSN")
  username := os.Getenv("LOOKUP_USERNAME")
  db, err := sql.Open("mysql", dsn)
  if err != nil {
    log.Fatal(err)
  }
  defer db.Close()

  var id int64
  if err := db.QueryRow("SELECT id FROM users WHERE username = ? LIMIT 1", username).Scan(&id); err != nil {
    log.Fatal(err)
  }
  fmt.Print(id)
}
EOF

  local resolved
  if ! resolved="$(MYSQL_DSN="${MYSQL_DSN}" LOOKUP_USERNAME="${lookup_username}" go run "${tmp_go_file}" 2>/dev/null)"; then
    rm -f "${tmp_go_file}"
    echo "[ERROR] cannot resolve numeric owner id by username=${lookup_username}" >&2
    echo "[HINT] set NFS_OWNER_ID to a numeric id, or make sure the user exists in DB first." >&2
    exit 1
  fi
  rm -f "${tmp_go_file}"

  if ! [[ "${resolved}" =~ ^[0-9]+$ ]]; then
    echo "[ERROR] resolved owner id is invalid: ${resolved}" >&2
    exit 1
  fi
  NFS_OWNER_ID="${resolved}"
  echo "[INFO] resolved owner_id=${NFS_OWNER_ID}"
}

resolve_owner_id

SERVER_PID=""
cleanup() {
  set +e
  if mountpoint -q "${MOUNT_DIR}"; then
    echo "[CLEANUP] unmount ${MOUNT_DIR}"
    sudo umount "${MOUNT_DIR}" >/dev/null 2>&1 || true
  fi
  if [[ -n "${SERVER_PID}" ]]; then
    echo "[CLEANUP] stop server pid=${SERVER_PID}"
    kill "${SERVER_PID}" >/dev/null 2>&1 || true
    wait "${SERVER_PID}" >/dev/null 2>&1 || true
  fi
  rm -rf "${MOUNT_DIR}"
}
trap cleanup EXIT

is_port_listening() {
  local port="$1"
  ss -ltn "( sport = :${port} )" | grep -q ":${port}"
}

port_pid() {
  local port="$1"
  ss -ltnp "( sport = :${port} )" 2>/dev/null | sed -n 's/.*pid=\([0-9]\+\).*/\1/p' | head -n1
}

ensure_port_free() {
  local port="$1"
  local label="$2"

  if ! is_port_listening "${port}"; then
    return 0
  fi

  if [[ "${AUTO_KILL_PORTS}" == "1" ]]; then
    local pid
    pid="$(port_pid "${port}")"
    if [[ -n "${pid}" ]]; then
      echo "[WARN] ${label} port ${port} is in use by pid=${pid}, killing it because AUTO_KILL_PORTS=1"
      kill "${pid}" >/dev/null 2>&1 || true
      sleep 0.5
    fi
  fi

  if is_port_listening "${port}"; then
    echo "[ERROR] ${label} port ${port} already in use." >&2
    if [[ "${AUTO_KILL_PORTS}" == "1" ]]; then
      echo "[HINT] auto kill attempted but port is still occupied; please free it manually." >&2
    else
      echo "[HINT] stop existing service, set another port, or set AUTO_KILL_PORTS=1." >&2
    fi
    exit 1
  fi
}

json_get() {
  local expr="$1"
  local payload="$2"
  python3 - "$expr" "$payload" <<'PY'
import json,sys
expr=sys.argv[1]
payload=sys.argv[2]
obj=json.loads(payload)
cur=obj
for part in expr.split('.'):
    if not part:
        continue
    if part.isdigit():
        cur=cur[int(part)]
    else:
        cur=cur.get(part)
print(cur if cur is not None else "")
PY
}

api_post_json() {
  local url="$1"
  local payload="$2"
  curl -sS -X POST "${url}" -H 'Content-Type: application/json' -d "${payload}"
}

api_get_auth() {
  local url="$1"
  local token="$2"
  curl -sS "${url}" -H "Authorization: Bearer ${token}"
}

api_post_auth() {
  local url="$1"
  local token="$2"
  local payload="$3"
  curl -sS -X POST "${url}" -H "Authorization: Bearer ${token}" -H 'Content-Type: application/json' -d "${payload}"
}

echo "[STEP] start NetDisk server with NFS enabled"

nfs_port="${NFS_ADDR##*:}"
if [[ -z "${nfs_port}" || ! "${nfs_port}" =~ ^[0-9]+$ ]]; then
  echo "[ERROR] invalid NFS_ADDR=${NFS_ADDR}, expected like :2049" >&2
  exit 1
fi

ensure_port_free "${API_PORT}" "API"
ensure_port_free "${nfs_port}" "NFS"

(
  cd "${ROOT_DIR}" || exit 1
  PORT="${API_PORT}" NFS_ENABLE=1 NFS_ADDR="${NFS_ADDR}" NFS_OWNER_ID="${NFS_OWNER_ID}" MYSQL_DSN="${MYSQL_DSN}" \
    go run ./cmd/server >/tmp/netdisk-nfs-e2e-server.log 2>&1
) &
SERVER_PID=$!

for i in {1..40}; do
  if ! kill -0 "${SERVER_PID}" >/dev/null 2>&1; then
    echo "[ERROR] server process exited unexpectedly." >&2
    tail -n 120 /tmp/netdisk-nfs-e2e-server.log || true
    exit 1
  fi
  if curl -fsS "${API_BASE}/health" >/dev/null 2>&1; then
    echo "[OK] server ready"
    break
  fi
  sleep 0.5
  if [[ "$i" -eq 40 ]]; then
    echo "[ERROR] server health check timeout" >&2
    tail -n 80 /tmp/netdisk-nfs-e2e-server.log || true
    exit 1
  fi
done

for i in {1..40}; do
  if grep -q "NFS server running" /tmp/netdisk-nfs-e2e-server.log 2>/dev/null; then
    echo "[OK] nfs ready"
    break
  fi
  sleep 0.5
  if [[ "$i" -eq 40 ]]; then
    echo "[ERROR] NFS startup timeout." >&2
    tail -n 120 /tmp/netdisk-nfs-e2e-server.log || true
    exit 1
  fi
done

echo "[STEP] ensure test user exists and login"
register_resp="$(api_post_json "${API_BASE}/api/v1/auth/register" "{\"username\":\"${TEST_USERNAME}\",\"password\":\"${TEST_PASSWORD}\"}")" || true
register_code="$(json_get code "${register_resp}" 2>/dev/null || true)"
if [[ "${register_code}" == "0" ]]; then
  echo "[OK] register success user=${TEST_USERNAME}"
else
  echo "[INFO] register skipped/failed code=${register_code:-unknown}, continue to login"
fi

login_resp="$(api_post_json "${API_BASE}/api/v1/auth/login" "{\"username\":\"${TEST_USERNAME}\",\"password\":\"${TEST_PASSWORD}\"}")"
login_code="$(json_get code "${login_resp}")"
if [[ "${login_code}" != "0" ]]; then
  echo "[ERROR] login failed: ${login_resp}" >&2
  exit 1
fi
token="$(json_get data.token "${login_resp}")"
if [[ -z "${token}" ]]; then
  echo "[ERROR] empty token in login response: ${login_resp}" >&2
  exit 1
fi

echo "[STEP] verify logged-in user id matches NFS owner id"
me_resp="$(api_get_auth "${API_BASE}/api/v1/users/me" "${token}")"
me_code="$(json_get code "${me_resp}")"
if [[ "${me_code}" != "0" ]]; then
  echo "[ERROR] users/me failed: ${me_resp}" >&2
  exit 1
fi
me_id="$(json_get data.id "${me_resp}")"
if [[ "${me_id}" != "${NFS_OWNER_ID}" ]]; then
  echo "[ERROR] logged-in user id (${me_id}) != NFS_OWNER_ID (${NFS_OWNER_ID})."
  echo "[HINT] set NFS_OWNER_ID to this user id or use credentials for that owner." >&2
  exit 1
fi

echo "[STEP] mount NFS"
mkdir -p "${MOUNT_DIR}"
mount_opts="-o port=${nfs_port},mountport=${nfs_port},nfsvers=3,noacl,tcp -t nfs localhost:/ ${MOUNT_DIR}"

# 先尝试当前用户直接挂载（容器或已授权环境可能可行）
if timeout 15s mount ${mount_opts} >/dev/null 2>&1; then
  echo "[OK] mounted without sudo"
else
  # 再尝试无交互 sudo，避免脚本在密码提示处卡死
  if timeout 15s sudo -n mount ${mount_opts} >/dev/null 2>&1; then
    echo "[OK] mounted with sudo -n"
  else
    echo "[ERROR] mount failed and sudo requires password (or mount permission denied)." >&2
    echo "[HINT] run: sudo -v  # refresh sudo ticket" >&2
    echo "[HINT] then rerun script within a few minutes." >&2
    exit 1
  fi
fi

# 1) create dirs
echo "[CASE] mkdir"
mkdir -p "${MOUNT_DIR}/dirA" "${MOUNT_DIR}/dirB"
[[ -d "${MOUNT_DIR}/dirA" && -d "${MOUNT_DIR}/dirB" ]] || { echo "[FAIL] mkdir" >&2; exit 1; }

# 2) upload/write
echo "[CASE] upload(write)"
printf 'hello-v1\n' >"${MOUNT_DIR}/dirA/nfs.txt"
[[ -f "${MOUNT_DIR}/dirA/nfs.txt" ]] || { echo "[FAIL] upload(write)" >&2; exit 1; }

# 3) overwrite
echo "[CASE] overwrite"
printf 'hello-v2-overwrite\n' >"${MOUNT_DIR}/dirA/nfs.txt"
grep -q 'hello-v2-overwrite' "${MOUNT_DIR}/dirA/nfs.txt" || { echo "[FAIL] overwrite" >&2; exit 1; }

# 4) cross-dir rename
echo "[CASE] cross-dir rename"
mv "${MOUNT_DIR}/dirA/nfs.txt" "${MOUNT_DIR}/dirB/nfs-renamed.txt"
[[ ! -f "${MOUNT_DIR}/dirA/nfs.txt" && -f "${MOUNT_DIR}/dirB/nfs-renamed.txt" ]] || { echo "[FAIL] cross-dir rename" >&2; exit 1; }

# 5) delete
echo "[CASE] delete"
rm -f "${MOUNT_DIR}/dirB/nfs-renamed.txt"
[[ ! -f "${MOUNT_DIR}/dirB/nfs-renamed.txt" ]] || { echo "[FAIL] delete" >&2; exit 1; }

# 6) migrated OSS read
echo "[CASE] read migrated OSS file"
OSS_CONTENT="oss-payload-$(date +%s)"
printf '%s\n' "${OSS_CONTENT}" >"${MOUNT_DIR}/dirA/oss.txt"

files_resp="$(api_get_auth "${API_BASE}/api/v1/files" "${token}")"
files_code="$(json_get code "${files_resp}")"
if [[ "${files_code}" != "0" ]]; then
  echo "[ERROR] list files failed: ${files_resp}" >&2
  exit 1
fi

oss_file_id="$(python3 - <<'PY' "${files_resp}"
import json,sys
obj=json.loads(sys.argv[1])
items=obj.get('data') or []
for item in items:
    if item.get('name') == 'oss.txt':
        print(item.get('id',''))
        break
PY
)"

if [[ -z "${oss_file_id}" ]]; then
  echo "[ERROR] cannot find oss.txt id in files list" >&2
  exit 1
fi

migrate_resp="$(api_post_auth "${API_BASE}/api/v1/files/${oss_file_id}/migrate" "${token}" "{}")"
migrate_code="$(json_get code "${migrate_resp}")"
if [[ "${migrate_code}" != "0" ]]; then
  echo "[ERROR] migrate failed: ${migrate_resp}" >&2
  exit 1
fi

blob_hash="$(json_get data.blob_hash "${migrate_resp}")"
if [[ -z "${blob_hash}" ]]; then
  echo "[ERROR] migrate response missing blob_hash: ${migrate_resp}" >&2
  exit 1
fi

# Force local blob absence so NFS read path must fallback to OSS download.
echo "[STEP] remove local blob file to force OSS read"
tmp_go_file="/tmp/netdisk_blob_path_lookup_$$.go"
cat >"${tmp_go_file}" <<'EOF'
package main
import (
  "database/sql"
  "fmt"
  "log"
  "os"
  _ "github.com/go-sql-driver/mysql"
)
func main(){
 dsn:=os.Getenv("MYSQL_DSN")
 hash:=os.Getenv("BLOB_HASH")
 db,err:=sql.Open("mysql",dsn)
 if err!=nil { log.Fatal(err) }
 defer db.Close()
 var p string
 err=db.QueryRow("SELECT disk_path FROM file_blobs WHERE hash = ?",hash).Scan(&p)
 if err!=nil { log.Fatal(err) }
 fmt.Print(p)
}
EOF
blob_path="$(MYSQL_DSN="${MYSQL_DSN}" BLOB_HASH="${blob_hash}" go run "${tmp_go_file}")"
rm -f "${tmp_go_file}"

if [[ -n "${blob_path}" && -f "${blob_path}" ]]; then
  rm -f "${blob_path}"
fi

read_back="$(cat "${MOUNT_DIR}/dirA/oss.txt" | tr -d '\r\n')"
if [[ "${read_back}" != "${OSS_CONTENT}" ]]; then
  echo "[FAIL] OSS migrated read mismatch: want=${OSS_CONTENT} got=${read_back}" >&2
  exit 1
fi

echo "[PASS] NFS e2e regression finished successfully"
