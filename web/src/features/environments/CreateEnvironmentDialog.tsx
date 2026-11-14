import { randomUUID } from "../../foundation/id";
import { Button, Modal, Select, Switch, TextInput } from "@mantine/core";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Layers3 } from "lucide-react";
import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { api, type Blueprint } from "../../api/client";
import { ErrorMessage } from "../../foundation/Feedback";
import { LoadMore } from "../../foundation/LoadMore";
import { useCursorList } from "../../foundation/useCursorList";
import styles from "./Environments.module.css";

export function CreateEnvironmentDialog({
  blueprint,
  onClose,
}: {
  blueprint?: Blueprint;
  onClose: () => void;
}) {
  const [name, setName] = useState(blueprint?.name ?? "");
  const [blueprintId, setBlueprintId] = useState(blueprint?.id ?? "blank");
  const [run, setRun] = useState(true);
  const [clientRequestId] = useState(() => randomUUID());
  const blueprints = useCursorList(["blueprints"], api.blueprints);
  const choices = [
    ...new Map(
      [...(blueprint ? [blueprint] : []), ...(blueprints.data ?? [])].map(
        (item) => [item.id, item],
      ),
    ).values(),
  ];
  const selected = choices.find((item) => item.id === blueprintId);
  const summary = selected;
  const navigate = useNavigate();
  const client = useQueryClient();
  const create = useMutation({
    mutationFn: () =>
      api.createEnvironment({
        name,
        clientRequestId,
        ...(blueprintId === "blank"
          ? { spec: { assets: [], networks: [] } }
          : {
              projectId: selected!.projectId,
              blueprintVersionId: selected!.latestVersionId,
              run,
            }),
      }),
    onSuccess: (environment) => {
      void client.invalidateQueries({ queryKey: ["environments"] });
      navigate(`/environments/${environment.id}`);
      onClose();
    },
  });
  return (
    <Modal opened onClose={onClose} title="新建环境" centered size={640}>
      <form
        className={styles.creation}
        onSubmit={(event) => {
          event.preventDefault();
          create.mutate();
        }}
      >
        <section>
          <Select
            label="环境模板"
            searchable
            allowDeselect={false}
            value={blueprintId}
            data={[
              { value: "blank", label: "空白环境" },
              ...choices.map((item) => ({
                value: item.id,
                label: item.name,
              })),
            ]}
            onChange={(value) => {
              setBlueprintId(value!);
              if (!name)
                setName(choices.find((item) => item.id === value)?.name ?? "");
            }}
          />
          <LoadMore list={blueprints} />
          {blueprintId !== "blank" && (
            <>
              {summary && (
                <div className="blueprint-scale">
                  <Layers3 size={18} />
                  <span>{summary.assetCount} 个资产</span>
                  <span>{summary.networkCount} 个网段</span>
                </div>
              )}
            </>
          )}
        </section>
        <section>
          <TextInput
            label="环境名称"
            data-autofocus
            required
            value={name}
            onChange={(event) => setName(event.currentTarget.value)}
          />
          {blueprintId !== "blank" && (
            <Switch
              label="创建后运行"
              checked={run}
              onChange={(event) => setRun(event.currentTarget.checked)}
            />
          )}
          <ErrorMessage error={blueprints.error ?? create.error} />
        </section>
        <div className={styles.creationFooter}>
          <Button variant="default" onClick={onClose}>
            取消
          </Button>
          <Button
            type="submit"
            loading={create.isPending}
            disabled={blueprintId !== "blank" && !summary}
          >
            {blueprintId !== "blank" && run ? "创建并运行" : "创建环境"}
          </Button>
        </div>
      </form>
    </Modal>
  );
}
