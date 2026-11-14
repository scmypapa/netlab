import { Button, Modal, TextInput } from "@mantine/core";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { CheckCircle2 } from "lucide-react";
import { useState } from "react";
import { Link } from "react-router-dom";
import { api, type EnvironmentSpec } from "../../api/client";
import { ErrorMessage } from "../../foundation/Feedback";

export function SaveBlueprintDialog({
  environmentId,
  environmentName,
  expectedRevision,
  spec,
  onClose,
}: {
  environmentId: string;
  environmentName: string;
  expectedRevision: number;
  spec: EnvironmentSpec;
  onClose: () => void;
}) {
  const [name, setName] = useState(environmentName);
  const client = useQueryClient();
  const save = useMutation({
    mutationFn: () =>
      api.saveBlueprint(environmentId, { name, expectedRevision, spec }),
    onSuccess: () => client.invalidateQueries({ queryKey: ["blueprints"] }),
  });
  return (
    <Modal opened onClose={onClose} title="保存为环境模板" centered size="sm">
      {save.data ? (
        <>
          <div className="blueprint-saved">
            <CheckCircle2 size={32} />
            <h3>{save.data.name}</h3>
            <span>已保存</span>
          </div>
          <div className="dialog-actions">
            <Button
              variant="default"
              component={Link}
              to="/templates?tab=environments"
            >
              查看模板
            </Button>
            <Button onClick={onClose}>完成</Button>
          </div>
        </>
      ) : (
        <form
          className="form-stack"
          onSubmit={(event) => {
            event.preventDefault();
            save.mutate();
          }}
        >
          <TextInput
            label="模板名称"
            required
            autoFocus
            value={name}
            onChange={(event) => setName(event.currentTarget.value)}
          />
          <div className="blueprint-scale">
            <span>{spec.assets.length} 个资产</span>
            <span>{spec.networks.length} 个网段</span>
            {Boolean(spec.routers?.length) && (
              <span>{spec.routers?.length} 个路由器</span>
            )}
          </div>
          <ErrorMessage error={save.error} />
          <div className="dialog-actions">
            <Button variant="default" onClick={onClose}>
              取消
            </Button>
            <Button type="submit" loading={save.isPending}>
              保存模板
            </Button>
          </div>
        </form>
      )}
    </Modal>
  );
}
