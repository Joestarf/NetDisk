# NetDisk 全量手动演示流程（按题目功能点，命令可复制）

这份文档按“题目验收”组织，覆盖当前代码中已实现的核心能力：

1. 健康检查、注册登录、用户信息
2. 文件上传/列表/重命名/删除/下载
3. 文件夹创建/重命名/子项查询/移动/压缩下载
4. 分享（普通下载 + 密码分享 + P2P 分享）
5. 断点续传下载（Range）
6. 分片上传 + 秒传
7. 对象存储迁移（可选）
8. NFS 映射演示（脚本）
9. P2P 直传（文件、文件夹 zip）

## 0）终端 A：启动服务

```bash
cd /home/joestar/HUST-code/project/NetDisk

export MYSQL_DSN='user:password@tcp(127.0.0.1:3306)/netdisk?parseTime=true&charset=utf8mb4'
export PORT=8080

# NFS 可选
export NFS_ENABLE=1
export NFS_ADDR=':2049'
export NFS_REQUIRE_MOUNT_AUTH=0

go run ./cmd/server
```

保持该终端运行。

## 1）终端 B：准备变量与工具函数

```bash
cd /home/joestar/HUST-code/project/NetDisk
set -euo pipefail

export API='http://127.0.0.1:8080'
export USER_A='demo_owner'
export PASS_A='Passw0rd123!'
export USER_B='demo_peer'
export PASS_B='Passw0rd123!'

json_get() {
  python3 - "$1" "$2" <<'PY'
import json,sys
expr=sys.argv[1]
payload=sys.argv[2]
obj=json.loads(payload)
cur=obj
for p in expr.split('.'):
    if not p:
        continue
    if p.isdigit():
        cur=cur[int(p)]
    else:
        cur=cur.get(p)
print(cur if cur is not None else "")
PY
}

api_post_json() {
  curl -sS -X POST "$1" -H 'Content-Type: application/json' -d "$2"
}

api_patch_json_auth() {
  curl -sS -X PATCH "$1" -H "Authorization: Bearer $2" -H 'Content-Type: application/json' -d "$3"
}

api_get_auth() {
  curl -sS "$1" -H "Authorization: Bearer $2"
}
```

## 2）系统与鉴权

### 2.1 健康检查

```bash
curl -sS "$API/health"
```

### 2.2 注册 + 登录 A（owner）

```bash
api_post_json "$API/api/v1/auth/register" "{\"username\":\"$USER_A\",\"password\":\"$PASS_A\"}" || true

LOGIN_A=$(api_post_json "$API/api/v1/auth/login" "{\"username\":\"$USER_A\",\"password\":\"$PASS_A\"}")
echo "$LOGIN_A"
TOKEN_A=$(json_get data.token "$LOGIN_A")
echo "TOKEN_A=$TOKEN_A"
```

### 2.3 用户信息读取与修改

```bash
ME_A=$(api_get_auth "$API/api/v1/users/me" "$TOKEN_A")
echo "$ME_A"

PATCH_A=$(api_patch_json_auth "$API/api/v1/users/me" "$TOKEN_A" "{\"bio\":\"netdisk demo owner\"}")
echo "$PATCH_A"
```

### 2.4 修改密码（可选）

```bash
api_patch_json_auth "$API/api/v1/users/me/password" "$TOKEN_A" "{\"old_password\":\"$PASS_A\",\"new_password\":\"$PASS_A\"}"
```

## 3）文件主链路

### 3.1 上传文件

```bash
echo "hello from netdisk demo" > /tmp/netdisk_demo_file.txt

UPLOAD_A=$(curl -sS -X POST "$API/api/v1/files/upload" \
  -H "Authorization: Bearer $TOKEN_A" \
  -F "file=@/tmp/netdisk_demo_file.txt")
echo "$UPLOAD_A"
FILE_ID=$(json_get data.id "$UPLOAD_A")
echo "FILE_ID=$FILE_ID"
```

### 3.2 文件列表

```bash
LIST_A=$(api_get_auth "$API/api/v1/files" "$TOKEN_A")
echo "$LIST_A"
```

### 3.3 文件重命名

```bash
RENAME_A=$(api_patch_json_auth "$API/api/v1/files/$FILE_ID/rename" "$TOKEN_A" "{\"name\":\"renamed-demo.txt\"}")
echo "$RENAME_A"
```

### 3.4 普通下载

```bash
curl -sS -L -H "Authorization: Bearer $TOKEN_A" "$API/api/v1/files/$FILE_ID/download" -o /tmp/netdisk_download.txt
cat /tmp/netdisk_download.txt
```

### 3.5 断点续传（Range）下载

```bash
curl -sS -L -H "Authorization: Bearer $TOKEN_A" -H 'Range: bytes=0-4' "$API/api/v1/files/$FILE_ID/download" -o /tmp/netdisk_range_part1.txt
curl -sS -L -H "Authorization: Bearer $TOKEN_A" -H 'Range: bytes=5-' "$API/api/v1/files/$FILE_ID/download" -o /tmp/netdisk_range_part2.txt
cat /tmp/netdisk_range_part1.txt /tmp/netdisk_range_part2.txt
```

