# NetDisk Dev Log

## Day 1

- 时间：2026-04-06
- 完成：
  1. 读了新版题目，补全并重写了规划文档（流程、看板、API、数据库草案、DevLog 模板）。
  2. 写了最小可运行后端入口，新增 `GET /health`。
  3. 统一了健康检查接口返回 JSON 结构（code/message/data）。
  4. 修复了 `test/test.go` 的未使用导入，`go test ./...` 可通过。
  5. （续）实现文件核心接口：上传、列表、下载、重命名、删除。
  6. （续）增加统一错误返回与 JSON 输出工具函数（writeError/writeJSON）。
  7. （续）按学习需求补全详细中文注释（依赖说明、路由分发、文件流、锁、错误处理）。
  8. （记录）确认 `.gitignore` 本身需要被提交；它的作用是告诉 Git 忽略哪些文件，而不是忽略自己。

## Day 2

- 时间：2026-04-08
- 完成：
  1. 对于文件核心接口：上传、列表、下载、重命名、删除进行了测试，基本符合流程。
  2. 在**保留原有内存索引作为缓存**的基础上，增加了 ​**MySQL 数据库持久化层**​，解决了“服务重启元数据丢失”的核心问题。
  3. 把该死的每次push都输入token改掉了,烦死了。

## Day 3

- 时间：2026-04-09

- 完成：

  1. 测试了本地的MySQL部署下的数据可持久化的上传、列表、下载、重命名、删除。

  2. 决定继续采用内存缓存+MySQL。

  3. 增加了用户注册登录、Token 认证、文件与用户绑定。

  4. 修复了有关MySQL默认不允许多语句的报错。

  5. 通过了关于用户注册/重复校验、JWT 登录鉴权、文件上传绑定、跨用户隔离保护、登出失效、服务重启后数据持久化等测试。

  6. 理解了约定式提交规范（所以前面不按照规范提交真的对不起）。

  7. 实现了文件夹相关功能（待测试）
     现在数据库表中存在着files表和folders表，两者都包含ID、名称、所属用户、所在文件夹（可以为空，即为root目录）。
     与文件的内存缓存+MySQL不同，文件夹直接走数据库。

  8. 修复了不能查看用户根目录下的文件（夹）子项的问题。

  9. 完成了文件夹管理相关功能的测试。

  10. 对代码进行了MVC重构并顺利通过了之前的测试，终于从一坨大史山变成多坨小史山了。

      - models: APIResponse, FileRecord, FolderRecord, AuthUser
      - config: 加载环境变量，存储配置
      - db: 全局DB连接，内存索引FilesByID和锁，初始化表，加载文件到内存
      - utils: 工具函数（生成ID、hash密码、解析路径、JSON响应等）
      - middleware: 认证中间件和相关函数
      - handlers: 各个HTTP处理函数（分文件：health, auth, files, folders）

      ```text
      handlers → middleware → models, utils, db
      handlers → db, models, utils, config
      db → models
      middleware → db, models, utils
      utils → models
      config 无外部依赖
      ```

## Day 4

- 时间： 2026-4-10

- 完成：

  1. 增加了分享功能，本次改动实现了创建文件或文件夹的分享链接（包括合法性检验），列出当前用户的分享链接（倒叙排列），删除和软撤销（保留记录与否）两种取消链接的方式，分享还可可设置密码，有效期，有效次数等限制，文件夹分享返回树状的json（后续应该加入支持zip打包），此外还有删除文件时同步失效。

  2. 测试过程中发现下载时候如果不指定文件名，会默认采用路径最后一段为文件名，如果以token为路径的话，会有泄漏风险，尝试给所有下载返回（包括错误）均设定文件名。（已修复）

  3. 测试时发现不能一键删除（虽然说是为了防止误删，但是还是有点麻烦）考虑一下要不要加上一个递归查询加删除的功能

  4. 测试时发现了一个错误返回的问题（返回的错误码不正确）

     ```go
     name, err := resolveShareNodeName(ownerID, req.NodeType, req.NodeID)
     if err != nil {
         if errors.Is(err, dbErrNotExist) {
             utils.WriteError(w, http.StatusNotFound, 10005, "node not found") // 预期走这里
             return
         }
         utils.WriteError(w, http.StatusInternalServerError, 10110, "failed to resolve node")
         return
     }
     ```

     （已修复）

  5. 发现分享不支持head请求（已修复）
