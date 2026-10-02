import {
  Button,
  Collapse,
  NumberInput,
  SegmentedControl,
  Select,
  Switch,
  TextInput,
} from "@mantine/core";
import { useMutation } from "@tanstack/react-query";
import { Box, ChevronDown, Monitor } from "lucide-react";
import { useEffect, useState } from "react";
import { api, type Schema, type Template } from "../../api/client";
import { Empty, ErrorMessage, Loading } from "../../foundation/Feedback";
import {
  defaultHardware,
  useTemplateHardware,
  type HardwareProfile,
} from "./useTemplateHardware";

type Hardware = Schema<"Hardware">;
type Format = NonNullable<Template["format"]>;

export function TemplateImportForm({ onCreated }: { onCreated: () => void }) {
  const [kind, setKind] = useState<Template["kind"]>("container");
  const [format, setFormat] = useState<Format>("oci");
  const [name, setName] = useState("");
  const [os, setOs] = useState("Linux");
  const [source, setSource] = useState("");
  const [cpu, setCpu] = useState(2);
  const [memoryGiB, setMemoryGiB] = useState(2);
  const [disk, setDisk] = useState(20);
  const [initialization, setInitialization] =
    useState<NonNullable<Template["initialization"]>>("none");
  const [hardware, setHardware] = useState<Hardware>();
  const [advanced, setAdvanced] = useState(false);
  const [specifyHardware, setSpecifyHardware] = useState(false);
  const appliance = format === "ova" || format === "ovf";
  const needsHardware = kind === "vm" && (!appliance || specifyHardware);
  const profiles = useTemplateHardware(needsHardware);
  useEffect(() => {
    if (!hardware && profiles.data?.length) {
      const profile =
        profiles.data.find(({ machine }) => machine.aliases.includes("q35")) ??
        profiles.data[0];
      setHardware(defaultHardware(profile));
    }
  }, [hardware, profiles.data]);
  const profile = profiles.data?.find(
    ({ machine }) => machine.name === hardware?.machine,
  );
  const create = useMutation({
    mutationFn: () =>
      api.createTemplate({
        id: crypto.randomUUID(),
        name,
        kind,
        os,
        source,
        version: 1,
        format,
        initialization: kind === "vm" ? initialization : "none",
        resources: { cpu, memoryMiB: memoryGiB * 1024, diskGiB: disk },
        ...(kind === "vm" ? { ...(needsHardware ? { hardware } : {}) } : {}),
      }),
    onSuccess: onCreated,
  });
  return (
    <form
      className="form-stack"
      onSubmit={(event) => {
        event.preventDefault();
        create.mutate();
      }}
    >
      <div className="type-picker" role="group" aria-label="资产类型">
        {(["container", "vm"] as const).map((value) => (
          <button
            key={value}
            type="button"
            className={kind === value ? "selected" : ""}
            aria-pressed={kind === value}
            onClick={() => {
              setKind(value);
              setFormat(value === "container" ? "oci" : "qcow2");
              setSpecifyHardware(false);
            }}
          >
            {value === "container" ? <Box size={21} /> : <Monitor size={21} />}
            {value === "container" ? "容器" : "虚拟机"}
          </button>
        ))}
      </div>
      <SegmentedControl
        fullWidth
        aria-label="镜像来源"
        value={kind === "container" ? format : appliance ? "appliance" : "disk"}
        onChange={(value) => {
          setFormat(
            (value === "appliance"
              ? "ova"
              : value === "disk"
                ? "qcow2"
                : value) as Format,
          );
          setSpecifyHardware(false);
        }}
        data={
          kind === "container"
            ? [
                { value: "oci", label: "Registry" },
                { value: "docker", label: "镜像包" },
              ]
            : [
                { value: "disk", label: "系统磁盘" },
                { value: "appliance", label: "整机镜像" },
              ]
        }
      />
      <TextInput
        label="模板名称"
        required
        value={name}
        onChange={(event) => setName(event.currentTarget.value)}
      />
      <div className="form-columns">
        <Select
          label="操作系统"
          value={os}
          onChange={(value) => setOs(value!)}
          data={["Linux", "Windows", "其他"]}
          allowDeselect={false}
        />
        {kind === "vm" && (
          <Select
            label="文件格式"
            value={format}
            allowDeselect={false}
            onChange={(value) => setFormat(value as Format)}
            data={(appliance ? ["ova", "ovf"] : ["qcow2", "raw", "vmdk"]).map(
              (value) => ({ value, label: value.toUpperCase() }),
            )}
          />
        )}
      </div>
      <TextInput
        label={format === "oci" ? "镜像地址" : "文件地址"}
        placeholder={
          format === "oci"
            ? "docker.io/library/ubuntu:24.04"
            : `https://storage.example.com/image.${format === "docker" ? "tar" : format}`
        }
        required
        value={source}
        onChange={(event) => setSource(event.currentTarget.value)}
      />
      {!appliance && (
        <>
          <h3 className="form-section-title">默认规格</h3>
          <div className="form-columns three">
            <NumberInput
              label="CPU · 核"
              min={1}
              value={cpu}
              onChange={(value) => setCpu(Number(value))}
            />
            <NumberInput
              label="内存 · GiB"
              min={0.25}
              step={0.25}
              value={memoryGiB}
              onChange={(value) => setMemoryGiB(Number(value))}
            />
            <NumberInput
              label="磁盘 · GiB"
              min={1}
              value={disk}
              onChange={(value) => setDisk(Number(value))}
            />
          </div>
        </>
      )}
      {kind === "vm" && (
        <>
          {appliance && (
            <button
              className="disclosure"
              type="button"
              aria-expanded={specifyHardware}
              onClick={() => setSpecifyHardware(!specifyHardware)}
            >
              指定虚拟硬件
              <ChevronDown
                size={16}
                className={specifyHardware ? "rotated" : ""}
              />
            </button>
          )}
          {needsHardware && (
            <>
              {!appliance && <h3 className="form-section-title">虚拟硬件</h3>}
              <ErrorMessage error={profiles.error} />
              {profiles.isPending ? (
                <Loading />
              ) : profile && hardware ? (
                <HardwareFields
                  profiles={profiles.data ?? []}
                  profile={profile}
                  value={hardware}
                  onChange={setHardware}
                />
              ) : (
                !profiles.error && (
                  <Empty
                    icon={<Monitor size={26} />}
                    title="没有可用的虚拟机节点"
                  />
                )
              )}
            </>
          )}
          <button
            className="disclosure"
            type="button"
            aria-expanded={advanced}
            onClick={() => setAdvanced(!advanced)}
          >
            来宾初始化
            <ChevronDown size={16} className={advanced ? "rotated" : ""} />
          </button>
          <Collapse in={advanced}>
            <Select
              label="初始化方式"
              value={initialization}
              allowDeselect={false}
              onChange={(value) =>
                setInitialization(value as typeof initialization)
              }
              data={[
                { value: "none", label: "保留镜像配置" },
                { value: "cloud-init", label: "cloud-init" },
                { value: "cloudbase-init", label: "Cloudbase-Init" },
              ]}
            />
          </Collapse>
        </>
      )}
      <ErrorMessage error={create.error} />
      <div className="drawer-footer">
        <Button
          fullWidth
          type="submit"
          loading={create.isPending}
          disabled={needsHardware && !profile}
        >
          导入模板
        </Button>
      </div>
    </form>
  );
}

