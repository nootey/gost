<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from "vue";
import { useRouter } from "vue-router";
import { useClipboardStore } from "../../../pkg/stores/clipboard_store.ts";
import { useDownloadStore } from "../../../pkg/stores/download_store.ts";
import { useFileStore } from "../../../pkg/stores/file_store.ts";
import { useToastStore } from "../../../pkg/stores/toast_store.ts";
import vueHelper from "../../../pkg/utils/vue_helper.ts";
import { startDownload } from "../../../pkg/utils/download_link.ts";
import { folderRoute, joinDir } from "../../../pkg/utils/folder_route.ts";
import { itemView } from "../../../pkg/utils/item_view.ts";
import { useClipboardKeys } from "../../../pkg/utils/use_clipboard_keys.ts";
import { ORDER_BY_SORT_ORDER, useCursorPager } from "../../../pkg/utils/use_cursor_pager.ts";
import { useRowMenu } from "../../../pkg/utils/use_row_menu.ts";
import DetailsDialog from "./DetailsDialog.vue";
import RenameDialog from "./RenameDialog.vue";
import type {
    DataTablePageEvent,
    DataTableRowClickEvent,
    DataTableRowContextMenuEvent,
    DataTableSelectAllChangeEvent,
    DataTableSortEvent,
} from "primevue/datatable";
import type { MenuItem } from "primevue/menuitem";
import type { RouteLocationRaw } from "vue-router";
import type { SuccessResponse } from "../../../pkg/api/api_models.ts";
import type { ClipboardMode } from "../../../pkg/stores/clipboard_store.ts";
import type { ItemsForm, ListSortField, PasteForm, StoredFile } from "../models.ts";

// Hide and Add show while any target is not hidden or no favorite yet, so a mixed selection changes the rest.
// Unhide and Remove show when all targets are hidden or favorites.
const MENU_ITEMS: MenuItem[] = [
    { label: "Rename", icon: "pi pi-pencil", command: openRename, disabled: notOneTarget },
    { label: "Cut", icon: "pi pi-file-export", shortcut: "Ctrl+X", command: cutMenuTargets },
    { label: "Copy", icon: "pi pi-copy", shortcut: "Ctrl+C", command: copyMenuTargets },
    { label: "Download", icon: "pi pi-download", command: downloadMenuTargets },
    { label: "Details", icon: "pi pi-info-circle", command: openDetails, disabled: notOneTarget },
    { label: "Hide", icon: "pi pi-eye-slash", command: hideMenuTargets, visible: () => !allHidden() },
    { label: "Unhide", icon: "pi pi-eye", command: unhideMenuTargets, visible: allHidden },
    { label: "Add to favorites", icon: "pi pi-star", command: addFavorites, visible: () => !allFavorites() },
    { label: "Remove from favorites", icon: "pi pi-star-fill", command: removeFavorites, visible: allFavorites },
    { separator: true },
    { label: "Delete", icon: "pi pi-trash", danger: true, command: moveToTrash },
];

const SEARCH_DELAY_MS = 300;

// PrimeVue cannot turn off the checkbox of one row, so a copying row hides its own.
const COPYING_ROW = "[&>td[data-p-selection-column=true]>*]:invisible";

const CLIPBOARD_VIEWS: Record<ClipboardMode, { verb: string; icon: string }> = {
    cut: { verb: "cut", icon: "pi pi-file-export" },
    copy: { verb: "copied", icon: "pi pi-copy" },
};

const props = defineProps<{ folderId: number | null; dir: string }>();

const fileStore = useFileStore();
const clipboardStore = useClipboardStore();
const downloadStore = useDownloadStore();
const toastStore = useToastStore();
const router = useRouter();

const PASTE_ACTIONS: Record<ClipboardMode, (form: PasteForm) => Promise<SuccessResponse>> = {
    cut: (form) => fileStore.move(form),
    copy: (form) => fileStore.copy(form),
};

const rowOptions = [25, 50, 100];

const files = ref<StoredFile[]>([]);
const loading = ref<boolean>(true);
const { pageSize, nextCursor, cursor, first, total, turn, reset } = useCursorPager(rowOptions[0]);
const sortField = ref<ListSortField>("date");
const sortOrder = ref<number>(-1);
const search = ref<string>("");
const listedSearch = ref<string>("");
// Hidden items show greyed when this is on. The later "show hidden files" setting sets its default.
const showHidden = ref<boolean>(false);

