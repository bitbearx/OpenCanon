# 执行方案
按已批准 local://home-print-plan.md 实施，先记录驱动安装外部前提，随后逐步构建并验证服务、文档、打印、网页、部署。

后端负责 cmd/opencanon、internal/{document,printer,store,httpapi}、go.mod，严格遵循批准的 API 与安全限制。前端负责 internal/web，保留 design 原文件。集成负责真实驱动配置、部署、文档与最终验收。缺真实 profile 时必须 unavailable，禁止捏造能力。

模块导航：cmd/opencanon 入口；internal/httpapi HTTP；internal/discovery Bonjour 发现与地址持久化；internal/document 文档；internal/printer CUPS；internal/store 生命周期；internal/web 网页；config 驱动能力；deploy LaunchAgent。
