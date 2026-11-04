import { HardDrive } from "lucide-react";
import { byteUnit, reading } from "./format";
import styles from "./StorageLocations.module.css";

export function StorageLocations({
  locations,
}: {
  locations: { id: string; name: string; bytes: number }[];
}) {
  return (
    <div>
      <div className={styles.head}>
        <span>存放位置</span>
        <span>实际占用</span>
      </div>
      <dl className={styles.locations}>
        {locations.map((location) => {
          const { scale, unit } = byteUnit(location.bytes);
          return (
            <div key={location.id}>
              <dt>
                <HardDrive size={17} />
                <span>{location.name}</span>
              </dt>
              <dd>
                {reading(location.bytes, scale)} {unit}
              </dd>
            </div>
          );
        })}
      </dl>
    </div>
  );
}
