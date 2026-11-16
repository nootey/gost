<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { useRoute, useRouter } from "vue-router";
import { useFileStore } from "../../pkg/stores/file_store.ts";
import { useToastStore } from "../../pkg/stores/toast_store.ts";
import UploadDropzone from "../file/components/UploadDropzone.vue";
import FilesPaginated from "../file/components/FilesPaginated.vue";
import FolderBreadcrumb from "../file/components/FolderBreadcrumb.vue";
import NewFolderDialog from "../file/components/NewFolderDialog.vue";
import { joinDir } from "../../pkg/utils/folder_route.ts";
import type { PathEntry } from "../file/models.ts";

const route = useRoute();
const router = useRouter();
const fileStore = useFileStore();
const toastStore = useToastStore();

const filesRef = ref<{ refresh: () => Promise<void> } | null>(null);

const routeDir = computed<string>(() => (typeof route.query.dir === "string" ? route.query.dir : ""));

// The page switches to a folder only after its path loads, so an unknown folder never shows as an empty one.
const folderId = ref<number | null>(null);
const path = ref<PathEntry[]>([]);
const ready = ref<boolean>(false);
const newFolderVisible = ref<boolean>(false);

const folderName = computed<string | undefined>(() => path.value.at(-1)?.name);
const dir = computed<string>(() => path.value.reduce((parent, entry) => joinDir(parent, entry.name), ""));

// Only the newest request may switch the folder, so a slow old one cannot undo a later click.
let latestRequest = 0;

async function openFolder(target: string): Promise<void> {
    const request = ++latestRequest;
    if (target === "") {
        folderId.value = null;
        path.value = [];
        ready.value = true;
        return;
    }

    try {
        const response = await fileStore.resolveFolder(target);
        if (request !== latestRequest) return;
        path.value = response.path;
        folderId.value = response.path.at(-1)?.id ?? null;
        ready.value = true;
    } catch (error) {
        if (request !== latestRequest) return;
        toastStore.errorResponseToast(error);
        await router.replace({ name: "dashboard" });
    }
}

async function onChanged(): Promise<void> {
    await filesRef.value?.refresh();
}

watch(routeDir, openFolder, { immediate: true });
</script>

<template>
    <div class="flex flex-col gap-8">
        <div class="flex flex-col gap-1">
            <h1 class="text-xl font-semibold tracking-tight text-fg sm:text-2xl">Files</h1>
            <p class="text-sm text-fg-muted">Files are stored on this server's disk. Only you can see your files.</p>
        </div>

        <UploadDropzone :folder-id="folderId" :folder-name="folderName" @changed="onChanged" />

        <div class="flex flex-col gap-3">
            <div class="flex items-center justify-between gap-3">
                <FolderBreadcrumb :path="path" />
                <Button
                    label="New folder"
                    icon="pi pi-folder-plus"
                    size="small"
                    severity="secondary"
                    variant="outlined"
                    class="shrink-0"
                    :disabled="!ready"
                    @click="newFolderVisible = true"
                />
            </div>

            <FilesPaginated v-if="ready" ref="filesRef" :folder-id="folderId" :dir="dir" />
        </div>

        <NewFolderDialog v-model:visible="newFolderVisible" :folder-id="folderId" @created="onChanged" />
    </div>
</template>
