import { useState } from "react";
import { listPageSize } from "./ListPagination";

export function useLocalPage<T>(items: T[], filter: string) {
  const [navigation, setNavigation] = useState({ filter, page: 1 });
  if (navigation.filter !== filter) setNavigation({ filter, page: 1 });
  const page = Math.min(
    navigation.filter === filter ? navigation.page : 1,
    Math.max(1, Math.ceil(items.length / listPageSize)),
  );
  return {
    data: items.slice((page - 1) * listPageSize, page * listPageSize),
    page,
    hasPreviousPage: page > 1,
    hasNextPage: page * listPageSize < items.length,
    previousPage: () => setNavigation({ filter, page: page - 1 }),
    nextPage: () => setNavigation({ filter, page: page + 1 }),
  };
}
