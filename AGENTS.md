# 执行方案
当前目标：Go BJNP 直接连接 Canon E560，Poppler 与独立 Gutenprint 辅助程序生成设备流，运行时不依赖 macOS Canon 驱动或 CUPS 服务。具体能力、来源与验收边界见 README.md、config/README.md 和 native/render/NOTICE。

保持同源保护、文档生命周期、设置校验和 uncertain 禁止重印；缺精确型号、编码依赖或可解释设备状态时必须 unavailable。Gutenprint E560 标为 EXPERIMENTAL，模拟设备 ACK 不等于实物出纸。保留 design 原文件，不自动提交或部署。

模块导航：cmd/opencanon 入口与设备探测；internal/httpapi HTTP；internal/bjnp Go BJNP 协议；internal/discovery BJNP 发现与地址持久化；internal/document 文档；internal/render 渲染调度；native/render Gutenprint 辅助程序与许可；internal/printer 能力与串行提交；internal/store 生命周期；internal/web 网页；config 设备能力；deploy 可选 LaunchAgent。
