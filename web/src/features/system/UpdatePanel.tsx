import { Button, Drawer, Loader, Modal, Progress } from "@mantine/core";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  ArrowRight,
  Check,
  CircleAlert,
  Download,
  RefreshCw,
} from "lucide-react";
import { useState } from "react";
import Markdown from "react-markdown";
import { api, type Schema } from "../../api/client";
import { ErrorMessage, Loading } from "../../foundation/Feedback";
import { dateTime } from "../../foundation/format";
import styles from "./UpdatePanel.module.css";

type Phase = Schema<"UpdateActivity">["phase"];
const phases: Record<Phase, string> = {
  queued: "准备更新",
  downloading: "下载发布包",
  installing: "安装更新",
  waiting_operations: "等待当前任务完成",
  updating_nodes: "更新节点",
  restarting: "重启服务",
  succeeded: "更新完成",
  failed: "更新失败",
};
const active = (phase?: Phase) =>
  !!phase && phase !== "succeeded" && phase !== "failed";

export function UpdatePanel({ onClose }: { onClose: () => void }) {
  const client = useQueryClient();
  const [confirming, setConfirming] = useState(false);
  const query = useQuery({
    queryKey: ["system-update"],
    queryFn: api.updateStatus,
    retry: false,
    refetchInterval: (query) =>
      active(query.state.data?.activity?.phase) || !query.state.data?.checkedAt
        ? 3_000
        : false,
  });
  const update = (data: Schema<"SystemUpdate">) =>
    client.setQueryData(["system-update"], data);
  const check = useMutation({ mutationFn: api.checkUpdate, onSuccess: update });
  const apply = useMutation({
    mutationFn: api.applyUpdate,
    onSuccess: (data) => {
      setConfirming(false);
      update(data);
    },
  });
  const data = query.data;
  const busy = active(data?.activity?.phase);
  const latest = data?.latest;
  const retryVersion =
    data?.activity?.phase === "failed" ? data.activity.version : undefined;
  const targetVersion = retryVersion ?? latest?.version;
  const totalNodes = data?.activity?.totalNodes;
  const completedNodes = data?.activity?.completedNodes;
  const nodeProgress =
    totalNodes !== undefined && totalNodes > 0 && completedNodes !== undefined
      ? (100 * completedNodes) / totalNodes
      : undefined;
  return (
    <>
      <Drawer
        opened
        onClose={onClose}
        position="right"
        size={640}
        title="系统更新"
      >
        <div className={styles.panel}>
          <ErrorMessage error={query.error} />
          {query.isPending ? (
            <Loading />
          ) : (
            data && (
              <>
                <section
                  className={styles.versions}
                  aria-label="软件版本"
                  data-available={data.available && Boolean(targetVersion)}
                >
                  <div className={styles.version}>
                    <span>当前版本</span>
                    <strong>
                      {data.currentVersion === "dev"
                        ? "开发版本"
                        : data.currentVersion}
                    </strong>
                  </div>
                  {data.available && targetVersion && (
                    <>
                      <ArrowRight className={styles.upgradeArrow} size={20} />
                      <div className={styles.version}>
                        <span>可用更新</span>
                        <strong>{targetVersion}</strong>
                      </div>
                    </>
                  )}
                  <Button
                    className={styles.check}
                    variant="default"
                    leftSection={<RefreshCw size={15} />}
                    aria-label="检查更新"
                    loading={check.isPending}
                    disabled={busy}
                    onClick={() => check.mutate()}
                  >
                    检查更新
                  </Button>
                  {!data.available &&
                    !retryVersion &&
                    data.currentVersion !== "dev" &&
                    latest && (
                      <span className={styles.current}>
                        <Check size={15} />
                        已是最新版本
                      </span>
                    )}
                  {data.available && data.canApply && !retryVersion && (
                    <Button
                      className={styles.apply}
                      leftSection={<Download size={15} />}
                      disabled={busy}
                      onClick={() => {
                        apply.reset();
                        setConfirming(true);
                      }}
                    >
                      更新到 {latest?.version}
                    </Button>
                  )}
                </section>
                <ErrorMessage
                  error={
                    check.error ??
                    (data.checkError ? new Error(data.checkError) : null)
                  }
                />
                {data.activity && (
                  <section
                    className={styles.activity}
                    aria-label="更新进度"
                    aria-live="polite"
                    data-phase={data.activity.phase}
                  >
                    <header>
                      {busy ? (
                        <Loader
                          size={16}
                          aria-label={phases[data.activity.phase]}
                        />
                      ) : data.activity.phase === "failed" ? (
                        <CircleAlert size={17} />
                      ) : (
                        <Check size={17} />
                      )}
                      <strong>{phases[data.activity.phase]}</strong>
                      <span>{data.activity.version}</span>
                    </header>
                    {data.activity.phase === "updating_nodes" && (
                      <div className={styles.nodeProgress}>
                        <span>
                          {data.activity.nodeName} ·{" "}
                          {data.activity.completedNodes}/
                          {data.activity.totalNodes}
                        </span>
                        {nodeProgress !== undefined && (
                          <Progress value={nodeProgress} size={3} />
                        )}
                      </div>
                    )}
                    {data.activity.error && (
                      <ErrorMessage error={new Error(data.activity.error)} />
                    )}
                    {retryVersion && data.canApply && (
                      <Button
                        size="xs"
                        onClick={() => {
                          apply.reset();
                          setConfirming(true);
                        }}
                      >
                        继续更新 {retryVersion}
                      </Button>
                    )}
                  </section>
                )}
                {latest && (
                  <section className={styles.release}>
                    <header>
                      <h2>{latest.name || latest.version}</h2>
                      <time>{dateTime(latest.publishedAt)}</time>
                    </header>
                    <div className={styles.notes}>
                      <Markdown
                        components={{
                          a: ({ children, ...props }) => (
                            <a
                              {...props}
                              target="_blank"
                              rel="noopener noreferrer"
                            >
                              {children}
                            </a>
                          ),
                          img: ({ alt }) => <span>{alt}</span>,
                        }}
                      >
                        {latest.notes}
                      </Markdown>
                    </div>
                  </section>
                )}
              </>
            )
          )}
        </div>
      </Drawer>
      <Modal
        opened={confirming}
        onClose={() => !apply.isPending && setConfirming(false)}
        title={"更新到 " + targetVersion}
        centered
      >
        <p>服务将短暂重启。已运行的资产和数据保持不变。</p>
        <ErrorMessage error={apply.error} />
        <div className={styles.confirm}>
          <Button
            variant="default"
            disabled={apply.isPending}
            onClick={() => setConfirming(false)}
          >
            取消
          </Button>
          <Button
            loading={apply.isPending}
            onClick={() => targetVersion && apply.mutate(targetVersion)}
          >
            开始更新
          </Button>
        </div>
      </Modal>
    </>
  );
}
