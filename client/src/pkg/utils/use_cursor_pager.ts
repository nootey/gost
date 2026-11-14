import { computed, ref } from "vue";
import type { ComputedRef, Ref } from "vue";
import type { DataTablePageEvent } from "primevue/datatable";

export const ORDER_BY_SORT_ORDER: Record<number, "asc" | "desc"> = { 1: "asc", [-1]: "desc" };

export interface CursorPager {
    pageSize: Ref<number>;
    nextCursor: Ref<string | null>;
    cursor: ComputedRef<string | null>;
    first: ComputedRef<number>;
    total: (rows: number) => number;
    turn: (event: DataTablePageEvent) => boolean;
    reset: () => void;
}

// Cursor pages for a lazy DataTable. The server sends no prev cursor and no total.
// cursors[i] opens page i, and the first page has none. Prev reloads the page before from its cursor.
export function useCursorPager(firstPageSize: number): CursorPager {
    const pageSize = ref<number>(firstPageSize);
    const cursors = ref<(string | null)[]>([null]);
    const nextCursor = ref<string | null>(null);

    const cursor = computed<string | null>(() => cursors.value.at(-1) ?? null);
    const first = computed<number>(() => (cursors.value.length - 1) * pageSize.value);

    // The paginator needs a total. One row past the rows so far turns on Next only when a next page exists.
    function total(rows: number): number {
        return first.value + rows + Number(nextCursor.value !== null);
    }

    // Takes only an exact step: the first page, one back or one forward. Returns true when the page must load.
    // The used cursor is cleared until the page loads, so a fast second click cannot open the same page twice.
    function turn(event: DataTablePageEvent): boolean {
        if (event.rows !== pageSize.value) {
            pageSize.value = event.rows;
            reset();
            return true;
        }

        const page = cursors.value.length - 1;
        if (event.page === 0) cursors.value = [null];
        else if (event.page === page - 1) cursors.value.pop();
        else if (event.page === page + 1 && nextCursor.value !== null) cursors.value.push(nextCursor.value);
        else return false;
        nextCursor.value = null;
        return true;
    }

    function reset(): void {
        cursors.value = [null];
    }

    return { pageSize, nextCursor, cursor, first, total, turn, reset };
}
