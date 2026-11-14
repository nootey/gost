import { computed, shallowRef, useTemplateRef } from "vue";
import type { ComputedRef, ShallowRef, Ref } from "vue";
import type { DataTableRowContextMenuEvent } from "primevue/datatable";

export interface RowMenu<T> {
    menuTarget: ShallowRef<T | undefined>;
    menuTargets: ComputedRef<T[]>;
    showMenu: (event: DataTableRowContextMenuEvent) => void;
}

// menuKey is the template ref of the ContextMenu. A right-click on a selected row acts on the whole selection.
// On any other row it acts on that row only.
export function useRowMenu<T extends { id: number }>(selection: Ref<T[]>, menuKey: string): RowMenu<T> {
    const menu = useTemplateRef<{ show: (event: Event) => void }>(menuKey);
    const menuTarget = shallowRef<T | undefined>();

    const menuTargets = computed<T[]>(() => {
        const row = menuTarget.value;
        if (!row) return [];
        if (selection.value.some((item) => item.id === row.id)) return selection.value;
        return [row];
    });

    function showMenu(event: DataTableRowContextMenuEvent): void {
        menu.value?.show(event.originalEvent);
    }

    return { menuTarget, menuTargets, showMenu };
}
