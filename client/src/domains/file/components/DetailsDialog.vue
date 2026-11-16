<script setup lang="ts">
import { computed, ref } from "vue";
import { useFileStore } from "../../../pkg/stores/file_store.ts";
import { useSettingsStore } from "../../../pkg/stores/settings_store.ts";
import { useToastStore } from "../../../pkg/stores/toast_store.ts";
import vueHelper from "../../../pkg/utils/vue_helper.ts";
import { itemView } from "../../../pkg/utils/item_view.ts";
import { useCursorPager } from "../../../pkg/utils/use_cursor_pager.ts";
import { EVENT_VIEWS, eventText } from "../../../pkg/utils/activity_view.ts";
import { dirLabel } from "../../../pkg/utils/folder_route.ts";
import ItemIcon from "./ItemIcon.vue";
import type { DataTablePageEvent } from "primevue/datatable";
import type { Activity, StoredFile } from "../models.ts";

const PAGE_SIZE = 10;

// item is the row the menu opened on. dir is the open folder, such as "/Photos/Cars", and "" for Home.
const props = defineProps<{ item: StoredFile | null; dir: string }>();
const visible = defineModel<boolean>("visible", { required: true });

const fileStore = useFileStore();
const toastStore = useToastStore();
const settingsStore = useSettingsStore();

// The row shows at once. The server's copy replaces it, so the dialog shows the item as it is now.
const details = ref<StoredFile | null>(null);
const activity = ref<Activity[]>([]);
const loading = ref<boolean>(false);
const { nextCursor, cursor, first, total, turn, reset } = useCursorPager(PAGE_SIZE);

const view = computed(() => (details.value ? itemView(details.value) : null));
const location = computed<string>(() => dirLabel(props.dir));
const pagerTotal = computed<number>(() => total(activity.value.length));

// Only the newest request may write the log, so a slow old page cannot replace a new one.
let latestRequest = 0;

async function loadItem(id: number): Promise<void> {
    try {
        details.value = await fileStore.getItem(id);
    } catch (error) {
        toastStore.errorResponseToast(error);
    }
}

async function loadActivity(id: number): Promise<void> {
    const request = ++latestRequest;
    loading.value = true;
    try {
        const response = await fileStore.listActivity(id, { cursor: cursor.value, page_size: PAGE_SIZE });
        if (request !== latestRequest) return;
        activity.value = response.data;
        nextCursor.value = response.next_cursor;
    } catch (error) {
        if (request === latestRequest) toastStore.errorResponseToast(error);
    } finally {
        if (request === latestRequest) loading.value = false;
    }
}

async function onShow(): Promise<void> {
    const item = props.item;
    if (item === null) return;
    details.value = item;
    activity.value = [];
    reset();
    nextCursor.value = null;
    await Promise.all([loadItem(item.id), loadActivity(item.id)]);
}

async function onPage(event: DataTablePageEvent): Promise<void> {
    if (details.value && turn(event)) await loadActivity(details.value.id);
}
</script>

<template>
    <Dialog
        v-model:visible="visible"
        modal
        :draggable="false"
        class="w-[min(640px,calc(100vw-2rem))] rounded-2xl border-edge bg-panel"
        @show="onShow"
    >
        <template #header>
            <div v-if="details && view" class="flex min-w-0 items-center gap-3">
                <ItemIcon :item="details" />
                <span class="truncate text-lg font-semibold tracking-tight text-fg" :title="details.name">
                    {{ details.name }}
                </span>
                <i
                    v-if="details.is_hidden"
                    class="pi pi-eye-slash shrink-0 text-xs text-fg-subtle"
                    role="img"
                    aria-label="Hidden"
                    title="Hidden"
                />
                <i
                    v-if="details.is_favorite"
                    class="pi pi-star-fill shrink-0 text-xs text-accent"
                    role="img"
                    aria-label="Favorite"
                    title="Favorite"
                />
            </div>
        </template>

        <div v-if="details && view" class="flex flex-col gap-6">
            <dl class="grid grid-cols-[auto_1fr] gap-x-6 gap-y-2 text-sm">
                <dt class="text-fg-muted">Type</dt>
                <dd class="min-w-0 truncate text-fg">{{ view.type }}</dd>
                <template v-if="!details.is_folder">
                    <dt class="text-fg-muted">Size</dt>
                    <dd class="text-fg tabular-nums">
                        {{ view.size }}
                        <span class="text-fg-subtle">({{ details.size_bytes.toLocaleString() }} bytes)</span>
                    </dd>
                </template>
                <dt class="text-fg-muted">Location</dt>
                <dd class="min-w-0 truncate text-fg" :title="location">{{ location }}</dd>
                <dt class="text-fg-muted">Added</dt>
                <dd class="text-fg tabular-nums">
                    {{ vueHelper.formatDate(details.created_at, settingsStore.settings.use_24_hour_clock) }}
                </dd>
                <dt class="text-fg-muted">Changed</dt>
                <dd class="text-fg tabular-nums">
                    {{ vueHelper.formatDate(details.updated_at, settingsStore.settings.use_24_hour_clock) }}
                </dd>
            </dl>

            <div class="flex flex-col gap-2">
                <span class="text-sm font-semibold text-fg">Activity</span>
                <div class="overflow-hidden rounded-xl border border-edge">
                    <DataTable
                        :value="activity"
                        data-key="id"
                        lazy
                        :paginator="activity.length > 0"
                        size="small"
                        :loading="loading"
                        :first="first"
                        :rows="PAGE_SIZE"
                        :total-records="pagerTotal"
                        paginator-template="PrevPageLink CurrentPageReport NextPageLink"
                        current-page-report-template="{first}–{last}"
                        table-class="table-fixed"
                        @page="onPage"
                    >
                        <template #empty>
                            <span class="text-sm text-fg-muted">No activity yet.</span>
                        </template>

                        <Column header="Event">
                            <template #body="{ data }">
                                <div class="flex min-w-0 items-start gap-2 text-sm text-fg">
                                    <i
                                        class="mt-0.5 shrink-0 text-fg-muted"
                                        :class="EVENT_VIEWS[(data as Activity).event].icon"
                                    />
                                    <div class="flex min-w-0 flex-col">
                                        <span class="break-words">{{ eventText(data) }}</span>
                                        <span class="text-xs text-fg-subtle tabular-nums sm:hidden">
                                            {{
                                                vueHelper.formatDate(
                                                    data.created_at,
                                                    settingsStore.settings.use_24_hour_clock,
                                                )
                                            }}
                                        </span>
                                    </div>
                                </div>
                            </template>
                        </Column>

                        <Column
                            header="When"
                            header-class="hidden w-48 sm:table-cell"
                            body-class="hidden sm:table-cell"
                        >
                            <template #body="{ data }">
                                <span class="text-sm text-fg-muted tabular-nums">
                                    {{
                                        vueHelper.formatDate(data.created_at, settingsStore.settings.use_24_hour_clock)
                                    }}
                                </span>
                            </template>
                        </Column>
                    </DataTable>
                </div>
            </div>
        </div>
    </Dialog>
</template>
