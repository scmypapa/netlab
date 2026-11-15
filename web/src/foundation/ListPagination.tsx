import { Button } from "@mantine/core";
import { ChevronLeft, ChevronRight } from "lucide-react";

export const listPageSize = 20;

export type PageNavigation = {
  page: number;
  hasPreviousPage: boolean;
  hasNextPage: boolean;
  isFetching?: boolean;
  previousPage: () => void;
  nextPage: () => void;
};

export function ListPagination({ list }: { list: PageNavigation }) {
  if (!list.hasPreviousPage && !list.hasNextPage) return null;
  return (
    <nav className="list-pagination" aria-label="列表分页">
      <Button
        variant="default"
        size="xs"
        aria-label="上一页"
        leftSection={<ChevronLeft size={14} />}
        disabled={!list.hasPreviousPage || list.isFetching}
        onClick={list.previousPage}
      >
        上一页
      </Button>
      <span aria-live="polite">第 {list.page} 页</span>
      <Button
        variant="default"
        size="xs"
        aria-label="下一页"
        rightSection={<ChevronRight size={14} />}
        disabled={!list.hasNextPage || list.isFetching}
        onClick={list.nextPage}
      >
        下一页
      </Button>
    </nav>
  );
}