## 4）文件夹与节点

### 4.1 创建文件夹

```bash
FOLDER_CREATE=$(api_post_json "$API/api/v1/folders" "{\"name\":\"demo-folder\"}" | cat)
echo "$FOLDER_CREATE"
FOLDER_ID=$(json_get data.id "$FOLDER_CREATE")
echo "FOLDER_ID=$FOLDER_ID"
```

### 4.2 文件夹重命名

```bash
api_patch_json_auth "$API/api/v1/folders/$FOLDER_ID/rename" "$TOKEN_A" "{\"name\":\"demo-folder-renamed\"}"
```

### 4.3 移动文件到该文件夹

```bash
MOVE_RESP=$(api_post_json "$API/api/v1/nodes/move" "{\"node_type\":\"file\",\"node_id\":\"$FILE_ID\",\"target_folder_id\":$FOLDER_ID}" | cat)
echo "$MOVE_RESP"
```

### 4.4 查询子项

```bash
CHILDREN=$(api_get_auth "$API/api/v1/folders/$FOLDER_ID/children" "$TOKEN_A")
echo "$CHILDREN"
```

### 4.5 文件夹下载（压缩包）

```bash
curl -sS -L -H "Authorization: Bearer $TOKEN_A" "$API/api/v1/folders/$FOLDER_ID/download" -o /tmp/demo-folder.zip
ls -lh /tmp/demo-folder.zip
```

## 5）分享能力（普通 + 密码 + 文件夹）

### 5.1 普通下载分享

```bash
SHARE_FILE=$(curl -sS -X POST "$API/api/v1/shares" \
  -H "Authorization: Bearer $TOKEN_A" \
  -H 'Content-Type: application/json' \
  -d "{\"share_type\":\"public_download\",\"node_type\":\"file\",\"node_id\":\"$FILE_ID\"}")
echo "$SHARE_FILE"
SHARE_FILE_TOKEN=$(json_get data.token "$SHARE_FILE")
echo "SHARE_FILE_TOKEN=$SHARE_FILE_TOKEN"

curl -sS -L "$API/s/$SHARE_FILE_TOKEN" -o /tmp/share-file-download.txt
cat /tmp/share-file-download.txt
```

### 5.2 密码分享

```bash
SHARE_PWD=$(curl -sS -X POST "$API/api/v1/shares" \
  -H "Authorization: Bearer $TOKEN_A" \
  -H 'Content-Type: application/json' \
  -d "{\"share_type\":\"public_download\",\"node_type\":\"file\",\"node_id\":\"$FILE_ID\",\"password\":\"demo123\"}")
echo "$SHARE_PWD"
SHARE_PWD_TOKEN=$(json_get data.token "$SHARE_PWD")
echo "SHARE_PWD_TOKEN=$SHARE_PWD_TOKEN"

curl -sS "$API/s/$SHARE_PWD_TOKEN?password=demo123" | head -c 120 || true
```

### 5.3 文件夹分享

```bash
SHARE_FOLDER=$(curl -sS -X POST "$API/api/v1/shares" \
  -H "Authorization: Bearer $TOKEN_A" \
  -H 'Content-Type: application/json' \
  -d "{\"share_type\":\"public_download\",\"node_type\":\"folder\",\"node_id\":\"$FOLDER_ID\"}")
echo "$SHARE_FOLDER"
SHARE_FOLDER_TOKEN=$(json_get data.token "$SHARE_FOLDER")
echo "SHARE_FOLDER_TOKEN=$SHARE_FOLDER_TOKEN"

curl -sS "$API/s/$SHARE_FOLDER_TOKEN"
```

### 5.4 查看我的分享 + 撤销 + 删除

```bash
MY_SHARES=$(api_get_auth "$API/api/v1/shares" "$TOKEN_A")
echo "$MY_SHARES"

SHARE_ID=$(json_get data.0.id "$MY_SHARES")
echo "SHARE_ID=$SHARE_ID"

curl -sS -X PATCH "$API/api/v1/shares/$SHARE_ID" -H "Authorization: Bearer $TOKEN_A"
curl -sS -X DELETE "$API/api/v1/shares/$SHARE_ID" -H "Authorization: Bearer $TOKEN_A"
```

## 6）分片上传与秒传

### 6.1 准备文件和哈希

```bash
printf 'chunk-demo-1234567890abcdef\n' > /tmp/chunk-demo.txt
FILE_HASH=$(sha256sum /tmp/chunk-demo.txt | awk '{print $1}')
echo "FILE_HASH=$FILE_HASH"
split -b 8 -d -a 3 /tmp/chunk-demo.txt /tmp/chunk-part-
ls -l /tmp/chunk-part-*
```

### 6.2 init

