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
      <form
        onSubmit={(event) => {
          event.preventDefault();
          save.mutate();
        }}
      >
        <section className={styles.schedulingResource} aria-label="CPU 分配">
          <div className={styles.schedulingHeading}>
            <h3>CPU 分配</h3>
            <NumberInput
              label="CPU 超配倍数"
              aria-label="CPU 超配倍数"
              suffix=" 倍"
              min={1}
              step={0.5}
              value={ratio}
              disabled={save.isPending}
              onChange={(value) => setRatio(Number(value))}
            />
          </div>
          <dl>
            <div>
              <dt>已分配</dt>
              <dd>{reading(node.reserved.cpu)} 虚拟核</dd>
            </div>
            <div>
              <dt>还可分配</dt>
              <dd>
                {reading(
                  Math.max(
                    0,
                    node.capacity.cpu * node.cpuAllocationRatio -
                      node.reserved.cpu,
                  ),
                )}{" "}
                虚拟核
              </dd>
            </div>
            <div>
              <dt>分配上限</dt>
              <dd>
                {reading(node.capacity.cpu * node.cpuAllocationRatio)} 虚拟核
              </dd>
            </div>
          </dl>
        </section>
        <section className={styles.schedulingResource} aria-label="内存分配">
          <div className={styles.schedulingHeading}>
            <h3>内存分配</h3>
          </div>
          <dl>
            <div>
              <dt>已分配</dt>
              <dd>{memory(node.reserved.memoryMiB)}</dd>
            </div>
            <div>
              <dt>还可分配</dt>
              <dd>
                {memory(
                  Math.max(
                    0,
                    node.allocatableMemoryMiB - node.reserved.memoryMiB,
                  ),
                )}
              </dd>
            </div>
            <div>
              <dt>分配上限</dt>
              <dd>{memory(node.allocatableMemoryMiB)}</dd>
            </div>
          </dl>
          <div className={styles.schedulingControls}>
            <div className={styles.memorySetting}>
              <span id="host-memory-label">系统预留内存</span>
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
              aria-label="系统预留内存 MiB"
              suffix=" MiB"
              min={1}
              max={node.capacity.memoryMiB - 1}
              step={256}
              placeholder="自动"
              value={automatic ? "" : reserve}
              disabled={automatic || save.isPending}
              onChange={(value) => setReserve(Number(value))}
            />
          </div>
        </section>
        <div className={styles.schedulingFooter}>
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
        </div>
      </form>
      <ErrorMessage error={save.error} />
    </section>
  );
}
