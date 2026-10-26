import { Button, NumberInput } from "@mantine/core";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { api, type Node } from "../../api/client";
import { ErrorMessage } from "../../foundation/Feedback";
import { reading } from "../../foundation/format";
import styles from "./NodeInventory.module.css";

export function NodeScheduling({
  node,
  onSaved,
}: {
  node: Node;
  onSaved: (node: Node) => void;
}) {
  const [ratio, setRatio] = useState(node.cpuAllocationRatio);
  const client = useQueryClient();
  const save = useMutation({
    mutationFn: () =>
      api.configureNodeScheduling(node.id, { cpuAllocationRatio: ratio }),
    onSuccess: () => {
      onSaved({ ...node, cpuAllocationRatio: ratio });
      void client.invalidateQueries({ queryKey: ["nodes"] });
    },
  });
  return (
    <section className={styles.scheduling} aria-label="CPU 分配">
      <dl>
        <div>
          <dt>物理 CPU</dt>
          <dd>{reading(node.capacity.cpu)} 核</dd>
        </div>
        <div>
          <dt>已分配</dt>
          <dd>{reading(node.reserved.cpu)} vCPU</dd>
        </div>
        <div>
          <dt>调度额度</dt>
          <dd>{reading(node.capacity.cpu * node.cpuAllocationRatio)} vCPU</dd>
        </div>
      </dl>
      <form
        onSubmit={(event) => {
          event.preventDefault();
          save.mutate();
        }}
      >
        <NumberInput
          label="CPU 分配比例"
          aria-label="CPU 分配比例"
          suffix=" : 1"
          min={1}
          step={0.5}
          value={ratio}
          disabled={save.isPending}
          onChange={(value) => setRatio(Number(value))}
        />
        <Button
          variant="default"
          type="submit"
          loading={save.isPending}
          disabled={ratio === node.cpuAllocationRatio}
        >
          应用
        </Button>
      </form>
      <ErrorMessage error={save.error} />
    </section>
  );
}
