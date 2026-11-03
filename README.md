# OpenCanon 家庭打印

在当前 Mac 上运行的局域网打印网站。上传一个 PDF 或静态 JPEG、PNG、HEIC／HEIF、WebP，选择纸张、页码、份数、色彩和单双面，提交到 Canon E560 的本机 CUPS 队列。无需登录，不包含公网发布。

## 本机运行

需要 Go 1.26.1、macOS 的 CUPS／sips、Poppler 的 pdfinfo，以及官方 Canon E560 驱动和 `OpenCanon_E560` 队列。实际映射和依据见 [config/README.md](config/README.md)。不要将通用 PostScript 队列替代该驱动。

```sh
brew install poppler
go test ./...
go build -o bin/opencanon ./cmd/opencanon
./bin/opencanon -listen 0.0.0.0:8088 \
  -printer-config config/printer.json \
  -data-dir "$HOME/Library/Application Support/OpenCanon/data" \
  -pdfinfo /opt/homebrew/bin/pdfinfo
```

本机 8080 已被其他服务占用，因此 LaunchAgent 使用 8088；程序的默认监听参数仍为 `0.0.0.0:8080`。当前家庭地址为 `http://192.168.50.14:8088`，IP 变化时以启动日志列出的局域网地址为准。手机须连接相同 Wi-Fi。系统若询问入站网络授权，请在系统窗口允许。

Mac 必须开机并连接家庭网络，休眠时网站不可用。程序不更改睡眠策略、路由器设置或系统默认打印机。不要在不可信网络运行无登录服务，不要配置端口转发或公网隧道。

缺少配置、pdfinfo 或队列时首页仍可访问，但会显示具体原因并禁止打印。调整安装或配置后重启服务，再点击网页“重新扫描”。“队列可接收”仅指本机 CUPS 接受任务，不证明打印机在线。

## 打印机地址发现

页面不再固定显示打印机 IP。首次没有保存记录时，页面自动请求后端扫描；之后显示上次保存的地址及发现时间，点击“重新扫描”会重新发现并保存。扫描期间禁止重复点击和打印确认，已有纸张、色彩、份数等设置保留。多个页面同时打开时共享正在进行的扫描结果。

发现过程从固定 Canon 队列的实际设备 URI 获取 Bonjour 服务名，通过 macOS `dns-sd` 解析主机与局域网 IPv4。仅发现当前已配置的 Canon 网络设备，不进行任意网段探测或自动添加其他型号。实际打印继续使用 Canon Bonjour 设备 URI，IP 变化时无需重建队列。

成功结果以 0600 权限原子保存到 `-data-dir/printer-discovery.json`，包含名称、IP、主机名、设备 URI 和发现时间；服务重启会恢复。该文件独立于会在启动时清理的上传目录。扫描失败保留上次确认记录并显示失败原因；缓存地址和“上次发现”时间不证明设备当前在线。地址冲突或已过期的 DNS 记录不会覆盖保存结果。

已实测真实 Bonjour 扫描、再次扫描更新时间、重启读取及两个页面同时首次打开；扫描相关改动后的完整测试为 32 项通过。本次验证未提交打印任务。


## Docker 运行

容器运行网页和文档处理，Mac 继续运行 CUPS、Canon 驱动及固定打印队列。容器通过 IPP 访问 Mac 的 CUPS；Mac 必须保持开机且连接打印机所在网络。Linux 容器使用 ImageMagick 和 img2pdf 转换图片。设备发现模块依赖 macOS 的 `dns-sd` 和本地 CUPS socket，容器内的设备扫描目前不可用；队列状态和打印独立于扫描。

启动前，需让容器访问 Mac 的 CUPS，并限制接入范围。当前机器使用 OrbStack，实测 Mac 虚拟网卡地址为 `192.168.139.3`，容器连接的来源为 `192.168.139.2`；地址可能随环境变化。本机已完成以下配置并验证容器可以访问队列，其他机器需要管理员自行配置：

