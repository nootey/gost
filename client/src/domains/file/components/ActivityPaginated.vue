<script setup lang="ts">
import { computed, onMounted, ref } from "vue";
import { useFileStore } from "../../../pkg/stores/file_store.ts";
import { useToastStore } from "../../../pkg/stores/toast_store.ts";
import vueHelper from "../../../pkg/utils/vue_helper.ts";
import { EVENT_VIEWS, eventText } from "../../../pkg/utils/activity_view.ts";
import { useCursorPager } from "../../../pkg/utils/use_cursor_pager.ts";
import type { DataTablePageEvent } from "primevue/datatable";
import type { Activity } from "../models.ts";

const fileStore = useFileStore();
const toastStore = useToastStore();

const rowOptions = [25, 50, 100];

const items = ref<Activity[]>([]);
const loading = ref<boolean>(true);
const { pageSize, nextCursor, cursor, first, total, turn } = useCursorPager(rowOptions[0]);

const pagerTotal = computed<number>(() => total(items.value.length));

// Only the newest request may write the table, so a slow old page cannot replace a new one.
let latestRequest = 0;

async function getData(): Promise<void> {
    const request = ++latestRequest;
    loading.value = true;
    try {
        const response = await fileStore.listUserActivity({ cursor: cursor.value, page_size: pageSize.value });
        if (request !== latestRequest) return;
        items.value = response.data;
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

onMounted(getData);
</script>

<template>
    <div class="overflow-hidden rounded-2xl border border-edge bg-panel">
        <DataTable
            :value="items"
            data-key="id"
            lazy
            paginator
            :loading="loading"
            :first="first"
            :rows="pageSize"
            :total-records="pagerTotal"
            :rows-per-page-options="rowOptions"
            :paginator-template="{
                '640px': 'PrevPageLink CurrentPageReport NextPageLink',
                default: 'FirstPageLink PrevPageLink CurrentPageReport NextPageLink RowsPerPageDropdown',
            }"
            current-page-report-template="{first}–{last}"
            table-class="table-fixed"
            @page="onPage"
        >
            <template #empty>
                <div class="flex flex-col items-center gap-1 py-8 text-center">
                    <span class="font-medium text-fg">No activity yet</span>
                    <span class="text-sm text-fg-muted">Uploads, renames, moves and deletes show here.</span>
                </div>
            </template>

            <Column header="Event">
                <template #body="{ data }">
                    <div class="flex min-w-0 items-center gap-3">
                        <span class="grid size-9 shrink-0 place-items-center rounded-lg bg-inset text-fg-muted">
                            <i :class="EVENT_VIEWS[(data as Activity).event].icon" />
                        </span>
                        <div class="flex min-w-0 flex-col">
                            <span class="truncate text-sm font-medium text-fg" :title="data.metadata.name">
                                {{ data.metadata.name }}
                            </span>
                            <span class="break-words text-xs text-fg-muted">{{ eventText(data) }}</span>
                            <span class="text-xs text-fg-subtle tabular-nums sm:hidden">
                                {{ vueHelper.formatDate(data.created_at) }}
                            </span>
                        </div>
                    </div>
                </template>
            </Column>

            <Column header="When" header-class="hidden w-48 sm:table-cell" body-class="hidden sm:table-cell">
                <template #body="{ data }">
                    <span class="text-sm text-fg-muted tabular-nums">{{ vueHelper.formatDate(data.created_at) }}</span>
                </template>
            </Column>
        </DataTable>
    </div>
</template>
