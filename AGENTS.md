# Netlab 开发约定

- 本项目独立于 YINYU；设计依据为 `design/design.md` 和 `design/product-comparison.md`。
- 使用 Go、PostgreSQL、libvirt、containerd、OVN/OVS、React/TypeScript。所有产品能力经过同一 `/api/v1` 接口。
- 配置、运行事实、画布视图分别保存；操作任务只有一套，成功依据执行面结果。
- 复用成熟运行时接口，不拼 shell 字符串，不增加影子状态、兼容层和备用执行链。
- 校验集中于业务所有者，只处理归属、并发正确性、资源和安全边界。
- 前端使用对象上下文、分栏和局部工作区；不展示内部编号、摘要、票据或设计说明。
- OpenAPI 是产品契约源；Go 与 TypeScript 类型生成，生成文件不手改。
- 并行改动使用独立 worktree；不得覆盖其他人的文件。全程简体中文沟通。
- 定向测试后执行构建。真实执行面验收与单元测试分开记录，不以模拟结果代替真实结果。