1. 备份 `/etc/cups/cupsd.conf`，保留原有本机监听和管理策略。增加仅绑定虚拟网卡地址的 `Listen 192.168.139.3:631` 及 `ServerAlias 192.168.139.3`；在已有 `<Location />` 的 `Order allow,deny` 后加入 `Allow 192.168.139.2`。不要改为监听 `*:631`，不要放宽 `/admin` 或管理操作的认证。
2. 使用 `sudo cupsd -t` 检查配置，确认无误后通过 `sudo launchctl kickstart -k system/org.cups.cupsd` 重启 CUPS。检查系统打印队列仍能正常工作；失败时恢复备份。所有使用同一个 OrbStack 虚拟机的容器可能共享这个来源地址，因此它们也能访问该打印接口，只应运行可信容器。
3. 验证容器可以访问固定队列；若队列未共享或 CUPS 拒绝远程访问，需要检查队列共享和访问策略。IP 地址改变时需重新核对监听、授权和下面的环境变量；其他 Docker 环境的宿主地址及来源也需要实测，不能照搬。

本机 `.env` 已保存 CUPS 地址及 8088 端口，文件不纳入版本管理。其他机器在仓库目录创建 `.env`，设置 `CUPS_ADDRESS=ipp://<宿主机地址>:631`；可选设置 `OPENCANON_PORT`。

```sh
docker compose up -d --build
docker compose logs -f
```

在 Mac 上访问 `http://localhost:8088`，同一家庭 Wi-Fi 的设备访问 `http://<Mac 的局域网 IP>:8088`。本机已停用旧 LaunchAgent，避免占用端口；切回本机服务时，先停止容器，再执行 `launchctl enable "gui/$(id -u)/local.opencanon"` 并按下文 bootstrap。完成后执行 `docker compose down`；容器重建或服务重启后旧上传失效。不要把网站或 CUPS 暴露到公网或不可信网络。

已通过容器提交一份 A4 单面黑白测试页（任务编号 `OpenCanon_E560-10`）；CUPS 返回 completed，报告已处理 1 张纸，实际出纸仍需现场确认。应用现有状态模型对已完成任务返回 unknown。

## 常驻运行

[deploy/local.opencanon.plist](deploy/local.opencanon.plist) 使用此 Mac 用户的绝对路径；换用户或安装位置时须同步修改所有路径。构建后将程序、配置和 plist 安装到对应位置：

```sh
install -d -m 700 "$HOME/Library/Application Support/OpenCanon"
install -m 700 bin/opencanon "$HOME/Library/Application Support/OpenCanon/opencanon"
install -m 600 config/printer.json "$HOME/Library/Application Support/OpenCanon/printer.json"
install -d "$HOME/Library/LaunchAgents"
install -m 600 deploy/local.opencanon.plist "$HOME/Library/LaunchAgents/local.opencanon.plist"
launchctl bootstrap "gui/$(id -u)" "$HOME/Library/LaunchAgents/local.opencanon.plist"
```

已注册服务更新时，先 `launchctl bootout "gui/$(id -u)/local.opencanon"`，替换文件后重新 bootstrap。不要同时启动两个使用相同数据目录的进程：应用启动会清理上次运行遗留的上传目录。

查看服务：`launchctl print "gui/$(id -u)/local.opencanon"`。日志为应用目录中的 `service.log` 和 `service-error.log`。停用服务使用上面的 bootout 命令；重新登录前若不希望再启动，还需移除对应的用户 LaunchAgent plist。此服务不使用 root LaunchDaemon。

## 文件与任务

单文件最大 20 MiB，请求体最大 21 MiB；份数 1–99。PDF 真实页数由 pdfinfo 读取，损坏、密码保护或禁止打印的文件会被拒绝。页码支持 `1-3,5`、中文逗号及分隔符周围空白，去重并升序合并，拒绝倒序、空片段和越界。图片仅有第 1 页。HEIC 多张静态图仅取首图；不导入相册，不支持动画、SVG 或 TIFF。

上传文件在应用中保留 1 小时，每 5 分钟清理过期文件；重启立即失效，旧页面需重新上传。CUPS 接收任务后立即清理应用副本，系统的 spool 和日志由系统管理。不要将这理解为清除了系统打印记录。

自动双面仅支持 A4／Letter 普通纸。奇数页的最后背面留白，用纸估算逐份计算，例如 3 页 × 2 份双面约 4 张纸。长／短边始终指纸张实际边；竖向长边通常左右翻，短边上下翻，横向相反。灰度不保证只使用黑墨。

