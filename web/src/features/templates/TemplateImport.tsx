import {
  Button,
  Collapse,
  FileInput,
  FileButton,
  NumberInput,
  PasswordInput,
  Progress,
  SegmentedControl,
  Select,
  Switch,
  TextInput,
} from "@mantine/core";
import { useMutation } from "@tanstack/react-query";
import { Box, ChevronDown, Monitor } from "lucide-react";
import { useEffect, useRef, useState } from "react";
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
  const [inputMode, setInputMode] = useState("file");
  const [files, setFiles] = useState<File[]>([]);
  const [driverFiles, setDriverFiles] = useState<File[]>([]);
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [authentication, setAuthentication] = useState(false);
  const [plainHttp, setPlainHttp] = useState(false);
  const [progress, setProgress] = useState(0);
  const upload = useRef<AbortController | null>(null);
  useEffect(() => () => upload.current?.abort(), []);
  const [cpu, setCpu] = useState(2);
  const [memoryGiB, setMemoryGiB] = useState(2);
  const [disk, setDisk] = useState(20);
  const [initialization, setInitialization] =
    useState<NonNullable<Template["initialization"]>>("none");
  const [hardware, setHardware] = useState<Hardware>();
  const [advanced, setAdvanced] = useState(false);
  const [specifyHardware, setSpecifyHardware] = useState(false);
  const appliance = format === "ova" || format === "ovf";
  const registry = kind === "container" && format === "oci";
  const local = !registry && inputMode === "file";
  const needsHardware = kind === "vm" && (!appliance || specifyHardware);
  const profiles = useTemplateHardware(needsHardware);
  function installationDefaults(system: string) {
    setHardware(undefined);
    setMemoryGiB(system.startsWith("Windows") ? 4 : 2);
    setDisk(system.startsWith("Windows") ? 64 : 20);
  }
  useEffect(() => {
    if (!hardware && profiles.data?.length) {
      const available = profiles.data.filter(
        ({ machine }) =>
          os !== "Windows 11" ||
          (machine.firmware.includes("uefi") &&
            machine.secureBoot &&
            machine.tpm2),
      );
      const profile =
        available.find(({ machine }) => machine.aliases.includes("q35")) ??
        available[0];
      if (profile)
        setHardware(defaultHardware(profile, format === "iso" ? os : "Linux"));
    }
  }, [hardware, profiles.data, os, format]);
  const profile = profiles.data?.find(
    ({ machine }) => machine.name === hardware?.machine,
  );
  const chooseFiles = (selected: File[]) => {
    setFiles(selected);
    const main =
      selected.find((file) => file.name.toLowerCase().endsWith(`.${format}`)) ??
      selected[0];
    setSource(main ? main.webkitRelativePath || main.name : "");
  };
  const create = useMutation({
    mutationFn: () => {
      const input: Schema<"TemplateImport"> = {
        id: crypto.randomUUID(),
        name,
        kind,
        os,
        source,
        version: 1,
        format,
        initialization:
          kind === "vm" && format !== "iso" ? initialization : "none",
        ...(format === "iso"
          ? {
              media: driverFiles.map((file, index) => ({
                id: `drivers-${index}`,
                source: file.name,
              })),
            }
          : {}),
        resources: { cpu, memoryMiB: memoryGiB * 1024, diskGiB: disk },
        ...(kind === "vm" ? { ...(needsHardware ? { hardware } : {}) } : {}),
        ...(registry && (authentication || plainHttp)
          ? {
              registry: {
                ...(authentication ? { username, password } : {}),
                plainHttp,
              },
            }
          : {}),
      };
      setProgress(0);
      upload.current = new AbortController();
      return local
        ? api.uploadTemplate(
            input,
            [...files, ...(format === "iso" ? driverFiles : [])],
            setProgress,
            upload.current.signal,
          )
        : api.createTemplate(input);
    },
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
              setFiles([]);
              setSource("");
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
        value={
          kind === "container"
            ? format
            : appliance
              ? "appliance"
              : format === "iso"
                ? "iso"
                : "disk"
        }
        onChange={(value) => {
          setFormat(
            (value === "appliance"
              ? "ova"
              : value === "disk"
                ? "qcow2"
                : value) as Format,
          );
          setSpecifyHardware(false);
          setFiles([]);
          setSource("");
          if (value === "iso") installationDefaults(os);
        }}
        data={
          kind === "container"
            ? [
                { value: "oci", label: "镜像仓库" },
                { value: "docker", label: "镜像包" },
              ]
            : [
                { value: "disk", label: "系统磁盘" },
                { value: "appliance", label: "整机镜像" },
                { value: "iso", label: "安装系统" },
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
          onChange={(value) => {
            setOs(value!);
            if (format === "iso") installationDefaults(value!);
          }}
          data={[
            "Linux",
            "Windows",
            "Windows 7",
            "Windows 10",
            "Windows 11",
            "Windows Server",
            "其他",
          ]}
          allowDeselect={false}
        />
        {kind === "vm" && (
          <Select
            label="文件格式"
            value={format}
            allowDeselect={false}
            onChange={(value) => {
              setFormat(value as Format);
              setFiles([]);
              setSource("");
            }}
            data={(appliance
              ? ["ova", "ovf"]
              : format === "iso"
                ? ["iso"]
                : ["qcow2", "raw", "vmdk"]
            ).map((value) => ({ value, label: value.toUpperCase() }))}
          />
        )}
      </div>
      {!registry && (
        <SegmentedControl
          fullWidth
          aria-label="文件来源"
          value={inputMode}
          onChange={(value) => {
            setInputMode(value);
            setDriverFiles([]);
            setSource("");
            setFiles([]);
          }}
          data={[
            { value: "file", label: "上传文件" },
            { value: "address", label: "链接或节点路径" },
          ]}
        />
      )}
      {local ? (
        <>
          <FileInput
            label={
              format === "ovf" || format === "vmdk"
                ? "镜像及关联磁盘"
                : "镜像文件"
            }
            placeholder="选择文件"
            required
            multiple={format === "ovf" || format === "vmdk"}
            value={
              format === "ovf" || format === "vmdk" ? files : (files[0] ?? null)
            }
            onChange={(value) => {
              const selected = Array.isArray(value)
                ? value
                : value
                  ? [value]
                  : [];
              chooseFiles(selected);
            }}
          />
          {(format === "ovf" || format === "vmdk") && (
            <FileButton
              multiple
              onChange={chooseFiles}
              inputProps={{
                "aria-label": "镜像文件夹",
                ...{ webkitdirectory: "" },
              }}
            >
              {(props) => (
                <Button {...props} variant="subtle" size="xs">
                  选择文件夹
                </Button>
              )}
            </FileButton>
          )}
          {files.length > 1 && (
            <Select
              label="主镜像文件"
              value={source}
              allowDeselect={false}
              data={files.map((file) => ({
                value: file.webkitRelativePath || file.name,
                label: file.name,
              }))}
              onChange={(value) => setSource(value!)}
            />
          )}
        </>
      ) : (
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
      )}
      {registry && (
        <>
          <button
            className="disclosure"
            type="button"
            aria-expanded={authentication}
            onClick={() => setAuthentication(!authentication)}
          >
            仓库认证
            <ChevronDown
              size={16}
              className={authentication ? "rotated" : ""}
            />
          </button>
          <Collapse in={authentication}>
            <div className="form-columns">
              <TextInput
                label="仓库用户名"
                value={username}
                onChange={(event) => setUsername(event.currentTarget.value)}
                autoComplete="off"
              />
              <PasswordInput
                label="仓库密码或访问令牌"
                value={password}
                onChange={(event) => setPassword(event.currentTarget.value)}
                autoComplete="new-password"
              />
            </div>
          </Collapse>
          <Switch
            label="HTTP 仓库"
            checked={plainHttp}
            onChange={(event) => setPlainHttp(event.currentTarget.checked)}
          />
        </>
      )}
      {format === "iso" && local && (
        <FileInput
          label="驱动光盘"
          placeholder="选择 ISO 文件"
          accept=".iso"
          multiple
          value={driverFiles}
          onChange={setDriverFiles}
        />
      )}
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
          {format !== "iso" && (
            <>
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
        </>
      )}
      <ErrorMessage error={create.error} />
      {create.isPending && local && (
        <Progress value={progress} aria-label="上传进度" />
      )}
      <div className="drawer-footer">
        <Button
          fullWidth
          type="submit"
          loading={create.isPending}
          disabled={(needsHardware && !profile) || (local && !files.length)}
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
        <Select
          label="CPU 模型"
          searchable
          data={[...profile.hardware.cpuModes, ...profile.hardware.cpuModels]}
          value={value.cpuModel ?? "host-model"}
          allowDeselect={false}
          onChange={(cpuModel) => change({ cpuModel: cpuModel! })}
        />
        <NumberInput
          label="CPU 插槽"
          min={1}
          allowDecimal={false}
          value={value.cpuTopology?.sockets ?? 1}
          onChange={(sockets) =>
            change({
              cpuTopology: {
                sockets: Number(sockets),
                threads: value.cpuTopology?.threads ?? 1,
              },
            })
          }
        />
        <NumberInput
          label="每核线程"
          min={1}
          allowDecimal={false}
          value={value.cpuTopology?.threads ?? 1}
          onChange={(threads) =>
            change({
              cpuTopology: {
                sockets: value.cpuTopology?.sockets ?? 1,
                threads: Number(threads),
              },
            })
          }
        />
        <NumberInput
          label="NUMA 节点"
          min={1}
          allowDecimal={false}
          value={value.numaNodes ?? 1}
          onChange={(numaNodes) => change({ numaNodes: Number(numaNodes) })}
        />
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
