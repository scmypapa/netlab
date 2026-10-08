# Netlab

Netlab 是一个用来快速搭建**隔离网络环境**、管理**虚拟机与容器混合拓扑**的独立组网平台。

无论你是要做网络实验、工控协议仿真、安全攻防靶场，还是混合多节点的教学培训，Netlab 的目标都是让你像画架构图一样：拉几台 Windows、几台 Linux、几个容器，给它们划好子网、配上限速或丢包规则，一键启动。在浏览器里就能直接开 VNC/RDP 运维，随时点开任意接口抓包，玩坏了随时用快照恢复。

系统使用同一套 `/api/v1` 提供 Web 工作台与外部平台对接，底层不依赖 Kubernetes，采用 Linux 原生组件配合 systemd 守护，简单稳定。

---

## 为什么用 Netlab？

网络实验和混合系统测试通常需要同时处理三个问题：

* **混合资产**：在同一拓扑中运行 Windows、Linux 和容器。
* **可调整的网络**：隔离网段、连接多网卡，设置时延、丢包和带宽，并在运行中应用变更。
* **统一运维**：从浏览器连接资产、管理文件、观察流量和恢复环境。

**Netlab 的解法：**

1. **混部同构**：同一个子网里既可以跑 Windows 虚拟机（支持 TPM 2.0、Secure Boot 与 AD 域控），也可以跑 QEMU Linux 和轻量 containerd 容器。
2. **SDN 真实组网**：基于 OVN/OVS 构建分布式虚拟网络，支持跨物理机同网段、重叠 IP 段隔离，支持 Linux `netem` 动态链路注入（时延/丢包/带宽限制）。
3. **内置观测与调试**：直接在资产真实接口抓包并导出 `.pcapng`，内嵌基于 sFlow 的实时拓扑流量图谱。
4. **支持热变**：网络结构不用推倒重来，在前端画布调整接口和网段，保存草稿、预览差异后应用到运行环境。

---

## 整体架构

Netlab 采用 **Controller（控制面）** + **Node（执行节点）** 分布式设计，可以单机一体化安装，也可以横向添加多个执行节点。

```text
  浏览器工作台 / 外部业务平台
              │
          /api/v1 (REST / WebSocket)
              │
   ┌──────────▼──────────┐
   │  Netlab Controller  │  (Go 1.26 + pgx / sqlc)
   └──────────┬──────────┘
              ├─ PostgreSQL       (元数据、操作任务、排版与草稿)
              ├─ VictoriaMetrics  (10s 周期指标采集与历史度量)
              │
           mTLS 安全加密通道 (TCP 9443)
              │
   ┌──────────▼──────────┐
   │     Netlab Node     │  (执行节点)
   └──────────┬──────────┘
       ┌──────┴──────┬────────────┬─────────────┐
       ▼             ▼            ▼             ▼
   libvirt/KVM   containerd    OVN / OVS      存储与远程连接
  (Windows/Linux) (OCI 容器)   (SDN/Geneve)   (本地/Btrfs/Ceph/Guacamole)
```

- **控制通信与业务网络**：控制面与节点通过 mTLS 认证通信；跨节点业务流量通过 Geneve 隧道（UDP 6081）传输。
- **解耦设计**：执行面依赖 Linux 原生接口，不依赖 Redis，无重型消息队列；即便控制面短暂重启，执行节点的虚拟机与容器网络不会中断。

---

## 核心能力一览

### 1. 虚拟化与容器

* **虚拟机（QEMU/KVM）**：支持 BIOS、UEFI、Secure Boot；支持 vTPM、独立 NVRAM、CPU 拓扑、来宾 NUMA、cloud-init / Cloudbase-Init 初始化。
* **容器（containerd + runc）**：支持标准 OCI/Docker 镜像、持久化卷、资源限额（cgroups）以及重启策略。
* **模板兼容**：支持 `qcow2`、`raw`、`vmdk`、`ova`、`ovf`、`iso`、`oci/docker` 镜像，支持把配好的 VM 直接固化为新模板。
* **已验证 Guest**：Ubuntu 24.04、Windows 11、Windows Server 2022/2025，以及双机 Windows AD 域控环境（验证过 Generation ID 与安全通道恢复）。

### 2. 软件定义网络（SDN）与链路模拟