// A copying row cannot be selected, so no action reaches it before its copy job ends.
const selection = ref<StoredFile[]>([]);
const selectableRows = computed<StoredFile[]>(() => files.value.filter((item) => !item.is_copying));
const allSelected = computed<boolean>(
    () =>
        selectableRows.value.length > 0 &&
        selectableRows.value.every((row) => selection.value.some((item) => item.id === row.id)),
);
const { menuTarget, menuTargets, showMenu } = useRowMenu(selection, "rowMenu");

const pasting = ref<boolean>(false);
const canPaste = computed<boolean>(() => clipboardStore.canPasteInto(props.folderId));
const clipboardView = computed(() => CLIPBOARD_VIEWS[clipboardStore.content?.mode ?? "cut"]);
const clipboardLabel = computed<string>(() => {
    const count = clipboardStore.count;
    return `${count} ${count === 1 ? "item" : "items"} ${clipboardView.value.verb}`;
});

const renameVisible = ref<boolean>(false);
const renameTarget = ref<StoredFile | null>(null);

const detailsVisible = ref<boolean>(false);
const detailsTarget = ref<StoredFile | null>(null);

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
            show_hidden: showHidden.value,
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
    sortField.value = event.sortField as ListSortField;
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
    if (item.is_copying) return COPYING_ROW;
    const row = itemView(item).row;
    if (clipboardStore.isCut(item.id)) return `${row} opacity-50`;
    return item.is_hidden ? `${row} opacity-60` : row;
}

function onSelectionChange(rows: StoredFile[]): void {
    selection.value = rows.filter((item) => !item.is_copying);
}

function onSelectAllChange(event: DataTableSelectAllChangeEvent): void {
    selection.value = event.checked ? selectableRows.value : [];
}

// A copying row has no menu, and the browser menu stays closed too.
function onRowContextMenu(event: DataTableRowContextMenuEvent): void {
    if ((event.data as StoredFile).is_copying) {
        event.originalEvent.preventDefault();
        menuTarget.value = undefined;
        return;
    }
    showMenu(event);
}

// The whole folder row opens it. A click on the name link already navigates by itself. A copying folder does not open.
async function onRowClick(event: DataTableRowClickEvent<StoredFile>): Promise<void> {
    if (!event.data.is_folder || event.data.is_copying) return;
    const target = event.originalEvent.target;
    if (target instanceof Element && target.closest("a, [data-p-selection-column='true']")) return;
    await router.push(childRoute(event.data));
}

function notOneTarget(): boolean {
    return menuTargets.value.length !== 1;
}

// One file is a plain download. A folder or many items come as a zip that a job builds. The zip waits in the downloads
// list in the header. The items are read before the first await, because the menu clears its target when it hides.
async function downloadMenuTargets(): Promise<void> {
    const items = menuTargets.value;
    if (items.length === 0) return;
    if (items.length === 1 && !items[0].is_folder) {
        startDownload(fileStore.downloadUrl(items[0].id));
        return;
    }
    try {
        const response = await fileStore.requestZip({ ids: items.map((item) => item.id) });
        downloadStore.add(response.download);
        toastStore.successResponseToast(response);
    } catch (error) {
        toastStore.errorResponseToast(error);
    }
}

function allFavorites(): boolean {
    return menuTargets.value.length > 0 && menuTargets.value.every((item) => item.is_favorite);
}

function allHidden(): boolean {
    return menuTargets.value.length > 0 && menuTargets.value.every((item) => item.is_hidden);
}

// The targets are read before the first await, because the menu clears its target when it hides.
async function addFavorites(): Promise<void> {
    await changeItems(fileStore.addFavorites, menuTargets.value);
}

async function removeFavorites(): Promise<void> {
    await changeItems(fileStore.removeFavorites, menuTargets.value);
}

async function hideMenuTargets(): Promise<void> {
    await changeItems(fileStore.hide, menuTargets.value);
}

async function unhideMenuTargets(): Promise<void> {
    await changeItems(fileStore.unhide, menuTargets.value);
}

// changeItems runs one action on the items, shows the server's toast and reads the page again.
async function changeItems(change: (form: ItemsForm) => Promise<SuccessResponse>, items: StoredFile[]): Promise<void> {
    try {
        const response = await change({ ids: items.map((item) => item.id) });
        toastStore.successResponseToast(response);
    } catch (error) {
        toastStore.errorResponseToast(error);
    }
    await getData();
}

