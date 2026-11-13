<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from "vue";
import { useRouter } from "vue-router";
import { useClipboardStore } from "../../../pkg/stores/clipboard_store.ts";
import { useFileStore } from "../../../pkg/stores/file_store.ts";
import { useToastStore } from "../../../pkg/stores/toast_store.ts";
import vueHelper from "../../../pkg/utils/vue_helper.ts";
import { folderRoute, joinDir } from "../../../pkg/utils/folder_route.ts";
import { itemView } from "../../../pkg/utils/item_view.ts";
import { useClipboardKeys } from "../../../pkg/utils/use_clipboard_keys.ts";
import { ORDER_BY_SORT_ORDER, useCursorPager } from "../../../pkg/utils/use_cursor_pager.ts";
import { useRowMenu } from "../../../pkg/utils/use_row_menu.ts";
import RenameDialog from "./RenameDialog.vue";
import type { DataTablePageEvent, DataTableRowClickEvent, DataTableSortEvent } from "primevue/datatable";
import type { MenuItem } from "primevue/menuitem";
import type { RouteLocationRaw } from "vue-router";
import type { SortField, StoredFile } from "../models.ts";

const MENU_ITEMS: MenuItem[] = [
    { label: "Add to favorites", icon: "pi pi-star" },
    { label: "Rename", icon: "pi pi-pencil", command: openRename, disabled: notOneTarget },
    { label: "Cut", icon: "pi pi-file-export", shortcut: "Ctrl+X", command: cutMenuTargets },
    { label: "Copy", icon: "pi pi-copy" },
    { separator: true },
    { label: "Delete", icon: "pi pi-trash", danger: true, command: moveToTrash },
];

const SEARCH_DELAY_MS = 300;

const props = defineProps<{ folderId: number | null; dir: string }>();

const fileStore = useFileStore();
const clipboardStore = useClipboardStore();
const toastStore = useToastStore();
const router = useRouter();

const rowOptions = [25, 50, 100];

const files = ref<StoredFile[]>([]);
const loading = ref<boolean>(true);
const { pageSize, nextCursor, cursor, first, total, turn, reset } = useCursorPager(rowOptions[0]);
const sortField = ref<SortField>("date");
const sortOrder = ref<number>(-1);
const search = ref<string>("");
const listedSearch = ref<string>("");

const selection = ref<StoredFile[]>([]);
const { menuTarget, menuTargets, showMenu } = useRowMenu(selection, "rowMenu");

const pasting = ref<boolean>(false);
const canPaste = computed<boolean>(() => clipboardStore.canPasteInto(props.folderId));
const clipboardLabel = computed<string>(() => {
    const count = clipboardStore.count;
    return `${count} ${count === 1 ? "item" : "items"} cut`;
});

const renameVisible = ref<boolean>(false);
const renameTarget = ref<StoredFile | null>(null);

const pagerTotal = computed<number>(() => total(files.value.length));

// Only the newest request may write the table, so a slow old page cannot replace a new one.
let latestRequest = 0;
let searchTimer: ReturnType<typeof setTimeout> | undefined;

