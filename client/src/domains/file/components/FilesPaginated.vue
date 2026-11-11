<script setup lang="ts">
import { onMounted, ref, watch } from "vue";
import { useRouter } from "vue-router";
import { useFileStore } from "../../../pkg/stores/file_store.ts";
import { useToastStore } from "../../../pkg/stores/toast_store.ts";
import vueHelper from "../../../pkg/utils/vue_helper.ts";
import { folderRoute, joinDir } from "../../../pkg/utils/folder_route.ts";
import type { DataTablePageEvent, DataTableRowClickEvent, DataTableRowContextMenuEvent } from "primevue/datatable";
import type { MenuItem } from "primevue/menuitem";
import type { RouteLocationRaw } from "vue-router";
import type { StoredFile } from "../models.ts";

interface ItemView {
    iconBox: string;
    icon: string;
    type: string;
    size: string;
    row: string;
}

const FOLDER_VIEW: ItemView = {
    iconBox: "bg-accent-soft text-accent",
    icon: "pi pi-folder",
    type: "Folder",
    size: "—",
    row: "cursor-pointer",
};

const MENU_ITEMS: MenuItem[] = [
    { label: "Add to favorites", icon: "pi pi-star" },
    { label: "Rename", icon: "pi pi-pencil" },
    { label: "Move", icon: "pi pi-arrow-right" },
    { label: "Copy", icon: "pi pi-copy" },
    { separator: true },
    { label: "Delete", icon: "pi pi-trash", danger: true },
];

const props = defineProps<{ folderId: number | null; dir: string }>();

const fileStore = useFileStore();
const toastStore = useToastStore();
const router = useRouter();

const rowOptions = [10, 20, 50];

const files = ref<StoredFile[]>([]);
const loading = ref<boolean>(true);
const page = ref<number>(1);
const pageSize = ref<number>(rowOptions[0]);
const total = ref<number>(0);

const menuRef = ref<{ show: (event: Event) => void } | null>(null);
const menuTarget = ref<StoredFile | undefined>();

// Only the newest request may write the table, so a slow old page cannot replace a new one.
let latestRequest = 0;

async function getData(): Promise<void> {
    const request = ++latestRequest;
    loading.value = true;
    try {
        const response = await fileStore.list(props.folderId, page.value, pageSize.value);
        if (request !== latestRequest) return;
        files.value = response.data;
        total.value = response.total;
        page.value = response.page;
        pageSize.value = response.page_size;
    } catch (error) {
        if (request === latestRequest) toastStore.errorResponseToast(error);
    } finally {
        if (request === latestRequest) loading.value = false;
    }
}

async function onPage(event: DataTablePageEvent): Promise<void> {
    page.value = event.page + 1;
    pageSize.value = event.rows;
    await getData();
}

// New items are the newest in their group, so go back to the first page to show them.
async function refresh(): Promise<void> {
    page.value = 1;
    await getData();
}

function childRoute(folder: StoredFile): RouteLocationRaw {
    return folderRoute(joinDir(props.dir, folder.name));
}

function itemView(item: StoredFile): ItemView {
    if (item.is_folder) return FOLDER_VIEW;
    return {
        iconBox: "bg-inset text-fg-muted",
        icon: vueHelper.fileIcon(item.content_type),
        type: vueHelper.mimeType(item.content_type),
        size: vueHelper.formatBytes(item.size_bytes),
        row: "",
    };
}

function rowClass(item: StoredFile): string {
    return itemView(item).row;
}

// The whole folder row opens it. A click on the name link already navigates by itself.
async function onRowClick(event: DataTableRowClickEvent<StoredFile>): Promise<void> {
    if (!event.data.is_folder) return;
    if (event.originalEvent.target instanceof Element && event.originalEvent.target.closest("a")) return;
    await router.push(childRoute(event.data));
}

function onRowContextMenu(event: DataTableRowContextMenuEvent): void {
    menuRef.value?.show(event.originalEvent);
}

onMounted(getData);

watch(() => props.folderId, refresh);

defineExpose({ refresh });
</script>

<template>
    <div class="overflow-hidden rounded-2xl border border-edge bg-panel">
        <DataTable
            v-model:context-menu-selection="menuTarget"
            :value="files"
            data-key="id"
            lazy
            paginator
            :loading="loading"
            :first="(page - 1) * pageSize"
            :rows="pageSize"
            :total-records="total"
            :rows-per-page-options="rowOptions"
            :paginator-template="{
                '640px': 'PrevPageLink CurrentPageReport NextPageLink',
                default: 'FirstPageLink PrevPageLink CurrentPageReport NextPageLink LastPageLink RowsPerPageDropdown',
            }"
            current-page-report-template="{first}–{last} of {totalRecords}"
            table-class="table-fixed"
            :row-class="rowClass"
            context-menu
            @page="onPage"
            @row-click="onRowClick"
            @row-contextmenu="onRowContextMenu"
        >
            <template #empty>
                <div class="flex flex-col items-center gap-1 py-8 text-center">
                    <template v-if="folderId === null">
                        <span class="font-medium text-fg">No files yet</span>
                        <span class="text-sm text-fg-muted">Files and folders you add show up here.</span>
                    </template>
                    <template v-else>
                        <span class="font-medium text-fg">This folder is empty</span>
                        <span class="text-sm text-fg-muted">Upload files or create a folder here.</span>
                    </template>
                </div>
            </template>

            <Column header="Name">
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

            <Column header="Type" header-class="hidden w-44 md:table-cell" body-class="hidden md:table-cell">
                <template #body="{ data }">
                    <span class="block truncate text-sm text-fg-muted">
                        {{ itemView(data).type }}
                    </span>
                </template>
            </Column>

            <Column header="Size" header-class="w-24">
                <template #body="{ data }">
                    <span class="text-sm tabular-nums text-fg-muted">
                        {{ itemView(data).size }}
                    </span>
                </template>
            </Column>

            <Column header="Added" header-class="hidden w-48 sm:table-cell" body-class="hidden sm:table-cell">
                <template #body="{ data }">
                    <span class="text-sm tabular-nums text-fg-muted">{{ vueHelper.formatDate(data.created_at) }}</span>
                </template>
            </Column>
        </DataTable>

        <ContextMenu ref="menuRef" :model="MENU_ITEMS" @hide="menuTarget = undefined">
            <template #item="{ item, props: itemProps }">
                <a v-bind="itemProps.action" :class="{ 'text-danger': item.danger }">
                    <span v-bind="itemProps.icon" :class="{ 'text-danger': item.danger }" />
                    <span v-bind="itemProps.label">{{ item.label }}</span>
                </a>
            </template>
        </ContextMenu>
    </div>
</template>
