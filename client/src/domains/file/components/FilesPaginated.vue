<script setup lang="ts">
import { onMounted, ref, watch } from "vue";
import { useRouter } from "vue-router";
import { useFileStore } from "../../../pkg/stores/file_store.ts";
import { useToastStore } from "../../../pkg/stores/toast_store.ts";
import vueHelper from "../../../pkg/utils/vue_helper.ts";
import type { DataTablePageEvent, DataTableRowClickEvent } from "primevue/datatable";
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

const props = defineProps<{ folderId: number | null }>();

const fileStore = useFileStore();
const toastStore = useToastStore();
const router = useRouter();

const rowOptions = [10, 20, 50];

const files = ref<StoredFile[]>([]);
const loading = ref<boolean>(true);
const page = ref<number>(1);
const pageSize = ref<number>(rowOptions[0]);
const total = ref<number>(0);

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

function folderRoute(folder: StoredFile): RouteLocationRaw {
    return { name: "folder", params: { id: folder.id } };
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
    await router.push(folderRoute(event.data));
}

onMounted(getData);

watch(() => props.folderId, refresh);

defineExpose({ refresh });
</script>

<template>
    <div class="overflow-hidden rounded-2xl border border-edge bg-panel">
        <DataTable
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
            @page="onPage"
            @row-click="onRowClick"
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
                                :to="folderRoute(data)"
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
    </div>
</template>
