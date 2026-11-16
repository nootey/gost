<script setup lang="ts">
import { computed, onMounted, ref } from "vue";
import { useRouter } from "vue-router";
import { useFileStore } from "../../../pkg/stores/file_store.ts";
import { useToastStore } from "../../../pkg/stores/toast_store.ts";
import vueHelper from "../../../pkg/utils/vue_helper.ts";
import { dirLabel, folderRoute, joinDir, namesDir } from "../../../pkg/utils/folder_route.ts";
import { isPicture, itemView } from "../../../pkg/utils/item_view.ts";
import { ORDER_BY_SORT_ORDER, useCursorPager } from "../../../pkg/utils/use_cursor_pager.ts";
import { useRowMenu } from "../../../pkg/utils/use_row_menu.ts";
import { useViewRoute } from "../../../pkg/utils/use_view_route.ts";
import DetailsDialog from "./DetailsDialog.vue";
import ItemIcon from "./ItemIcon.vue";
import PictureViewer from "./PictureViewer.vue";
import type { DataTablePageEvent, DataTableRowClickEvent, DataTableSortEvent } from "primevue/datatable";
import type { MenuItem } from "primevue/menuitem";
import type { RouteLocationRaw } from "vue-router";
import type { Favorite, SortField } from "../models.ts";

const MENU_ITEMS: MenuItem[] = [
    { label: "Open", icon: "pi pi-folder-open", command: openMenuTarget, visible: oneFolderTarget },
    { label: "Open", icon: "pi pi-image", command: openMenuTarget, visible: onePictureTarget },
    { label: "Show in folder", icon: "pi pi-directions", command: showMenuTarget, disabled: notOneTarget },
    { label: "Details", icon: "pi pi-info-circle", command: openDetails, disabled: notOneTarget },
    { separator: true },
    { label: "Remove from favorites", icon: "pi pi-star-fill", command: removeMenuTargets },
];

const fileStore = useFileStore();
const toastStore = useToastStore();
const router = useRouter();
const { viewRoute } = useViewRoute();

const rowOptions = [25, 50, 100];

const items = ref<Favorite[]>([]);
const loading = ref<boolean>(true);
const { pageSize, nextCursor, cursor, first, total, turn, reset } = useCursorPager(rowOptions[0]);
const sortField = ref<SortField>("name");
const sortOrder = ref<number>(1);
const showHidden = ref<boolean>(false);

const selection = ref<Favorite[]>([]);
const { menuTarget, menuTargets, showMenu } = useRowMenu(selection, "rowMenu");

const detailsVisible = ref<boolean>(false);
const detailsTarget = ref<Favorite | null>(null);
const detailsDir = computed<string>(() => namesDir(detailsTarget.value?.path ?? []));

const pagerTotal = computed<number>(() => total(items.value.length));

// Only the newest request may write the table, so a slow old page cannot replace a new one.
let latestRequest = 0;

