import { Button, NumberInput, SegmentedControl } from "@mantine/core";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { api, type Node } from "../../api/client";
import { ErrorMessage } from "../../foundation/Feedback";
import { memory, reading } from "../../foundation/format";
import styles from "./NodeInventory.module.css";

export function NodeScheduling({ node }: { node: Node }) {
  const [ratio, setRatio] = useState(node.cpuAllocationRatio);
  const [automatic, setAutomatic] = useState(node.memoryReserveMiB === 0);
  const [reserve, setReserve] = useState(
    node.memoryReserveMiB ||
      node.capacity.memoryMiB - node.allocatableMemoryMiB,
  );
  const memoryReserveMiB = automatic ? 0 : reserve;
  const client = useQueryClient();
  const save = useMutation({
    mutationFn: () =>
      api.configureNodeScheduling(node.id, {
        cpuAllocationRatio: ratio,
        memoryReserveMiB,
      }),
    onSuccess: () => client.invalidateQueries({ queryKey: ["nodes"] }),
  });
  return (
    <section className={styles.scheduling} aria-label="计算资源分配">
      <dl>
        <div>
          <dt>物理 CPU</dt>
          <dd>{reading(node.capacity.cpu)} 核</dd>
        </div>
        <div>
          <dt>已分配 CPU</dt>
          <dd>{reading(node.reserved.cpu)} vCPU</dd>
        </div>
        <div>
          <dt>可分配 CPU</dt>
          <dd>{reading(node.capacity.cpu * node.cpuAllocationRatio)} vCPU</dd>
        </div>
        <div>
          <dt>宿主预留内存</dt>
          <dd>{memory(node.capacity.memoryMiB - node.allocatableMemoryMiB)}</dd>
        </div>
        <div>
          <dt>已分配内存</dt>
          <dd>{memory(node.reserved.memoryMiB)}</dd>
        </div>
        <div>
          <dt>可分配内存</dt>
          <dd>{memory(node.allocatableMemoryMiB)}</dd>
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
        <div className={styles.memorySetting}>
          <span id="host-memory-label">宿主预留内存</span>
          <SegmentedControl
            aria-labelledby="host-memory-label"
            value={automatic ? "automatic" : "custom"}
            data={[
              { label: "自动", value: "automatic" },
              { label: "自定义", value: "custom" },
            ]}
            disabled={save.isPending}
            onChange={(value) => setAutomatic(value === "automatic")}
          />
        </div>
        <NumberInput
          aria-label="宿主预留内存 MiB"
          suffix=" MiB"
          min={1}
          max={node.capacity.memoryMiB - 1}
          step={256}
          placeholder="自动"
          value={automatic ? "" : reserve}
          disabled={automatic || save.isPending}
          onChange={(value) => setReserve(Number(value))}
        />
        <Button
          variant="default"
          type="submit"
          loading={save.isPending}
          disabled={
            ratio === node.cpuAllocationRatio &&
            memoryReserveMiB === node.memoryReserveMiB
          }
        >
          应用
        </Button>
      </form>
      <ErrorMessage error={save.error} />
    </section>
  );
}