成功提示为“已提交到打印队列”，不是物理完成。同一 documentId 重复提交返回相同任务 ID；结果不明时禁止再次提交该文档，先查看 Mac 系统打印队列。不要通过重新上传绕过不确定提示，避免重复纸张。任务最多自动查询 60 秒；活动任务消失但无法确认结果时显示 unknown。

## 接口

- `GET /api/printer`：实际 profile、禁止组合、队列可接收状态和 `discovery`；顶层 `address` 来自已保存记录，无记录为空，不触发扫描。
- `POST /api/printer/scan`：无请求体和查询参数，要求 `X-OpenCanon-Request: 1`；200 返回 `{discovery}`，409 表示扫描正在进行，503 表示发现失败。
- `POST /api/documents`：multipart 单字段 `file`；201 返回 `{id,name,kind,pageCount,expiresAt,previewUrl}`，不返回文件路径。
- `GET /api/documents/{id}/preview`：图片 JPEG 预览；PDF 无预览阅读器。
- `POST /api/print-jobs`：JSON `{documentId,mediaId,pageRanges,copies,colorMode,duplex}`，同时要求 `X-OpenCanon-Request: 1`。纸张 ID 来自能力接口；colorMode 为 color／mono，duplex 为 single／long／short。
- `GET /api/print-jobs/{id}`：仅查询本进程提交的任务，返回 waiting／processing／failed／unknown。

变更请求校验同源 Origin，不开放跨域 CORS。错误体统一为 `{error:{code,message}}`；404 表示未知或过期，413 超限，422 文档／设置无效，503 缺打印前提或提交结果不明。

## 模块

入口 `cmd/opencanon`；HTTP 与同源保护 `internal/httpapi`；Bonjour 发现及地址持久化 `internal/discovery`；文件识别和转换 `internal/document`；队列映射与提交 `internal/printer`；内存状态及文件生命周期 `internal/store`；嵌入式网页 `internal/web`。`design/index.html` 保留为原型来源，运行网页不再使用演示页数或模拟发送计时器。

## 本机验证记录

已运行 `go test ./...`（27 项通过）及构建；真实服务 API 冒烟 23 项通过，包含 5 页 PDF、PNG／JPEG／HEIC 转换与预览、损坏／加密／禁止打印拒绝、413、非法设置及同源保护。另验证静态 WebP 可上传、动画 WebP 拒绝。EXIF 1–8 的 PDF 渲染和 JPEG 预览共 16 项与 ImageIO 基准比较通过，使用生成的带方向样本，未代替真实手机照片的现场验收。

浏览器已检查 320×640、390×844、1280×900，无横向溢出；验证工单 3 页／份 × 2 份长边双面约 4 张纸、取消后修改、超限保留设置、HEIC 预览、非法组合禁用、60 秒停止查询和重启后重新上传提示。LaunchAgent 已在 gui/501 注册，退出终端所属的验证服务后，局域网地址仍可访问；重启 LaunchAgent 后旧文档返回 404。

本机 CUPS 即使设置 LC_ALL=C 仍可能输出中文，localhost:631 也曾无法连接。因此状态查询使用 macOS CUPS 域套接字 `ipp://%2Fprivate%2Fvar%2Frun%2Fcupsd` 的结构化 IPP 属性，不要求设备开放 IPP。提交成功只接受实测的中文或标准英文 lp 完整成功响应；标准错误和仅含任务编号的诊断不算成功。不明结果保持 uncertain，避免自动重印。

真实任务已提交到固定队列：`OpenCanon_E560-1`（5 页样本仅 1–3 页，2 份，彩色长边）、`-2`（2 页彩色短边）、`-3`（单页彩色）、`-4`（单页单色）。同文档重复请求返回 `-1`，应用源文件已删除。最后观察到 `-1` 为 processing，CUPS 报告已处理 4 张纸，`-2`／`-3`／`-4` 为 waiting；这不是物理完成证明。另有两个仅用于验证成功响应的 held 任务 `-5`／`-6`，已在输出纸张前取消。

尚需现场完成：确认实际出纸、正反长／短边方向、奇数页末尾背面留白、彩色／单色和逐份排序；装入对应纸张后验收其他尺寸；断网恢复后确认没有重复打印；同 Wi-Fi 手机实际访问。现有队列中的待处理测试任务保留，不会由网站自动重复提交。