function openDetails(): void {
    detailsTarget.value = menuTarget.value ?? null;
    detailsVisible.value = true;
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

function putItems(mode: ClipboardMode, items: StoredFile[]): void {
    if (items.length === 0) return;
    clipboardStore.put(
        mode,
        props.folderId,
        items.map((item) => item.id),
    );
}

function cutMenuTargets(): void {
    putItems("cut", menuTargets.value);
}

function copyMenuTargets(): void {
    putItems("copy", menuTargets.value);
}

// A paste that works clears the clipboard, for a cut and for a copy. A paste that fails keeps it, so the user can
// fix the cause and paste again.
async function paste(): Promise<void> {
    const content = clipboardStore.content;
    if (!content || pasting.value || !canPaste.value) return;
    pasting.value = true;
    try {
        const response = await PASTE_ACTIONS[content.mode]({ ids: content.ids, parent_id: props.folderId });
        // A cut or copy made during the request is a new clipboard, so it stays.
        if (clipboardStore.content === content) clipboardStore.clear();
        toastStore.successResponseToast(response);
    } catch (error) {
        toastStore.errorResponseToast(error);
    } finally {
        pasting.value = false;
    }
    await getData();
}

useClipboardKeys({
    cut: () => putItems("cut", selection.value),
    copy: () => putItems("copy", selection.value),
    paste,
});

onMounted(getData);
onBeforeUnmount(() => clearTimeout(searchTimer));

watch(() => props.folderId, onFolderChange);

defineExpose({ refresh });
</script>

<template>
    <div class="overflow-hidden rounded-2xl border border-edge bg-panel">
        <DataTable
            v-model:context-menu-selection="menuTarget"
            :selection="selection"
            :select-all="allSelected"
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
            @update:selection="onSelectionChange"
            @select-all-change="onSelectAllChange"
            @row-click="onRowClick"
            @row-contextmenu="onRowContextMenu"
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
                        <div class="flex shrink-0 items-center gap-3">
                            <div v-if="selection.length > 0" class="flex items-center gap-1">
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
                            <label
                                for="show-hidden-switch"
                                class="flex cursor-pointer items-center gap-2 text-sm text-fg-muted"
                                title="Show hidden items"
                            >
                                <i class="pi pi-eye-slash text-sm" />
                                <span class="hidden sm:inline">Show hidden</span>
                                <ToggleSwitch
                                    v-model="showHidden"
                                    input-id="show-hidden-switch"
                                    aria-label="Show hidden items"
                                    @update:model-value="refresh"
                                />
                            </label>
                        </div>
                    </div>
                    <div
                        v-if="clipboardStore.count > 0"
                        class="flex items-center justify-between gap-3 rounded-xl bg-inset py-1.5 pr-1.5 pl-3"
                    >
                        <div class="flex min-w-0 items-center gap-2 text-sm">
                            <i class="text-fg-muted" :class="clipboardView.icon" />
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
                            <div class="flex min-w-0 items-center gap-2">
                                <router-link
                                    v-if="data.is_folder && !data.is_copying"
                                    :to="childRoute(data)"
                                    class="truncate rounded-sm text-sm font-medium text-fg no-underline hover:underline"
                                    :title="data.name"
                                >
                                    {{ data.name }}
                                </router-link>
                                <span v-else class="truncate text-sm font-medium text-fg" :title="data.name">
                                    {{ data.name }}
                                </span>
                                <i
                                    v-if="data.is_hidden"
                                    class="pi pi-eye-slash shrink-0 text-xs text-fg-subtle"
                                    role="img"
                                    aria-label="Hidden"
                                    title="Hidden"
                                />
                                <i
                                    v-if="data.is_favorite"
                                    class="pi pi-star-fill shrink-0 text-xs text-accent"
                                    role="img"
                                    aria-label="Favorite"
                                    title="Favorite"
                                />
                                <Tag
                                    v-if="data.is_copying"
                                    value="Copying"
                                    icon="pi pi-clock"
                                    severity="secondary"
                                    class="shrink-0 text-xs"
                                    title="Reload the page to see when the copy is done."
                                />
                            </div>
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

            <Column
                header="Modified"
                sort-field="modified"
                sortable
                header-class="hidden w-48 xl:table-cell"
                body-class="hidden xl:table-cell"
            >
                <template #body="{ data }">
                    <span class="text-sm tabular-nums text-fg-muted">{{ vueHelper.formatDate(data.updated_at) }}</span>
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
        <DetailsDialog v-model:visible="detailsVisible" :item="detailsTarget" :dir="dir" />
    </div>
</template>
