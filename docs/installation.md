# 安装与更新

## 主站

安装目标为 Ubuntu 24.04 x86-64。主站与执行节点可以部署在同一台机器；运行虚拟机的节点需具备 KVM。

从仓库 Releases 下载 `netlab_linux_amd64.tar.gz`，解压后安装。以下 `10.0.0.10` 为主站管理网地址。

```bash
mkdir netlab-release
tar -xzf netlab_linux_amd64.tar.gz -C netlab-release
sudo bash netlab-release/scripts/install-controller.sh /etc/netlab/controller.env 10.0.0.10
```

安装器配置 PostgreSQL、OVN 中央数据库、节点 CA 和资源指标服务。主站目录为 `/opt/netlab`，业务数据目录为 `/var/lib/netlab`。面板地址为 `http://10.0.0.10:8090`；初始账号 `admin`，随机密码保存在 `/etc/netlab/controller.env` 的 `NETLAB_ADMIN_PASSWORD`。配置文件属于 root 和 netlab 组。

已有 PostgreSQL 可通过安装命令的第一个参数提供配置文件，其中写入 `NETLAB_DATABASE_URL`、首次管理员密码与节点证书路径。安装器仍配置本机 OVN 和指标服务。

## 执行节点

在主站签发节点证书。以下 `10.0.0.11` 为节点管理网地址，证书目录为新建目录。

```bash
sudo bash /opt/netlab/current/scripts/issue-node-certificate.sh 10.0.0.11 /root/netlab-node-certs
```

将生成的 `ca.crt`、`node.crt`、`node.key` 经管理网的 SSH 文件传输复制到节点 `/root/netlab-node-certs`。CA 私钥保留在主站。

在节点解压同一版本发布包，执行：

```bash
sudo bash netlab-release/scripts/install-node.sh 10.0.0.10 /root/netlab-node-certs
```

节点目录为 `/opt/netlab-node`，资产及缓存保存在 `/var/lib/netlab-node`。安装器配置 containerd、libvirt、OVN/OVS、虚拟机固件、TPM、抓包、备份和远程桌面依赖。节点使用主站 CA 接入 OVN NB/SB，不开放明文管理数据库。

在面板「资源 → 节点」登记 `https://10.0.0.11:9443`。节点上报实际能力和容量；模板与运行环境沿既有 API 创建。

## 管理网络

| 连接方向 | 端口 | 用途 |
| --- | --- | --- |
| 浏览器、API 调用方 → 主站 | TCP 8090 | 面板及 API；外部访问由反向代理提供 HTTPS |
| 主站、执行节点 → 执行节点 | TCP 9443 | 节点操作与制品分发，双向 TLS |
| 执行节点 → 主站 | TCP 6641、6642 | OVN NB/SB，双向 TLS |
| 执行节点之间 | UDP 6081 | Geneve 组网 |

PostgreSQL、VictoriaMetrics 与 guacd 默认只供本机或本机服务使用。资产服务映射和 WireGuard 使用实际分配的端口；防火墙按所启用的入口放行。

## 版本更新

主站配置为 `/etc/netlab/controller.env`，节点配置为 `/etc/netlab-node/node.env`。默认发布仓库为 `scmypapa/netlab`；私有仓库在两类配置文件中填写 `NETLAB_GITHUB_TOKEN`，自定义仓库使用 `NETLAB_RELEASE_REPOSITORY`。修改配置后重启相应服务。

从账号菜单打开「系统更新」，查看正式发布日志并选择更新。流程为：下载主站发布包 → 暂停新任务领取 → 等现有任务完成 → 逐节点更新并核对版本 → 切换主站 → 核对主站版本。任一节点失败即停止，面板显示节点与错误。

程序保存在 `releases/<version>`，`current` 原子切换；`previous` 保留上一个程序目录。新服务启动失败时恢复程序链接；数据库迁移保持前向。节点与主站的安装目录分离，同机部署互不提前切换。

GitHub Actions 在 `main` 的正式 `v主版本.次版本.修订号` 标签触发构建和发布；普通分支提交不生成正式版本。

节点更新失败后，面板选择「继续更新」，恢复原版本任务；原版本完成后再更新到更高版本。更新进程被终止时，请求与维护状态保留，执行以下命令继续：

```bash
sudo systemctl start netlab-update.service
```

维护状态保存在现有任务表中，数据库连接中断不会解除业务任务的暂停。继续时先等已受理的节点更新结束，再处理后续节点。

更新日志：

```bash
sudo journalctl -u netlab-update.service -u netlab-node-update.service -n 100
```

本轮完成定向验证与构建，尚未执行空白宿主安装、真实多节点版本切换和宿主/服务进程强杀验收。