async function getData(): Promise<void> {
    const request = ++latestRequest;
    const query = search.value.trim();
    loading.value = true;
    try {
        const response = await fileStore.list({
            parent_id: props.folderId,
            cursor: cursor.value,
            page_size: pageSize.value,
            sort: sortField.value,
            order: ORDER_BY_SORT_ORDER[sortOrder.value],
            search: query || null,
        });
        if (request !== latestRequest) return;
        files.value = response.data;
        const selectedIds = new Set(selection.value.map((item) => item.id));
        selection.value = response.data.filter((item) => selectedIds.has(item.id));
        nextCursor.value = response.next_cursor;
        listedSearch.value = query;
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
    await refresh();
}

function onSearchInput(): void {
    clearTimeout(searchTimer);
    searchTimer = setTimeout(refresh, SEARCH_DELAY_MS);
}

// A new sort, search, folder or item can change what each page holds, so go back to the first page.
async function refresh(): Promise<void> {
    reset();
    await getData();
}

// The search belongs to the open folder. The sort stays.
async function onFolderChange(): Promise<void> {
    clearTimeout(searchTimer);
    search.value = "";
    await refresh();
}

function childRoute(folder: StoredFile): RouteLocationRaw {
    return folderRoute(joinDir(props.dir, folder.name));
}

function rowClass(item: StoredFile): string {
    const row = itemView(item).row;
    return clipboardStore.isCut(item.id) ? `${row} opacity-50` : row;
}

// The whole folder row opens it. A click on the name link already navigates by itself.
async function onRowClick(event: DataTableRowClickEvent<StoredFile>): Promise<void> {
    if (!event.data.is_folder) return;
    const target = event.originalEvent.target;
    if (target instanceof Element && target.closest("a, [data-p-selection-column='true']")) return;
    await router.push(childRoute(event.data));
}

function notOneTarget(): boolean {
    return menuTargets.value.length !== 1;
}

function openRename(): void {
    renameTarget.value = menuTarget.value ?? null;
    renameVisible.value = true;
}

// The ids are read before the first await, because the menu clears its target when it hides.
async function moveToTrash(): Promise<void> {
    const ids = menuTargets.value.map((item) => item.id);
    try {
        const response = await fileStore.trash({ ids });
        clipboardStore.drop(ids);
        toastStore.successResponseToast(response);
    } catch (error) {
        toastStore.errorResponseToast(error);
    }
    await getData();
}

function cutItems(items: StoredFile[]): void {
    if (items.length === 0) return;
    clipboardStore.cut(
        props.folderId,
        items.map((item) => item.id),
    );
}

function cutMenuTargets(): void {
    cutItems(menuTargets.value);
}

// A paste that fails keeps the clipboard, so the user can fix the cause and paste again.
async function paste(): Promise<void> {
    const content = clipboardStore.content;
    if (!content || pasting.value || !canPaste.value) return;
    pasting.value = true;
    try {
        const response = await fileStore.move({ ids: content.ids, parent_id: props.folderId });
        // A cut made during the request is a new clipboard, so it stays.
        if (clipboardStore.content === content) clipboardStore.clear();
        toastStore.successResponseToast(response);
    } catch (error) {
        toastStore.errorResponseToast(error);
    } finally {
        pasting.value = false;
    }
    await getData();
}

useClipboardKeys({ cut: () => cutItems(selection.value), paste });

onMounted(getData);
onBeforeUnmount(() => clearTimeout(searchTimer));

watch(() => props.folderId, onFolderChange);

defineExpose({ refresh });
</script>

<template>
    <div class="overflow-hidden rounded-2xl border border-edge bg-panel">
        <DataTable
            v-model:selection="selection"
            v-model:context-menu-selection="menuTarget"
            :value="files"
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
            :row-class="rowClass"
            context-menu
            @page="onPage"
            @sort="onSort"
            @row-click="onRowClick"
            @row-contextmenu="showMenu"
        >
            <template #header>
                <div class="flex flex-col gap-3">
                    <div class="flex items-center justify-between gap-3">
                        <IconField class="min-w-0 flex-1 sm:flex-none">
                            <InputIcon class="pi pi-search" />
                            <InputText
                                v-model="search"
                                type="search"
                                placeholder="Search this folder"
                                aria-label="Search this folder"
                                maxlength="255"
                                size="small"
                                class="w-full rounded-xl sm:w-72"
                                @update:model-value="onSearchInput"
                            />
                        </IconField>
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
                    <div
                        v-if="clipboardStore.count > 0"
                        class="flex items-center justify-between gap-3 rounded-xl bg-inset py-1.5 pr-1.5 pl-3"
                    >
                        <div class="flex min-w-0 items-center gap-2 text-sm">
                            <i class="pi pi-file-export text-fg-muted" />
                            <span class="shrink-0 font-medium text-fg">{{ clipboardLabel }}</span>
                            <span v-if="!canPaste" class="hidden truncate text-fg-muted sm:inline">
                                Open another folder to paste.
                            </span>
                        </div>
                        <div class="flex shrink-0 items-center gap-1">
                            <Button
                                label="Paste here"
                                icon="pi pi-clipboard"
                                size="small"
                                :disabled="!canPaste"
                                :loading="pasting"
                                @click="paste"
                            />
                            <Button
                                label="Cancel"
                                icon="pi pi-times"
                                size="small"
                                severity="secondary"
                                variant="text"
                                @click="clipboardStore.clear()"
                            />
                        </div>
                    </div>
                </div>
            </template>

            <template #empty>
                <div class="flex flex-col items-center gap-1 py-8 text-center">
                    <template v-if="listedSearch">
                        <span class="font-medium text-fg">No matches</span>
                        <span class="text-sm text-fg-muted">Nothing in this folder matches "{{ listedSearch }}".</span>
                    </template>
                    <template v-else-if="folderId === null">
                        <span class="font-medium text-fg">No files yet</span>
                        <span class="text-sm text-fg-muted">Files and folders you add show up here.</span>
                    </template>
                    <template v-else>
                        <span class="font-medium text-fg">This folder is empty</span>
                        <span class="text-sm text-fg-muted">Upload files or create a folder here.</span>
                    </template>
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
                            <router-link
                                v-if="data.is_folder"
                                :to="childRoute(data)"
                                class="truncate rounded-sm text-sm font-medium text-fg no-underline hover:underline"
                                :title="data.name"
                            >
                                {{ data.name }}
                            </router-link>
                            <span v-else class="truncate text-sm font-medium text-fg" :title="data.name">
                                {{ data.name }}
                            </span>
                            <span class="truncate text-xs text-fg-subtle sm:hidden">
                                {{ vueHelper.formatDate(data.created_at) }}
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
                header="Added"
                sort-field="date"
                sortable
                header-class="hidden w-48 sm:table-cell"
                body-class="hidden sm:table-cell"
            >
                <template #body="{ data }">
                    <span class="text-sm tabular-nums text-fg-muted">{{ vueHelper.formatDate(data.created_at) }}</span>
                </template>
            </Column>
        </DataTable>

        <ContextMenu ref="rowMenu" :model="MENU_ITEMS" @hide="menuTarget = undefined">
            <template #item="{ item, props: itemProps }">
                <a v-bind="itemProps.action" :class="{ 'text-danger': item.danger }">
                    <span v-bind="itemProps.icon" :class="{ 'text-danger': item.danger }" />
                    <span v-bind="itemProps.label">{{ item.label }}</span>
                    <span v-if="item.shortcut" class="ml-auto pl-6 text-xs text-fg-subtle">{{ item.shortcut }}</span>
                </a>
            </template>
        </ContextMenu>

        <RenameDialog v-model:visible="renameVisible" :item="renameTarget" @renamed="getData" />
    </div>
</template>