async function getData(): Promise<void> {
    const request = ++latestRequest;
    loading.value = true;
    try {
        const response = await fileStore.listFavorites({
            cursor: cursor.value,
            page_size: pageSize.value,
            sort: sortField.value,
            order: ORDER_BY_SORT_ORDER[sortOrder.value],
            show_hidden: showHidden.value,
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
    sortOrder.value = event.sortOrder ?? 1;
    await refresh();
}

// A new sort or the hidden toggle can change what each page holds, so go back to the first page.
async function refresh(): Promise<void> {
    reset();
    await getData();
}

function parentRoute(item: Favorite): RouteLocationRaw {
    return folderRoute(namesDir(item.path));
}

// A folder opens in Files, and a picture opens in the viewer. Other files do not open.
function openRoute(item: Favorite): RouteLocationRaw | null {
    if (item.is_folder) return folderRoute(joinDir(namesDir(item.path), item.name));
    if (isPicture(item)) return viewRoute(item.id);
    return null;
}

function rowClass(item: Favorite): string {
    const row = itemView(item).row;
    return item.is_hidden ? `${row} opacity-60` : row;
}

// The whole row of a folder or a picture opens it. A click on a link already navigates by itself.
async function onRowClick(event: DataTableRowClickEvent<Favorite>): Promise<void> {
    const to = openRoute(event.data);
    if (to === null) return;
    const target = event.originalEvent.target;
    if (target instanceof Element && target.closest("a, [data-p-selection-column='true']")) return;
    await router.push(to);
}

function notOneTarget(): boolean {
    return menuTargets.value.length !== 1;
}

function oneFolderTarget(): boolean {
    return menuTargets.value.length === 1 && menuTargets.value[0].is_folder;
}

function onePictureTarget(): boolean {
    return menuTargets.value.length === 1 && isPicture(menuTargets.value[0]);
}

// The target is read before the first await, because the menu clears its target when it hides.
async function openMenuTarget(): Promise<void> {
    const to = menuTarget.value ? openRoute(menuTarget.value) : null;
    if (to) await router.push(to);
}

async function showMenuTarget(): Promise<void> {
    const item = menuTarget.value;
    if (item) await router.push(parentRoute(item));
}

function openDetails(): void {
    detailsTarget.value = menuTarget.value ?? null;
    detailsVisible.value = true;
}

// The ids are read before the first await, because the menu clears its target when it hides.
async function remove(targets: Favorite[]): Promise<void> {
    const ids = targets.map((item) => item.id);
    try {
        const response = await fileStore.removeFavorites({ ids });
        toastStore.successResponseToast(response);
    } catch (error) {
        toastStore.errorResponseToast(error);
    }
    await getData();
}

async function removeMenuTargets(): Promise<void> {
    await remove(menuTargets.value);
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
            :row-class="rowClass"
            context-menu
            @page="onPage"
            @sort="onSort"
            @row-click="onRowClick"
            @row-contextmenu="showMenu"
        >
            <template #header>
                <div class="flex min-h-8 items-center justify-between gap-3">
                    <Button
                        label="Remove from favorites"
                        icon="pi pi-star-fill"
                        size="small"
                        severity="secondary"
                        variant="outlined"
                        :disabled="selection.length === 0"
                        @click="remove(selection)"
                    />
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
                            for="favorites-show-hidden-switch"
                            class="flex cursor-pointer items-center gap-2 text-sm text-fg-muted"
                            title="Show hidden items"
                        >
                            <i class="pi pi-eye-slash text-sm" />
                            <span class="hidden sm:inline">Show hidden</span>
                            <ToggleSwitch
                                v-model="showHidden"
                                input-id="favorites-show-hidden-switch"
                                aria-label="Show hidden items"
                                @update:model-value="refresh"
                            />
                        </label>
                    </div>
                </div>
            </template>

            <template #empty>
                <div class="flex flex-col items-center gap-1 py-8 text-center">
                    <span class="font-medium text-fg">No favorites yet</span>
                    <span class="text-sm text-fg-muted">
                        Right-click an item in Files and pick "Add to favorites".
                    </span>
                </div>
            </template>

            <Column selection-mode="multiple" header-class="w-14" />

            <Column header="Name" sort-field="name" sortable>
                <template #body="{ data }">
                    <div class="flex min-w-0 items-center gap-3">
                        <ItemIcon :item="data" />
                        <div class="flex min-w-0 flex-col">
                            <div class="flex min-w-0 items-center gap-2">
                                <router-link
                                    v-if="openRoute(data)"
                                    :to="openRoute(data)!"
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
                            </div>
                            <router-link
                                :to="parentRoute(data)"
                                class="truncate text-xs text-fg-subtle no-underline hover:underline md:hidden"
                                :title="dirLabel(namesDir(data.path))"
                            >
                                {{ dirLabel(namesDir(data.path)) }}
                            </router-link>
                        </div>
                    </div>
                </template>
            </Column>

            <Column header="Location" header-class="hidden w-56 md:table-cell" body-class="hidden md:table-cell">
                <template #body="{ data }">
                    <router-link
                        :to="parentRoute(data)"
                        class="flex min-w-0 items-center gap-1.5 rounded-sm text-sm text-fg-muted no-underline hover:text-fg hover:underline"
                        :title="`Show in ${dirLabel(namesDir(data.path))}`"
                    >
                        <i class="pi pi-folder-open shrink-0 text-xs" />
                        <span class="truncate">{{ dirLabel(namesDir(data.path)) }}</span>
                    </router-link>
                </template>
            </Column>

            <Column
                header="Type"
                sort-field="type"
                sortable
                header-class="hidden w-40 lg:table-cell"
                body-class="hidden lg:table-cell"
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
                <a v-bind="itemProps.action">
                    <span v-bind="itemProps.icon" />
                    <span v-bind="itemProps.label">{{ item.label }}</span>
                </a>
            </template>
        </ContextMenu>

        <DetailsDialog v-model:visible="detailsVisible" :item="detailsTarget" :dir="detailsDir" />
        <PictureViewer :items="items" />
    </div>
</template>