```bash
INIT_RESP=$(curl -sS -X POST "$API/api/v1/files/upload/init" \
  -H "Authorization: Bearer $TOKEN_A" \
  -H 'Content-Type: application/json' \
  -d "{\"name\":\"chunked-demo.txt\",\"total_chunks\":4,\"file_hash\":\"$FILE_HASH\"}")
echo "$INIT_RESP"
UPLOAD_ID=$(json_get data.upload_id "$INIT_RESP")
echo "UPLOAD_ID=$UPLOAD_ID"
```

### 6.3 上传分片 + 状态

```bash
for i in 0 1 2 3; do
  PART=$(printf "/tmp/chunk-part-%03d" "$i")
  CH=$(sha256sum "$PART" | awk '{print $1}')
  curl -sS -X POST "$API/api/v1/files/upload/chunk" \
    -H "Authorization: Bearer $TOKEN_A" \
    -F "upload_id=$UPLOAD_ID" \
    -F "chunk_index=$i" \
    -F "chunk_hash=$CH" \
    -F "chunk=@$PART"
  echo
done

curl -sS -G "$API/api/v1/files/upload/status" \
  -H "Authorization: Bearer $TOKEN_A" \
  --data-urlencode "upload_id=$UPLOAD_ID"
```

### 6.4 complete

```bash
COMPLETE_RESP=$(curl -sS -X POST "$API/api/v1/files/upload/complete" \
  -H "Authorization: Bearer $TOKEN_A" \
  -H 'Content-Type: application/json' \
  -d "{\"upload_id\":\"$UPLOAD_ID\",\"file_hash\":\"$FILE_HASH\"}")
echo "$COMPLETE_RESP"
```

### 6.5 秒传（同 hash）

```bash
INSTANT=$(curl -sS -X POST "$API/api/v1/files/upload/init" \
  -H "Authorization: Bearer $TOKEN_A" \
  -H 'Content-Type: application/json' \
  -d "{\"name\":\"instant-copy.txt\",\"total_chunks\":1,\"file_hash\":\"$FILE_HASH\"}")
echo "$INSTANT"
```

## 7）对象存储迁移（可选）

说明：需先配置 OSS 环境变量并重启服务。

```bash
curl -sS -X POST "$API/api/v1/files/$FILE_ID/migrate" \
  -H "Authorization: Bearer $TOKEN_A"

# 再次下载应走重定向到对象存储 URL（若迁移成功）
curl -I -H "Authorization: Bearer $TOKEN_A" "$API/api/v1/files/$FILE_ID/download"
```

## 8）NFS 映射演示（可选）

```bash
cd /home/joestar/HUST-code/project/NetDisk
sudo -v
TEST_USERNAME="$USER_A" TEST_PASSWORD="$PASS_A" MYSQL_DSN="$MYSQL_DSN" bash scripts/nfs_e2e.sh
```

## 9）P2P 直传演示

### 9.1 文件直传

#### 终端 B（发送方）

```bash
cd /home/joestar/HUST-code/project/NetDisk
MY_LAN_ADDR='192.168.1.10:9099'  # 改成你的局域网地址

go run ./cmd/p2pclient host \
  --server "$API" \
  --auth-token "$TOKEN_A" \
  --node-type file \
  --node-id "$FILE_ID" \
  --listen ':9099' \
  --advertise "$MY_LAN_ADDR" \
  --source /tmp/netdisk_demo_file.txt
```

#### 终端 C（接收方）

```bash
cd /home/joestar/HUST-code/project/NetDisk
export API='http://127.0.0.1:8080'
export P2P_TOKEN='replace_with_token_from_host_output'

go run ./cmd/p2pclient recv \
  --server "$API" \
  --share-token "$P2P_TOKEN" \
  --output-dir ./cmd/p2pclient/p2p_downloads
```

### 9.2 文件夹直传（自动 zip）

```bash
mkdir -p /tmp/netdisk_demo_folder
printf 'A\n' > /tmp/netdisk_demo_folder/a.txt
printf 'B\n' > /tmp/netdisk_demo_folder/b.txt

# 这里用上面创建的 folder_id
go run ./cmd/p2pclient host \
  --server "$API" \
  --auth-token "$TOKEN_A" \
  --node-type folder \
  --node-id "$FOLDER_ID" \
  --listen ':9099' \
  --advertise "$MY_LAN_ADDR" \
  --source-dir /tmp/netdisk_demo_folder
```

接收端仍使用 `recv`，会收到 zip 文件。

## 10）可选：浏览器演示

打开：

```text
http://127.0.0.1:8080/demo/
```

## 11）收尾清理（删除与登出）

### 11.1 删除文件（验证文件删除）

```bash
curl -sS -X DELETE "$API/api/v1/files/$FILE_ID" -H "Authorization: Bearer $TOKEN_A"
```

### 11.2 删除文件夹（需先空目录）

```bash
curl -sS -X DELETE "$API/api/v1/folders/$FOLDER_ID" -H "Authorization: Bearer $TOKEN_A"
```

### 11.3 登出

```bash
curl -sS -X POST "$API/api/v1/auth/logout" -H "Authorization: Bearer $TOKEN_A"
```
