package operation

import "netlab.local/core/api"

type diskBudget struct {
	system int64
	data   int64
}

func assetDiskBudget(a api.Asset) diskBudget {
	b := diskBudget{system: a.Resources.DiskGiB}
	if a.Volumes != nil {
		for _, v := range *a.Volumes {
			if v.PersistentVolumeId == nil {
				b.data += v.SizeGiB
			}
		}
	}
	return b
}

// Peak reservations retain both data sets until their actual cleanup succeeds.
func peakDiskBudget(before, after api.Asset, restoring bool) diskBudget {
	old, next := assetDiskBudget(before), assetDiskBudget(after)
	if restoring {
		return diskBudget{system: old.system + next.system, data: old.data + next.data}
	}
	next.system = max(old.system, next.system)
	volumes := map[string]int64{}
	for _, a := range []api.Asset{before, after} {
		if a.Volumes != nil {
			for _, v := range *a.Volumes {
				if v.PersistentVolumeId == nil {
					volumes[v.Id] = max(volumes[v.Id], v.SizeGiB)
				}
			}
		}
	}
	next.data = 0
	for _, size := range volumes {
		next.data += size
	}
	return next
}
