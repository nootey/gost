<script setup lang="ts">
import { computed, onMounted, ref } from "vue";
import { useConfirm } from "primevue/useconfirm";
import { useFileStore } from "../../../pkg/stores/file_store.ts";
import { useSettingsStore } from "../../../pkg/stores/settings_store.ts";
import { useToastStore } from "../../../pkg/stores/toast_store.ts";
import vueHelper from "../../../pkg/utils/vue_helper.ts";
import { itemView, shownName } from "../../../pkg/utils/item_view.ts";
import { ORDER_BY_SORT_ORDER, useCursorPager } from "../../../pkg/utils/use_cursor_pager.ts";
import { useRowMenu } from "../../../pkg/utils/use_row_menu.ts";
import type { DataTablePageEvent, DataTableSortEvent } from "primevue/datatable";
import type { MenuItem } from "primevue/menuitem";
import type { SortField, TrashedFile } from "../models.ts";

const MENU_ITEMS: MenuItem[] = [
    { label: "Restore", icon: "pi pi-undo", command: restoreMenuTargets },
    { separator: true },
    { label: "Delete forever", icon: "pi pi-trash", danger: true, command: purgeMenuTargets },
];

const fileStore = useFileStore();
const toastStore = useToastStore();
const settingsStore = useSettingsStore();
const confirm = useConfirm();

const rowOptions = [25, 50, 100];

const items = ref<TrashedFile[]>([]);
const loading = ref<boolean>(true);
const { pageSize, nextCursor, cursor, first, total, turn, reset } = useCursorPager(rowOptions[0]);
const sortField = ref<SortField>("date");
const sortOrder = ref<number>(-1);

const selection = ref<TrashedFile[]>([]);
const { menuTarget, menuTargets, showMenu } = useRowMenu(selection, "rowMenu");

const pagerTotal = computed<number>(() => total(items.value.length));

// Only the newest request may write the table, so a slow old page cannot replace a new one.
let latestRequest = 0;

async function getData(): Promise<void> {
    const request = ++latestRequest;
    loading.value = true;
    try {
        const response = await fileStore.listTrash({
            cursor: cursor.value,
            page_size: pageSize.value,
            sort: sortField.value,
            order: ORDER_BY_SORT_ORDER[sortOrder.value],
            folders_first: settingsStore.settings.sort_folders_first,
        });
        if (request !== latestRequest) return;
        items.value = response.data;
        const selectedIds = new Set(selection.value.map((item) => item.id));
        selection.value = response.data.filter((item) => selectedIds.has(item.id));
        nextCursor.value = response.next_cursor;
    } catch (error) {
        if (request === latestRequest) toastStore.errorResponseToast(error);
    } finally {
        if (request === latestRequest) loading.value = false;
    }
}

async function onPage(event: DataTablePageEvent): Promise<void> {
    if (turn(event)) await getData();
}

async function onSort(event: DataTableSortEvent): Promise<void> {
    sortField.value = event.sortField as SortField;
    sortOrder.value = event.sortOrder ?? -1;
    reset();
    await getData();
}

// The ids are read before the first await, because the menu clears its target when it hides.
async function restore(targets: TrashedFile[]): Promise<void> {
    const ids = targets.map((item) => item.id);
    try {
        const response = await fileStore.restore({ ids });
        toastStore.successResponseToast(response);
    } catch (error) {
        toastStore.errorResponseToast(error);
    }
    await getData();
}

async function restoreMenuTargets(): Promise<void> {
    await restore(menuTargets.value);
}

// The ids are read before the prompt opens, because the menu clears its target when it hides.
function purge(targets: TrashedFile[]): void {
    const ids = targets.map((item) => item.id);
    const what = ids.length === 1 ? `"${targets[0].name}"` : `${ids.length} items`;
    confirm.require({
        header: "Delete forever",
        message: `Delete ${what} forever? You cannot undo this.`,
        rejectProps: { label: "Cancel" },
        acceptProps: { label: "Delete forever", severity: "danger" },
        accept: async () => {
            try {
                const response = await fileStore.purge({ ids });
                toastStore.successResponseToast(response);
            } catch (error) {
                toastStore.errorResponseToast(error);
            }
            await getData();
        },
    });
}

function purgeMenuTargets(): void {
    purge(menuTargets.value);
}

onMounted(getData);
</script>