* 多网段、多网卡、同节点/跨节点同网段打通。
* 独立网段之间允许 IP/MAC 完全重叠而不产生冲突。
* 支持接入已有物理 LAN 与 VLAN，保留已有网关并管理 Netlab 资产的地址。
* 链路级 QoS：实时注入**丢包、延迟、带宽限制**，随配随撤销。
* 访问与发布：内置 WireGuard VPN 网关直通拓扑内部，支持按需暴露资产端口（TCP/UDP）。

### 3. 运维、监控与流量观测

* **免客户端运维**：网页原生集成终端（xterm.js）、VM VNC、串口（Serial）、SSH，以及由 Guacamole 驱动的 RDP（支持剪贴板与窗口缩放自适应）。
* **接口抓包**：直接从虚拟网卡采集，保留真实网段与接口身份。支持 BPF 规则过滤，直出 PCAPNG 文件。
* **流向可视化**：基于 sFlow（采样率 1/512）在画布上实时绘制通信拓扑箭头与连接明细。
* **时序监控**：收集 CPU、内存（VM 采 QEMU 进程 RSS，容器采 cgroup）、磁盘 I/O、网卡速率与丢包计数，存入 VictoriaMetrics。

### 4. 存储、快照与灾备

* **存储支持**：本地目录、Btrfs CoW 原生快照、Ceph RBD 共享块存储。
* **共享镜像**：单机使用本地基础盘与独立写入层；多节点自动优先使用共享池。每个池、模板版本保存一份基础盘，实例原生克隆，其他节点仅传输模板配置、固件和挂载介质。
* **一致性恢复点**：
  * Linux VM：通过 QEMU Guest Agent 冻结文件系统。
  * Windows VM：通过 QGA 触发 VSS 卷影复制，实现应用级一致性快照。
* **备份迁移**：集成 Restic 支持将恢复点加密/去重备份至 S3 对象存储；支持 Ceph VM 在线热迁移以及带卷跨节点迁移。

---

## 硬件与环境要求

* **安装目标**：Ubuntu 24.04 LTS (x86_64)
* **CPU**：运行 VM 的节点开启硬件虚拟化（Intel VT-x 或 AMD-V）；虚拟机宿主上的执行节点开启嵌套虚拟化。
* **内存**：按运行资产的规格与宿主开销规划；节点上报实际容量供自动调度。
* **磁盘**：本地目录即可运行；Btrfs 提供原生快照，Ceph RBD 提供共享存储。

**常用通信端口：**

* `TCP 8090`：控制面 Web 控制台与 API
* `TCP 9443`：控制面 ↔ 节点 mTLS 安全端口
* `TCP 6641 / 6642`：OVN 数据库通信
* `UDP 6081`：节点间 Geneve 跨主机网络隧道

---

## 快速上手

发布包包含控制面、执行节点、前端，以及 VictoriaMetrics、Restic 和 guacd；安装器配置宿主依赖与服务。使用具有仓库读取权限的 GitHub 账号登录 `gh` 后下载。

### 1. 部署控制面（以单机 IP 10.0.0.10 为例）

```bash
# 获取发布包并解压
gh release download v1.0.1 --repo scmypapa/netlab --pattern netlab_linux_amd64.tar.gz
mkdir netlab-release && tar -xzf netlab_linux_amd64.tar.gz -C netlab-release

# 运行自动化安装脚本
sudo bash netlab-release/scripts/install-controller.sh \
  /etc/netlab/controller.env \
  10.0.0.10
```

安装脚本会自动配置好 PostgreSQL、OVN 数据库、系统 CA、VictoriaMetrics 并启动 systemd 服务。

- 访问：`http://10.0.0.10:8090`
- 初始账号：`admin`
- 初始密码：保存在 `/etc/netlab/controller.env` 文件中

### 2. 接入执行节点（以目标节点 10.0.0.11 为例）

先在**控制面节点**签发节点专属证书：

```bash
sudo bash /opt/netlab/current/scripts/issue-node-certificate.sh \
  10.0.0.11 \
  /root/netlab-node-certs

# 将同一发布包和节点证书传到执行节点
scp netlab_linux_amd64.tar.gz user@10.0.0.11:/tmp/
sudo tar -C /root -cf - netlab-node-certs | \
  ssh user@10.0.0.11 'tar -xf - -C /tmp'
```

登录到**执行节点**完成安装：

```bash
# 解压并安装执行节点
mkdir netlab-release
tar -xzf /tmp/netlab_linux_amd64.tar.gz -C netlab-release
sudo bash netlab-release/scripts/install-node.sh \
  10.0.0.10 \
  /tmp/netlab-node-certs
```

