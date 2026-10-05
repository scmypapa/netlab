# Netlab

独立组网与虚拟资产管理软件。使用 KVM/libvirt、containerd 和 OVN/OVS，管理 Windows、Linux 虚拟机与容器的模板、运行环境、网络、热更新、运维和流量观测。工作台与自动化调用使用同一套 API。

- [安装与更新](docs/installation.md)
- [v1.0.0 发布记录](docs/release-v1.0.0.md)
- [API 契约](api/openapi.yaml)
- [产品与架构设计](design/design.md)
- [实现范围与实测记录](docs/implementation.md)

正式发布包由 GitHub Actions 构建，目标为 Ubuntu 24.04 x86-64。当前实现与已执行验证以实施记录为准。
