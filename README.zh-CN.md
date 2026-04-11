# NetDisk（中文说明）

一个基于 Go 的网盘后端项目，包含认证、文件管理、分享、对象存储迁移、NFS 挂载和 P2P 直传演示能力。

英文版请看 [README.md](README.md)。

## 功能概览

- 用户注册、登录、登出、个人信息管理
- 文件上传、下载、重命名、删除
- 文件夹管理与节点移动
- 分享能力
  - 普通下载分享 `public_download`
  - P2P 分享 `p2p_file`（支持文件与文件夹，文件夹会自动打包为 zip）
- OSS 迁移
- NFS 挂载访问（支持多用户路径映射与可选挂载认证）
- P2P 信令交换与局域网演示客户端

## 快速启动

### 1. 环境变量

至少配置：

```bash
export MYSQL_DSN='user:password@tcp(127.0.0.1:3306)/netdisk?parseTime=true&charset=utf8mb4'
export PORT=8080
```

可选（NFS）：

```bash
export NFS_ENABLE=1
export NFS_ADDR=':2049'
export NFS_REQUIRE_MOUNT_AUTH=0
# export NFS_MOUNT_AUTH_MODE=token
```

可选（OSS）：

```bash
export OSS_ENDPOINT='your-endpoint'
export OSS_ACCESS_KEY_ID='your-ak'
export OSS_ACCESS_KEY_SECRET='your-sk'
export OSS_BUCKET='your-bucket'
```

### 2. 启动服务

```bash
go run ./cmd/server
```

### 3. 打开演示前端

- `http://127.0.0.1:8080/demo/`

## 手动演示流程

完整可复制命令流程见：

- [docs/manual-demo.md](docs/manual-demo.md)

## 目录结构（核心）

- `cmd/server`：服务端入口
- `cmd/p2pclient`：P2P 演示客户端
- `handlers`：HTTP 业务处理
- `db`：数据库与持久化
- `nfsadapter`：NFS 文件系统适配
- `storage`：对象存储后端
- `web/demo`：简易演示前端
