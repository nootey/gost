<script setup lang="ts">
import { onMounted, ref } from "vue";
import { useFileStore } from "../../../pkg/stores/file_store.ts";
import { useToastStore } from "../../../pkg/stores/toast_store.ts";
import vueHelper from "../../../pkg/utils/vue_helper.ts";
import type { DataTablePageEvent } from "primevue/datatable";
import type { StoredFile } from "../models.ts";

const fileStore = useFileStore();
const toastStore = useToastStore();

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
        const response = await fileStore.list(page.value, pageSize.value);
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

// New uploads are the newest files, so go back to the first page to show them.
async function refresh(): Promise<void> {
    page.value = 1;
    await getData();
}

onMounted(getData);

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
            @page="onPage"
        >
            <template #empty>
                <div class="flex flex-col items-center gap-1 py-8 text-center">
                    <span class="font-medium text-fg">No files yet</span>
                    <span class="text-sm text-fg-muted">Files you upload show up here.</span>
                </div>
            </template>

            <Column header="Name">
                <template #body="{ data }">
                    <div class="flex min-w-0 items-center gap-3">
                        <span class="grid size-9 shrink-0 place-items-center rounded-lg bg-inset text-fg-muted">
                            <i :class="vueHelper.fileIcon(data.content_type)" />
                        </span>
                        <div class="flex min-w-0 flex-col">
                            <span class="truncate text-sm font-medium text-fg" :title="data.name">
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
                        {{ vueHelper.mimeType(data.content_type) }}
                    </span>
                </template>
            </Column>

            <Column header="Size" header-class="w-24">
                <template #body="{ data }">
                    <span class="text-sm tabular-nums text-fg-muted">{{ vueHelper.formatBytes(data.size_bytes) }}</span>
                </template>
            </Column>

            <Column header="Uploaded" header-class="hidden w-48 sm:table-cell" body-class="hidden sm:table-cell">
                <template #body="{ data }">
                    <span class="text-sm tabular-nums text-fg-muted">{{ vueHelper.formatDate(data.created_at) }}</span>
                </template>
            </Column>
        </DataTable>
    </div>
</template>
