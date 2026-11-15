<script setup lang="ts">
import { ref } from "vue";
import { storeToRefs } from "pinia";
import { useDownloadStore } from "../../../pkg/stores/download_store.ts";
import { useFileStore } from "../../../pkg/stores/file_store.ts";
import vueHelper from "../../../pkg/utils/vue_helper.ts";
import type { Download, DownloadState } from "../models.ts";

interface StateView {
    label: string;
    icon: string;
    severity: "secondary" | "success" | "danger";
    detailClass: string;
    detail: (download: Download) => string;
}

function expiry(download: Download): string {
    return `Expires ${vueHelper.formatTimeLeft(download.expires_at)}`;
}

// A failed zip shows the server's error. Any other shows its size when ready and the time until the server removes it.
const STATE_VIEWS: Record<DownloadState, StateView> = {
    building: {
        label: "Building",
        icon: "pi pi-spin pi-spinner",
        severity: "secondary",
        detailClass: "text-fg-subtle",
        detail: expiry,
    },
    ready: {
        label: "Ready",
        icon: "pi pi-check",
        severity: "success",
        detailClass: "text-fg-subtle",
        detail: (d) => `${vueHelper.formatBytes(d.size_bytes)} · ${expiry(d)}`,
    },
    failed: {
        label: "Failed",
        icon: "pi pi-times",
        severity: "danger",
        detailClass: "text-danger",
        detail: (d) => d.error ?? "",
    },
};

const visible = defineModel<boolean>("visible", { required: true });

const downloadStore = useDownloadStore();
const fileStore = useFileStore();
const { downloads } = storeToRefs(downloadStore);

const removing = ref<number[]>([]);

// No confirm: the user can ask for the zip again.
async function remove(id: number): Promise<void> {
    removing.value = [...removing.value, id];
    await downloadStore.remove(id);
    removing.value = removing.value.filter((removed) => removed !== id);
}
</script>

<template>
    <Dialog
        v-model:visible="visible"
        modal
        header="Downloads"
        :draggable="false"
        class="w-[min(560px,calc(100vw-2rem))] rounded-2xl border-edge bg-panel"
        @show="downloadStore.load()"
    >
        <div v-if="downloads.length === 0" class="flex flex-col items-center gap-1 py-8 text-center">
            <span class="font-medium text-fg">No downloads</span>
            <span class="text-sm text-fg-muted">A folder or many items download as a zip. The zip waits here.</span>
        </div>
        <ul v-else class="-mx-1 flex flex-col divide-y divide-edge">
            <li v-for="download in downloads" :key="download.id" class="flex items-center gap-3 px-1 py-3">
                <span class="grid size-9 shrink-0 place-items-center rounded-lg bg-inset text-fg-muted">
                    <i class="pi pi-box" />
                </span>
                <div class="flex min-w-0 flex-1 flex-col gap-0.5">
                    <div class="flex min-w-0 items-center gap-2">
                        <span class="truncate text-sm font-medium text-fg" :title="download.name">
                            {{ download.name }}
                        </span>
                        <Tag
                            :value="STATE_VIEWS[download.state].label"
                            :icon="STATE_VIEWS[download.state].icon"
                            :severity="STATE_VIEWS[download.state].severity"
                            class="shrink-0 text-xs"
                        />
                    </div>
                    <span
                        class="truncate text-xs"
                        :class="STATE_VIEWS[download.state].detailClass"
                        :title="STATE_VIEWS[download.state].detail(download)"
                    >
                        {{ STATE_VIEWS[download.state].detail(download) }}
                    </span>
                </div>
                <Button
                    v-if="download.state === 'ready'"
                    as="a"
                    :href="fileStore.zipUrl(download.id)"
                    download
                    size="small"
                    :severity="download.downloaded ? 'secondary' : undefined"
                    class="shrink-0 gap-2 no-underline"
                    :aria-label="`Download ${download.name}`"
                    @click="downloadStore.markDownloaded(download.id)"
                >
                    <i class="pi pi-download" />
                    <span class="hidden sm:inline">Download</span>
                </Button>
                <Button
                    icon="pi pi-times"
                    text
                    rounded
                    size="small"
                    severity="secondary"
                    class="shrink-0"
                    :aria-label="`Remove ${download.name}`"
                    title="Remove"
                    :loading="removing.includes(download.id)"
                    @click="remove(download.id)"
                />
            </li>
        </ul>
    </Dialog>
</template>
