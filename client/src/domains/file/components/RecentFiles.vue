<script setup lang="ts">
import { onMounted, ref } from "vue";
import { useRouter } from "vue-router";
import { useFileStore } from "../../../pkg/stores/file_store.ts";
import { useSettingsStore } from "../../../pkg/stores/settings_store.ts";
import { useToastStore } from "../../../pkg/stores/toast_store.ts";
import vueHelper from "../../../pkg/utils/vue_helper.ts";
import { dirLabel, folderRoute, namesDir } from "../../../pkg/utils/folder_route.ts";
import { isViewable, itemView, shownName } from "../../../pkg/utils/item_view.ts";
import { useRowMenu } from "../../../pkg/utils/use_row_menu.ts";
import { useViewRoute } from "../../../pkg/utils/use_view_route.ts";
import DetailsDialog from "./DetailsDialog.vue";
import ItemIcon from "./ItemIcon.vue";
import FileViewer from "./FileViewer.vue";
import type { MenuItem } from "primevue/menuitem";
import type { RouteLocationRaw } from "vue-router";
import type { RecentFile } from "../models.ts";

const MENU_ITEMS: MenuItem[] = [
    { label: "Open", icon: "pi pi-eye", command: openMenuTarget, visible: viewableTarget },
    { label: "Show in folder", icon: "pi pi-directions", command: showMenuTarget },
    { label: "Details", icon: "pi pi-info-circle", command: openDetails },
];

const fileStore = useFileStore();
const toastStore = useToastStore();
const settingsStore = useSettingsStore();
const router = useRouter();
const { viewRoute } = useViewRoute();

const items = ref<RecentFile[]>([]);
const loading = ref<boolean>(true);

// The list has no bulk actions, so nothing is ever selected, and the menu acts on the row it opened on.
const { menuTarget, showMenu } = useRowMenu(ref<RecentFile[]>([]), "rowMenu");

const detailsVisible = ref<boolean>(false);
const detailsTarget = ref<RecentFile | null>(null);

async function getData(): Promise<void> {
    loading.value = true;
    try {
        const response = await fileStore.listRecents();
        items.value = response.files;
    } catch (error) {
        toastStore.errorResponseToast(error);
    } finally {
        loading.value = false;
    }
}

function parentRoute(item: RecentFile): RouteLocationRaw {
    return folderRoute(namesDir(item.path));
}

// A file opens in the viewer over this list. A copying file has no bytes yet, so it does not open.
function openRoute(item: RecentFile): RouteLocationRaw | null {
    return isViewable(item) ? viewRoute(item.id) : null;
}

function rowClass(item: RecentFile): string {
    return item.is_copying ? "opacity-60" : "";
}

function viewableTarget(): boolean {
    return menuTarget.value !== undefined && isViewable(menuTarget.value);
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

onMounted(getData);
</script>

<template>
    <div class="overflow-hidden rounded-2xl border border-edge bg-panel">
        <DataTable
            v-model:context-menu-selection="menuTarget"
            :value="items"
            data-key="id"
            :loading="loading"
            table-class="table-fixed"
            :row-class="rowClass"
            context-menu
            @row-contextmenu="showMenu"
        >
            <template #empty>
                <div class="flex flex-col items-center gap-1 py-8 text-center">
                    <span class="font-medium text-fg">No files yet</span>
                    <span class="text-sm text-fg-muted">The files you upload or change in Files show here.</span>
                </div>
            </template>

            <Column header="Name">
                <template #body="{ data }">
                    <div class="flex min-w-0 items-center gap-3">
                        <ItemIcon :item="data" :to="openRoute(data)" />
                        <div class="flex min-w-0 flex-col">
                            <router-link
                                v-if="openRoute(data)"
                                :to="openRoute(data)!"
                                class="truncate rounded-sm text-sm font-medium text-fg no-underline hover:underline"
                                :title="data.name"
                            >
                                {{ shownName(data, settingsStore.settings.show_extensions) }}
                            </router-link>
                            <span v-else class="truncate text-sm font-medium text-fg" :title="data.name">
                                {{ shownName(data, settingsStore.settings.show_extensions) }}
                            </span>
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

            <Column header="Type" header-class="hidden w-40 lg:table-cell" body-class="hidden lg:table-cell">
                <template #body="{ data }">
                    <span class="block truncate text-sm text-fg-muted">
                        {{ itemView(data).type }}
                    </span>
                </template>
            </Column>

            <Column header="Size" header-class="w-28">
                <template #body="{ data }">
                    <span class="text-sm tabular-nums text-fg-muted">
                        {{ itemView(data).size }}
                    </span>
                </template>
            </Column>

            <Column header="Modified" header-class="hidden w-48 sm:table-cell" body-class="hidden sm:table-cell">
                <template #body="{ data }">
                    <span class="text-sm tabular-nums text-fg-muted">
                        {{ vueHelper.formatDate(data.updated_at, settingsStore.settings.use_24_hour_clock) }}
                    </span>
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

        <DetailsDialog
            v-model:visible="detailsVisible"
            :item="detailsTarget"
            :dir="namesDir(detailsTarget?.path ?? [])"
        />
        <FileViewer :items="items" />
    </div>
</template>
