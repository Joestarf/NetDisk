# NetDisk

A Go-based cloud-drive backend with authentication, file management, sharing, OSS migration, NFS mount support, and LAN P2P transfer demo.

## Language Note

GitHub does not require README to be in English. English, Chinese, or bilingual docs are all acceptable.

Chinese version is available at [README.zh-CN.md](README.zh-CN.md).

## Features

- User register/login/logout/profile
- File upload/download/rename/delete
- Folder operations and node move
- Share types
  - `public_download`
  - `p2p_file` (file/folder, folder is zipped before transfer)
- OSS migration path
- NFS mount (multi-user path mapping + optional mount auth)
- P2P signaling APIs and demo client

## Quick Start

### 1) Environment Variables

Required:

```bash
export MYSQL_DSN='user:password@tcp(127.0.0.1:3306)/netdisk?parseTime=true&charset=utf8mb4'
export PORT=8080
```

Optional (NFS):

```bash
export NFS_ENABLE=1
export NFS_ADDR=':2049'
export NFS_REQUIRE_MOUNT_AUTH=0
# export NFS_MOUNT_AUTH_MODE=token
```

Optional (OSS):

```bash
export OSS_ENDPOINT='your-endpoint'
export OSS_ACCESS_KEY_ID='your-ak'
export OSS_ACCESS_KEY_SECRET='your-sk'
export OSS_BUCKET='your-bucket'
```

### 2) Run

```bash
go run ./cmd/server
```

### 3) Demo UI

Open:

- `http://127.0.0.1:8080/demo/`

## Copy-Paste Demo Flow

Use the full manual script in:

- [docs/manual-demo.md](docs/manual-demo.md)

## P2P Demo Client

Commands:

- `go run ./cmd/p2pclient host ...`
- `go run ./cmd/p2pclient recv ...`

For folder transfer, use `--source-dir`; it is packed into zip automatically.

## Key Directories

- `cmd/server`: backend entry
- `cmd/p2pclient`: P2P demo CLI
- `handlers`: HTTP handlers
- `db`: DB and persistence
- `nfsadapter`: NFS adapter
- `storage`: object storage backend
- `web/demo`: lightweight demo frontend

## FAQ

### Why folder transfer uses zip in P2P mode?

Current P2P data channel is a single stream transfer model. Folders are zipped first for a stable and simple demo flow.

### Why NFS mount may require sudo?

That is OS-level mount permission, not NetDisk account permission.