function HardwareFields({
  profiles,
  profile,
  value,
  onChange,
}: {
  profiles: HardwareProfile[];
  profile: HardwareProfile;
  value: Hardware;
  onChange: (hardware: Hardware) => void;
}) {
  const change = (changes: Partial<Hardware>) =>
    onChange({ ...value, ...changes });
  return (
    <div className="form-stack">
      <div className="form-columns">
        <Select
          label="机器类型"
          searchable
          value={value.machine}
          allowDeselect={false}
          data={profiles.map(({ machine }) => ({
            value: machine.name,
            label: machine.name,
          }))}
          onChange={(name) =>
            onChange(
              defaultHardware(
                profiles.find(({ machine }) => machine.name === name)!,
              ),
            )
          }
        />
        <Select
          label="固件"
          value={value.firmware}
          allowDeselect={false}
          data={profile.machine.firmware.map((firmware) => ({
            value: firmware,
            label: firmware.toUpperCase(),
          }))}
          onChange={(firmware) =>
            change({
              firmware: firmware as Hardware["firmware"],
              secureBoot: false,
            })
          }
        />
        <Select
          label="磁盘总线"
          value={value.diskBus}
          allowDeselect={false}
          data={profile.machine.diskBuses.map((bus) => ({
            value: bus,
            label: bus === "virtio" ? "VirtIO" : bus.toUpperCase(),
          }))}
          onChange={(bus) =>
            change({
              diskBus: bus as Hardware["diskBus"],
              diskController:
                bus === "scsi"
                  ? profile.hardware.diskControllers[0]
                  : undefined,
            })
          }
        />
        <Select
          label="网卡型号"
          value={value.nicModel}
          allowDeselect={false}
          data={profile.hardware.nicModels}
          onChange={(nicModel) =>
            change({ nicModel: nicModel as Hardware["nicModel"] })
          }
        />
        {value.diskBus === "scsi" && (
          <Select
            label="磁盘控制器"
            value={value.diskController ?? null}
            allowDeselect={false}
            data={profile.hardware.diskControllers}
            onChange={(diskController) =>
              change({ diskController: diskController! })
            }
          />
        )}
      </div>
      {(profile.machine.secureBoot || profile.machine.tpm2) && (
        <div className="hardware-options">
          {value.firmware === "uefi" && profile.machine.secureBoot && (
            <Switch
              label="Secure Boot"
              checked={value.secureBoot ?? false}
              onChange={(event) =>
                change({ secureBoot: event.currentTarget.checked })
              }
            />
          )}
          {profile.machine.tpm2 && (
            <Switch
              label="TPM 2.0"
              checked={value.tpm ?? false}
              onChange={(event) => change({ tpm: event.currentTarget.checked })}
            />
          )}
        </div>
      )}
    </div>
  );
}