最后在 Web 界面的 **“资源” -> “节点”** 页面，登记节点地址 `https://10.0.0.11:9443` 即可。

---

## 本地开发与代码编译

如果你想二次开发或自己打包：

### 依赖项

* Go `1.26`
* Node.js `22` / pnpm `10.33.4`
* Linux 节点构建依赖：`libvirt-dev` 与 C 编译工具链；节点运行依赖由安装脚本配置。
* 浏览器测试：当前 Playwright 配置使用 Microsoft Edge。

### 步骤

```bash
# 1. 启动本地开发依赖 (PostgreSQL + VictoriaMetrics)
docker compose -f deploy/compose.dev.yaml up -d

# 2. 生成代码 (OpenAPI 接口模型与 sqlc 查询)
go generate ./api
go generate ./db

# 3. 构建前端
cd web
pnpm install --frozen-lockfile
pnpm generate:api
pnpm build
cd ..

# 4. 构建二进制
go build -o bin/netlab-controller ./cmd/controller
go build -tags libvirt_dlopen -o bin/netlab-node ./cmd/node
```

运行测试：

```bash
go test -tags libvirt_dlopen ./...
go vet -tags libvirt_dlopen ./...
cd web && pnpm test
```

---

## v1.0.0 实测与基准数据

以下为真实执行环境的单轮测量。

### 双节点并发负载实测

- **拓扑规格**：2 台节点（其中 1 台为嵌套 VM 节点），部署 4 个容器 + 2 台真实 Ubuntu VM。
- **并发压力**：每节点 64 并发（总计 128 客户端），请求持续 60 秒，单次响应载荷 2 MiB。
- **同时运行任务**：跨节点通信、sFlow 采样、接口抓包与 10s 资源采集。抓包每节点 256 MiB，达到文件额度后结束。
- **测试结果**：
  - 完成请求数：**12,677 次**（失败数：0）
  - 综合吞吐量：**417.97 MiB/s**
  - P95 耗时：`1309.12 ms` / `1915.52 ms`（含 2 MiB 数据传输全过程）
  - 两节点内核抓包丢包数：`162` / `18,083`
  - 环境创建与来宾服务准备步骤：`46.508 s`

### 典型运维操作耗时参考

* **Ceph 共享存储 Ubuntu VM 在线热迁移**：`7.97 秒`（迁移后 SSH 与网络状态核验正常）
* **容器/VM 携带独立持久卷跨节点迁移**：`10.17 秒`
* **Windows Server 2025 VSS 快照与解冻**：`57.11 秒`
* **已有双 Windows AD 环境恢复、Generation ID 与域数据核对**：`146.18 秒`

---

## API 规范与权限设计

平台设计严格遵循 API 驱动，控制台前端与外部平台共用 `/api/v1`。接口文档位于 `api/openapi.yaml`。

所有异步操作均返回一个统一的 `Operation ID`，通过轮询该任务可获得阶段明细与错误回溯。

```bash
# 外部系统通过 Service Token 鉴权
curl -H "Authorization: Bearer <Your-Token>" \
     http://10.0.0.10:8090/api/v1/environments
```

**细粒度权限模型**：
权限作用域覆盖项目、环境与单项资产。支持把**远程会话**与**文件传输**完全解耦（例如：允许学生通过 RDP/SSH 连接测试靶机，但剥离其文件上传/下载的权限）。

---

## 版本更新

从账号菜单打开「系统更新」，查看 GitHub 正式发布日志并选择版本。更新器等待现有任务完成，逐节点更新后切换控制面；程序保存在版本目录，数据保留。私有仓库的更新访问令牌配置为 `NETLAB_GITHUB_TOKEN`。

---

## 许可证与第三方组件

- 本项目代码许可证待定，当前源码及发布包保留所有权利。
- 项目各组件尊重并保留上游开源项目的协议：QEMU/KVM、libvirt、containerd、OVN/Open vSwitch、VictoriaMetrics、Restic、Apache Guacamole、noVNC、React Flow。

## 文档

- [安装与更新](docs/installation.md)
- [API 契约](api/openapi.yaml)
- [v1.0.0 发布记录](docs/release-v1.0.0.md)
- [v1.0.1 发布记录](docs/release-v1.0.1.md)
- [实现与实测记录](docs/implementation.md)
- [存储、虚拟机与观测](docs/storage-vm-observation.md)
