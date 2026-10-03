import { useQuery } from "@tanstack/react-query";
import { api, type Schema } from "../../api/client";

export type HardwareProfile = {
  machine: Schema<"VmMachine">;
  hardware: Schema<"VmHardware">;
};

export function useTemplateHardware(enabled: boolean) {
  return useQuery({
    queryKey: ["nodes", "template-hardware"],
    enabled,
    queryFn: async () => {
      const profiles = new Map<string, HardwareProfile>();
      let cursor: string | undefined;
      for (;;) {
        const nodes = await api.nodes({ cursor, limit: 200 });
        for (const node of nodes) {
          if (node.state !== "ready" || !node.vmHardware) continue;
          for (const machine of node.vmHardware.machines)
            if (!profiles.has(machine.name))
              profiles.set(machine.name, {
                machine,
                hardware: node.vmHardware,
              });
        }
        if (nodes.length < 200) break;
        cursor = nodes[nodes.length - 1].id;
      }
      return [...profiles.values()];
    },
  });
}

export function defaultHardware(
  profile: HardwareProfile,
  os = "Linux",
): Schema<"Hardware"> {
  const { machine, hardware } = profile;
  const bus = machine.diskBuses.includes("sata")
    ? "sata"
    : machine.diskBuses[0];
  const windows11 = os === "Windows 11";
  return {
    machine: machine.name,
    firmware: (windows11
      ? "uefi"
      : machine.firmware[0]) as Schema<"Hardware">["firmware"],
    ...(windows11 ? { secureBoot: true, tpm: true } : {}),
    diskBus: bus as Schema<"Hardware">["diskBus"],
    diskController: bus === "scsi" ? hardware.diskControllers[0] : undefined,
    nicModel: (hardware.nicModels.includes("e1000")
      ? "e1000"
      : hardware.nicModels[0]) as Schema<"Hardware">["nicModel"],
    guestAgent: true,
  };
}
