import { Button, Modal, Select, TextInput } from "@mantine/core";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { api, type Asset, type Schema } from "../../api/client";
import { ErrorMessage } from "../../foundation/Feedback";

export function CaptureTemplateDialog({
  environmentId,
  asset,
  revision,
  onClose,
  onCreated,
  defaultInitialization,
}: {
  environmentId: string;
  asset: Asset;
  revision: number;
  onClose: () => void;
  onCreated: () => void;
  defaultInitialization?: Schema<"TemplateInitialization">;
}) {
  const [name, setName] = useState(asset.name);
  const [initialization, setInitialization] = useState<
    Schema<"TemplateInitialization">
  >(defaultInitialization ?? "none");
  const client = useQueryClient();
  const capture = useMutation({
    mutationFn: () =>
      api.captureTemplate(environmentId, asset.id, {
        name,
        expectedRevision: revision,
        initialization,
      }),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: ["templates"] });
      onCreated();
      onClose();
    },
  });
  return (
    <Modal opened onClose={onClose} title="固化资产模板" size="sm">
      <form
        className="form-stack"
        onSubmit={(event) => {
          event.preventDefault();
          capture.mutate();
        }}
      >
        <TextInput
          label="模板名称"
          required
          value={name}
          onChange={(event) => setName(event.currentTarget.value)}
        />
        <Select
          label="来宾初始化"
          value={initialization}
          allowDeselect={false}
          onChange={(value) =>
            setInitialization(value as Schema<"TemplateInitialization">)
          }
          data={[
            { value: "none", label: "保留系统配置" },
            { value: "cloud-init", label: "cloud-init" },
            { value: "cloudbase-init", label: "Cloudbase-Init" },
          ]}
        />
        <ErrorMessage error={capture.error} />
        <div className="drawer-footer">
          <Button type="submit" fullWidth loading={capture.isPending}>
            固化模板
          </Button>
        </div>
      </form>
    </Modal>
  );
}
