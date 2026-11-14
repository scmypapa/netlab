import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import type { PageRequest } from "./useCursorList";
import { listPageSize } from "./ListPagination";

export function useCursorPage<T extends { id: string }>(
  queryKey: readonly unknown[],
  fetchPage: (request: PageRequest) => Promise<T[]>,
  options: { refetchInterval?: (items: T[]) => number | false } = {},
) {
  const key = JSON.stringify(queryKey);
  const [navigation, setNavigation] = useState({
    key,
    cursors: [""],
    index: 0,
  });
  const current =
    navigation.key === key ? navigation : { key, cursors: [""], index: 0 };
  if (current !== navigation) setNavigation(current);
  const query = useQuery({
    queryKey: [
      ...queryKey,
      { cursor: current.cursors[current.index], limit: listPageSize + 1 },
    ],
    queryFn: ({ signal }) =>
      fetchPage({
        cursor: current.cursors[current.index],
        limit: listPageSize + 1,
        signal,
      }),
    refetchInterval: (query) =>
      options.refetchInterval?.(query.state.data ?? []) ?? false,
  });
  const data = query.data?.slice(0, listPageSize);
  return {
    ...query,
    data,
    page: current.index + 1,
    hasPreviousPage: current.index > 0,
    hasNextPage: (query.data?.length ?? 0) > listPageSize,
    previousPage: () => setNavigation({ ...current, index: current.index - 1 }),
    nextPage: () =>
      setNavigation({
        key,
        cursors: [
          ...current.cursors.slice(0, current.index + 1),
          data!.at(-1)!.id,
        ],
        index: current.index + 1,
      }),
  };
}
