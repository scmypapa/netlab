export function memory(mib: number) {
  return `${new Intl.NumberFormat("zh-CN", { maximumFractionDigits: 1 }).format(mib / 1024)} GiB`;
}
export function dateTime(value: string) {
  return new Intl.DateTimeFormat("zh-CN", {
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    hour12: false,
  }).format(new Date(value));
}
export const statusLabels: Record<string, string> = {
  draft: "待运行",
  deploying: "部署中",
  running: "运行中",
  stopped: "已停止",
  suspended: "已暂停",
  changing: "应用变更",
  destroying: "销毁中",
  destroyed: "已销毁",
  failed: "失败",
  unknown: "状态未知",
  ready: "可用",
  importing: "导入中",
  deleting: "删除中",
  capturing: "捕获中",
  online: "在线",
  offline: "离线",
  queued: "等待执行",
  succeeded: "已完成",
  partially_applied: "部分完成",
  created: "已创建",
  prepared: "已准备",
  reserved: "已分配",
};
export const actionLabels: Record<string, string> = {
  start: "启动",
  stop: "停止",
  "force-stop": "强制停止",
  reboot: "重启",
  suspend: "暂停",
  resume: "继续运行",
  rebuild: "重建",
  destroy: "销毁",
  changes: "应用变更",
  change: "应用变更",
  "prepare-template": "准备模板",
  "capture-template": "固化模板",
  "delete-template": "删除模板",
  "capture-recovery": "创建恢复点",
  "delete-recovery": "删除恢复点",
  create: "创建环境",
  "vpn-create": "创建 VPN",
  "vpn-revoke": "撤销 VPN",
};
export function stateLabel(value: string) {
  return statusLabels[value] ?? value;
}