<template>
    <div class="overflow-hidden rounded-2xl border border-edge bg-panel">
        <DataTable
            v-model:selection="selection"
            v-model:context-menu-selection="menuTarget"
            :value="items"
            data-key="id"
            highlight-on-select
            lazy
            paginator
            :loading="loading"
            :first="first"
            :rows="pageSize"
            :total-records="pagerTotal"
            :sort-field="sortField"
            :sort-order="sortOrder"
            :rows-per-page-options="rowOptions"
            :paginator-template="{
                '640px': 'PrevPageLink CurrentPageReport NextPageLink',
                default: 'FirstPageLink PrevPageLink CurrentPageReport NextPageLink RowsPerPageDropdown',
            }"
            current-page-report-template="{first}–{last}"
            table-class="table-fixed"
            context-menu
            @page="onPage"
            @sort="onSort"
            @row-contextmenu="showMenu"
        >
            <template #header>
                <div class="flex min-h-8 items-center justify-between gap-3">
                    <div class="flex items-center gap-2">
                        <Button
                            label="Restore"
                            icon="pi pi-undo"
                            size="small"
                            severity="secondary"
                            variant="outlined"
                            :disabled="selection.length === 0"
                            @click="restore(selection)"
                        />
                        <Button
                            label="Delete forever"
                            icon="pi pi-trash"
                            size="small"
                            severity="danger"
                            variant="outlined"
                            :disabled="selection.length === 0"
                            @click="purge(selection)"
                        />
                    </div>
                    <div v-if="selection.length > 0" class="flex shrink-0 items-center gap-1">
                        <span class="text-sm tabular-nums text-fg-muted">{{ selection.length }} selected</span>
                        <Button
                            label="Clear"
                            icon="pi pi-times"
                            size="small"
                            severity="secondary"
                            variant="text"
                            @click="selection = []"
                        />
                    </div>
                </div>
            </template>

            <template #empty>
                <div class="flex flex-col items-center gap-1 py-8 text-center">
                    <span class="font-medium text-fg">The trash is empty</span>
                    <span class="text-sm text-fg-muted">Items you delete show up here.</span>
                </div>
            </template>

            <Column selection-mode="multiple" header-class="w-14" />

            <Column header="Name" sort-field="name" sortable>
                <template #body="{ data }">
                    <div class="flex min-w-0 items-center gap-3">
                        <span
                            class="grid size-9 shrink-0 place-items-center rounded-lg"
                            :class="itemView(data).iconBox"
                        >
                            <i :class="itemView(data).icon" />
                        </span>
                        <div class="flex min-w-0 flex-col">
                            <span class="truncate text-sm font-medium text-fg" :title="data.name">
                                {{ shownName(data, settingsStore.settings.show_extensions) }}
                            </span>
                            <span class="truncate text-xs text-fg-subtle sm:hidden">
                                {{ vueHelper.formatDate(data.trashed_at) }}
                            </span>
                        </div>
                    </div>
                </template>
            </Column>

            <Column
                header="Type"
                sort-field="type"
                sortable
                header-class="hidden w-44 md:table-cell"
                body-class="hidden md:table-cell"
            >
                <template #body="{ data }">
                    <span class="block truncate text-sm text-fg-muted">
                        {{ itemView(data).type }}
                    </span>
                </template>
            </Column>

            <Column header="Size" sort-field="size" sortable header-class="w-28">
                <template #body="{ data }">
                    <span class="text-sm tabular-nums text-fg-muted">
                        {{ itemView(data).size }}
                    </span>
                </template>
            </Column>

            <Column
                header="Deleted"
                sort-field="date"
                sortable
                header-class="hidden w-48 sm:table-cell"
                body-class="hidden sm:table-cell"
            >
                <template #body="{ data }">
                    <span class="text-sm tabular-nums text-fg-muted">{{ vueHelper.formatDate(data.trashed_at) }}</span>
                </template>
            </Column>
        </DataTable>

        <ContextMenu ref="rowMenu" :model="MENU_ITEMS" @hide="menuTarget = undefined">
            <template #item="{ item, props: itemProps }">
                <a v-bind="itemProps.action" :class="{ 'text-danger': item.danger }">
                    <span v-bind="itemProps.icon" :class="{ 'text-danger': item.danger }" />
                    <span v-bind="itemProps.label">{{ item.label }}</span>
                </a>
            </template>
        </ContextMenu>
    </div>
</template>
